package event

import (
	"sync"
	"time"
)

// Call 是一次工具调用的归档：PreToolUse 携带 tool_use_id，而 PermissionRequest
// 不带——两者在 daemon 侧靠这张索引对上号，PostToolUse 才能反过来找到该收
// 哪张卡（2026-11-28 真机滞卡案的 hook 侧修复）。
type Call struct {
	SessionID   string
	ToolUseID   string
	ToolName    string
	InputDigest string
	// ApprovalID 非空表示这次调用已产生审批卡（阻塞或只通知），Link 后可查。
	ApprovalID string
	Seen       time.Time
}

// CallIndex 把 PreToolUse / PermissionRequest / PostToolUse 三路事件关联起来。
//
// 三张索引都按 session 分桶（键前缀 session\x00）：
//   - byUse：session+tool_use_id → Call（PreToolUse 全量登记）；
//   - byDigest：session+input_digest → useID 列表（PermissionRequest 缺
//     useID 时按 digest 反查；同 digest 多条时取最新的未关联条目）；
//   - byTool：session+tool_name → useID 列表（digest 落空时的关联兜底）。
//
// 收场动作绝不按 tool 名匹配（并行批次会误收仍在弹窗的另一条）；tool 名反
// 查只用于关联时刻，此刻 Pre 与 Permission 天然相邻，误联的最坏情形是并行
// 同工具双胞胎互串，而两者几乎同时收场，损害有界。
//
// 只存内存、daemon 重启即失：Stop/SessionEnd sweep 走 Registry 不依赖此表，
// 丢失的后果仅是退回「卡等 30 分钟过期」的现状，方向安全。
type CallIndex struct {
	mu       sync.Mutex
	byUse    map[string]*Call
	byDigest map[string][]string
	byTool   map[string][]string
	cap      int
}

// 默认容量上限：稳态只有进行中的调用（PostToolUse 即 Forget），TTL 扫描是
// 兜底；硬顶只防进程异常路径泄漏。
const defaultCallCap = 8192

// NewCallIndex 创建空索引。cap<=0 时用默认 8192。
func NewCallIndex(cap int) *CallIndex {
	if cap <= 0 {
		cap = defaultCallCap
	}
	return &CallIndex{
		byUse:    make(map[string]*Call),
		byDigest: make(map[string][]string),
		byTool:   make(map[string][]string),
		cap:      cap,
	}
}

// Observe 登记一次工具调用（PreToolUse，节流之前全量登记）。同 tool_use_id
// 重复观察视为调用变更（Claude Code 重试场景）：撤旧档再记新档。
func (c *CallIndex) Observe(call Call) {
	c.mu.Lock()
	defer c.mu.Unlock()
	call.Seen = time.Now()
	if old, ok := c.byUse[c.useKey(call.SessionID, call.ToolUseID)]; ok {
		c.removeLocked(old)
	}
	cur := call
	useKey := c.useKey(call.SessionID, call.ToolUseID)
	c.byUse[useKey] = &cur
	// 倒排存 byUse 的完整键（含 session 前缀），反查时无需再拼。
	c.byDigest[c.digestKey(call.SessionID, call.InputDigest)] =
		append(c.byDigest[c.digestKey(call.SessionID, call.InputDigest)], useKey)
	c.byTool[c.toolKey(call.SessionID, call.ToolName)] =
		append(c.byTool[c.toolKey(call.SessionID, call.ToolName)], useKey)
	if len(c.byUse) > c.cap {
		c.evictOldestLocked()
	}
}

// ResolveUse 为不带 tool_use_id 的 PermissionRequest 反查调用：优先 digest
// 精确命中且尚未关联审批的条目；落空时退同 session+tool 名最新的未关联条目。
func (c *CallIndex) ResolveUse(sessionID, toolName, digest string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if use, ok := c.pickUnlinked(c.byDigest[c.digestKey(sessionID, digest)]); ok {
		return use, true
	}
	return c.pickUnlinked(c.byTool[c.toolKey(sessionID, toolName)])
}

