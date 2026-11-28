package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/devpilot-app/apns/pkg/wire"
)

// HookFormat 是 hooks 配置文件的条目形状。
//
// 同一份合并算法(保留外来条目、按 marker 删自家、原子写)服务所有
// merged 型文件;owned 型文件整文件归我们,卸载即删。
type HookFormat string

const (
	// FormatNestedMatcher 是 Claude Code 形:{matcher, hooks:[{type,command,timeout}]}。
	// claude / codex / qwen / gemini 同形;grok 同形但独占文件。
	FormatNestedMatcher HookFormat = "nested"
	// FormatFlat 是 Cursor 形:{version, hooks:{事件:[{command, timeout}]}}——
	// 命令直接躺在事件数组里,没有 matcher/hooks 包装。
	FormatFlat HookFormat = "flat"
	// FormatCopilot 是 Copilot 形:{version, hooks:{事件:[{type,command,timeoutSec}]}}。
	// unix 下命令写在 command 键;单行 JSON 输出是它的纪律。
	FormatCopilot HookFormat = "copilot"
)

// FileOwnership 描述配置文件的归属。
type FileOwnership string

const (
	// OwnershipMerged 与用户(及其他工具)的条目共存,合并写入。
	OwnershipMerged FileOwnership = "merged"
	// OwnershipOwned 整文件归 devpilot-hook(固定文件名),卸载即删。
	OwnershipOwned FileOwnership = "owned"
)

// TimeoutUnit 是该 agent 的 hook 超时字段单位。
type TimeoutUnit string

const (
	// TimeoutSeconds 按秒解释(claude / codex / cursor / grok)。
	TimeoutSeconds TimeoutUnit = "s"
	// TimeoutMilliseconds 按毫秒解释(gemini 默认 60000)。
	TimeoutMilliseconds TimeoutUnit = "ms"
	// TimeoutLegacyMSOver1000 是 Qwen 规则:秒为准,值 ≥1000 兼容读作毫秒。
	// 30 分钟等待窗必须写毫秒(1800000),否则 1860 会被读成 1.86 秒。
	TimeoutLegacyMSOver1000 TimeoutUnit = "legacy-ms"
	// TimeoutSecField 是 Copilot 的字段名差异:timeoutSec(默认 30)。
	TimeoutSecField TimeoutUnit = "timeoutSec"
)

// HookFileSpec 描述一个 agent 的 hooks 配置落点。
type HookFileSpec struct {
	// Path 返回配置文件路径(CLI 相对 home 解析,便于测试注入)。
	Path func() (string, error)
	// Format 是条目形状(见 HookFormat)。
	Format HookFormat
	// Ownership 决定合并还是整文件接管。
	Ownership FileOwnership
	// TimeoutUnit 是超时字段单位(见 TimeoutUnit)。
	TimeoutUnit TimeoutUnit
}

// EventSpec 是要注册的一条 hook 事件。
type EventSpec struct {
	// Event 是该 agent 配置文件里的事件键(原样写入)。
	Event string
	// Matcher 是条目的 matcher 值;"" = 不写 matcher 键(生命周期事件)。
	Matcher string
}

