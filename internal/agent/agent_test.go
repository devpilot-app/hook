package agent_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/devpilot-app/apns/pkg/wire"
	"github.com/devpilot-app/hook/internal/agent"
	"github.com/devpilot-app/hook/internal/agent/claude"
	"github.com/devpilot-app/hook/internal/agent/codex"
	"github.com/devpilot-app/hook/internal/agent/opencode"
	"github.com/devpilot-app/hook/internal/config"
)

func loadInput(t *testing.T, agentName, file string) agent.Input {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hooks", agentName, file))
	if err != nil {
		t.Fatalf("读取 fixture 失败: %v", err)
	}
	in, err := agent.ParseInput(data)
	if err != nil {
		t.Fatalf("解析 fixture 失败: %v", err)
	}
	return in
}

func TestClaudeEventMapping(t *testing.T) {
	ad := claude.Adapter{}
	now := int64(1760770000)

	cases := []struct {
		file string
		want wire.EventType
	}{
		{"session_start.json", wire.EventSessionStarted},
		{"pre_tool_use.json", wire.EventToolRunning},
		{"permission_request.json", wire.EventApprovalRequired},
		{"post_tool_use.json", wire.EventToolFinished},
		{"stop.json", wire.EventTaskComplete},
		{"session_end.json", wire.EventSessionEnded},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			in := loadInput(t, "claude", tc.file)
			ev, err := ad.Normalize(in, "dd_test", now, config.ApprovalModePermissionRequest, false)
			if err != nil {
				t.Fatal(err)
			}
			if ev == nil || ev.Envelope.Type != tc.want {
				t.Fatalf("期望 %s，实际 %+v", tc.want, ev)
			}
			if err := ev.Envelope.Validate(); err != nil {
				t.Fatalf("生成的事件未通过协议校验: %v", err)
			}
			if tc.want == wire.EventApprovalRequired && !ev.NeedsApproval {
				t.Fatal("PermissionRequest 应阻塞审批")
			}
		})
	}
}

