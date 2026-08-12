package ui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/levmv/skot/agent"
	workspacetools "github.com/levmv/skot/tools"
)

func processDetailForTest(t *testing.T, result workspacetools.ProcessResult) agent.Detail {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return agent.Detail{Kind: workspacetools.ProcessResultDetailKind, Data: data}
}

func TestShellEscapeParsing(t *testing.T) {
	for _, test := range []struct {
		input   string
		command string
		private bool
		shell   bool
	}{
		{input: "hello"},
		{input: "! go test", command: "go test", shell: true},
		{input: "!!pwd", command: "pwd", private: true, shell: true},
		{input: "!", shell: true},
	} {
		command, private, shell := shellEscapeCommand(test.input)
		if command != test.command || private != test.private || shell != test.shell {
			t.Fatalf("shellEscapeCommand(%q) = %q, %v, %v", test.input, command, private, shell)
		}
	}
}

func TestSubmitStartsShellMaintenance(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.composer.setValue("! printf hello")

	model, cmd := model.submitInput()
	if cmd == nil || model.operation.kind != operationShell || model.composer.value() != "" {
		t.Fatalf("cmd=%v operation=%#v input=%q", cmd, model.operation, model.composer.value())
	}
	block := model.transcript.blocks[len(model.transcript.blocks)-1]
	if block.tool == nil || block.tool.shell == nil || block.tool.shell.private || block.text != "$ printf hello" {
		t.Fatalf("shell block = %#v", block)
	}
}

func TestPrivateShellUsesPrivateRuntimeMethod(t *testing.T) {
	fake := &fakeAgent{shellResult: agent.ToolResult{Content: "private"}}
	message := runShellCmd(context.Background(), fake, "printf private", true)()
	done, ok := message.(shellDoneMsg)
	if !ok || done.err != nil || fake.shellCommand != "printf private" || !fake.shellPrivate {
		t.Fatalf("message=%#v command=%q private=%v", message, fake.shellCommand, fake.shellPrivate)
	}
}

func TestShellResultRendersStatusAndOutput(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.startShell("printf hello", false)
	result := workspacetools.ProcessResult{
		Status: workspacetools.ProcessCompleted, DurationMillis: 1250, OutputBytes: 5, UserInitiated: true,
	}
	zero := 0
	result.ExitCode = &zero
	model.finishShell(agent.ToolResult{
		Content: "status: completed\nexit_code: 0\n\nhello\n",
		Details: []agent.Detail{processDetailForTest(t, result)},
	}, nil, model.operation.startedAt.Add(1250*time.Millisecond))

	if model.operation.kind != operationNone {
		t.Fatalf("operation = %#v", model.operation)
	}
	block := model.transcript.blocks[len(model.transcript.blocks)-1]
	rendered := strings.Join(model.renderBlockLines(block), "\n")
	for _, want := range []string{"✓", "$ printf hello", "1.2s", "5 B", "hello"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered shell missed %q: %q", want, rendered)
		}
	}
	if strings.Contains(rendered, "exit 0") {
		t.Fatalf("successful shell rendered redundant exit code: %q", rendered)
	}
}

func TestProcessStatusReportsManagedGroupSize(t *testing.T) {
	result := workspacetools.ProcessResult{
		Status: workspacetools.ProcessKilled, DurationMillis: 250, ManagedProcesses: 3,
	}
	status := processStatusText(result)
	if !strings.Contains(status, "killed") || !strings.Contains(status, "3 managed processes") {
		t.Fatalf("process status = %q", status)
	}
}

func TestModelBashResultUsesProcessPresentation(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	call := agent.ToolCall{ID: "call", Name: "bash", RawArguments: `{"command":"go test ./..."}`}
	model.addToolCallAt(call, time.Now())
	zero := 0
	result := workspacetools.ProcessResult{
		Status: workspacetools.ProcessCompleted, ExitCode: &zero, DurationMillis: 20, OutputBytes: 2,
	}
	model.finishTool(agent.ToolResult{
		CallID:  "call",
		Content: "status: completed\nexit_code: 0\n\nok\n",
		Details: []agent.Detail{processDetailForTest(t, result)},
	})
	block := model.transcript.blocks[len(model.transcript.blocks)-1]
	if block.tool == nil || block.tool.shell != nil || block.tool.process == nil || block.tool.process.UserInitiated {
		t.Fatalf("model bash block = %#v", block)
	}
	rendered := strings.Join(model.renderBlockLines(block), "\n")
	if !strings.Contains(rendered, "$ go test ./...") || !strings.Contains(rendered, "ok") {
		t.Fatalf("rendered model bash = %q", rendered)
	}
	if !strings.Contains(rendered, "\n    ok") {
		t.Fatalf("model bash output is not nested under the command: %q", rendered)
	}
}

