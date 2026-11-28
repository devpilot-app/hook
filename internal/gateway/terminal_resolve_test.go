package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/devpilot-app/apns/pkg/wire"
)

// hookBody 构造最小 hook stdin JSON。
func hookBody(event, session, tool, useID, input string) []byte {
	m := map[string]any{
		"hook_event_name": event,
		"session_id":      session,
		"tool_name":       tool,
		"tool_input":      json.RawMessage(input),
	}
	if useID != "" {
		m["tool_use_id"] = useID
	}
	data, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return data
}

// waitForType 等 pub.ch 里出现指定类型的事件（跳过其他类型，顺序不保证）。
func waitForType(t *testing.T, ch <-chan wire.Envelope, typ wire.EventType) wire.Envelope {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case env := <-ch:
			if env.Type == typ {
				return env
			}
		case <-deadline:
			t.Fatalf("等待 %s 超时", typ)
			return wire.Envelope{}
		}
	}
}

// blockedHookResp 是阻塞 hook 响应的最小快照。
type blockedHookResp struct {
	status int
	body   string
}

// fireBlockedHook 在后台发起一个会阻塞的 PermissionRequest hook（真机形态：
// 无 tool_use_id），返回响应快照通道。直接走 HTTP（不经 e.do）——goroutine
// 里不得使用 *testing.T。
func fireBlockedHook(e *testEnv, session, tool, input string) chan blockedHookResp {
	ch := make(chan blockedHookResp, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, e.http.URL+"/v1/hook/claude",
			bytes.NewReader(hookBody("PermissionRequest", session, tool, "", input)))
		if err != nil {
			ch <- blockedHookResp{status: -1, body: err.Error()}
			return
		}
		req.Host = "127.0.0.1:24543"
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := e.http.Client().Do(req)
		if err != nil {
			ch <- blockedHookResp{status: -1, body: err.Error()}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		ch <- blockedHookResp{status: resp.StatusCode, body: string(b)}
	}()
	return ch
}