// Link 绑定 tool_use_id 与 approval_id；first-wins。
func (c *CallIndex) Link(sessionID, toolUseID, approvalID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	call, ok := c.byUse[c.useKey(sessionID, toolUseID)]
	if !ok || call.ApprovalID != "" {
		return false
	}
	call.ApprovalID = approvalID
	return true
}

// ApprovalFor 查 tool_use_id 关联的审批；未关联返回 false。
func (c *CallIndex) ApprovalFor(sessionID, toolUseID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	call, ok := c.byUse[c.useKey(sessionID, toolUseID)]
	if !ok || call.ApprovalID == "" {
		return "", false
	}
	return call.ApprovalID, true
}

// Forget 删除一次调用的归档（PostToolUse 收场后）。
func (c *CallIndex) Forget(sessionID, toolUseID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if call, ok := c.byUse[c.useKey(sessionID, toolUseID)]; ok {
		c.removeLocked(call)
	}
}

// ForgetSession 清掉整个会话（SessionEnd）。
func (c *CallIndex) ForgetSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := sessionID + "\x00"
	var doomed []*Call
	for k, call := range c.byUse {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			doomed = append(doomed, call)
		}
	}
	for _, call := range doomed {
		c.removeLocked(call)
	}
}

// Sweep 删除过期条目与已不在 pending 的关联（gateway janitor 与 Registry
// Cleanup 同车次调用）。isPending 返回该 approval 是否仍待决；TTL 只兜漏网
// 之鱼（正常生命周期由 PostToolUse Forget / SessionEnd ForgetSession 收口）。
func (c *CallIndex) Sweep(now time.Time, ttl time.Duration, isPending func(id string) bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := 0
	var doomed []*Call
	for _, call := range c.byUse {
		expired := now.Sub(call.Seen) > ttl
		stale := call.ApprovalID != "" && !isPending(call.ApprovalID)
		if expired || stale {
			doomed = append(doomed, call)
		}
	}
	for _, call := range doomed {
		c.removeLocked(call)
		removed++
	}
	return removed
}

// pickUnlinked 从倒排列表里挑最新的未关联条目（列表按时间升序追加，元素是
// byUse 完整键），返回裸 tool_use_id。
func (c *CallIndex) pickUnlinked(useKeys []string) (string, bool) {
	for i := len(useKeys) - 1; i >= 0; i-- {
		if call, ok := c.byUse[useKeys[i]]; ok && call.ApprovalID == "" {
			return call.ToolUseID, true
		}
	}
	return "", false
}

// removeLocked 摘除一条归档及其全部倒排登记。要求持有 c.mu。
func (c *CallIndex) removeLocked(call *Call) {
	useKey := c.useKey(call.SessionID, call.ToolUseID)
	if cur, ok := c.byUse[useKey]; !ok || cur != call {
		return
	}
	delete(c.byUse, useKey)
	dk := c.digestKey(call.SessionID, call.InputDigest)
	c.byDigest[dk] = removeID(c.byDigest[dk], useKey)
	if len(c.byDigest[dk]) == 0 {
		delete(c.byDigest, dk)
	}
	tk := c.toolKey(call.SessionID, call.ToolName)
	c.byTool[tk] = removeID(c.byTool[tk], useKey)
	if len(c.byTool[tk]) == 0 {
		delete(c.byTool, tk)
	}
}

func (c *CallIndex) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, call := range c.byUse {
		if oldestKey == "" || call.Seen.Before(oldest) {
			oldestKey, oldest = k, call.Seen
		}
	}
	if oldestKey != "" {
		c.removeLocked(c.byUse[oldestKey])
	}
}

func removeID(list []string, useID string) []string {
	out := list[:0]
	for _, id := range list {
		if id != useID {
			out = append(out, id)
		}
	}
	return out
}

func (c *CallIndex) useKey(sessionID, useID string) string {
	return sessionID + "\x00" + useID
}

func (c *CallIndex) digestKey(sessionID, digest string) string {
	return sessionID + "\x00" + digest
}

func (c *CallIndex) toolKey(sessionID, tool string) string {
	return sessionID + "\x00" + tool
}
