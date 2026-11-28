// Package copilot 实现 GitHub Copilot CLI 的 hook 适配器。
//
// 官方 hooks reference:permissionRequest 在权限服务(规则引擎、自动
// 放行、用户弹窗)之前触发,behavior:"allow" 会短路整个原生流程——
// 与 Claude Code 的 PermissionRequest 同级代答能力。preToolUse 的
// permissionDecision allow/deny 也可用,作 pre-tool-use 模式的通道。
//
// 事件键用 PascalCase 注册(install 侧),载荷即 VS Code 兼容的
// snake_case(hook_event_name / tool_name / tool_input 齐全),无需
// 别名归一。Copilot 纪律:stdout JSON 必须单行(json.Marshal 天然单行)。
//
// TODO(device):permissionRequest 的输入字段官方无 schema,tool_name /
// tool_input 的实际拼写以抓样为准;当前按 snake_case 兼容形态实现。
package copilot

import (
	"encoding/json"

	"github.com/devpilot-app/apns/pkg/wire"
	"github.com/devpilot-app/hook/internal/agent"
	"github.com/devpilot-app/hook/internal/config"
)

// Adapter 是 Copilot 适配器。
type Adapter struct{}

// Name 返回 wire 中的 agent 标识。
func (Adapter) Name() wire.Agent { return wire.AgentCopilot }

// Normalize 把 Copilot hook 事件映射为 wire 事件。
func (Adapter) Normalize(in agent.Input, daemonID string, now int64, mode config.ApprovalMode, answerQuestions bool) (*agent.Event, error) {
	switch in.HookEventName {
	case "SessionStart":
		env := agent.NewEnvelope(wire.AgentCopilot, daemonID, in, wire.EventSessionStarted, agent.BasePayload(in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PreToolUse":
		if mode == config.ApprovalModePreToolUse && profile.IsMutating(in.ToolName) {
			env := agent.NewApprovalEnvelope(wire.AgentCopilot, daemonID, in, now)
			return &agent.Event{Envelope: env, HookEvent: in.HookEventName, NeedsApproval: true}, nil
		}
		env := agent.NewEnvelope(wire.AgentCopilot, daemonID, in, wire.EventToolRunning, agent.WithTool(agent.BasePayload(in), in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PermissionRequest":
		env := agent.NewApprovalEnvelope(wire.AgentCopilot, daemonID, in, now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName, NeedsApproval: true}, nil
	case "PostToolUse":
		env := agent.NewEnvelope(wire.AgentCopilot, daemonID, in, wire.EventToolFinished, agent.WithTool(agent.BasePayload(in), in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "Stop":
		p := agent.BasePayload(in)
		// agentStop 载荷无最终回复文本(subagentStop 才有 response);
		// summary 留空。
		env := agent.NewEnvelope(wire.AgentCopilot, daemonID, in, wire.EventTaskComplete, p, now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "SessionEnd":
		env := agent.NewEnvelope(wire.AgentCopilot, daemonID, in, wire.EventSessionEnded, agent.BasePayload(in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	default:
		return nil, nil
	}
}

// RenderDecision 生成 Copilot 期望的 hook 输出(单行 JSON)。
//
// PermissionRequest:{"behavior":"allow"|"deny","message"}——allow 短路
// 原生权限流;不带未文档化的字段(updatedInput 是否被接受待抓样)。
// PreToolUse:顶层 permissionDecision(+deny 必带 permissionDecisionReason)。
func (Adapter) RenderDecision(hookEvent string, d wire.DecisionKind, choice, toolName string, toolInput json.RawMessage) ([]byte, error) {
	if hookEvent == "PreToolUse" {
		if d == wire.DecisionAllow {
			return json.Marshal(map[string]any{"permissionDecision": "allow"})
		}
		return json.Marshal(map[string]any{
			"permissionDecision":       "deny",
			"permissionDecisionReason": agent.DenyReason(choice),
		})
	}
	if d == wire.DecisionDeny {
		return json.Marshal(map[string]any{
			"behavior": "deny",
			"message":  agent.DenyReason(choice),
		})
	}
	return json.Marshal(map[string]any{"behavior": "allow"})
}

// profile 是本包的工具名单来源。
var profile = mustProfile("copilot")

func mustProfile(name string) agent.Profile {
	p, ok := agent.Lookup(name)
	if !ok {
		panic("copilot: profile missing")
	}
	return p
}
