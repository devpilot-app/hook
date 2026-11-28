// Package cursor 实现 Cursor CLI 的 hook 适配器。
//
// 官方 hooks 文档:preToolUse 回 {"permission":"allow"} 即「proceeds
// with no user prompt」——这是 Cursor 唯一的手机代答通道(没有
// PermissionRequest 等价事件)。由此:
//
//   - 默认模式:无审批卡(Cursor 的原生 y/n 提示不经过任何 hook),
//     只有会话卡与完成卡;preToolUse 作 tool_running 遥测。
//   - pre-tool-use(闸门)模式:preToolUse 对可变工具阻塞等手机。
//     与 Gemini/Grok 不同,Cursor 的原生弹窗还在:等待窗耗尽时空响应
//     会回落原生询问(不用 fail-closed),所以 Gate=false。
//
// 字段是 camelCase(ParseInputAs 已回落);session 键优先 session_id,
// 缺位回落 conversation_id。
package cursor

import (
	"encoding/json"

	"github.com/devpilot-app/apns/pkg/wire"
	"github.com/devpilot-app/hook/internal/agent"
	"github.com/devpilot-app/hook/internal/config"
)

// Adapter 是 Cursor 适配器。
type Adapter struct{}

// Name 返回 wire 中的 agent 标识。
func (Adapter) Name() wire.Agent { return wire.AgentCursor }

// Normalize 把 Cursor hook 事件映射为 wire 事件。
// 事件名已由 ParseInputAs 归一化(preToolUse→PreToolUse 等)。
func (Adapter) Normalize(in agent.Input, daemonID string, now int64, mode config.ApprovalMode, answerQuestions bool) (*agent.Event, error) {
	switch in.HookEventName {
	case "SessionStart":
		env := agent.NewEnvelope(wire.AgentCursor, daemonID, in, wire.EventSessionStarted, agent.BasePayload(in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PreToolUse":
		if mode == config.ApprovalModePreToolUse && profile.IsMutating(in.ToolName) {
			env := agent.NewApprovalEnvelope(wire.AgentCursor, daemonID, in, now)
			return &agent.Event{Envelope: env, HookEvent: in.HookEventName, NeedsApproval: true}, nil
		}
		env := agent.NewEnvelope(wire.AgentCursor, daemonID, in, wire.EventToolRunning, agent.WithTool(agent.BasePayload(in), in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PostToolUse":
		env := agent.NewEnvelope(wire.AgentCursor, daemonID, in, wire.EventToolFinished, agent.WithTool(agent.BasePayload(in), in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "Stop":
		p := agent.BasePayload(in)
		// Cursor 的 stop 不携带最终回复文本(afterAgentResponse 才带,
		// 且按响应流触发);summary 留空,与 opencode 同款保守处理。
		env := agent.NewEnvelope(wire.AgentCursor, daemonID, in, wire.EventTaskComplete, p, now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "SessionEnd":
		env := agent.NewEnvelope(wire.AgentCursor, daemonID, in, wire.EventSessionEnded, agent.BasePayload(in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	default:
		return nil, nil
	}
}

// RenderDecision 生成 Cursor 期望的 hook 输出。
//
// 官方契约:permission 是 allow/deny/ask 三值(ask 当前 schema 接受但
// 不生效);deny 时 user_message 展示给用户/模型。只发 mutating 工具的
// 决策,allow 不带多余键。
func (Adapter) RenderDecision(hookEvent string, d wire.DecisionKind, choice, _ string, _ json.RawMessage) ([]byte, error) {
	if d == wire.DecisionAllow {
		return json.Marshal(map[string]any{"permission": "allow"})
	}
	return json.Marshal(map[string]any{
		"permission":   "deny",
		"user_message": agent.DenyReason(choice),
	})
}

// profile 是本包的工具名单来源。
var profile = mustProfile("cursor")

func mustProfile(name string) agent.Profile {
	p, ok := agent.Lookup(name)
	if !ok {
		panic("cursor: profile missing")
	}
	return p
}
