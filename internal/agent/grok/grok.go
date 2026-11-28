// Package grok 实现 Grok Build 的 hook 适配器。
//
// 官方开源仓核实的语义(xai-grok-shell tool_calls.rs):PreToolUse hook
// 的 permissionDecision:"allow" 是空操作(HookDecision::Allow => {}),
// 原生权限流程照走——hook 无法代答弹窗,只有 deny / ask / 改写参数生效。
// 由此:
//
//   - 默认模式:Notification 的权限类通知发「只通知不阻塞」等待卡
//     (notification_type 取值待真机抓样);PreToolUse 作 tool_running 遥测。
//   - pre-tool-use(闸门)模式:用户以 `grok --always-approve` 启动
//     (该模式下 deny 规则与 PreToolUse hooks 仍生效,官方文档明写),
//     BeforeTool 等价物 PreToolUse 成为唯一闸门——可变工具阻塞等手机,
//     批准 = 空输出放行,拒绝 = deny JSON;等待窗耗尽必须 fail-closed。
//
// Grok 会自动读取 ~/.claude/settings.json 的 hooks(Claude Code 兼容层),
// 我们的 Claude 条目会在 Grok 会话里被触发——运行期由 CLI 侧按
// GROK_* 环境变量识别并静默退出(hookcmd),事件归属不受污染。
package grok

import (
	"encoding/json"
	"strings"

	"github.com/devpilot-app/apns/pkg/wire"
	"github.com/devpilot-app/hook/internal/agent"
	"github.com/devpilot-app/hook/internal/config"
)

// Adapter 是 Grok 适配器。
type Adapter struct{}

// Name 返回 wire 中的 agent 标识。
func (Adapter) Name() wire.Agent { return wire.AgentGrok }

// Normalize 把 Grok hook 事件映射为 wire 事件。
// 事件名已由 ParseInputAs 归一化(snake_case 与 Claude 形别名都收敛到
// 六规范名);字段是 camelCase,ParseInputAs 已回落填齐。
func (Adapter) Normalize(in agent.Input, daemonID string, now int64, mode config.ApprovalMode, answerQuestions bool) (*agent.Event, error) {
	switch in.HookEventName {
	case "SessionStart":
		env := agent.NewEnvelope(wire.AgentGrok, daemonID, in, wire.EventSessionStarted, agent.BasePayload(in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PreToolUse":
		// 闸门模式 + 可变工具:--always-approve 下 PreToolUse 是唯一闸门
		// (Gate=true,超时 fail-closed)。
		if mode == config.ApprovalModePreToolUse && profile.IsMutating(in.ToolName) {
			env := agent.NewApprovalEnvelope(wire.AgentGrok, daemonID, in, now)
			return &agent.Event{Envelope: env, HookEvent: in.HookEventName, NeedsApproval: true, Gate: true}, nil
		}
		env := agent.NewEnvelope(wire.AgentGrok, daemonID, in, wire.EventToolRunning, agent.WithTool(agent.BasePayload(in), in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PermissionRequest":
		// Notification → 等待卡。Grok 的 notification_type 取值未文档化,
		// TODO(device):抓样后收紧过滤;当前策略 = 类型为空或含 permission
		// 才发卡,其余(普通通知)丢弃,宁缺毋滥。
		if t := strings.ToLower(in.NotificationType); t != "" && !strings.Contains(t, "permission") {
			return nil, nil
		}
		env := agent.NewNotifyApprovalEnvelope(wire.AgentGrok, daemonID, in, now)
		if env.Payload.ToolName == "" && in.Message != "" {
			env.Payload.CommandSummary = wire.Summarize(in.Message, wire.MaxCommandRunes)
		}
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PostToolUse":
		env := agent.NewEnvelope(wire.AgentGrok, daemonID, in, wire.EventToolFinished, agent.WithTool(agent.BasePayload(in), in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "Stop":
		p := agent.BasePayload(in)
		p.ReplySummary = wire.Truncate(in.LastAssistantMessage, agent.ReplySummaryRunes)
		env := agent.NewEnvelope(wire.AgentGrok, daemonID, in, wire.EventTaskComplete, p, now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "SessionEnd":
		env := agent.NewEnvelope(wire.AgentGrok, daemonID, in, wire.EventSessionEnded, agent.BasePayload(in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	default:
		return nil, nil
	}
}

// RenderDecision 生成 Grok 期望的 hook 输出(只有闸门模式的 PreToolUse
// 会到达这里)。批准 = 不输出(exit 0 放行);拒绝 = deny JSON。
func (Adapter) RenderDecision(hookEvent string, d wire.DecisionKind, choice, _ string, _ json.RawMessage) ([]byte, error) {
	if d == wire.DecisionAllow {
		return nil, nil
	}
	return json.Marshal(map[string]any{
		"decision": "deny",
		"reason":   agent.DenyReason(choice),
	})
}

// profile 是本包的工具名单来源。
var profile = mustProfile("grok")

func mustProfile(name string) agent.Profile {
	p, ok := agent.Lookup(name)
	if !ok {
		panic("grok: profile missing")
	}
	return p
}