// Profile 汇总一个 agent 的全部接入差异,是 hook 侧唯一的白名单与
// 分歧表:CLI 允许列表、argv 门、tmux 探测、安装器、doctor、self-heal
// 全部从 Profiles() 派生,不再各自硬编码。
type Profile struct {
	// Wire 是线上协议的 agent 标识(= 目录名 = `hook <name>` 的 argv)。
	Wire wire.Agent
	// ProcessNames 是 tmux 深探测认得的 argv0 basename 候选(含 node shim
	// 装法的裸名)。裸名过通用时(如 cursor/grok 共用的 agent)配合
	// PathHints 消歧。
	ProcessNames []string
	// PathHints 是 argv 全文里的路径段提示(node 布局
	// @google/gemini-cli/dist/index.js 的 basename 是 index.js,只能靠段)。
	PathHints []string
	// EventAliases 把该 agent 原始 hook_event_name 归一化为六规范名
	// (SessionStart/PreToolUse/PermissionRequest/PostToolUse/Stop/SessionEnd)。
	// 适配器据此共用一套网关白名单(tmux 探测、节流、锚点)。
	EventAliases map[string]string
	// ProjectDirEnv 是会话项目根的环境变量候选,按优先级排列,取首个非空。
	ProjectDirEnv []string
	// ReadOnlyTools 是闸门模式不拦的只读工具(小写比较);不在表内的
	// 工具一律视为会改状态——未知工具按可变处理,方向是宁滥勿漏。
	ReadOnlyTools []string
	// GateOnly 为 true = 该 agent 的 hook 体系无法代答原生弹窗(Gemini /
	// Grok):默认模式只出通知;pre-tool-use 模式必须以免确认参数启动,
	// 本 hook 是唯一闸门,失败必须 fail-closed。
	GateOnly bool
	// DropPRInPreToolUse 为 true = pre-tool-use 模式下安装器不注册
	// PermissionRequest(通道换成 PreToolUse,双注册会一调用两张卡)。
	// claude / copilot 为 true;codex 历史行为保留全集,不在此列。
	DropPRInPreToolUse bool
	// HookFile 是配置落点(opencode 无此字段,走插件)。
	HookFile *HookFileSpec
	// Events 是要注册的事件表(merged 文件的清理全集也由它派生)。
	Events []EventSpec
	// LaunchHint 是闸门模式的启动提示(doctor / install 输出用),空 =
	// 无需提示。
	LaunchHint string
}

// homeFile 返回 $HOME 下的相对路径;HOME 缺失时报错。
func homeFile(dir, name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("agent: cannot locate home directory: %w", err)
	}
	return filepath.Join(home, dir, name), nil
}

