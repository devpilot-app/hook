package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devpilot-app/apns/pkg/wire"
	"github.com/devpilot-app/hook/internal/agent"
	"github.com/devpilot-app/hook/internal/agent/gemini"
	"github.com/devpilot-app/hook/internal/agent/grok"
	"github.com/devpilot-app/hook/internal/config"
)

// fixtureAs 用 ParseInputAs 读取非 claude 的 fixture(事件名归一化)。
func fixtureAs(t *testing.T, agentName, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hooks", agentName, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// postHook 以指定 agent 通道 POST 原始 stdin。
func postHook(t *testing.T, ts *httptest.Server, agentName string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/hook/"+agentName, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestGateExpiryRendersDeny 闸门审批(Gemini/Grok yolo)等待窗耗尽必须
// fail-closed:200 + deny JSON,工具被拦;同时发 close(expired) 让手机侧
// 收场。普通审批保持 204 无输出(回落原生弹窗)。
func TestGateExpiryRendersDeny(t *testing.T) {
	env := newTestEnvOpts(t, 30*time.Millisecond, time.Second, func(p *Params) {
		p.Adapters[wire.AgentGemini] = gemini.Adapter{}
		p.ApprovalMode = config.ApprovalModePreToolUse
	})
	in, err := agent.ParseInputAs("gemini", fixtureAs(t, "gemini", "before_tool.json"))
	if err != nil {
		t.Fatal(err)
	}
	resp := postHook(t, env.http, "gemini", fixtureAs(t, "gemini", "before_tool.json"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	if !strings.Contains(buf.String(), `"decision": "deny"`) && !strings.Contains(strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' {
			return -1
		}
		return r
	}, buf.String()), `"decision":"deny"`) {
		t.Fatalf("闸门过期应回 deny JSON,实际 %q", buf.String())
	}
	// close(expired) 已入队
	deadline := time.Now().Add(2 * time.Second)
	for {
		if env.pub.count() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("闸门过期应补发 approval_closed(expired)")
		}
		time.Sleep(10 * time.Millisecond)
	}
	closes := env.pub.eventsOfType(wire.EventApprovalClosed)
	if len(closes) == 0 || closes[0].Payload.StatusText != wire.CloseReasonExpired {
		t.Fatalf("close 事件不符: %+v", closes)
	}
	_ = in
}

// TestNotifyOnlyCardLifecycle 等待卡(Gemini Notification)的会话级收卡:
// 同会话下一个工具事件补发 close(cancelled);普通遥测不触发登记。
func TestNotifyOnlyCardLifecycle(t *testing.T) {
	env := newTestEnvOpts(t, time.Second, time.Second, func(p *Params) {
		p.Adapters[wire.AgentGemini] = gemini.Adapter{}
		p.Adapters[wire.AgentGrok] = grok.Adapter{}
	})

	// 1. 权限弹窗通知 → notify-only 等待卡(不入等待登记)
	resp := postHook(t, env.http, "gemini", fixtureAs(t, "gemini", "notification_tool_permission.json"))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("notify-only 卡应 204,实际 %d", resp.StatusCode)
	}
	required := env.pub.eventsOfType(wire.EventApprovalRequired)
	if len(required) == 0 || !required[0].Payload.NotifyOnly {
		t.Fatalf("应发布 notify-only 审批事件: %+v", required)
	}
	card := &required[0]

	// 2. BeforeTool 遥测(默认模式)= 终端已处置 → close(cancelled)
	resp = postHook(t, env.http, "gemini", fixtureAs(t, "gemini", "before_tool.json"))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("遥测应 204,实际 %d", resp.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	var closed *wire.Envelope
	for time.Now().Before(deadline) {
		for _, e := range env.pub.eventsOfType(wire.EventApprovalClosed) {
			clone := e
			closed = &clone
			break
		}
		if closed != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if closed == nil {
		t.Fatal("等待卡应被同会话后续工具事件收掉")
	}
	if closed.EventID != "close-"+card.ApprovalID || closed.Payload.StatusText != wire.CloseReasonCancelled {
		t.Fatalf("close 事件不符: %+v", closed)
	}

	// 3. grok 的权限通知也登记;非权限类型不登记
	resp = postHook(t, env.http, "grok", fixtureAs(t, "grok", "notification.json"))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("grok 通知应 204,实际 %d", resp.StatusCode)
	}
	grokRequired := env.pub.eventsOfType(wire.EventApprovalRequired)
	var grokCard *wire.Envelope
	for i := range grokRequired {
		if grokRequired[i].Agent == wire.AgentGrok {
			grokCard = &grokRequired[i]
		}
	}
	if grokCard == nil || !grokCard.Payload.NotifyOnly {
		t.Fatalf("grok 等待卡缺失: %+v", grokRequired)
	}
}

// TestBatch2AgentsRouted 新 agent 的通道在网关可用且事件合法入队。
func TestBatch2AgentsRouted(t *testing.T) {
	env := newTestEnvOpts(t, time.Second, time.Second, func(p *Params) {
		p.Adapters[wire.AgentGrok] = grok.Adapter{}
		p.ApprovalMode = config.ApprovalModePreToolUse
	})
	resp := postHook(t, env.http, "grok", fixtureAs(t, "grok", "stop.json"))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("grok stop 应 204,实际 %d", resp.StatusCode)
	}
	completes := env.pub.eventsOfType(wire.EventTaskComplete)
	if len(completes) == 0 || completes[0].Agent != wire.AgentGrok {
		t.Fatalf("grok 完成卡缺失: %+v", completes)
	}
	if completes[0].Payload.ReplySummary == "" {
		t.Fatalf("grok 完成卡应带回复摘要: %+v", completes[0].Payload)
	}
}
