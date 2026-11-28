package agent_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/devpilot-app/apns/pkg/wire"
	"github.com/devpilot-app/hook/internal/agent"
	"github.com/devpilot-app/hook/internal/agent/copilot"
	"github.com/devpilot-app/hook/internal/agent/cursor"
	"github.com/devpilot-app/hook/internal/agent/gemini"
	"github.com/devpilot-app/hook/internal/agent/grok"
	"github.com/devpilot-app/hook/internal/agent/qwen"
	"github.com/devpilot-app/hook/internal/config"
)

// loadInputAs 用 ParseInputAs 读取 fixture(与生产一致:事件名按档案归一化)。
// 老的 loadInput 走 ParseInput(无归一化),claude/codex/opencode 的身份映射
// 是恒等,两者等价;新 agent 的事件名(BeforeTool / pre_tool_use / preToolUse)
// 必须走归一化路径。
func loadInputAs(t *testing.T, agentName, file string) agent.Input {
	t.Helper()
	in, err := agent.ParseInputAs(agentName, loadFixture(t, agentName, file))
	if err != nil {
		t.Fatalf("解析 %s/%s 失败: %v", agentName, file, err)
	}
	return in
}

func loadFixture(t *testing.T, agentName, file string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hooks", agentName, file))
	if err != nil {
		t.Fatalf("读取 fixture 失败: %v", err)
	}
	return data
}

// TestBatch2EventMapping 五个新 agent 的核心事件映射 + wire.Validate()。
func TestBatch2EventMapping(t *testing.T) {
	q := qwen.Adapter{}
	in := loadInputAs(t, "qwen", "permission_request.json")
	ev, err := q.Normalize(in, "dd_1", 1, config.ApprovalModePermissionRequest, false)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.NeedsApproval || ev.Envelope.Type != wire.EventApprovalRequired || ev.Envelope.Agent != wire.AgentQwen {
		t.Fatalf("qwen PermissionRequest 应为审批卡: %+v", ev)
	}
	if err := ev.Envelope.Validate(); err != nil {
		t.Fatalf("wire 校验失败: %v", err)
	}
	if ev.Envelope.ApprovalID == "" || ev.Envelope.Payload.NotifyOnly {
		t.Fatalf("qwen 审批应可代答(notify_only 不应置位): %+v", ev.Envelope.Payload)
	}
	// PreToolUse 恒遥测(Qwen 的 allow 不免弹窗,阻塞模式无效)
	in = loadInputAs(t, "qwen", "pre_tool_use.json")
	ev, _ = q.Normalize(in, "dd_1", 1, config.ApprovalModePreToolUse, false)
	if ev.NeedsApproval || ev.Envelope.Type != wire.EventToolRunning {
		t.Fatalf("qwen PreToolUse 应恒为遥测: %+v", ev)
	}

	g := gemini.Adapter{}
	in = loadInputAs(t, "gemini", "before_tool.json")
	if in.HookEventName != "PreToolUse" || in.ToolName != "run_shell_command" {
		t.Fatalf("gemini BeforeTool 应归一化为 PreToolUse: %+v", in)
	}
	ev, _ = g.Normalize(in, "dd_1", 1, config.ApprovalModePreToolUse, false)
	if !ev.NeedsApproval || !ev.Gate {
		t.Fatalf("gemini 闸门模式可变工具应为 Gate 审批: %+v", ev)
	}
	ev, _ = g.Normalize(in, "dd_1", 1, config.ApprovalModePermissionRequest, false)
	if ev.NeedsApproval || ev.Envelope.Type != wire.EventToolRunning {
		t.Fatalf("gemini 默认模式 BeforeTool 应为遥测: %+v", ev)
	}
	in = loadInputAs(t, "gemini", "notification_tool_permission.json")
	if in.HookEventName != "PermissionRequest" {
		t.Fatalf("gemini Notification 应归一化为 PermissionRequest: %q", in.HookEventName)
	}
	ev, _ = g.Normalize(in, "dd_1", 1, config.ApprovalModePermissionRequest, false)
	if ev.NeedsApproval || !ev.Envelope.Payload.NotifyOnly {
		t.Fatalf("gemini 弹窗通知应为 notify-only 等待卡: %+v", ev)
	}
	if ev.Envelope.Payload.ToolName != "shell" || ev.Envelope.Payload.CommandSummary != "npm test" {
		t.Fatalf("details 白名单摘要不符: %+v", ev.Envelope.Payload)
	}
	if err := ev.Envelope.Validate(); err != nil {
		t.Fatalf("wire 校验失败: %v", err)
	}
	// 非权限类通知丢弃
	in.NotificationType = "agent_idle"
	if ev, _ = g.Normalize(in, "dd_1", 1, config.ApprovalModePermissionRequest, false); ev != nil {
		t.Fatalf("非权限类通知应丢弃")
	}
	in = loadInputAs(t, "gemini", "after_agent.json")
	if in.LastAssistantMessage != "Done — 2 tests fixed." {
		t.Fatalf("prompt_response 应回落到 LastAssistantMessage: %q", in.LastAssistantMessage)
	}
	ev, _ = g.Normalize(in, "dd_1", 1, config.ApprovalModePermissionRequest, false)
	if ev.Envelope.Type != wire.EventTaskComplete || ev.Envelope.Payload.ReplySummary == "" {
		t.Fatalf("AfterAgent 应映射为带摘要的完成卡: %+v", ev.Envelope.Payload)
	}

	k := grok.Adapter{}
	in = loadInputAs(t, "grok", "pre_tool_use.json")
	if in.HookEventName != "PreToolUse" || in.ToolName != "Bash" || in.ToolUseID != "call_7f2a91" {
		t.Fatalf("grok snake 事件名/camel 字段归一化失败: %+v", in)
	}
	ev, _ = k.Normalize(in, "dd_1", 1, config.ApprovalModePreToolUse, false)
	if !ev.NeedsApproval || !ev.Gate {
		t.Fatalf("grok 闸门模式应为 Gate 审批: %+v", ev)
	}
	if err := ev.Envelope.Validate(); err != nil {
		t.Fatalf("wire 校验失败: %v", err)
	}
	in = loadInputAs(t, "grok", "notification.json")
	ev, _ = k.Normalize(in, "dd_1", 1, config.ApprovalModePermissionRequest, false)
	if ev == nil || !ev.Envelope.Payload.NotifyOnly {
		t.Fatalf("grok 权限通知应为等待卡: %+v", ev)
	}

	c := cursor.Adapter{}
	in = loadInputAs(t, "cursor", "pre_tool_use.json")
	if in.SessionID != "cur-77e10a2d94f8" || in.ToolName != "Shell" || in.ToolUseID != "tu_91ba77" {
		t.Fatalf("cursor conversation_id/camel 回落失败: %+v", in)
	}
	ev, _ = c.Normalize(in, "dd_1", 1, config.ApprovalModePreToolUse, false)
	if !ev.NeedsApproval || ev.Gate {
		t.Fatalf("cursor pre-tool-use 应为普通审批(有原生回落,Gate=false): %+v", ev)
	}
	if err := ev.Envelope.Validate(); err != nil {
		t.Fatalf("wire 校验失败: %v", err)
	}

	p := copilot.Adapter{}
	in = loadInputAs(t, "copilot", "permission_request.json")
	ev, _ = p.Normalize(in, "dd_1", 1, config.ApprovalModePermissionRequest, false)
	if !ev.NeedsApproval {
		t.Fatalf("copilot PermissionRequest 应为审批卡: %+v", ev)
	}
	// copilot 闸门模式丢弃 PermissionRequest 后走 PreToolUse
	in = loadInputAs(t, "copilot", "pre_tool_use.json")
	ev, _ = p.Normalize(in, "dd_1", 1, config.ApprovalModePreToolUse, false)
	if !ev.NeedsApproval || ev.Gate {
		t.Fatalf("copilot pre-tool-use 应为普通审批: %+v", ev)
	}
}

