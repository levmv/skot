package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
)

func TestDescribeToolCallUsesToolArguments(t *testing.T) {
	tests := []struct {
		name string
		tool string
		args string
		root string
		want string
	}{
		{name: "read", tool: "read", args: `{"path":"skot/main.go","offset":201,"limit":50}`, want: "read  skot/main.go:201+50"},
		{name: "read inside the workspace", tool: "read", args: `{"path":"/workspaces/skot/agent/runtime.go"}`, root: "/workspaces/skot", want: "read  agent/runtime.go"},
		{name: "read outside the workspace", tool: "read", args: `{"path":"/tmp/review.diff"}`, root: "/workspaces/skot", want: "read  /tmp/review.diff"},
		{name: "list", tool: "ls", args: `{"path":"skot/internal","offset":201}`, want: "list  skot/internal:201"},
		{name: "grep", tool: "grep", args: `{"pattern":"func \\(m \\*Model\\)","path":"skot","include":"*.go"}`, want: `grep  "func \\(m \\*Model\\)" · skot · *.go`},
		{name: "glob", tool: "glob", args: `{"pattern":"**/*_test.go","path":"skot"}`, want: "glob  **/*_test.go · skot"},
		{name: "edit", tool: "edit", args: `{"path":"skot/main.go","old_text":"before","new_text":"after"}`, want: "edit  skot/main.go"},
		{name: "write", tool: "write", args: `{"path":"skot/new.go","content":"package main"}`, want: "write  skot/new.go"},
		{name: "shell", tool: "bash", args: `{"command":"go test ./...","workdir":"skot","background":true}`, want: "$ go test ./... · in skot · background"},
		{name: "job list", tool: "job", args: `{"action":"list"}`, want: "job  list"},
		{name: "job wait", tool: "job", args: `{"action":"wait","job_id":"job-123","timeout":30}`, want: "job  wait job-123 · timeout 30s"},
		{name: "agent start", tool: "agent", args: `{"action":"start","prompt":"inspect parser behavior","model":"openai/gpt-5-mini"}`, want: `agent  start · openai/gpt-5-mini · "inspect parser behavior"`},
		{name: "agent check", tool: "agent", args: `{"action":"check","ids":["agent_1234","agent_5678"],"wait":"any"}`, want: "agent  check agent_1234, agent_5678 · wait any"},
		{name: "web search", tool: "web_search", args: `{"query":"current Go release"}`, want: `web  "current Go release"`},
		{name: "web fetch", tool: "web_fetch", args: `{"url":"https://example.com/docs?q=skot"}`, want: "fetch  https://example.com/docs?…"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := describeToolCall(test.tool, test.args, test.root).Text; got != test.want {
				t.Fatalf("description = %q, want %q", got, test.want)
			}
		})
	}
}

func TestConsecutiveReadsFromDirectoryAreGrouped(t *testing.T) {
	fake := &fakeAgent{}
	model := testScreenModel(t, fake)
	model.clearTranscript()
	model.transcript.root = "/home/dev"
	// Models name the same directory both ways, so both forms join one group.
	for index, file := range []string{"skot/main.go", "/home/dev/skot/config.go", "skot/process.go"} {
		model.addToolCall(agent.ToolCall{
			ID:           string(rune('a' + index)),
			Name:         "read",
			RawArguments: `{"path":"` + file + `"}`,
		})
	}

	if len(model.transcript.blocks) != 3 {
		t.Fatalf("source calls were merged: %#v", model.transcript.blocks)
	}
	model.refreshTranscript()
	if got := strings.Join(model.transcript.lines, "\n"); !strings.Contains(got, "read  skot/ → main.go, config.go, process.go") {
		t.Fatalf("read group = %q", got)
	}
	model.finishTool(agent.ToolResult{CallID: "a"})
	model.finishTool(agent.ToolResult{CallID: "b"})
	model.finishTool(agent.ToolResult{CallID: "c"})
	for _, block := range model.transcript.blocks {
		if block.tool == nil || !block.tool.done || len(block.tool.callIDs) != 0 {
			t.Fatalf("completed call = %#v", block)
		}
	}
}

func TestImageReadShowsDimensionsWithoutPayload(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.addToolCall(agent.ToolCall{ID: "image", Name: "read", RawArguments: `{"path":"shot.png"}`})
	model.finishTool(agent.ToolResult{CallID: "image", Content: agent.ImageToolContent("metadata", agent.ImageContent{
		MediaType: "image/png", Data: []byte("payload-must-stay-hidden"), Width: 1200, Height: 800,
	})})

	text := model.transcript.blocks[len(model.transcript.blocks)-1].text
	if !strings.Contains(text, "[image/png 1200×800]") || strings.Contains(text, "payload-must-stay-hidden") {
		t.Fatalf("image tool display = %q", text)
	}
	model.refreshTranscript()
	rendered := strings.Join(model.transcript.lines, "\n")
	if !strings.Contains(rendered, "[image/png 1200×800]") || strings.Contains(rendered, "payload-must-stay-hidden") {
		t.Fatalf("rendered image tool = %q", rendered)
	}
}

func TestFailedReadKeepsItsDiagnosticOutsidePresentationGroup(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.clearTranscript()
	model.addToolCall(agent.ToolCall{ID: "first", Name: "read", RawArguments: `{"path":"internal/a.go"}`})
	model.finishTool(agent.ToolResult{CallID: "first"})
	model.addToolCall(agent.ToolCall{ID: "second", Name: "read", RawArguments: `{"path":"internal/b.go"}`})
	model.finishTool(agent.ToolResult{CallID: "second", Content: agent.TextContent("permission denied"), Error: true})
	model.refreshTranscript()

	rendered := strings.Join(model.transcript.lines, "\n")
	for _, want := range []string{"internal/a.go", "internal/b.go: permission denied"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("failed read lost %q: %q", want, rendered)
		}
	}
}

func TestReadGroupTimesOnlyTheToolWorkAcrossSeparateCalls(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.addToolCallAt(agent.ToolCall{ID: "first", Name: "read", RawArguments: `{"path":"agent/runtime.go"}`}, time.Now())
	model.finishTool(agent.ToolResult{CallID: "first"})
	// Backdate the finished read: the model then spent three seconds thinking
	// before asking for the next file in the same directory.
	model.transcript.blocks[len(model.transcript.blocks)-1].tool.startedAt = time.Now().Add(-3 * time.Second)

	model.addToolCallAt(agent.ToolCall{ID: "second", Name: "read", RawArguments: `{"path":"agent/details.go"}`}, time.Now())
	model.finishTool(agent.ToolResult{CallID: "second"})

	model.refreshTranscript()
	rendered := strings.Join(model.transcript.lines, "\n")
	if strings.ContainsAny(rendered, "0123456789") {
		t.Fatalf("grouped read charged the model gap to the tool: %q", rendered)
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
	result := agent.ToolResult{CallID: call.ID, Content: agent.TextContent("tool iteration limit reached"), Error: true}
	model.applyAgentEvent(agent.Event{Kind: agent.EventToolRejected, Call: &call, Result: &result})

	block := model.transcript.blocks[len(model.transcript.blocks)-1]
	if block.tool == nil || !block.tool.done || !block.tool.failed || len(block.tool.callIDs) != 0 {
		t.Fatalf("rejected tool block = %#v", block)
	}
	if !strings.Contains(block.text, "tool iteration limit reached") {
		t.Fatalf("rejected tool text = %q", block.text)
	}
}