// TestPermissionRequestResolvedByPostToolUse 钉 2026-11-28 滞卡案主修复：
// 终端里批了 Bash → 工具跑完 → PostToolUse 按 tool_use_id 关联收卡，阻塞中
// 的 PermissionRequest hook 立即拿到空 204，relay 恰好收到一条
// close(cancelled)。
func TestPermissionRequestResolvedByPostToolUse(t *testing.T) {

	e := newTestEnv(t, 30*time.Second, 0)
	const sess = "sess_term"

	if resp, body := e.do(t, reqOpts{agent: "claude", token: testToken,
		body: hookBody("PreToolUse", sess, "Bash", "toolu_X", `{"command":"git push"}`)}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("pre HTTP %d: %s", resp.StatusCode, body)
	}

	blocked := fireBlockedHook(e, sess, "Bash", `{"command":"git push"}`)
	required := waitForType(t, e.pub.ch, wire.EventApprovalRequired)

	// 终端已批：工具真的跑完 → PostToolUse。
	if resp, body := e.do(t, reqOpts{agent: "claude", token: testToken,
		body: hookBody("PostToolUse", sess, "Bash", "toolu_X", `{"command":"git push"}`)}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("post HTTP %d: %s", resp.StatusCode, body)
	}

	// 阻塞中的 hook 必须 promptly 返回（不等 30s 超时）。
	select {
	case r := <-blocked:
		if r.status != http.StatusNoContent {
			t.Fatalf("被外部收场的 hook 应空 204,得 %d %s", r.status, r.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PostToolUse 后阻塞 hook 未被解锁")
	}

	closes := e.pub.eventsOfType(wire.EventApprovalClosed)
	if len(closes) != 1 {
		t.Fatalf("应恰好一条 close,得 %d 条", len(closes))
	}
	if closes[0].EventID != "close-"+required.ApprovalID ||
		closes[0].ApprovalID != required.ApprovalID ||
		closes[0].Payload.StatusText != wire.CloseReasonCancelled {
		t.Fatalf("close = %+v (required=%s)", closes[0], required.ApprovalID)
	}
}

// TestAskUserQuestionClosedByPostToolUseWithDifferentInput 钉 R2 修复：Pre 与
// Post 的 tool_input 不同源（Claude Code 注入 answers）时，收卡必须命中提问
// 卡的真实 id——历史上这里重派生必得幽灵 id（真机 WAL 三例全错位）。
func TestAskUserQuestionClosedByPostToolUseWithDifferentInput(t *testing.T) {

	e := newTestEnv(t, 30*time.Second, 0)
	const sess = "sess_q"
	const qInput = `{"questions":[{"question":"Which DB?","header":"DB","options":[{"label":"Postgres"},{"label":"SQLite"}]}]}`

	if resp, body := e.do(t, reqOpts{agent: "claude", token: testToken,
		body: hookBody("PreToolUse", sess, "AskUserQuestion", "toolu_Q", qInput)}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("pre HTTP %d: %s", resp.StatusCode, body)
	}
	required := waitForType(t, e.pub.ch, wire.EventApprovalRequired)

	// 终端作答后的 PostToolUse：tool_input 带上了 answers（不同源）。
	answered := `{"questions":[{"question":"Which DB?","header":"DB","options":[{"label":"Postgres"},{"label":"SQLite"}]}],"answers":{"Which DB?":"Postgres"}}`
	if resp, body := e.do(t, reqOpts{agent: "claude", token: testToken,
		body: hookBody("PostToolUse", sess, "AskUserQuestion", "toolu_Q", answered)}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("post HTTP %d: %s", resp.StatusCode, body)
	}

	closes := e.pub.eventsOfType(wire.EventApprovalClosed)
	if len(closes) != 1 || closes[0].ApprovalID != required.ApprovalID ||
		closes[0].Payload.StatusText != wire.CloseReasonCancelled {
		t.Fatalf("close = %+v (required=%s)", closes, required.ApprovalID)
	}
}

// TestPostToolUseUnrelatedToolDoesNotClosePendingApproval 并行批次保护：
// 工具 B 跑完（自动放行）不得收掉仍在弹窗的工具 A 的卡；A 自己的 PostToolUse
// 才收 A。
func TestPostToolUseUnrelatedToolDoesNotClosePendingApproval(t *testing.T) {

	e := newTestEnv(t, 30*time.Second, 0)
	const sess = "sess_par"

	// A：Bash 待权限（阻塞）。
	_, _ = e.do(t, reqOpts{agent: "claude", token: testToken,
		body: hookBody("PreToolUse", sess, "Bash", "toolu_A", `{"command":"rm -rf x"}`)})
	blocked := fireBlockedHook(e, sess, "Bash", `{"command":"rm -rf x"}`)
	waitForType(t, e.pub.ch, wire.EventApprovalRequired)

	// B：Read 自动放行，先跑完。
	if resp, _ := e.do(t, reqOpts{agent: "claude", token: testToken,
		body: hookBody("PreToolUse", sess, "Read", "toolu_B", `{"file_path":"/tmp/b.txt"}`)}); resp.StatusCode != http.StatusNoContent {
		t.Fatal("pre B")
	}
	if resp, _ := e.do(t, reqOpts{agent: "claude", token: testToken,
		body: hookBody("PostToolUse", sess, "Read", "toolu_B", `{"file_path":"/tmp/b.txt"}`)}); resp.StatusCode != http.StatusNoContent {
		t.Fatal("post B")
	}
	if closes := e.pub.eventsOfType(wire.EventApprovalClosed); len(closes) != 0 {
		t.Fatalf("无关工具的 PostToolUse 不得收卡,得 %+v", closes)
	}

	// A 自己的 PostToolUse：只收 A。
	if resp, _ := e.do(t, reqOpts{agent: "claude", token: testToken,
		body: hookBody("PostToolUse", sess, "Bash", "toolu_A", `{"command":"rm -rf x"}`)}); resp.StatusCode != http.StatusNoContent {
		t.Fatal("post A")
	}
	closes := e.pub.eventsOfType(wire.EventApprovalClosed)
	if len(closes) != 1 || closes[0].Payload.ToolName != "Bash" {
		t.Fatalf("应恰好收掉 A,得 %+v", closes)
	}
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("A 的 hook 未解锁")
	}
}

// TestStopSweepClosesStalePending 钉终端 deny 路径：deny 无事件可依赖，
// Stop 时到龄仍 pending 的审批按 cancelled 收场。
func TestStopSweepClosesStalePending(t *testing.T) {

	e := newTestEnv(t, 30*time.Second, 0)
	const sess = "sess_deny"

	_, _ = e.do(t, reqOpts{agent: "claude", token: testToken,
		body: hookBody("PreToolUse", sess, "Bash", "toolu_A", `{"command":"nope"}`)})
	blocked := fireBlockedHook(e, sess, "Bash", `{"command":"nope"}`)
	waitForType(t, e.pub.ch, wire.EventApprovalRequired)

	if resp, _ := e.do(t, reqOpts{agent: "claude", token: testToken,
		body: hookBody("Stop", sess, "", "", `{}`)}); resp.StatusCode != http.StatusNoContent {
		t.Fatal("stop")
	}
	if closes := e.pub.eventsOfType(wire.EventApprovalClosed); len(closes) != 0 {
		t.Fatalf("宽限内的 Stop 不得收年轻条目,得 %+v", closes)
	}

	// 超过 3s 宽限后的下一次 Stop：收卡。
	time.Sleep(3100 * time.Millisecond)
	if resp, _ := e.do(t, reqOpts{agent: "claude", token: testToken,
		body: hookBody("Stop", sess, "", "", `{}`)}); resp.StatusCode != http.StatusNoContent {
		t.Fatal("stop 2")
	}
	closes := e.pub.eventsOfType(wire.EventApprovalClosed)
	if len(closes) != 1 || closes[0].Payload.StatusText != wire.CloseReasonCancelled {
		t.Fatalf("过宽限的 Stop 应回收 pending,得 %+v", closes)
	}
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("hook 未解锁")
	}
}

