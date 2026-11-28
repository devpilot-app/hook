// Package qwen 实现 Qwen Code 的 hook 适配器。
//
// Qwen Code 是 Gemini CLI 的分支,但 hook 体系是 Claude Code 形状:
// settings.json 的 hooks 键、六规范事件名、PermissionRequest 可代答
// (hookSpecificOutput.decision.behavior)。差异点:
//   - PreToolUse 的 permissionDecision:"allow" 不跳过原生弹窗(官方文档
//     明写 usual permission checks still apply),pre-tool-use 阻塞模式
//     对 Qwen 无意义——拦下来手机批完终端还要再问一次。PreToolUse 恒作
//     tool_running 遥测,审批只走 PermissionRequest(mode 参数被有意忽略)。
//   - PermissionRequest 载荷带 tool_use_id(与 Claude 同源),审批 ID 稳定。
package qwen

import (
	"encoding/json"

	"github.com/devpilot-app/apns/pkg/wire"
	"github.com/devpilot-app/hook/internal/agent"
	"github.com/devpilot-app/hook/internal/config"
)

// Adapter 是 Qwen 适配器。
type Adapter struct{}

// Name 返回 wire 中的 agent 标识。
func (Adapter) Name() wire.Agent { return wire.AgentQwen }

// Normalize 把 Qwen hook 事件映射为 wire 事件。answerQuestions 是
// Claude Code 专用开关(Qwen 无 AskUserQuestion 工具),忽略该参。
func (Adapter) Normalize(in agent.Input, daemonID string, now int64, mode config.ApprovalMode, answerQuestions bool) (*agent.Event, error) {
	switch in.HookEventName {
	case "SessionStart":
		env := agent.NewEnvelope(wire.AgentQwen, daemonID, in, wire.EventSessionStarted, agent.BasePayload(in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PreToolUse":
		// 见包注释:Qwen 的 PreToolUse allow 不免弹窗,阻塞模式无效。
		env := agent.NewEnvelope(wire.AgentQwen, daemonID, in, wire.EventToolRunning, agent.WithTool(agent.BasePayload(in), in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PermissionRequest":
		env := agent.NewApprovalEnvelope(wire.AgentQwen, daemonID, in, now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName, NeedsApproval: true}, nil
	case "PostToolUse":
		env := agent.NewEnvelope(wire.AgentQwen, daemonID, in, wire.EventToolFinished, agent.WithTool(agent.BasePayload(in), in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "Stop":
		p := agent.BasePayload(in)
		p.ReplySummary = wire.Truncate(in.LastAssistantMessage, agent.ReplySummaryRunes)
		env := agent.NewEnvelope(wire.AgentQwen, daemonID, in, wire.EventTaskComplete, p, now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "SessionEnd":
		env := agent.NewEnvelope(wire.AgentQwen, daemonID, in, wire.EventSessionEnded, agent.BasePayload(in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	default:
		return nil, nil
	}
}

// RenderDecision 生成 Qwen 期望的 hook 输出。
//
// PermissionRequest 与 Claude Code 同构:behavior allow 必须回显
// updatedInput(与 claude.RenderDecision 的 2026-11 真机修正同一道理,
// 原生弹窗只认完整入参回显)。PreToolUse 分支是防御位(当前 Normalize
// 不会在阻塞模式放行 PreToolUse),形状按官方 permissionDecision 契约。
func (Adapter) RenderDecision(hookEvent string, d wire.DecisionKind, choice, toolName string, toolInput json.RawMessage) ([]byte, error) {
	if hookEvent == "PreToolUse" {
		if d == wire.DecisionAllow {
			return json.Marshal(map[string]any{
				"hookSpecificOutput": map[string]any{
					"hookEventName":      "PreToolUse",
					"permissionDecision": "allow",
				},
			})
		}
		return json.Marshal(map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":            "PreToolUse",
				"permissionDecision":       "deny",
				"permissionDecisionReason": agent.DenyReason(choice),
			},
		})
	}

	decision := map[string]any{"behavior": "allow"}
	if d == wire.DecisionDeny {
		decision["behavior"] = "deny"
		decision["message"] = agent.DenyReason(choice)
	} else if obj, ok := decodeObject(toolInput); ok {
		decision["updatedInput"] = obj
	}
	return json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName": "PermissionRequest",
			"decision":      decision,
		},
	})
}

// decodeObject 把原始 tool_input 解析成对象;空/非法/非对象一律 false。
func decodeObject(raw json.RawMessage) (map[string]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}
