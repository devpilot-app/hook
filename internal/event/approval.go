// Package event 提供审批状态机与事件节流。
package event

import (
	"context"
	"sync"
	"time"

	"github.com/devpilot-app/apns/pkg/wire"
)

// Status 是审批状态。
type Status string

const (
	StatusPending   Status = "pending"
	StatusApproved  Status = "approved"
	StatusDenied    Status = "denied"
	StatusExpired   Status = "expired"
	StatusCancelled Status = "cancelled"
	StatusUnknown   Status = "unknown"
)

// Approval 是一条等待手机决策的审批。
type Approval struct {
	ID        string
	Envelope  wire.Envelope
	CreatedAt time.Time
	Deadline  time.Time

	// 以下字段由 Registry.mu 保护。
	status   Status
	decision wire.DecisionKind
	// choice 是选项型审批所选的候选标签（可空）；allow/deny 语义之外的
	// 附加信息，RenderDecision 据此生成喂回 agent 的理由文本。
	choice string
	// external 标记「收场动作已由外部执行」（终端里答完后的 CancelExternal
	// 路径）：close 事件由收场方发出，Wait 路径不得重复发，否则同一个
	// approval_id 会出现 cancelled + expired 两条 close（relay 去重窗只有
	// 10 分钟，挡不住 30 分钟后的 timer）。
	external bool
	done     chan struct{}
}

// Result 是 Wait 的返回值。
type Result struct {
	Status   Status
	Decision wire.DecisionKind
	// Choice 随决策带回（选项型审批所选标签，无则空串）。
	Choice string
	// External=true 表示审批已被外部收场（终端答完），close 已由收场方
	// 发出：调用方应静默返回 204，不再 emitClose、也不渲染决策。
	External bool
}

// DecideResult 是 Decide 的返回值。
type DecideResult struct {
	Status   Status
	Decision wire.DecisionKind
	// Choice 原样带回本次提交的选项标签。
	Choice string
	// Accepted 表示本次调用真正改变了状态；false 表示重复决策或已过期。
	Accepted bool
}

// Registry 保存本 daemon 所有 pending 审批。
//
// 只存内存：daemon 崩溃后 hook 会因超时回落原生询问，这是安全的方向。
type Registry struct {
	mu    sync.Mutex
	items map[string]*Approval
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{items: make(map[string]*Approval)}
}

// Add 登记一条审批。同一 approval_id 已存在时返回已有对象与 false。
func (r *Registry) Add(env wire.Envelope, now time.Time, ttl time.Duration) (*Approval, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.items[env.ApprovalID]; ok {
		return a, false
	}
	a := &Approval{
		ID:        env.ApprovalID,
		Envelope:  env,
		CreatedAt: now,
		Deadline:  now.Add(ttl),
		status:    StatusPending,
		done:      make(chan struct{}),
	}
	r.items[env.ApprovalID] = a
	return a, true
}

// Wait 阻塞直到决策到达、超时或调用方取消。
//
// 超时只产生 expired；调用方断开产生 cancelled。两者都意味着「无决策」。
// 终端里已答完（CancelExternal）会关闭 done 让本调用立即返回真实终态。
func (r *Registry) Wait(ctx context.Context, id string) Result {
	r.mu.Lock()
	a, ok := r.items[id]
	if !ok {
		r.mu.Unlock()
		return Result{Status: StatusUnknown}
	}
	if a.status != StatusPending {
		res := Result{Status: a.status, Decision: a.decision, Choice: a.choice, External: a.external}
		r.mu.Unlock()
		return res
	}
	r.mu.Unlock()

	timer := time.NewTimer(time.Until(a.Deadline))
	defer timer.Stop()

	select {
	case <-a.done:
		// done 关闭可能来自手机决策、外部收场或另一路 finish；读锁内真实
		// 状态，不臆测。
		return r.resultOf(a)
	case <-timer.C:
		// 与 CancelExternal/Decide 存在竞态：finish 可能是 no-op（别人先
		// 收场了），必须回读实际状态，否则外部收场后会错发 expired close。
		r.finish(a, StatusExpired, "", "")
		return r.resultOf(a)
	case <-ctx.Done():
		r.finish(a, StatusCancelled, "", "")
		return r.resultOf(a)
	}
}

// resultOf 在锁内构造快照。要求 a 仍在本注册表中（Wait 的调用约定保证）。
func (r *Registry) resultOf(a *Approval) Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Result{Status: a.status, Decision: a.decision, Choice: a.choice, External: a.external}
}

