package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devpilot-app/hook/internal/config"
)

// readHooks 读取配置中的 hooks 字段，方便断言。
func readHooks(t *testing.T, path string) map[string][]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("解析 %s 失败: %v\n%s", path, err, data)
	}
	return root.Hooks
}

func countOurs(entries []json.RawMessage, agentName string) int {
	n := 0
	for _, e := range entries {
		if isOurEntry(e, agentName) {
			n++
		}
	}
	return n
}

// inHome 把 HOME 指到临时目录,返回该目录;档案的 homeFile 走
// os.UserHomeDir(darwin 读 $HOME),测试据此重定向配置落点。
func inHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

func TestInstallPreservesThirdPartyHooks(t *testing.T) {
	home := inHome(t)
	path := filepath.Join(home, ".claude", "settings.json")
	original := `{
  "model": "opus",
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/usr/local/bin/audit.sh"}]}
    ],
    "Notification": [
      {"hooks": [{"type": "command", "command": "say hi"}]}
    ]
  }
}`
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	changed, err := installAgentHooks("claude", "/opt/homebrew/bin/devpilot-hook", config.ApprovalModePermissionRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("首次安装应写入")
	}

	hooks := readHooks(t, path)
	if got := countOurs(hooks["PreToolUse"], "claude"); got != 1 {
		t.Fatalf("PreToolUse 自家条目 = %d", got)
	}
	if len(hooks["PreToolUse"]) != 2 {
		t.Fatalf("第三方 PreToolUse 条目丢失: %d", len(hooks["PreToolUse"]))
	}
	if !strings.Contains(string(hooks["PreToolUse"][0]), "audit.sh") {
		t.Fatalf("第三方条目内容变更: %s", hooks["PreToolUse"][0])
	}
	// 未涉及的事件原样保留。
	if len(hooks["Notification"]) != 1 || !strings.Contains(string(hooks["Notification"][0]), "say hi") {
		t.Fatalf("Notification 条目丢失: %v", hooks["Notification"])
	}
	for _, ev := range []string{"SessionStart", "PermissionRequest", "PostToolUse", "Stop", "SessionEnd"} {
		if countOurs(hooks[ev], "claude") != 1 {
			t.Fatalf("%s 缺少自家条目", ev)
		}
	}

	// 顶层未知字段保留。
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"model": "opus"`) {
		t.Fatalf("顶层字段丢失:\n%s", data)
	}

	// 幂等：第二次安装不应产生变化。
	changed, err = installAgentHooks("claude", "/opt/homebrew/bin/devpilot-hook", config.ApprovalModePermissionRequest)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("重复安装不应改写文件")
	}
}

func TestUninstallKeepsThirdPartyOnly(t *testing.T) {
	home := inHome(t)
	path := filepath.Join(home, ".claude", "settings.json")
	original := `{
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/usr/local/bin/audit.sh"}]}
    ]
  }
}`
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installAgentHooks("claude", "/opt/homebrew/bin/devpilot-hook", config.ApprovalModePermissionRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := uninstallAgentHooks("claude"); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "devpilot-hook") {
		t.Fatalf("卸载后仍有自家条目:\n%s", data)
	}
	if !strings.Contains(string(data), "audit.sh") {
		t.Fatalf("卸载误删第三方条目:\n%s", data)
	}
}

func TestInstallModeSwitchCleansPermissionRequest(t *testing.T) {
	home := inHome(t)
	path := filepath.Join(home, ".claude", "settings.json")
	if _, err := installAgentHooks("claude", "/bin/devpilot-hook", config.ApprovalModePermissionRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := installAgentHooks("claude", "/bin/devpilot-hook", config.ApprovalModePreToolUse); err != nil {
		t.Fatal(err)
	}
	hooks := readHooks(t, path)
	if _, ok := hooks["PermissionRequest"]; ok {
		t.Fatalf("切换模式后 PermissionRequest 应被清理: %v", hooks["PermissionRequest"])
	}
	if countOurs(hooks["PreToolUse"], "claude") != 1 {
		t.Fatalf("PreToolUse = %v", hooks["PreToolUse"])
	}
	// claude 不是闸门型(GateOnly=false):pre-tool-use 超时回落原生弹窗,
	// 命令不带 --gate(--gate 是 Gemini/Grok 这类「无原生弹窗可回落」的
	// fail-closed 专属开关,见 TestInstallGrokGateCommandLine)。
	if strings.Contains(string(hooks["PreToolUse"][0]), "--gate") {
		t.Fatalf("claude 命令不应带 --gate: %s", hooks["PreToolUse"][0])
	}
}

func TestInstallRejectsInvalidJSON(t *testing.T) {
	home := inHome(t)
	path := filepath.Join(home, ".claude", "settings.json")
	broken := []byte("{not json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installAgentHooks("claude", "/bin/devpilot-hook", config.ApprovalModePermissionRequest); err == nil {
		t.Fatal("非法 JSON 应拒绝写入")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(broken) {
		t.Fatalf("文件被改动:\n%s", after)
	}
}

func TestInstallCreatesBackup(t *testing.T) {
	home := inHome(t)
	path := filepath.Join(home, ".codex", "hooks.json")
	original := `{"hooks":{}}`
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installAgentHooks("codex", "/bin/devpilot-hook", config.ApprovalModePermissionRequest); err != nil {
		t.Fatal(err)
	}
	bak, err := os.ReadFile(path + ".devpilot.bak")
	if err != nil {
		t.Fatalf("缺少备份: %v", err)
	}
	if string(bak) != original {
		t.Fatalf("备份内容 = %q", bak)
	}
}

// TestInstallGeminiTimeoutMilliseconds gemini 的 hook timeout 单位是毫秒
// (默认 60000):30 分钟等待窗必须写 1860000,写 1860 等于 1.86 秒。
func TestInstallGeminiTimeoutMilliseconds(t *testing.T) {
	home := inHome(t)
	if _, err := installAgentHooks("gemini", "/bin/devpilot-hook", config.ApprovalModePermissionRequest); err != nil {
		t.Fatal(err)
	}
	hooks := readHooks(t, filepath.Join(home, ".gemini", "settings.json"))
	want := (config.DefaultApprovalTimeoutSeconds + 60) * 1000
	if got := nestedTimeout(t, firstOurEntry(t, hooks, "BeforeTool", "gemini")); got != want {
		t.Fatalf("gemini timeout = %d, want %d (毫秒)", got, want)
	}
}

// TestInstallQwenTimeoutLegacyMS qwen 的 timeout ≥1000 兼容读作毫秒:
// 等待窗按毫秒写,避免 1860 被读成 1.86 秒。
func TestInstallQwenTimeoutLegacyMS(t *testing.T) {
	home := inHome(t)
	if _, err := installAgentHooks("qwen", "/bin/devpilot-hook", config.ApprovalModePermissionRequest); err != nil {
		t.Fatal(err)
	}
	hooks := readHooks(t, filepath.Join(home, ".qwen", "settings.json"))
	want := (config.DefaultApprovalTimeoutSeconds + 60) * 1000
	if got := nestedTimeout(t, firstOurEntry(t, hooks, "PermissionRequest", "qwen")); got != want {
		t.Fatalf("qwen timeout = %d, want %d", got, want)
	}
}

// TestInstallCursorFlatEntriesPreserveForeign cursor 形是扁平条目
// ({command,timeout} 直接躺在数组),合并必须保留第三方条目。
func TestInstallCursorFlatEntriesPreserveForeign(t *testing.T) {
	home := inHome(t)
	path := filepath.Join(home, ".cursor", "hooks.json")
	original := `{
  "version": 1,
  "hooks": {
    "stop": [
      {"command": "/usr/local/bin/notify.sh", "timeout": 10}
    ]
  }
}`
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installAgentHooks("cursor", "/bin/devpilot-hook", config.ApprovalModePermissionRequest); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "notify.sh") {
		t.Fatalf("第三方 stop 条目丢失:\n%s", data)
	}
	var root struct {
		Version int                          `json:"version"`
		Hooks   map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	if root.Version != 1 {
		t.Fatalf("version = %d", root.Version)
	}
	if len(root.Hooks["stop"]) != 2 {
		t.Fatalf("stop 条目数 = %d, want 2(第三方+自家)", len(root.Hooks["stop"]))
	}
	var flat struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	ours := root.Hooks["stop"][1]
	if err := json.Unmarshal(ours, &flat); err != nil {
		t.Fatalf("cursor 条目应为扁平形: %s", ours)
	}
	if !strings.Contains(flat.Command, "hook cursor") {
		t.Fatalf("command = %q", flat.Command)
	}
	if flat.Timeout != config.DefaultApprovalTimeoutSeconds+60 {
		t.Fatalf("timeout = %d, want %d 秒", flat.Timeout, config.DefaultApprovalTimeoutSeconds+60)
	}
}

// TestInstallOwnedFileLifecycle grok/copilot 的 owned 文件:整文件写入、
// 幂等、拒覆盖外人文件、卸载即删。
func TestInstallOwnedFileLifecycle(t *testing.T) {
	for _, name := range []string{"grok", "copilot"} {
		t.Run(name, func(t *testing.T) {
			home := inHome(t)
			changed, err := installAgentHooks(name, "/bin/devpilot-hook", config.ApprovalModePermissionRequest)
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("首次安装应写入")
			}
			var path string
			switch name {
			case "grok":
				path = filepath.Join(home, ".grok", "hooks", "devpilot.json")
			default:
				path = filepath.Join(home, ".copilot", "hooks", "devpilot.json")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("owned 文件缺失: %v", err)
			}
			if !strings.Contains(string(data), "hook "+name) {
				t.Fatalf("条目命令缺失:\n%s", data)
			}
			if strings.Contains(string(data), ".devpilot.bak") {
				t.Fatalf("owned 文件不应有备份机制")
			}
			// 幂等
			changed, err = installAgentHooks(name, "/bin/devpilot-hook", config.ApprovalModePermissionRequest)
			if err != nil {
				t.Fatal(err)
			}
			if changed {
				t.Fatal("重复安装不应改写")
			}
			// 卸载即删
			removed, err := uninstallAgentHooks(name)
			if err != nil {
				t.Fatal(err)
			}
			if !removed {
				t.Fatal("卸载应删除 owned 文件")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("owned 文件应已删除")
			}
			// 外人文件拒覆盖
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(`{"hooks":{}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := installAgentHooks(name, "/bin/devpilot-hook", config.ApprovalModePermissionRequest); err == nil {
				t.Fatal("非自家 owned 文件应拒覆盖")
			}
		})
	}
}