// TestBatch2RenderDecisions 各家 deny 输出形状;gemini/grok 批准 = 空输出。
func TestBatch2RenderDecisions(t *testing.T) {
	input := json.RawMessage(`{"command":"npm test"}`)

	g := gemini.Adapter{}
	if out, _ := g.RenderDecision("PreToolUse", wire.DecisionAllow, "", "run_shell_command", input); out != nil {
		t.Fatalf("gemini allow 应为空输出,实际 %s", out)
	}
	out, _ := g.RenderDecision("PreToolUse", wire.DecisionDeny, "", "run_shell_command", input)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil || m["decision"] != "deny" || m["reason"] == "" {
		t.Fatalf("gemini deny 形状不符: %s", out)
	}

	k := grok.Adapter{}
	if out, _ := k.RenderDecision("PreToolUse", wire.DecisionAllow, "", "Bash", input); out != nil {
		t.Fatalf("grok allow 应为空输出,实际 %s", out)
	}

	c := cursor.Adapter{}
	out, _ = c.RenderDecision("PreToolUse", wire.DecisionAllow, "", "Shell", input)
	if err := json.Unmarshal(out, &m); err != nil || m["permission"] != "allow" {
		t.Fatalf("cursor allow 形状不符: %s", out)
	}
	out, _ = c.RenderDecision("PreToolUse", wire.DecisionDeny, "", "Shell", input)
	if err := json.Unmarshal(out, &m); err != nil || m["permission"] != "deny" {
		t.Fatalf("cursor deny 形状不符: %s", out)
	}

	p := copilot.Adapter{}
	out, _ = p.RenderDecision("PermissionRequest", wire.DecisionDeny, "", "shell", input)
	if err := json.Unmarshal(out, &m); err != nil || m["behavior"] != "deny" || m["message"] == "" {
		t.Fatalf("copilot PR deny 形状不符: %s", out)
	}
	out, _ = p.RenderDecision("PreToolUse", wire.DecisionAllow, "", "shell", input)
	if err := json.Unmarshal(out, &m); err != nil || m["permissionDecision"] != "allow" {
		t.Fatalf("copilot PTU allow 形状不符: %s", out)
	}

	q := qwen.Adapter{}
	out, _ = q.RenderDecision("PermissionRequest", wire.DecisionAllow, "", "run_shell_command", input)
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	hs, _ := m["hookSpecificOutput"].(map[string]any)
	dec, _ := hs["decision"].(map[string]any)
	if hs["hookEventName"] != "PermissionRequest" || dec["behavior"] != "allow" || dec["updatedInput"] == nil {
		t.Fatalf("qwen PR allow 应回显 updatedInput: %s", out)
	}
}