// Decide 提交手机决策；first-wins。choice 是选项型审批的所选标签（可空），
// 与决策绑定后经 Result/DecideResult 带回给渲染层。
func (r *Registry) Decide(id string, d wire.DecisionKind, choice string, now time.Time) DecideResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.items[id]
	if !ok {
		return DecideResult{Status: StatusUnknown}
	}
	if a.status != StatusPending {
		return DecideResult{Status: a.status, Decision: a.decision, Choice: a.choice}
	}
	if now.After(a.Deadline) {
		r.finishLocked(a, StatusExpired, "", "")
		return DecideResult{Status: StatusExpired}
	}
	status := StatusApproved
	if d == wire.DecisionDeny {
		status = StatusDenied
	}
	r.finishLocked(a, status, d, choice)
	return DecideResult{Status: status, Decision: d, Choice: choice, Accepted: true}
}

// Expire 清理所有已超时的 pending 审批，返回本次过期的列表。
func (r *Registry) Expire(now time.Time) []*Approval {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Approval
	for _, a := range r.items {
		if a.status == StatusPending && now.After(a.Deadline) {
			r.finishLocked(a, StatusExpired, "", "")
			out = append(out, a)
		}
	}
	return out
}

// Cancel 取消一条审批（例如 hook 进程退出）。
func (r *Registry) Cancel(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.items[id]; ok && a.status == StatusPending {
		r.finishLocked(a, StatusCancelled, "", "")
	}
}

// CancelExternal 外部收场：终端里已答完（PostToolUse / Stop / SessionEnd
// 对账），审批不再等待手机决策。仅 pending 时生效；close 事件由调用方用
// 返回的存量 envelope 构造（保留 title / tmux / hostname 线索，I8），本方法
// 只负责置位并唤醒阻塞中的 Wait（done 关闭 → Wait 立即返回 External=true，
// hookcmd 输出空 204：原生弹窗已答完，不能渲染任何决策）。
func (r *Registry) CancelExternal(id string) (wire.Envelope, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.items[id]
	if !ok || a.status != StatusPending {
		return wire.Envelope{}, false
	}
	a.external = true
	r.finishLocked(a, StatusCancelled, "", "")
	return a.Envelope, true
}

// PendingInSession 返回某会话所有仍 pending 的审批 id，跳过比 cutoff 更晚
// 创建的条目（Stop 对账传 now-宽限；SessionEnd 对账传 now，即宽限 0——
// 全部命中）。
func (r *Registry) PendingInSession(sessionID string, cutoff time.Time) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, a := range r.items {
		if a.status != StatusPending || a.Envelope.SessionID != sessionID {
			continue
		}
		if a.CreatedAt.After(cutoff) {
			continue
		}
		out = append(out, a.ID)
	}
	return out
}

// PendingMatching 按 (session, tool, input_digest) 精确匹配仍 pending 的审批，
// 供 PostToolUse 在 tool_use_id 关联缺失时兜底（Pre/Post 输入漂移）。找不到
// 或多条命中时返回 false——多条命中说明关联键不可信，宁可留卡等 Stop sweep
// 也不误收并行批次里另一条仍在弹窗的审批。
func (r *Registry) PendingMatching(sessionID, toolName, digest string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var found string
	for _, a := range r.items {
		if a.status != StatusPending || a.Envelope.SessionID != sessionID {
			continue
		}
		if a.Envelope.Payload.ToolName != toolName || a.Envelope.Payload.InputDigest != digest {
			continue
		}
		if found != "" {
			return "", false
		}
		found = a.ID
	}
	return found, found != ""
}

// Has 判断审批是否存在。
func (r *Registry) Has(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.items[id]
	return ok
}

// IsPending 判断审批是否存在且仍待决（CallIndex 清扫的失联判据）。
func (r *Registry) IsPending(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.items[id]
	return ok && a.status == StatusPending
}

// Len 返回当前保留的审批数量（含已结束但尚未清理的）。
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.items)
}

// Pending 返回所有仍处于 pending 的审批快照。
func (r *Registry) Pending() []wire.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]wire.Envelope, 0, len(r.items))
	for _, a := range r.items {
		if a.status == StatusPending {
			out = append(out, a.Envelope)
		}
	}
	return out
}

// Cleanup 删除已结束且超过 keep 的审批，返回删除数量。
func (r *Registry) Cleanup(now time.Time, keep time.Duration) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	removed := 0
	for id, a := range r.items {
		if a.status != StatusPending && now.Sub(a.Deadline) > keep {
			delete(r.items, id)
			removed++
		}
	}
	return removed
}

func (r *Registry) finish(a *Approval, status Status, d wire.DecisionKind, choice string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finishLocked(a, status, d, choice)
}

// finishLocked 要求调用方已持有 r.mu。
func (r *Registry) finishLocked(a *Approval, status Status, d wire.DecisionKind, choice string) {
	if a.status != StatusPending {
		return
	}
	a.status = status
	a.decision = d
	a.choice = choice
	close(a.done)
}
