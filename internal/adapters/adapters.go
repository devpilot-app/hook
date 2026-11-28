// Package adapters 汇总全部 hooks 型适配器。
//
// serve / gateway / e2e 共用同一张表,新增 agent 只改这里 + agent 包;
// opencode 是插件形态,不在此表(cli 的 opencode 专用路径处理)。
package adapters

import (
	"github.com/devpilot-app/apns/pkg/wire"
	"github.com/devpilot-app/hook/internal/agent"
	"github.com/devpilot-app/hook/internal/agent/claude"
	"github.com/devpilot-app/hook/internal/agent/codex"
	"github.com/devpilot-app/hook/internal/agent/copilot"
	"github.com/devpilot-app/hook/internal/agent/cursor"
	"github.com/devpilot-app/hook/internal/agent/gemini"
	"github.com/devpilot-app/hook/internal/agent/grok"
	"github.com/devpilot-app/hook/internal/agent/opencode"
	"github.com/devpilot-app/hook/internal/agent/qwen"
)

// All 返回 hooks 型适配器表(opencode 除外)。
func All() map[wire.Agent]agent.Adapter {
	return map[wire.Agent]agent.Adapter{
		wire.AgentClaude:  claude.Adapter{},
		wire.AgentCodex:   codex.Adapter{},
		wire.AgentQwen:    qwen.Adapter{},
		wire.AgentGemini:  gemini.Adapter{},
		wire.AgentGrok:    grok.Adapter{},
		wire.AgentCursor:  cursor.Adapter{},
		wire.AgentCopilot: copilot.Adapter{},
	}
}

// OpenCode 是插件型适配器,单独暴露(serve 注册、e2e 使用)。
func OpenCode() agent.Adapter { return opencode.Adapter{} }