func TestModelProcessOutputUsesBoundedHeadAndTailPreview(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	call := agent.ToolCall{ID: "call", Name: "bash", RawArguments: `{"command":"verbose"}`}
	model.addToolCallAt(call, time.Now())
	zero := 0
	outputLines := []string{"line 1", "line 2", "line 3", "line 4", "line 5", "line 6", "line 7", "line 8", "line 9", "line 10"}
	model.finishTool(agent.ToolResult{
		CallID:  "call",
		Content: "status: completed\nexit_code: 0\n\n" + strings.Join(outputLines, "\n") + "\n",
		Details: []agent.Detail{processDetailForTest(t, workspacetools.ProcessResult{
			Status: workspacetools.ProcessCompleted, ExitCode: &zero, OutputBytes: 71,
		})},
	})
	rendered := strings.Join(model.renderBlockLines(model.transcript.blocks[len(model.transcript.blocks)-1]), "\n")
	for _, want := range []string{"line 1", "line 2", "… +5 lines", "line 8", "line 9", "line 10"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("process preview missed %q: %q", want, rendered)
		}
	}
	for _, line := range strings.Split(rendered, "\n")[1:] {
		if !strings.HasPrefix(line, "    ") {
			t.Fatalf("process preview line is not nested: %q in %q", line, rendered)
		}
	}
	if strings.Contains(rendered, "line 3") || strings.Contains(rendered, "line 7") {
		t.Fatalf("process preview was not bounded: %q", rendered)
	}
}

func TestLongBashCommandUsesHangingIndent(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.resize(32, 20)
	lines := model.renderToolSummaryLines("✓", "$ echo one two three four five six seven eight", "24ms")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "✓ $ ") {
		t.Fatalf("bash header = %#v", lines)
	}
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line, "    ") {
			t.Fatalf("bash continuation is not aligned under command text: %q in %#v", line, lines)
		}
	}
}

func TestUserShellKeepsCompleteOutput(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.startShell("verbose", false)
	zero := 0
	outputLines := []string{"line 1", "line 2", "line 3", "line 4", "line 5", "line 6", "line 7", "line 8"}
	model.finishShell(agent.ToolResult{
		Content: "status: completed\nexit_code: 0\n\n" + strings.Join(outputLines, "\n") + "\n",
		Details: []agent.Detail{processDetailForTest(t, workspacetools.ProcessResult{
			Status: workspacetools.ProcessCompleted, ExitCode: &zero, OutputBytes: 55, UserInitiated: true,
		})},
	}, nil, model.operation.startedAt.Add(time.Millisecond))
	rendered := strings.Join(model.renderBlockLines(model.transcript.blocks[len(model.transcript.blocks)-1]), "\n")
	if !strings.Contains(rendered, "line 3") || !strings.Contains(rendered, "line 7") || strings.Contains(rendered, "… +") {
		t.Fatalf("user shell output was abbreviated: %q", rendered)
	}
}

func TestOmittedOutputLabelUsesSingularLine(t *testing.T) {
	if got := omittedOutputLabel(1); got != "… +1 line" {
		t.Fatalf("omittedOutputLabel(1) = %q", got)
	}
}

func TestFailedModelProcessFallsBackToFailureTail(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.addToolCallAt(agent.ToolCall{ID: "call", Name: "bash", RawArguments: `{"command":"fail"}`}, time.Now())
	exit := 1
	model.finishTool(agent.ToolResult{
		CallID:  "call",
		Content: "status: failed\nexit_code: 1\n",
		Details: []agent.Detail{processDetailForTest(t, workspacetools.ProcessResult{
			Status: workspacetools.ProcessFailed, ExitCode: &exit, FailureTail: "useful failure\nlast line",
		})},
	})
	rendered := strings.Join(model.renderBlockLines(model.transcript.blocks[len(model.transcript.blocks)-1]), "\n")
	if !strings.Contains(rendered, "exit 1") || !strings.Contains(rendered, "useful failure") || !strings.Contains(rendered, "last line") {
		t.Fatalf("failure tail was hidden: %q", rendered)
	}
}