// TestClaudePermissionRequestIDIsRandomWithoutToolUseID 钉 2026-11-28 真机
// 取证：Claude Code 的 PermissionRequest 载荷不带 tool_use_id（fixture 已对
// 齐真机），审批 id 落随机 uuidv7——这正是「终端答完无法同源派生收卡」的
// 根因，收卡必须走 gateway 的 tool_use_id 关联对账。确定性只对带
// tool_use_id 的 PreToolUse 通道成立（TestClaudePreToolUseApprovalIDDeterministic）。
func TestClaudePermissionRequestIDIsRandomWithoutToolUseID(t *testing.T) {
	ad := claude.Adapter{}
	in := loadInput(t, "claude", "permission_request.json")
	if in.ToolUseID != "" {
		t.Fatalf("fixture 必须对齐真机（PermissionRequest 无 tool_use_id），得 %q", in.ToolUseID)
	}
	a, err := ad.Normalize(in, "dd_test", 1, config.ApprovalModePermissionRequest, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ad.Normalize(in, "dd_test", 2, config.ApprovalModePermissionRequest, false)
	if err != nil {
		t.Fatal(err)
	}
	// 无 tool_use_id：两次 hook 是两次独立审批，id 必须不同（随机兜底）。
	if a.Envelope.ApprovalID == "" || a.Envelope.ApprovalID == b.Envelope.ApprovalID {
		t.Fatalf("无 tool_use_id 时 id 应随机独立: %q vs %q", a.Envelope.ApprovalID, b.Envelope.ApprovalID)
	}
	if a.Envelope.EventID == b.Envelope.EventID {
		t.Fatal("每次 hook 的 event_id 应不同")
	}
}

func TestClaudePreToolUseFallbackMode(t *testing.T) {
	ad := claude.Adapter{}
	in := loadInput(t, "claude", "pre_tool_use.json")
	ev, err := ad.Normalize(in, "dd_test", 1, config.ApprovalModePreToolUse, false)
	if err != nil {
		t.Fatal(err)
	}
	if ev == nil || ev.Envelope.Type != wire.EventApprovalRequired || !ev.NeedsApproval {
		t.Fatalf("回退模式应把 PreToolUse 变成审批: %+v", ev)
	}
}

func TestClaudePayloadWhitelist(t *testing.T) {
	ad := claude.Adapter{}
	in := loadInput(t, "claude", "permission_request.json")
	ev, _ := ad.Normalize(in, "dd_test", 1, config.ApprovalModePermissionRequest, false)
	p := ev.Envelope.Payload
	if p.ToolName != "Bash" || p.CommandSummary != "git push --force origin main" {
		t.Fatalf("摘要不符: %+v", p)
	}
	if p.InputDigest == "" {
		t.Fatal("应带 input_digest")
	}
	// 完整输入不得进入载荷。
	raw, _ := json.Marshal(ev.Envelope)
	if string(raw) == "" || json.Valid(raw) == false {
		t.Fatal("事件序列化失败")
	}
	if containsKey(raw, "tool_input") {
		t.Fatalf("载荷泄露了完整 tool_input: %s", raw)
	}
}

func containsKey(raw []byte, key string) bool {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

func TestClaudeRenderDecision(t *testing.T) {
	ad := claude.Adapter{}

	allow, err := ad.RenderDecision("PermissionRequest", wire.DecisionAllow, "", "ExitPlanMode", json.RawMessage(`{"plan":"# Plan"}`))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		HookSpecificOutput struct {
			HookEventName string `json:"hookEventName"`
			Decision      struct {
				Behavior string `json:"behavior"`
				Message  string `json:"message"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(allow, &got); err != nil {
		t.Fatal(err)
	}
	if got.HookSpecificOutput.HookEventName != "PermissionRequest" ||
		got.HookSpecificOutput.Decision.Behavior != "allow" {
		t.Fatalf("输出不符: %s", allow)
	}

	deny, _ := ad.RenderDecision("PreToolUse", wire.DecisionDeny, "", "Bash", json.RawMessage(`{"command":"ls"}`))
	var pre struct {
		HookSpecificOutput struct {
			PermissionDecision string `json:"permissionDecision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(deny, &pre); err != nil {
		t.Fatal(err)
	}
	if pre.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("回退模式输出不符: %s", deny)
	}
}

func TestCodexNormalizeAndRender(t *testing.T) {
	ad := codex.Adapter{}
	in := loadInput(t, "codex", "pre_tool_use.json")
	ev, err := ad.Normalize(in, "dd_test", 1, config.ApprovalModePermissionRequest, false)
	if err != nil {
		t.Fatal(err)
	}
	if ev == nil || ev.Envelope.Agent != wire.AgentCodex || ev.Envelope.SessionID != "sess_codex_001" {
		t.Fatalf("Codex 归一化不符: %+v", ev)
	}

	stop := loadInput(t, "codex", "stop.json")
	ev, err = ad.Normalize(stop, "dd_test", 1, config.ApprovalModePermissionRequest, false)
	if err != nil || ev == nil || ev.Envelope.Type != wire.EventTaskComplete {
		t.Fatalf("Codex Stop 映射不符: %+v %v", ev, err)
	}

	out, err := ad.RenderDecision("PermissionRequest", wire.DecisionDeny, "", "Bash", json.RawMessage(`{"command":"ls"}`))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	hso := payload["hookSpecificOutput"].(map[string]any)
	if hso["permissionDecision"] != "deny" {
		t.Fatalf("Codex 决策输出不符: %s", out)
	}
}

// TestOpenCodeEventMappingFromFixture 钉住插件→EXE 的 JSON 契约：fixture
// 逐字复刻 JS 插件 callHook 拼出的载荷（snake_case session_id、无
// transcript_path、小写工具名），必须原样穿过 ParseInput 并映射到 wire。
func TestOpenCodeEventMappingFromFixture(t *testing.T) {
	ad := opencode.Adapter{}
	now := int64(1760770000)

	cases := []struct {
		file string
		want wire.EventType
	}{
		{"session_start.json", wire.EventSessionStarted},
		{"pre_tool_use.json", wire.EventToolRunning},
		{"permission_request.json", wire.EventApprovalRequired},
		{"post_tool_use.json", wire.EventToolFinished},
		{"stop.json", wire.EventTaskComplete},
		{"session_end.json", wire.EventSessionEnded},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			in := loadInput(t, "opencode", tc.file)
			// 插件载荷只带 session_id：若解析层动了派生逻辑（transcript
			// 派生、camelCase 回退），这里的原值断言最先碎。
			if in.SessionID != "ses_ocdemo4t1bfhklpvbx" {
				t.Fatalf("session_id 应原样保留，实际 %q", in.SessionID)
			}
			ev, err := ad.Normalize(in, "dd_test", now, config.ApprovalModePermissionRequest, false)
			if err != nil {
				t.Fatal(err)
			}
			if ev == nil || ev.Envelope.Type != tc.want {
				t.Fatalf("期望 %s，实际 %+v", tc.want, ev)
			}
			if ev.Envelope.Agent != wire.AgentOpenCode {
				t.Fatalf("agent = %s，期望 opencode", ev.Envelope.Agent)
			}
			if err := ev.Envelope.Validate(); err != nil {
				t.Fatalf("生成的事件未通过协议校验: %v", err)
			}
		})
	}
}

