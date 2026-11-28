package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureServer 返回一个记录请求体的 204 服务,用于观察客户端转发行为。
func captureServer(t *testing.T, status int) (*httptest.Server, *[]string) {
	t.Helper()
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		bodies = append(bodies, string(buf[:n]))
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

// TestHookGateFailsClosedWhenDaemonDown 闸门客户端(--gate):daemon 不可达
// 时,PreToolUse 的可变工具必须输出 deny JSON(fail-closed);只读工具与其余
// 事件保持静默(yolo 下没有原生弹窗,空响应=放行,必须显式拦)。
func TestHookGateFailsClosedWhenDaemonDown(t *testing.T) {
	paths, _ := newTestPaths(t, 1) // 端口 1 = 无服务
	var stdout, stderr bytes.Buffer
	runHook([]string{"--dir", paths.Dir, "--gate", "gemini"},
		strings.NewReader(`{"session_id":"s1","hook_event_name":"BeforeTool","tool_name":"run_shell_command","tool_input":{"command":"npm test"},"cwd":"/tmp"}`),
		&stdout, &stderr)
	var m map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &m); err != nil || m["decision"] != "deny" {
		t.Fatalf("闸门 + daemon 宕机应输出 deny,实际 %q / err=%v", stdout.String(), err)
	}

	// 只读工具:静默放行(拦读是更大的灾难)
	stdout.Reset()
	runHook([]string{"--dir", paths.Dir, "--gate", "gemini"},
		strings.NewReader(`{"session_id":"s1","hook_event_name":"BeforeTool","tool_name":"read_file","tool_input":{"path":"a.go"},"cwd":"/tmp"}`),
		&stdout, &stderr)
	if stdout.Len() != 0 {
		t.Fatalf("只读工具不应输出决策: %q", stdout.String())
	}

	// 非 PreToolUse 事件:静默
	stdout.Reset()
	runHook([]string{"--dir", paths.Dir, "--gate", "gemini"},
		strings.NewReader(`{"session_id":"s1","hook_event_name":"AfterAgent","prompt_response":"done","cwd":"/tmp"}`),
		&stdout, &stderr)
	if stdout.Len() != 0 {
		t.Fatalf("非闸门事件不应输出决策: %q", stdout.String())
	}

	// 不带 --gate 的同一调用:保持历史 fail-open(配置损坏时静默退出)
	stdout.Reset()
	runHook([]string{"--dir", paths.Dir, "gemini"},
		strings.NewReader(`{"session_id":"s1","hook_event_name":"BeforeTool","tool_name":"run_shell_command","tool_input":{"command":"npm test"},"cwd":"/tmp"}`),
		&stdout, &stderr)
	if stdout.Len() != 0 {
		t.Fatalf("无 --gate 应保持静默: %q", stdout.String())
	}
}

// TestHookGateFailsClosedOn204 闸门事件的 204 = 网关没按闸门处理(配置
// 漂移),按 fail-closed 输出 deny。
func TestHookGateFailsClosedOn204(t *testing.T) {
	srv, _ := captureServer(t, http.StatusNoContent)
	paths, _ := newTestPaths(t, portOf(t, srv.URL))
	var stdout, stderr bytes.Buffer
	runHook([]string{"--dir", paths.Dir, "--gate", "grok"},
		strings.NewReader(`{"hookEventName":"pre_tool_use","sessionId":"grk-1","toolName":"Bash","toolInput":{"command":"npm test"},"workspaceRoot":"/tmp"}`),
		&stdout, &stderr)
	var m map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &m); err != nil || m["decision"] != "deny" {
		t.Fatalf("闸门 204 应输出 deny,实际 %q", stdout.String())
	}
}

