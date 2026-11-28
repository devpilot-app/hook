package agent

import (
	"encoding/json"
	"testing"

	"github.com/devpilot-app/apns/pkg/wire"
)

// TestToolSummaryVerbs 钉问题 2 修复：文件类工具的审批卡正文必须是带动词的
// 人话短语（裸路径看不出要批什么），其余工具逐字保留原摘要；InputDigest 始终
// 对原始输入计算，键控与关联不受动词改写影响。
func TestToolSummaryVerbs(t *testing.T) {
	cases := []struct {
		name  string
		agent wire.Agent
		tool  string
		input string
		want  string
	}{
		{"read", wire.AgentClaude, "Read", `{"file_path":"/Users/x/notes/a b.jpg"}`, "Claude wants to read: a b.jpg"},
		{"edit", wire.AgentClaude, "Edit", `{"file_path":"/tmp/x.swift"}`, "Claude wants to edit: x.swift"},
		{"multi-edit", wire.AgentClaude, "MultiEdit", `{"file_path":"/tmp/x.swift"}`, "Claude wants to edit: x.swift"},
		{"write", wire.AgentCodex, "Write", `{"file_path":"/tmp/y.md"}`, "Codex wants to write: y.md"},
		{"notebook", wire.AgentClaude, "NotebookEdit", `{"notebook_path":"/tmp/z.ipynb"}`, "Claude wants to edit notebook: z.ipynb"},
		{"bash-unchanged", wire.AgentClaude, "Bash", `{"command":"git push --force"}`, "git push --force"},
		{"question-unchanged", wire.AgentClaude, "AskUserQuestion", `{"questions":[]}`, ""},
		{"missing-path", wire.AgentClaude, "Read", `{"offset":3}`, ""},
		{"empty-path", wire.AgentClaude, "Read", `{"file_path":"  "}`, ""},
		{"root-path", wire.AgentClaude, "Read", `{"file_path":"/"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := Input{SessionID: "s1", HookEventName: "PermissionRequest", ToolName: tc.tool, ToolInput: json.RawMessage(tc.input)}
			env := NewApprovalEnvelope(tc.agent, "dd_1", in, 1)
			want := tc.want
			if want == "" {
				// 回落场景：期望值 = 原摘要产出（SummarizeToolInput），不硬编码
				// 其折叠细节。
				want = wire.SummarizeToolInput(in.ToolInput)
			}
			if env.Payload.CommandSummary != want {
				t.Fatalf("CommandSummary = %q, want %q", env.Payload.CommandSummary, want)
			}
			// digest 对原始输入：同输入两次派生一致，且不受动词改写影响。
			if env.Payload.InputDigest != wire.InputDigest(in.ToolInput) {
				t.Fatalf("digest 被动词改写污染: %q", env.Payload.InputDigest)
			}
		})
	}
}