// Profiles 返回全部受支持 agent 的接入档案(不含 opencode——它是插件,
// 没有 hooks 配置文件,由 cli 的 opencode 专用路径处理)。
func Profiles() map[string]Profile {
	return map[string]Profile{
		"claude": {
			Wire:          wire.AgentClaude,
			ProcessNames:  []string{"claude"},
			EventAliases:  map[string]string{},
			ProjectDirEnv: []string{"CLAUDE_PROJECT_DIR"},
			ReadOnlyTools: claudeReadOnlyTools,
			// pre-tool-use 模式下 PreToolUse 就是审批通道,PermissionRequest
			// 不再注册(2026-11 前的既有行为)。
			DropPRInPreToolUse: true,
			HookFile: &HookFileSpec{
				Path:        func() (string, error) { return homeFile(".claude", "settings.json") },
				Format:      FormatNestedMatcher,
				Ownership:   OwnershipMerged,
				TimeoutUnit: TimeoutSeconds,
			},
			Events: []EventSpec{
				{"SessionStart", "*"},
				{"PreToolUse", "*"},
				{"PermissionRequest", "*"},
				{"PostToolUse", "*"},
				{"Stop", ""},
				{"SessionEnd", ""},
			},
		},
		"codex": {
			Wire:          wire.AgentCodex,
			ProcessNames:  []string{"codex"},
			EventAliases:  map[string]string{},
			ProjectDirEnv: []string{"CODEX_PROJECT_DIR", "CLAUDE_PROJECT_DIR"},
			ReadOnlyTools: claudeReadOnlyTools,
			HookFile: &HookFileSpec{
				Path:        func() (string, error) { return homeFile(".codex", "hooks.json") },
				Format:      FormatNestedMatcher,
				Ownership:   OwnershipMerged,
				TimeoutUnit: TimeoutSeconds,
			},
			Events: []EventSpec{
				{"SessionStart", "*"},
				{"PreToolUse", "*"},
				{"PermissionRequest", "*"},
				{"PostToolUse", "*"},
				{"Stop", ""},
			},
		},
		"qwen": {
			Wire:          wire.AgentQwen,
			ProcessNames:  []string{"qwen"},
			PathHints:     []string{"/@qwen-code/"},
			EventAliases:  map[string]string{}, // 事件名与 Claude Code 同构
			ProjectDirEnv: []string{"QWEN_PROJECT_DIR", "CLAUDE_PROJECT_DIR", "GEMINI_PROJECT_DIR"},
			ReadOnlyTools: geminiReadOnlyTools,
			HookFile: &HookFileSpec{
				Path:        func() (string, error) { return homeFile(".qwen", "settings.json") },
				Format:      FormatNestedMatcher,
				Ownership:   OwnershipMerged,
				TimeoutUnit: TimeoutLegacyMSOver1000,
			},
			Events: []EventSpec{
				{"SessionStart", "*"},
				{"PreToolUse", "*"},
				{"PermissionRequest", "*"},
				{"PostToolUse", "*"},
				{"Stop", ""},
				{"SessionEnd", ""},
			},
		},
		"gemini": {
			Wire:         wire.AgentGemini,
			ProcessNames: []string{"gemini"},
			PathHints:    []string{"/@google/gemini-cli/"},
			EventAliases: map[string]string{
				"BeforeTool":   "PreToolUse",
				"AfterTool":    "PostToolUse",
				"AfterAgent":   "Stop",
				"Notification": "PermissionRequest",
			},
			ProjectDirEnv: []string{"GEMINI_PROJECT_DIR", "CLAUDE_PROJECT_DIR"},
			ReadOnlyTools: geminiReadOnlyTools,
			GateOnly:      true,
			// Gemini 的原生弹窗在 BeforeTool 之前决策,hook 无法代答;
			// 闸门模式必须 --approval-mode yolo 启动,hook 是唯一闸门。
			LaunchHint: "gemini --approval-mode yolo",
			HookFile: &HookFileSpec{
				Path:        func() (string, error) { return homeFile(".gemini", "settings.json") },
				Format:      FormatNestedMatcher,
				Ownership:   OwnershipMerged,
				TimeoutUnit: TimeoutMilliseconds,
			},
			Events: []EventSpec{
				{"SessionStart", "*"},
				{"BeforeTool", "*"},
				{"AfterTool", "*"},
				{"AfterAgent", ""},
				{"SessionEnd", "*"},
				{"Notification", "*"},
			},
		},
		"grok": {
			Wire:         wire.AgentGrok,
			ProcessNames: []string{"grok", "agent"},
			PathHints:    []string{"/.grok/", "grok-build"},
			EventAliases: map[string]string{
				"pre_tool_use":  "PreToolUse",
				"post_tool_use": "PostToolUse",
				"stop":          "Stop",
				"session_start": "SessionStart",
				"session_end":   "SessionEnd",
				"notification":  "PermissionRequest",
				// Grok 亦接受 Claude 形别名,一并归一。
				"PreToolUse":   "PreToolUse",
				"PostToolUse":  "PostToolUse",
				"Stop":         "Stop",
				"SessionStart": "SessionStart",
				"SessionEnd":   "SessionEnd",
				"Notification": "PermissionRequest",
			},
			ProjectDirEnv: []string{"GROK_WORKSPACE_ROOT", "CLAUDE_PROJECT_DIR"},
			ReadOnlyTools: geminiReadOnlyTools,
			GateOnly:      true,
			// Grok 的 PreToolUse allow 是空操作,原生弹窗不可代答;闸门模式
			// 必须 --always-approve 启动。安装器独占 ~/.grok/hooks/devpilot.json,
			// 与 Grok 自动读取的 ~/.claude/settings.json 互不干扰(运行期由
			// GROK_* 环境变量识别并抑制 Claude 条目)。
			LaunchHint: "grok --always-approve",
			HookFile: &HookFileSpec{
				Path:        func() (string, error) { return homeFile(".grok", filepath.Join("hooks", "devpilot.json")) },
				Format:      FormatNestedMatcher,
				Ownership:   OwnershipOwned,
				TimeoutUnit: TimeoutSeconds,
			},
			Events: []EventSpec{
				{"SessionStart", ""},
				{"PreToolUse", ""},
				{"PostToolUse", ""},
				{"Stop", ""},
				{"SessionEnd", ""},
				{"Notification", ""},
			},
		},
		"cursor": {
			Wire:         wire.AgentCursor,
			ProcessNames: []string{"cursor-agent", "agent"},
			PathHints:    []string{"/cursor-agent/", "/.cursor/"},
			EventAliases: map[string]string{
				"preToolUse":   "PreToolUse",
				"postToolUse":  "PostToolUse",
				"stop":         "Stop",
				"sessionStart": "SessionStart",
				"sessionEnd":   "SessionEnd",
			},
			ProjectDirEnv: []string{"CURSOR_PROJECT_DIR", "CLAUDE_PROJECT_DIR"},
			ReadOnlyTools: cursorReadOnlyTools,
			HookFile: &HookFileSpec{
				Path:        func() (string, error) { return homeFile(".cursor", "hooks.json") },
				Format:      FormatFlat,
				Ownership:   OwnershipMerged,
				TimeoutUnit: TimeoutSeconds,
			},
			Events: []EventSpec{
				{"sessionStart", ""},
				{"preToolUse", ""},
				{"postToolUse", ""},
				{"stop", ""},
				{"sessionEnd", ""},
			},
		},
		"copilot": {
			Wire:          wire.AgentCopilot,
			ProcessNames:  []string{"copilot"},
			PathHints:     []string{"/@github/copilot/"},
			EventAliases:  map[string]string{}, // PascalCase 注册 → snake_case 载荷,规范名直读
			ProjectDirEnv: []string{"COPILOT_PROJECT_DIR", "CLAUDE_PROJECT_DIR"},
			ReadOnlyTools: copilotReadOnlyTools,
			// pre-tool-use 模式下 PreToolUse 是审批通道,PermissionRequest
			// 不再注册(双注册 = 一调用两张卡)。
			DropPRInPreToolUse: true,
			HookFile: &HookFileSpec{
				Path:        func() (string, error) { return homeFile(".copilot", filepath.Join("hooks", "devpilot.json")) },
				Format:      FormatCopilot,
				Ownership:   OwnershipOwned,
				TimeoutUnit: TimeoutSecField,
			},
			// PascalCase 事件键 = VS Code 兼容载荷(snake_case 字段 +
			// hook_event_name),camelCase 注册会换一套字段名,统一走前者。
			Events: []EventSpec{
				{"SessionStart", ""},
				{"PreToolUse", ""},
				{"PermissionRequest", ""},
				{"PostToolUse", ""},
				{"Stop", ""},
				{"SessionEnd", ""},
			},
		},
	}
}