// TestHookGrokEntrySuppressedForClaudeChannel Grok 会读 ~/.claude/settings.json
// 的 hooks(CC 兼容层):Claude 通道在 Grok 会话里触发时必须静默退出,
// 事件由 ~/.grok/hooks/ 自家条目以 agent=grok 上报。
func TestHookGrokEntrySuppressedForClaudeChannel(t *testing.T) {
	srv, bodies := captureServer(t, http.StatusNoContent)
	paths, _ := newTestPaths(t, portOf(t, srv.URL))
	t.Setenv("GROK_HOOK_EVENT", "pre_tool_use")
	t.Setenv("GROK_SESSION_ID", "grk-1")
	var stdout, stderr bytes.Buffer
	runHook([]string{"--dir", paths.Dir, "claude"},
		strings.NewReader(`{"session_id":"cc-1","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"},"cwd":"/tmp"}`),
		&stdout, &stderr)
	if len(*bodies) != 0 {
		t.Fatal("Claude 通道在 Grok 会话内不应转发任何请求")
	}
	if stdout.Len() != 0 {
		t.Fatalf("抑制路径不应有输出: %q", stdout.String())
	}

	// grok 通道自身不受抑制
	runHook([]string{"--dir", paths.Dir, "grok"},
		strings.NewReader(`{"hookEventName":"pre_tool_use","sessionId":"grk-1","toolName":"Bash","toolInput":{"command":"ls"},"workspaceRoot":"/tmp"}`),
		&stdout, &stderr)
	if len(*bodies) != 1 {
		t.Fatalf("grok 通道应照常转发,实际 %d 次", len(*bodies))
	}
}

// TestHookProjectDirEnvPerAgent 各 agent 的项目根环境变量按档案取第一个
// 非空值,经 header 交给网关。
func TestHookProjectDirEnvPerAgent(t *testing.T) {
	var gotDir string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDir = r.Header.Get("X-DevPilot-Project-Dir")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	paths, _ := newTestPaths(t, portOf(t, srv.URL))

	t.Setenv("GEMINI_PROJECT_DIR", "/tmp/gem-proj")
	t.Setenv("CLAUDE_PROJECT_DIR", "/tmp/claude-proj")
	var stdout, stderr bytes.Buffer
	runHook([]string{"--dir", paths.Dir, "gemini"},
		strings.NewReader(`{"session_id":"s1","hook_event_name":"SessionStart","cwd":"/tmp"}`),
		&stdout, &stderr)
	if gotDir != "/tmp/gem-proj" {
		t.Fatalf("gemini 项目根 = %q, want GEMINI_PROJECT_DIR 优先", gotDir)
	}

	// qwen 无专属变量时回落 CLAUDE_PROJECT_DIR
	t.Setenv("QWEN_PROJECT_DIR", "")
	t.Setenv("GEMINI_PROJECT_DIR", "")
	runHook([]string{"--dir", paths.Dir, "qwen"},
		strings.NewReader(`{"session_id":"s2","hook_event_name":"SessionStart","cwd":"/tmp"}`),
		&stdout, &stderr)
	if gotDir != "/tmp/claude-proj" {
		t.Fatalf("qwen 项目根 = %q, want 回落 CLAUDE_PROJECT_DIR", gotDir)
	}
}

// TestHookAcceptsNewAgents 新 agent 的 argv 门放行且事件入网关。
func TestHookAcceptsNewAgents(t *testing.T) {
	for _, name := range []string{"qwen", "gemini", "grok", "cursor", "copilot"} {
		t.Run(name, func(t *testing.T) {
			srv, _ := captureServer(t, http.StatusNoContent)
			paths, _ := newTestPaths(t, portOf(t, srv.URL))
			var stdout, stderr bytes.Buffer
			runHook([]string{"--dir", paths.Dir, name},
				strings.NewReader(`{"session_id":"s-x-12345678","hook_event_name":"SessionStart","cwd":"/tmp"}`),
				&stdout, &stderr)
			if stdout.Len() != 0 {
				t.Fatalf("%s: SessionStart 不应输出决策: %q", name, stdout.String())
			}
		})
	}
}

// TestHookUnknownAgentSilent 未登记的 agent 静默退出(绝不破坏 agent)。
func TestHookUnknownAgentSilent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runHook([]string{"zzz"}, strings.NewReader(`{}`), &stdout, &stderr); code != 0 {
		t.Fatalf("未知 agent 退出码应为 0")
	}
	if stdout.Len() != 0 {
		t.Fatalf("未知 agent 不应输出: %q", stdout.String())
	}
}
