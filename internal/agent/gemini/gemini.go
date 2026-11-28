// Package gemini 实现 Gemini CLI 的 hook 适配器。
//
// 官方源码核实的时序(coreToolScheduler.ts):default 审批模式下原生弹窗
// 在 BeforeTool **之前**决策,BeforeTool 是批准后、执行前的钩子——所以
// 它的 allow 没有代答语义(源码里 approve/allow 无消费点),只有 deny /
// 改写参数生效。由此:
//
//   - 默认(permission-request)模式:Notification(notification_type=
//     ToolPermission,弹窗出现前一瞬触发)发「只通知不阻塞」等待卡,
//     手机导航回终端;BeforeTool 作 tool_running 遥测。
//   - pre-tool-use(闸门)模式:用户以 `gemini --approval-mode yolo`
//     启动,原生弹窗被关闭,BeforeTool 成为唯一闸门——可变工具阻塞等
//     手机裁决,批准 = 空输出放行,拒绝 = deny JSON;等待窗耗尽必须
//     fail-closed(网关回 deny),否则 yolo 下的工具会无闸执行。
//
// Notification 的 details 可能携带完整文件内容(edit 的 fileDiff /
// originalContent / newContent)——**隐私红线**:只允许摘白名单子集
// (command / filePath / toolName / serverName / title)构造摘要,
// 原文绝不透传(wire.Payload 注释同款纪律)。
package gemini

import (
	"encoding/json"
	"strings"

	"github.com/devpilot-app/apns/pkg/wire"
	"github.com/devpilot-app/hook/internal/agent"
	"github.com/devpilot-app/hook/internal/config"
)

// Adapter 是 Gemini 适配器。
type Adapter struct{}

// Name 返回 wire 中的 agent 标识。
func (Adapter) Name() wire.Agent { return wire.AgentGemini }

// Normalize 把 Gemini hook 事件映射为 wire 事件。
// 事件名已由 ParseInputAs 按 Profile.EventAliases 归一化:
// BeforeTool→PreToolUse、AfterTool→PostToolUse、AfterAgent→Stop、
// Notification→PermissionRequest。
func (Adapter) Normalize(in agent.Input, daemonID string, now int64, mode config.ApprovalMode, answerQuestions bool) (*agent.Event, error) {
	switch in.HookEventName {
	case "SessionStart":
		env := agent.NewEnvelope(wire.AgentGemini, daemonID, in, wire.EventSessionStarted, agent.BasePayload(in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PreToolUse":
		// 闸门模式 + 可变工具:BeforeTool 是 yolo 下唯一的执行前钩子,
		// 在这里等手机裁决(Gate=true,超时 fail-closed)。只读工具与其余
		// 模式作遥测。
		if mode == config.ApprovalModePreToolUse && profile.IsMutating(in.ToolName) {
			env := agent.NewApprovalEnvelope(wire.AgentGemini, daemonID, in, now)
			return &agent.Event{Envelope: env, HookEvent: in.HookEventName, NeedsApproval: true, Gate: true}, nil
		}
		env := agent.NewEnvelope(wire.AgentGemini, daemonID, in, wire.EventToolRunning, agent.WithTool(agent.BasePayload(in), in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PermissionRequest":
		// Notification:只有权限弹窗类值得发卡;其余(会话空闲等)丢弃。
		// TODO(device):notification_type 的取值以真机抓样为准,当前按
		// 官方文档 "ToolPermission" 精确匹配。
		if in.NotificationType != "ToolPermission" {
			return nil, nil
		}
		toolName, summary := notifyPayload(in)
		env := agent.NewNotifyApprovalEnvelope(wire.AgentGemini, daemonID, in, now)
		env.Payload.ToolName = toolName
		env.Payload.CommandSummary = summary
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PostToolUse":
		env := agent.NewEnvelope(wire.AgentGemini, daemonID, in, wire.EventToolFinished, agent.WithTool(agent.BasePayload(in), in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "Stop":
		p := agent.BasePayload(in)
		p.ReplySummary = wire.Truncate(in.LastAssistantMessage, agent.ReplySummaryRunes)
		env := agent.NewEnvelope(wire.AgentGemini, daemonID, in, wire.EventTaskComplete, p, now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "SessionEnd":
		env := agent.NewEnvelope(wire.AgentGemini, daemonID, in, wire.EventSessionEnded, agent.BasePayload(in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	default:
		return nil, nil
	}
}

// RenderDecision 生成 Gemini 期望的 hook 输出(只有闸门模式的 PreToolUse
// 会到达这里)。
//
// 批准 = 不输出任何内容(exit 0 即放行,Gemini 的纪律是 stdout 非最终
// JSON 不许有输出)——返回 nil,由网关发 200 空体。拒绝 = deny JSON,
// 理由喂回模型。choice(选项型)防御位:答案原样进理由文本。
func (Adapter) RenderDecision(hookEvent string, d wire.DecisionKind, choice, _ string, _ json.RawMessage) ([]byte, error) {
	if d == wire.DecisionAllow {
		return nil, nil
	}
	return json.Marshal(map[string]any{
		"decision": "deny",
		"reason":   agent.DenyReason(choice),
	})
}

// profile 是本包的工具名单来源(只读判断与 Normalize 的闸门分支共用)。
var profile = mustProfile("gemini")

func mustProfile(name string) agent.Profile {
	p, ok := agent.Lookup(name)
	if !ok {
		panic("gemini: profile missing")
	}
	return p
}

// notifyPayload 从 Notification 的 details 摘白名单子集构造卡面摘要。
// details 的 exec/edit/mcp/info 各形态只有 command / filePath / fileName /
// serverName / toolName / title 允许进入;fileDiff、originalContent、
// newContent、prompt 一律不取(隐私红线,见包注释)。
func notifyPayload(in agent.Input) (toolName, summary string) {
	var d struct {
		Type       string `json:"type"`
		Title      string `json:"title"`
		Command    string `json:"command"`
		FileName   string `json:"fileName"`
		FilePath   string `json:"filePath"`
		ServerName string `json:"serverName"`
		ToolName   string `json:"toolName"`
	}
	if len(in.Details) > 0 {
		_ = json.Unmarshal(in.Details, &d)
	}
	switch strings.ToLower(d.Type) {
	case "exec":
		toolName = "shell"
	case "edit":
		toolName = "edit"
	case "mcp":
		toolName = "mcp"
	default:
		toolName = d.ToolName
	}
	if toolName == "" {
		toolName = d.Type
	}
	switch {
	case d.Command != "":
		summary = d.Command
	case d.FilePath != "":
		summary = d.FilePath
	case d.FileName != "":
		summary = d.FileName
	case d.ServerName != "" && d.ToolName != "":
		summary = d.ServerName + ":" + d.ToolName
	case d.Title != "":
		summary = d.Title
	}
	summary = strings.TrimSpace(summary)
	// 摘要帽:通知单行可读优先(wire.MaxCommandRunes 是 256,锁屏压
	// PlainPreview 由 relay 做;这里只做原始截断)。
	if len(summary) > 512 {
		summary = summary[:512]
	}
	return toolName, summary
}
