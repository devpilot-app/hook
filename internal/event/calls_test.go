package event

import (
	"testing"
	"time"
)

// TestCallIndexObserveAndResolveByDigest 钉 PermissionRequest 反查主路径：
// PreToolUse 先登记，PermissionRequest 缺 tool_use_id 时按 digest 命中。
func TestCallIndexObserveAndResolveByDigest(t *testing.T) {
	c := NewCallIndex(0)
	c.Observe(Call{SessionID: "s1", ToolUseID: "tu_1", ToolName: "Read", InputDigest: "d1"})

	use, ok := c.ResolveUse("s1", "Read", "d1")
	if !ok || use != "tu_1" {
		t.Fatalf("ResolveUse by digest = %q %v", use, ok)
	}
	// digest 不匹配时允许退同会话同名最新未关联条目（仅关联时刻的兜底，
	// Pre 与 Permission 天然相邻；收场路径严禁此路）。
	if use, ok := c.ResolveUse("s1", "Read", "d_other"); !ok || use != "tu_1" {
		t.Fatalf("同名兜底 = %q %v", use, ok)
	}
	if _, ok := c.ResolveUse("s2", "Read", "d1"); ok {
		t.Fatal("跨 session 不得命中")
	}
}

// TestCallIndexPrefersUnlinkedOnAmbiguousDigest digest 反查命中多条已关联
// 条目时，必须跳到最新的未关联条目——已关联的归上一张卡。
func TestCallIndexPrefersUnlinkedOnAmbiguousDigest(t *testing.T) {
	c := NewCallIndex(0)
	c.Observe(Call{SessionID: "s1", ToolUseID: "tu_1", ToolName: "Read", InputDigest: "d1"})
	c.Observe(Call{SessionID: "s1", ToolUseID: "tu_2", ToolName: "Read", InputDigest: "d1"})
	if !c.Link("s1", "tu_1", "apv_1") {
		t.Fatal("link tu_1")
	}
	use, ok := c.ResolveUse("s1", "Read", "d1")
	if !ok || use != "tu_2" {
		t.Fatalf("应跳过已关联的 tu_1 命中 tu_2,得 %q %v", use, ok)
	}
}

// TestCallIndexLinkFirstWins 与 ApprovalFor 查询。
func TestCallIndexLinkFirstWins(t *testing.T) {
	c := NewCallIndex(0)
	c.Observe(Call{SessionID: "s1", ToolUseID: "tu_1", ToolName: "Bash", InputDigest: "d1"})
	if !c.Link("s1", "tu_1", "apv_1") {
		t.Fatal("首链应成功")
	}
	if c.Link("s1", "tu_1", "apv_2") {
		t.Fatal("重复链必须 first-wins 失败")
	}
	if id, ok := c.ApprovalFor("s1", "tu_1"); !ok || id != "apv_1" {
		t.Fatalf("ApprovalFor = %q %v", id, ok)
	}
	if _, ok := c.ApprovalFor("s1", "tu_missing"); ok {
		t.Fatal("未关联 use_id 不得命中")
	}
}

// TestCallIndexForgetAndSweep PostToolUse 收场后 Forget；Sweep 清 TTL 过期与
// 已不在 pending 的关联。
func TestCallIndexForgetAndSweep(t *testing.T) {
	c := NewCallIndex(0)
	c.Observe(Call{SessionID: "s1", ToolUseID: "tu_1", ToolName: "Bash", InputDigest: "d1"})
	c.Observe(Call{SessionID: "s1", ToolUseID: "tu_2", ToolName: "Read", InputDigest: "d2"})
	if !c.Link("s1", "tu_1", "apv_1") {
		t.Fatal("link")
	}
	// Observe 会盖 Seen（生产语义），TTL 测试用真实短窗：两条都已过 30ms
	// TTL。TTL 是权威的泄漏护栏，pending 与否都不豁免（生产 TTL=审批窗
	// +10min，远大于审批等待，不会误伤在途审批）。
	time.Sleep(60 * time.Millisecond)
	removed := c.Sweep(time.Now(), 30*time.Millisecond, func(id string) bool { return id == "apv_1" })
	if removed != 2 {
		t.Fatalf("Sweep removed = %d, want 2", removed)
	}
	if _, ok := c.ResolveUse("s1", "Read", "d2"); ok {
		t.Fatal("TTL 过期条目应被清除")
	}

	// 重新登记并关联：审批不再 pending（已被收场）→ stale 清除。
	c.Observe(Call{SessionID: "s1", ToolUseID: "tu_1", ToolName: "Bash", InputDigest: "d1"})
	if !c.Link("s1", "tu_1", "apv_1") {
		t.Fatal("link")
	}
	removed = c.Sweep(time.Now(), time.Hour, func(string) bool { return false })
	if removed != 1 {
		t.Fatalf("stale sweep removed = %d, want 1", removed)
	}
	if _, ok := c.ApprovalFor("s1", "tu_1"); ok {
		t.Fatal("已收场审批的关联应被清除")
	}

	// Forget 直接摘档。
	c.Observe(Call{SessionID: "s1", ToolUseID: "tu_3", ToolName: "Edit", InputDigest: "d3"})
	c.Forget("s1", "tu_3")
	if _, ok := c.ResolveUse("s1", "Edit", "d3"); ok {
		t.Fatal("Forget 后不得再命中")
	}
}

// TestCallIndexForgetSession 与容量硬顶逐出。
func TestCallIndexForgetSessionAndCap(t *testing.T) {
	c := NewCallIndex(2)
	c.Observe(Call{SessionID: "s1", ToolUseID: "tu_1", ToolName: "Bash", InputDigest: "d1"})
	c.Observe(Call{SessionID: "s2", ToolUseID: "tu_2", ToolName: "Bash", InputDigest: "d2"})
	c.ForgetSession("s1")
	if _, ok := c.ResolveUse("s1", "Bash", "d1"); ok {
		t.Fatal("ForgetSession 后不得命中")
	}
	if _, ok := c.ResolveUse("s2", "Bash", "d2"); !ok {
		t.Fatal("其他会话不得被误清")
	}

	// 硬顶：cap=2 的空索引里挤进第 3 条，最旧的 tu_2 被逐出。
	c2 := NewCallIndex(2)
	c2.Observe(Call{SessionID: "s1", ToolUseID: "tu_1", ToolName: "Bash", InputDigest: "d1"})
	c2.Observe(Call{SessionID: "s2", ToolUseID: "tu_2", ToolName: "Bash", InputDigest: "d2"})
	c2.Observe(Call{SessionID: "s3", ToolUseID: "tu_3", ToolName: "Bash", InputDigest: "d3"})
	if _, ok := c2.ResolveUse("s1", "Bash", "d1"); ok {
		t.Fatal("超硬顶应逐出最旧条目(tu_1)")
	}
	if _, ok := c2.ResolveUse("s3", "Bash", "d3"); !ok {
		t.Fatal("最新条目必须保留")
	}
}
