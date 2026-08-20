package ui

import (
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
)

func TestDescribeToolCallUsesWorkspaceArguments(t *testing.T) {
	tests := []struct {
		name string
		tool string
		args string
		want string
	}{
		{name: "read", tool: "read", args: `{"path":"skot/main.go","offset":201,"limit":50}`, want: "read  skot/main.go:201+50"},
		{name: "list", tool: "ls", args: `{"path":"skot/internal","offset":201}`, want: "list  skot/internal:201"},
		{name: "grep", tool: "grep", args: `{"pattern":"func \\(m \\*Model\\)","path":"skot","include":"*.go"}`, want: `grep  "func \\(m \\*Model\\)" · skot · *.go`},
		{name: "glob", tool: "glob", args: `{"pattern":"**/*_test.go","path":"skot"}`, want: "glob  **/*_test.go · skot"},
		{name: "edit", tool: "edit", args: `{"path":"skot/main.go","old_text":"before","new_text":"after"}`, want: "edit  skot/main.go"},
		{name: "write", tool: "write", args: `{"path":"skot/new.go","content":"package main"}`, want: "write  skot/new.go"},
		{name: "job list", tool: "job", args: `{"action":"list"}`, want: "job  list"},
		{name: "job wait", tool: "job", args: `{"action":"wait","job_id":"job-123","timeout":30}`, want: "job  wait job-123 · timeout 30s"},
		{name: "agent start", tool: "agent", args: `{"action":"start","prompt":"inspect parser behavior","model":"openai/gpt-5-mini"}`, want: `agent  start · openai/gpt-5-mini · "inspect parser behavior"`},
		{name: "agent check", tool: "agent", args: `{"action":"check","ids":["agent_1234","agent_5678"],"wait":"any"}`, want: "agent  check agent_1234, agent_5678 · wait any"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := describeToolCall(test.tool, test.args).Text; got != test.want {
				t.Fatalf("description = %q, want %q", got, test.want)
			}
		})
	}
}

func TestConsecutiveReadsFromDirectoryAreGrouped(t *testing.T) {
	fake := &fakeAgent{}
	model := testScreenModel(t, fake)
	for index, file := range []string{"skot/main.go", "skot/config.go", "skot/process.go"} {
		model.addToolCall(agent.ToolCall{
			ID:           string(rune('a' + index)),
			Name:         "read",
			RawArguments: `{"path":"` + file + `"}`,
		})
	}

	last := model.transcript.blocks[len(model.transcript.blocks)-1]
	if got := last.text; got != "read  skot/ → main.go, config.go, process.go" {
		t.Fatalf("read group = %q", got)
	}
	if last.tool == nil || len(last.tool.callIDs) != 3 {
		t.Fatalf("pending calls = %#v", last.tool)
	}
	model.finishTool(agent.ToolResult{CallID: "a"})
	model.finishTool(agent.ToolResult{CallID: "b"})
	model.finishTool(agent.ToolResult{CallID: "c"})
	last = model.transcript.blocks[len(model.transcript.blocks)-1]
	if last.tool == nil || !last.tool.done || len(last.tool.callIDs) != 0 {
		t.Fatalf("completed group = %#v", last)
	}
}

func TestToolDisplayIsSanitizedBeforeTerminal(t *testing.T) {
	fake := &fakeAgent{}
	model := testScreenModel(t, fake)
	model.addToolCall(agent.ToolCall{ID: "call", Name: "read", RawArguments: `{"path":"safe\u001b[31mred"}`})
	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; strings.Contains(got, "\x1b") {
		t.Fatalf("tool display contains an escape: %q", got)
	}
}

func TestCompactToolTextSanitizesTerminalControls(t *testing.T) {
	input := "safe\x1b[31mred\x1b[0m \x1b]52;c;Y2xpcGJvYXJk\aafter\nnext"
	if got, want := compactSingleLine(input, 80), "safered after next"; got != want {
		t.Fatalf("compact single line = %q, want %q", got, want)
	}
	command := "safe\x1b[31mred\x1b[0m \x1b]52;c;Y2xpcGJvYXJk\aafter\r\nnext\rlast\targ"
	if got, want := compactCommand(command, 80), "safered after ↵ next ↵ last⇥arg"; got != want {
		t.Fatalf("compact command = %q, want %q", got, want)
	}
}

func TestRejectedToolCallIsShownWithoutRemainingPending(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	call := agent.ToolCall{ID: "rejected", Name: "read", RawArguments: `{"path":"large.txt"}`}
	result := agent.ToolResult{CallID: call.ID, Content: "tool iteration limit reached", Error: true}
	model.applyAgentEvent(agent.Event{Kind: agent.EventToolRejected, Call: &call, Result: &result})

	block := model.transcript.blocks[len(model.transcript.blocks)-1]
	if block.tool == nil || !block.tool.done || !block.tool.failed || len(block.tool.callIDs) != 0 {
		t.Fatalf("rejected tool block = %#v", block)
	}
	if !strings.Contains(block.text, "tool iteration limit reached") {
		t.Fatalf("rejected tool text = %q", block.text)
	}
}