// Lookup 按 CLI/argv 名取档案;未登记返回 false——调用方一律静默退出
// (hook 客户端)或 404(网关),绝不猜测。
func Lookup(name string) (Profile, bool) {
	p, ok := Profiles()[name]
	return p, ok
}

// Names 返回全部受支持的名字(安装器/doctor 遍历用)。
func Names() []string {
	return []string{"claude", "codex", "opencode", "qwen", "gemini", "grok", "cursor", "copilot"}
}

// CanonicalEvent 把原始 hook_event_name 归一化为六规范名;未知事件原样
// 返回(适配器的 default 分支丢弃)。
func (p Profile) CanonicalEvent(raw string) string {
	if c, ok := p.EventAliases[raw]; ok {
		return c
	}
	return raw
}

// IsMutating 判断工具是否需要闸门:只读名单内的工具放行,名单外(含
// 未知工具)一律按可变处理。比较不分大小写——各家工具名有 Bash/bash、
// Read/read 两种拼写。
func (p Profile) IsMutating(tool string) bool {
	if tool == "" {
		return true
	}
	for _, t := range p.ReadOnlyTools {
		if strings.EqualFold(t, tool) {
			return false
		}
	}
	return true
}

// ProjectDir 按优先级取第一个非空的项目根环境变量。
func (p Profile) ProjectDir(getenv func(string) string) string {
	for _, k := range p.ProjectDirEnv {
		if v := getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// geminiReadOnlyTools 是 Gemini / Qwen / Grok 共用的只读工具名单
// (三家同源于 Gemini CLI 的工具集;大小写不敏感比较在 IsMutating 前
// 统一 ToLower——此处存小写)。
var geminiReadOnlyTools = []string{
	"read_file", "read_many_files", "glob_search", "search_file_content",
	"ls", "list_directory", "grep_search", "web_fetch", "google_web_fetch",
	"web_search", "google_web_search", "todo_read", "memory",
}

var claudeReadOnlyTools = []string{
	"read", "glob", "grep", "todowrite", "todoread", "notebookread",
	"webfetch", "websearch", "listmcpresources", "readmcpresource",
}

var cursorReadOnlyTools = []string{
	"read", "grep", "glob", "websearch", "webfetch", "list_dir",
	"codebase_search", "search",
}

var copilotReadOnlyTools = []string{
	"read", "glob", "grep", "web-fetch", "webfetch", "search",
	"list_mcp_resources", "think",
}
