package agent

import (
	"strings"
	"testing"
)

// TestDenyReason 共享 deny 文案的语义：空 choice 回落兜底，非空把答案
// 原样喂回（claude/codex/opencode 三个适配器共用，见 deny.go）。
func TestDenyReason(t *testing.T) {
	if got := DenyReason(""); got != DenyFallbackMessage {
		t.Fatalf("空 choice 应回落兜底文案，实际 %q", got)
	}
	got := DenyReason("Option A")
	if !strings.Contains(got, `"Option A"`) {
		t.Fatalf("choice 应原样喂回，实际 %q", got)
	}
	if strings.Contains(got, DenyFallbackMessage) {
		t.Fatal("作答文案不应混入兜底文案")
	}
}
