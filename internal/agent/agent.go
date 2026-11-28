// Package agent 负责把各 Agent 的 hook 输入归一化为 wire 事件。
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/devpilot-app/apns/pkg/wire"
	"github.com/devpilot-app/hook/internal/config"
	"github.com/devpilot-app/hook/internal/id"
)

// ReplySummaryRunes 是 Stop 回复摘要的详情帽（2000 runes）。
// wire.MaxReplyRunes=80 是横幅/预览口径——收件箱详情页要求全文级,80 必出
// 省略号（2026-11-02 用户反馈）;列表预览由 App 侧 2 行截断。APNs 侧 relay
// 组包自会再按横幅帽截,两头口径各自成立。取 Truncate 而非 Summarize:
// 换行不压扁,App 端 markdown 渲染（栅栏/列表）才能出形。
const ReplySummaryRunes = 2000

// Input 是 Claude Code / Codex hook 的公共输入字段。
//
// 各家字段高度重合但拼写两态(snake_case / camelCase):这里把两态都收进
// 来,ParseInputAs 做「缺谁补谁」的回落;agent 专属语义(Gemini 的
// prompt_response / Notification.details、Cursor 的 conversation_id)由
// 对应适配器自行取用。ProjectDir 不在 stdin JSON 里,由 CLI 经网关
// header 转交。
type Input struct {
	SessionID       string `json:"session_id"`
	SessionIDCamel  string `json:"sessionId"`
	TranscriptPath  string `json:"transcript_path"`
	TranscriptCamel string `json:"transcriptPath"`
	Cwd             string `json:"cwd"`
	// ProjectDir 是会话启动时的项目根(agent 按 Profile.ProjectDirEnv 导出到
	// hook 进程,由 CLI 经网关 header 转交;不出现在 hook 输入 JSON 里)。
	// 它整个会话不变,而 Cwd「跟随 Claude 的 cd」(Claude Code 官方文档)
	// ——标题与会话归属必须以它为准,Cwd 只作回落。
	ProjectDir           string          `json:"-"`
	HookEventName        string          `json:"hook_event_name"`
	HookEventNameCamel   string          `json:"hookEventName"`
	ToolName             string          `json:"tool_name"`
	ToolNameCamel        string          `json:"toolName"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolInputCamel       json.RawMessage `json:"toolInput"`
	ToolUseID            string          `json:"tool_use_id"`
	ToolUseIDCamel       string          `json:"toolUseId"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	ToolResponseCamel    json.RawMessage `json:"toolResponse"`
	ToolArgs             json.RawMessage `json:"toolArgs"`
	Prompt               string          `json:"prompt"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	LastAssistantCamel   string          `json:"lastAssistantMessage"`
	// PromptResponse 是 Gemini AfterAgent 的最终回复(prompt_response),
	// 语义同 LastAssistantMessage,ParseInputAs 回落补齐。
	PromptResponse string `json:"prompt_response"`
	// ConversationID 是 Cursor 的会话键(全事件携带;session_id 只在
	// sessionStart/End 出现),ParseInputAs 回落补齐。
	ConversationID string `json:"conversation_id"`
	// WorkspaceRoot 是 Grok 的工作区根,项目根回落用。
	WorkspaceRoot  string `json:"workspaceRoot"`
	Source         string `json:"source"`
	Reason         string `json:"reason"`
	StopHookActive bool   `json:"stop_hook_active"`
	// Notification* 是「原生弹窗正在展示」类事件(Gemini Notification /
	// Grok notification)的载荷:notification_type 区分 ToolPermission 与
	// 其他;details 里 Gemini 带完整文件内容(edit),**只允许适配器摘
	// 白名单子集**,原样透传是隐私红线。
	NotificationType  string          `json:"notification_type"`
	NotificationCamel string          `json:"notificationType"`
	Message           string          `json:"message"`
	Details           json.RawMessage `json:"details"`
}

// ParseInput 解析 hook stdin(按 Claude/Codex 习惯,别名两态均认)。
func ParseInput(data []byte) (Input, error) {
	return ParseInputAs("", data)
}

// ParseInputAs 解析 hook stdin 并按 agent 档案归一化:camelCase / 别名字段
// 回落填空,事件名归一化为六规范名。name 为空(旧调用方/测试)时跳过
// 事件名归一化,行为与历史版本逐字节一致。
func ParseInputAs(name string, data []byte) (Input, error) {
	var in Input
	if err := json.Unmarshal(data, &in); err != nil {
		return in, fmt.Errorf("agent: failed to parse hook input: %w", err)
	}
	// 缺谁补谁:两态拼写并存时 snake_case 优先(历史行为),camelCase 兜底。
	if in.SessionID == "" {
		in.SessionID = in.SessionIDCamel
	}
	if in.SessionID == "" && in.ConversationID != "" {
		in.SessionID = in.ConversationID
	}
	if in.TranscriptPath == "" {
		in.TranscriptPath = in.TranscriptCamel
	}
	if in.HookEventName == "" {
		in.HookEventName = in.HookEventNameCamel
	}
	if in.ToolName == "" {
		in.ToolName = in.ToolNameCamel
	}
	if len(in.ToolInput) == 0 {
		in.ToolInput = in.ToolInputCamel
	}
	if len(in.ToolInput) == 0 {
		// Copilot camelCase 载荷的工具入参键。
		in.ToolInput = in.ToolArgs
	}
	if in.ToolUseID == "" {
		in.ToolUseID = in.ToolUseIDCamel
	}
	if len(in.ToolResponse) == 0 {
		in.ToolResponse = in.ToolResponseCamel
	}
	if in.LastAssistantMessage == "" {
		in.LastAssistantMessage = in.LastAssistantCamel
	}
	// Gemini AfterAgent 的最终回复与 Stop 同语义。
	if in.LastAssistantMessage == "" {
		in.LastAssistantMessage = in.PromptResponse
	}
	if in.NotificationType == "" {
		in.NotificationType = in.NotificationCamel
	}
	if in.SessionID == "" {
		// 没有会话 ID 时用 transcript 路径派生稳定 ID,避免审批无法路由;
		// 两者都没有则拒绝,因为无法保证同一会话内幂等。
		if in.TranscriptPath == "" {
			return in, fmt.Errorf("agent: hook input missing session_id and transcript_path")
		}
		sum := sha256.Sum256([]byte(in.TranscriptPath))
		in.SessionID = "sess_" + hex.EncodeToString(sum[:])[:16]
	}
	// 事件名归一化放最后:tmux 探测白名单与适配器只认六规范名。
	if name != "" {
		if p, ok := Lookup(name); ok {
			in.HookEventName = p.CanonicalEvent(in.HookEventName)
		}
	}
	return in, nil
}

// Event 是归一化结果。
//
// NeedsApproval=false 而 Envelope.Type=approval_required 是合法组合：
// AskUserQuestion 的「只通知不阻塞」——事件照发（手机卡片告知 + 导航），
// 但不登记等待、不放行/拒绝任何决策，hook 立即退出让原生问题框回终端。
type Event struct {
	Envelope      wire.Envelope
	HookEvent     string
	NeedsApproval bool
	// Gate 为 true = 该审批是「闸门」:agent 以免确认参数启动(Gemini
	// yolo / Grok always-approve),本 hook 是唯一闸门,没有原生弹窗可
	// 回落。网关据此在等待窗耗尽/取消时回 200+deny(fail-closed),而非
	// 204 让 agent 走原生询问。普通审批卡(有原生弹窗兜底)必须保持
	// false——超时空响应回落原生询问是全链路的设计底线。
	Gate bool
}

// Adapter 隔离 Agent 之间的差异。
type Adapter interface {
	Name() wire.Agent
	// Normalize 返回 nil 表示该 hook 事件不需要上报。answerQuestions 表示
	// 允许把 PreToolUse 的 AskUserQuestion 拦成审批（Claude Code 专用；
	// 其他适配器可忽略）。
	Normalize(in Input, daemonID string, now int64, mode config.ApprovalMode, answerQuestions bool) (*Event, error)
	// RenderDecision 生成 hook stdout；只应在拿到 allow/deny 时调用。
	// choice 是选项型审批所选的候选标签（无则空串）。toolName / toolInput
	// 是原 hook 的工具名与完整入参：PermissionRequest 的 allow 必须回显
	// updatedInput 才生效，AskUserQuestion 作答也要靠它构造 answers。
	RenderDecision(hookEvent string, d wire.DecisionKind, choice, toolName string, toolInput json.RawMessage) ([]byte, error)
}

// BasePayload 生成所有事件共有的摘要字段。
//
// 标题取会话项目根（ProjectDir）的目录名，缺位才回落 Cwd：Cwd 会跟随
// agent 的 `cd`，拿它当标题，同一条会话后半程的卡片会集体改姓（2026-11-23
// 真机：一条会话先叫 DevPilot_App、跑过 `cd …/DevPilot-Hook` 之后全变
// DevPilot-Hook）。
func BasePayload(in Input) wire.Payload {
	title := "Agent session"
	if base := filepath.Base(ProjectRoot(in)); base != "." && base != string(filepath.Separator) {
		title = base
	}
	title = wire.Truncate(title, wire.MaxTitleRunes)
	p := wire.Payload{Title: title, SessionTitle: title, ProjectDir: strings.TrimSpace(in.ProjectDir)}
	if in.Prompt != "" {
		p.PromptSummary = wire.Summarize(in.Prompt, wire.MaxPromptRunes)
	}
	return p
}

// ProjectRoot 返回会话的项目根：ProjectDir 优先，缺位回落 Cwd，都没有为空。
// hook 进程的当前目录（os.Getwd）绝不参与——hook 被 agent 以独立工作目录
// 拉起，那里不是会话所在。
func ProjectRoot(in Input) string {
	if dir := strings.TrimSpace(in.ProjectDir); dir != "" {
		return dir
	}
	return strings.TrimSpace(in.Cwd)
}

// WithTool 补充工具调用摘要（不含完整输入）。
func WithTool(p wire.Payload, in Input) wire.Payload {
	p.ToolName = wire.Truncate(in.ToolName, 64)
	p.CommandSummary = wire.SummarizeToolInput(in.ToolInput)
	p.InputDigest = wire.InputDigest(in.ToolInput)
	// 选项型审批（AskUserQuestion 恰好一问）的候选标签：App 审批卡渲染
	// 选项钮。非该形态 ExtractOptions 返回 nil,不设键。
	p.Options = wire.ExtractOptions(in.ToolInput)
	return p
}

// toolLead 为文件类工具生成带动词的卡面短语（2026-11-28 真机反馈：Read
// 权限卡正文只有裸路径，用户看不出「要批什么」）。Read/Edit/Write/
// NotebookEdit 各给一句人话，其余工具（Bash/ExitPlanMode/AskUserQuestion
// 等）保持原摘要。ok=false 回落 wire.SummarizeToolInput 的产出。
//
// relay 侧 ApprovalBody 有同形短语透传闸（pkg/wire/alert.go），不会二次
// 包装成 "<Agent> wants to run Read: Claude wants to read: …"。
func toolLead(a wire.Agent, in Input) (string, bool) {
	var key, verb string
	switch in.ToolName {
	case "Read":
		key, verb = "file_path", "read"
	case "Edit", "MultiEdit":
		key, verb = "file_path", "edit"
	case "Write":
		key, verb = "file_path", "write"
	case "NotebookEdit":
		key, verb = "notebook_path", "edit notebook"
	default:
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal(in.ToolInput, &m); err != nil {
		return "", false
	}
	raw, _ := m[key].(string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	base := filepath.Base(raw)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "", false
	}
	return wire.Truncate(fmt.Sprintf("%s wants to %s: %s", wire.DisplayAgent(a), verb, base),
		wire.MaxCommandRunes), true
}

// NewEnvelope 构造事件包封。
func NewEnvelope(a wire.Agent, daemonID string, in Input, t wire.EventType, p wire.Payload, now int64) wire.Envelope {
	return wire.Envelope{
		V:         wire.Version,
		Kind:      wire.KindEvent,
		MsgID:     id.New(),
		EventID:   id.New(),
		Type:      t,
		Agent:     a,
		DaemonID:  daemonID,
		SessionID: in.SessionID,
		Payload:   p,
		TS:        now,
	}
}

// NewApprovalEnvelope 构造审批事件；tool_use_id 缺失时退化为随机 ID。
func NewApprovalEnvelope(a wire.Agent, daemonID string, in Input, now int64) wire.Envelope {
	return newApprovalEnvelope(a, daemonID, in, now, false)
}

// NewNotifyApprovalEnvelope 构造「只通知不阻塞」的等待卡:payload 带
// notify_only,relay 据此复用问题类 category(无动作)、过期免推
// Missed,App 只出 Answer at Terminal。
func NewNotifyApprovalEnvelope(a wire.Agent, daemonID string, in Input, now int64) wire.Envelope {
	env := newApprovalEnvelope(a, daemonID, in, now, true)
	env.Payload.NotifyOnly = true
	return env
}

func newApprovalEnvelope(a wire.Agent, daemonID string, in Input, now int64, notifyOnly bool) wire.Envelope {
	payload := WithTool(BasePayload(in), in)
	// 文件类工具换带动词的卡面短语（问题 2：裸路径看不出要批什么）。摘要
	// 不参与 InputDigest（digest 在 WithTool 里已对原始输入算完），键控与
	// 关联不受影响。
	if lead, ok := toolLead(a, in); ok {
		payload.CommandSummary = lead
	}
	env := NewEnvelope(a, daemonID, in, wire.EventApprovalRequired, payload, now)
	if notifyOnly {
		env.Payload.NotifyOnly = true
	}
	approvalID := ""
	if in.ToolUseID != "" {
		if v, err := wire.ApprovalID(in.SessionID, in.ToolName, in.ToolUseID, in.ToolInput); err == nil {
			approvalID = v
		}
	}
	if approvalID == "" {
		approvalID = "apv_" + id.New()
	}
	env.ApprovalID = approvalID
	return env
}