func TestManagedBashStatusRefreshesFromRuntime(t *testing.T) {
	running := workspacetools.ProcessResult{JobID: "job-1", Status: workspacetools.ProcessRunning, DurationMillis: 1000}
	zero := 0
	completed := workspacetools.ProcessResult{
		JobID: "job-1", Status: workspacetools.ProcessCompleted, ExitCode: &zero, DurationMillis: 2500, OutputBytes: 3,
	}
	fake := &fakeAgent{status: []agent.Detail{processDetailForTest(t, completed)}, statusFound: true}
	model := testScreenModel(t, fake)
	model.appendBlock(screenBlock{
		kind: screenBlockTool, text: "$ sleep 2", tool: &toolBlock{done: true, process: &running},
	})
	model.refreshProcessResults()
	block := model.transcript.blocks[len(model.transcript.blocks)-1]
	if block.tool == nil || block.tool.process == nil || block.tool.process.Status != workspacetools.ProcessCompleted || block.tool.failed {
		t.Fatalf("refreshed process block = %#v", block)
	}
}

func TestManagedBashCompletionAppendsAfterBlockEnteredScrollback(t *testing.T) {
	running := workspacetools.ProcessResult{JobID: "job-1", Status: workspacetools.ProcessRunning, DurationMillis: 1000}
	zero := 0
	completed := workspacetools.ProcessResult{
		JobID: "job-1", Status: workspacetools.ProcessCompleted, ExitCode: &zero, DurationMillis: 3000,
	}
	fake := &fakeAgent{status: []agent.Detail{processDetailForTest(t, completed)}, statusFound: true}
	model := testScreenModel(t, fake)
	model.appendBlock(screenBlock{kind: screenBlockTool, text: "$ sleep 2", tool: &toolBlock{done: true, process: &running}})
	for range 20 {
		model.addBlock(screenBlockSystem, "later output")
	}
	model.refreshTranscript()
	processIndex := 1 // the banner is block zero
	model.renderer.previousViewportTop = model.transcript.renderCache[processIndex].end
	previousBlocks := len(model.transcript.blocks)

	if !model.refreshProcessResults() {
		t.Fatal("completion did not change the model")
	}
	if !model.transcript.blocks[processIndex].tool.superseded || len(model.transcript.blocks) != previousBlocks+1 {
		t.Fatalf("offscreen process blocks = %#v", model.transcript.blocks)
	}
	completion := model.transcript.blocks[len(model.transcript.blocks)-1]
	if completion.tool == nil || completion.tool.process == nil || completion.tool.process.Status != workspacetools.ProcessCompleted {
		t.Fatalf("appended completion = %#v", completion)
	}
}

func TestRecordedShellHistoryCollapsesSyntheticItems(t *testing.T) {
	zero := 0
	result := workspacetools.ProcessResult{
		Status: workspacetools.ProcessCompleted, ExitCode: &zero, DurationMillis: 10, OutputBytes: 2, UserInitiated: true,
	}
	fake := &fakeAgent{state: agent.State{Items: []agent.Item{
		{Kind: agent.ItemUserText, Text: "!printf hi"},
		{Kind: agent.ItemToolCall, ToolCall: &agent.ToolCall{ID: "call", Name: "bash", RawArguments: `{"command":"printf hi"}`}},
		{Kind: agent.ItemToolResult, ToolResult: &agent.ToolResult{
			CallID: "call", Content: "status: completed\nexit_code: 0\n\nhi\n",
			Details: []agent.Detail{processDetailForTest(t, result)},
		}},
	}}}
	model := testScreenModel(t, fake)

	if len(model.transcript.blocks) != 2 || model.transcript.blocks[1].kind != screenBlockTool || model.transcript.blocks[1].tool == nil || model.transcript.blocks[1].tool.shell == nil {
		t.Fatalf("history blocks = %#v", model.transcript.blocks)
	}
	if model.transcript.blocks[1].tool.output != "hi" || len(model.composer.history) != 1 || model.composer.history[0] != "!printf hi" {
		t.Fatalf("block=%#v history=%#v", model.transcript.blocks[1], model.composer.history)
	}
}

func TestEscapeCancelsShellMaintenance(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.operation.kind = operationShell
	cancelled := false
	model.operation.cancel = func() { cancelled = true }

	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !cancelled || model.operation.kind != operationShell {
		t.Fatalf("cancelled=%v operation=%#v", cancelled, model.operation)
	}
}
