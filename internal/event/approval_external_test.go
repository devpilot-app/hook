package event

import (
	"context"
	"testing"
	"time"

	"github.com/devpilot-app/apns/pkg/wire"
)

func extEnv(id string) wire.Envelope {
	return wire.Envelope{
		V: wire.Version, Kind: wire.KindEvent, MsgID: "m_" + id, EventID: "ev_" + id,
		ApprovalID: id, Type: wire.EventApprovalRequired, Agent: wire.AgentClaude,
		DaemonID: "dd_1", SessionID: "sess_1", TS: 1,
		Payload: wire.Payload{ToolName: "Bash", CommandSummary: "ls"},
	}
}

// TestCancelExternalUnblocksWaitWithExternalFlag 钉终端已答的 Wait 解锁：
// CancelExternal 关闭 done，阻塞中的 Wait 立即返回 StatusCancelled +
// External=true——hookcmd 据此空 204（原生弹窗已答完，不能渲染决策）。
func TestCancelExternalUnblocksWaitWithExternalFlag(t *testing.T) {
	r := NewRegistry()
	env := extEnv("apv_1")
	r.Add(env, time.Now(), time.Minute)

	done := make(chan Result, 1)
	go func() { done <- r.Wait(context.Background(), "apv_1") }()

	// 等 Wait 真正进入阻塞再收场。
	deadline := time.Now().Add(2 * time.Second)
	for len(done) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	closeEnv, ok := r.CancelExternal("apv_1")
	if !ok {
		t.Fatal("CancelExternal 应成功")
	}
	if closeEnv.ApprovalID != "apv_1" || closeEnvelopesMatch(closeEnv, env) == false {
		t.Fatalf("应返回存量 envelope 快照,得 %+v", closeEnv)
	}

	res := <-done
	if res.Status != StatusCancelled || !res.External {
		t.Fatalf("Wait = %+v, want cancelled+external", res)
	}
}

func closeEnvelopesMatch(a, b wire.Envelope) bool {
	return a.SessionID == b.SessionID && a.Payload.ToolName == b.Payload.ToolName
}

// TestCancelExternalNoopAfterDecision 手机已决策后外部收场必须 no-op：
// close 的归属在决策路径（relay 合成），不能出现 cancelled 覆盖 approved。
func TestCancelExternalNoopAfterDecision(t *testing.T) {
	r := NewRegistry()
	r.Add(extEnv("apv_2"), time.Now(), time.Minute)
	if dr := r.Decide("apv_2", wire.DecisionAllow, "", time.Now()); !dr.Accepted {
		t.Fatalf("decide = %+v", dr)
	}
	if _, ok := r.CancelExternal("apv_2"); ok {
		t.Fatal("已决审批不得被外部收场改写")
	}
}

// TestWaitReturnsActualStatusWhenExternalRace 钉 Wait 竞态修复：外部收场与
// 超时几乎同时发生时，Wait 必须回读锁内真实状态（External+cancelled），
// 不得硬编码返回 expired——否则 gateway 会错发第二条 close。
func TestWaitReturnsActualStatusWhenExternalRace(t *testing.T) {
	r := NewRegistry()
	r.Add(extEnv("apv_3"), time.Now(), 20*time.Millisecond)

	done := make(chan Result, 1)
	go func() { done <- r.Wait(context.Background(), "apv_3") }()

	// 卡在 timer 到期前后窗口里抢 CancelExternal；两种输赢都必须返回
	// 真实终态且不产生矛盾的第二次 close。
	time.Sleep(10 * time.Millisecond)
	if _, ok := r.CancelExternal("apv_3"); !ok {
		t.Fatal("pending 中应可外部收场")
	}
	res := <-done
	if res.Status != StatusCancelled || !res.External {
		t.Fatalf("竞态后 Wait = %+v, want cancelled+external", res)
	}
}

// TestPendingInSessionGrace 钉 Stop sweep 的取数语义：只取本会话、只取到龄。
func TestPendingInSessionGrace(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(extEnv("apv_old"), now.Add(-time.Minute), time.Hour)
	e2 := extEnv("apv_young")
	e2.SessionID = "sess_1"
	r.Add(e2, now.Add(-time.Second), time.Hour)
	e3 := extEnv("apv_other_sess")
	e3.SessionID = "sess_2"
	r.Add(e3, now.Add(-time.Minute), time.Hour)

	ids := r.PendingInSession("sess_1", now.Add(-30*time.Second))
	if len(ids) != 1 || ids[0] != "apv_old" {
		t.Fatalf("PendingInSession = %v, want [apv_old]", ids)
	}
	// 宽限 0（SessionEnd 传 now）：同会话全部命中。
	ids = r.PendingInSession("sess_1", now)
	if len(ids) != 2 {
		t.Fatalf("grace 0 应命中全部,得 %v", ids)
	}
}

// TestPendingMatchingByDigest 钉 PostToolUse 的 digest 兜底：唯一命中返回
// id；多条命中宁可放弃（关联键不可信，留卡等 Stop sweep）。
func TestPendingMatchingByDigest(t *testing.T) {
	r := NewRegistry()
	e1 := extEnv("apv_a")
	e1.Payload.InputDigest = "d1"
	r.Add(e1, time.Now(), time.Hour)

	if id, ok := r.PendingMatching("sess_1", "Bash", "d1"); !ok || id != "apv_a" {
		t.Fatalf("PendingMatching = %q %v", id, ok)
	}
	if _, ok := r.PendingMatching("sess_1", "Bash", "d_missing"); ok {
		t.Fatal("digest 不匹配不得命中")
	}
	// 两条同 digest：放弃匹配。
	e2 := extEnv("apv_b")
	e2.Payload.InputDigest = "d1"
	r.Add(e2, time.Now(), time.Hour)
	if _, ok := r.PendingMatching("sess_1", "Bash", "d1"); ok {
		t.Fatal("多条命中必须放弃")
	}
}