// TestOpenCodeApprovalFromFixture 审批链路的关键契约：v1 permissionInput
// 把 command/title/pattern 合进 tool_input，摘要必须取 command；审批 ID
// 对同载荷稳定；PreToolUse 永不升格审批（插件无法否决工具执行）。
func TestOpenCodeApprovalFromFixture(t *testing.T) {
	ad := opencode.Adapter{}
	in := loadInput(t, "opencode", "permission_request.json")
	ev, err := ad.Normalize(in, "dd_test", 1, config.ApprovalModePermissionRequest, false)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.NeedsApproval {
		t.Fatal("PermissionRequest 应阻塞审批")
	}
	if ev.Envelope.Payload.CommandSummary != "git push --force origin main" {
		t.Fatalf("摘要应取 command 而非 title/pattern: %+v", ev.Envelope.Payload)
	}
	again, err := ad.Normalize(in, "dd_test", 2, config.ApprovalModePermissionRequest, false)
	if err != nil {
		t.Fatal(err)
	}
	if again.Envelope.ApprovalID == "" || again.Envelope.ApprovalID != ev.Envelope.ApprovalID {
		t.Fatalf("同一工具调用应得到相同 approval_id: %q vs %q", ev.Envelope.ApprovalID, again.Envelope.ApprovalID)
	}

	pre := loadInput(t, "opencode", "pre_tool_use.json")
	for _, mode := range []config.ApprovalMode{config.ApprovalModePermissionRequest, config.ApprovalModePreToolUse} {
		ev, err := ad.Normalize(pre, "dd_test", 1, mode, false)
		if err != nil {
			t.Fatal(err)
		}
		if ev.NeedsApproval {
			t.Fatalf("模式 %s 下 PreToolUse 不应升格为审批", mode)
		}
	}
}

func TestUnknownHookIgnored(t *testing.T) {
	in := agent.Input{SessionID: "s", HookEventName: "Notification"}
	ev, err := claude.Adapter{}.Normalize(in, "dd_test", 1, config.ApprovalModePermissionRequest, false)
	if err != nil || ev != nil {
		t.Fatalf("未知 hook 应忽略: %+v %v", ev, err)
	}
}

func TestParseInputRequiresSession(t *testing.T) {
	if _, err := agent.ParseInput([]byte(`{"hook_event_name":"Stop"}`)); err == nil {
		t.Fatal("缺少 session_id 与 transcript_path 应报错")
	}
	in, err := agent.ParseInput([]byte(`{"transcript_path":"/tmp/x.jsonl","hook_event_name":"Stop"}`))
	if err != nil {
		t.Fatal(err)
	}
	if in.SessionID == "" {
		t.Fatal("应由 transcript_path 派生 session_id")
	}
}