// TestInstallGrokGateCommandLine grok 的 pre-tool-use 是闸门:命令带
// --gate;默认模式不带。
func TestInstallGrokGateCommandLine(t *testing.T) {
	home := inHome(t)
	if _, err := installAgentHooks("grok", "/bin/devpilot-hook", config.ApprovalModePreToolUse); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".grok", "hooks", "devpilot.json"))
	if !strings.Contains(string(data), "hook --gate grok") {
		t.Fatalf("闸门命令应带 --gate:\n%s", data)
	}
	if _, err := installAgentHooks("grok", "/bin/devpilot-hook", config.ApprovalModePermissionRequest); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(home, ".grok", "hooks", "devpilot.json"))
	if strings.Contains(string(data), "--gate") {
		t.Fatalf("默认模式命令不应带 --gate:\n%s", data)
	}
}

// TestInstallCodexProductsByteStable claude/codex 的既有产物必须逐字节
// 稳定:嵌套 matcher 形、秒单位、不带 --gate(默认模式)。
func TestInstallCodexProductsByteStable(t *testing.T) {
	home := inHome(t)
	if _, err := installAgentHooks("codex", "/bin/devpilot-hook", config.ApprovalModePermissionRequest); err != nil {
		t.Fatal(err)
	}
	hooks := readHooks(t, filepath.Join(home, ".codex", "hooks.json"))
	var entry struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			Timeout int    `json:"timeout"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(firstOurEntry(t, hooks, "PreToolUse", "codex"), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Matcher != "*" {
		t.Fatalf("matcher = %q", entry.Matcher)
	}
	if len(entry.Hooks) != 1 || entry.Hooks[0].Type != "command" {
		t.Fatalf("hooks = %+v", entry.Hooks)
	}
	if entry.Hooks[0].Command != "/bin/devpilot-hook hook codex" {
		t.Fatalf("command = %q", entry.Hooks[0].Command)
	}
	if entry.Hooks[0].Timeout != config.DefaultApprovalTimeoutSeconds+60 {
		t.Fatalf("timeout = %d, want %d 秒", entry.Hooks[0].Timeout, config.DefaultApprovalTimeoutSeconds+60)
	}
}

func TestCommandLineQuotesSpaces(t *testing.T) {
	cases := []struct {
		exe  string
		gate bool
		want string
	}{
		{"/usr/local/bin/devpilot-hook", false, "/usr/local/bin/devpilot-hook hook claude"},
		{"/usr/local/bin/devpilot-hook", true, "/usr/local/bin/devpilot-hook hook --gate claude"},
		{"/Users/eric/My Tools/devpilot-hook", false, `"/Users/eric/My Tools/devpilot-hook" hook claude`},
	}
	for _, tc := range cases {
		if got := commandLine(tc.exe, "claude", tc.gate); got != tc.want {
			t.Fatalf("commandLine(%q,%v) = %q, want %q", tc.exe, tc.gate, got, tc.want)
		}
	}
}

func TestIsVersionedPath(t *testing.T) {
	for _, p := range []string{
		"/opt/homebrew/Cellar/devpilot-hook/0.1.0/bin/devpilot-hook",
		"/opt/homebrew/Caskroom/devpilot-hook/0.1.0/devpilot-hook",
	} {
		if !isVersionedPath(p) {
			t.Fatalf("%s 应判定为版本目录", p)
		}
	}
	for _, p := range []string{
		"/opt/homebrew/bin/devpilot-hook",
		"/usr/local/bin/devpilot-hook",
	} {
		if isVersionedPath(p) {
			t.Fatalf("%s 不应判定为版本目录", p)
		}
	}
}

// firstOurEntry 取事件数组里第一条自家条目;缺失即失败。
func firstOurEntry(t *testing.T, hooks map[string][]json.RawMessage, event, agentName string) json.RawMessage {
	t.Helper()
	for _, e := range hooks[event] {
		if isOurEntry(e, agentName) {
			return e
		}
	}
	t.Fatalf("%s 缺少自家条目", event)
	return nil
}

// nestedTimeout 读嵌套形条目(nested matcher)里 hooks[0].timeout。
func nestedTimeout(t *testing.T, entry json.RawMessage) int {
	t.Helper()
	var e struct {
		Hooks []struct {
			Timeout int `json:"timeout"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(entry, &e); err != nil {
		t.Fatalf("嵌套形条目解析失败: %v\n%s", err, entry)
	}
	if len(e.Hooks) != 1 {
		t.Fatalf("嵌套形条目应有 1 条 hook: %s", entry)
	}
	return e.Hooks[0].Timeout
}