// TestSessionEndSweepsApprovalsBeforeSessionEnded 钉顺序：逐卡 close 先于
// session_ended 上行（relay 端 token 随 close 剪除，会话收尾循环不再重复 end）。
func TestSessionEndSweepsApprovalsBeforeSessionEnded(t *testing.T) {

	e := newTestEnv(t, 30*time.Second, 0)
	const sess = "sess_end"

	_, _ = e.do(t, reqOpts{agent: "claude", token: testToken,
		body: hookBody("PreToolUse", sess, "Bash", "toolu_A", `{"command":"x"}`)})
	blocked := fireBlockedHook(e, sess, "Bash", `{"command":"x"}`)
	required := waitForType(t, e.pub.ch, wire.EventApprovalRequired)

	if resp, _ := e.do(t, reqOpts{agent: "claude", token: testToken,
		body: hookBody("SessionEnd", sess, "", "", `{}`)}); resp.StatusCode != http.StatusNoContent {
		t.Fatal("session end")
	}

	// fakePublisher 按序追加：close 的下标必须小于 session_ended 的下标。
	e.pub.mu.Lock()
	closeIdx, endedIdx := -1, -1
	for i, env := range e.pub.events {
		if env.Type == wire.EventApprovalClosed && closeIdx == -1 {
			closeIdx = i
		}
		if env.Type == wire.EventSessionEnded && endedIdx == -1 {
			endedIdx = i
		}
	}
	e.pub.mu.Unlock()
	if closeIdx == -1 || endedIdx == -1 || closeIdx > endedIdx {
		t.Fatalf("close 应先于 session_ended: close=%d ended=%d (required=%s)",
			closeIdx, endedIdx, required.ApprovalID)
	}
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("hook 未解锁")
	}
}

// TestDecidedApprovalNotClosedByPostToolUse 手机先决策后，迟到的 PostToolUse
// 不得再补 cancelled close（close 归属决策路径，relay 合成）。
func TestDecidedApprovalNotClosedByPostToolUse(t *testing.T) {

	e := newTestEnv(t, 30*time.Second, 0)
	const sess = "sess_decided"

	_, _ = e.do(t, reqOpts{agent: "claude", token: testToken,
		body: hookBody("PreToolUse", sess, "Bash", "toolu_A", `{"command":"y"}`)})
	done := fireBlockedHook(e, sess, "Bash", `{"command":"y"}`)
	required := waitForType(t, e.pub.ch, wire.EventApprovalRequired)

	// 手机批准（gateway 决策入口）。决策体是完整 wire.Decision（V/Kind 必带）。
	decBody, _ := json.Marshal(wire.Decision{
		V: wire.Version, Kind: wire.KindDecision, MsgID: "m_dec",
		ApprovalID: required.ApprovalID, Decision: wire.DecisionAllow, TS: time.Now().Unix(),
	})
	if resp, body := e.do(t, reqOpts{token: testToken, path: "/v1/decisions", body: decBody}); resp.StatusCode != http.StatusOK {
		t.Fatalf("decision HTTP %d: %s", resp.StatusCode, body)
	}

	// 迟到的 PostToolUse：审批已 approved，不得再发 cancelled。
	if resp, _ := e.do(t, reqOpts{agent: "claude", token: testToken,
		body: hookBody("PostToolUse", sess, "Bash", "toolu_A", `{"command":"y"}`)}); resp.StatusCode != http.StatusNoContent {
		t.Fatal("post")
	}
	if closes := e.pub.eventsOfType(wire.EventApprovalClosed); len(closes) != 0 {
		t.Fatalf("已决审批不得被 PostToolUse 收场,得 %+v", closes)
	}
	select {
	case r := <-done:
		if r.status != http.StatusOK {
			t.Fatalf("手机决策应 200 带决策 JSON,得 %d", r.status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hook 未解锁")
	}
}
