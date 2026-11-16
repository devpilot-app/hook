package agent

// DenyFallbackMessage 是 deny 决策缺失 message 时的兜底文案。OpenCode 2.x
// 插件模板注入同一常量（见 cli.openCodePluginSource），插件与三个 CLI
// 适配器共用一份，改措辞只落一处。
const DenyFallbackMessage = "Denied from phone"

// DenyReason 生成 deny 输出里的理由文本；选项作答（choice 非空）时把标签
// 原样喂回模型。claude / codex / opencode 三个适配器的 RenderDecision 共用
// 这一份文案，避免同一动作在不同 Agent 里呈现不同的拒绝理由。
func DenyReason(choice string) string {
	if choice == "" {
		return DenyFallbackMessage
	}
	return "The user has already answered this question from their phone. " +
		"Their answer is: \"" + choice + "\". Treat it as the user's final choice and proceed accordingly without asking again."
}
