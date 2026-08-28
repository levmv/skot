package ui

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/levmv/skot/agent"
)

func TestCompactDisplayGroupsContiguousModelTools(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.clearTranscript()
	model.keymap = newDefaultKeyMapFor("linux")
	model.displayProfile = DisplayCompact

	startedAt := time.Now().Add(-3 * time.Second)
	model.addToolCallAt(agent.ToolCall{ID: "bash", Name: "bash", RawArguments: `{"command":"go test ./..."}`}, startedAt)
	zero := 0
	model.finishTool(agent.ToolResult{
		CallID:  "bash",
		Content: agent.TextContent("status: completed\nexit_code: 0\n\nnoisy output\n"),
		Details: []agent.Detail{processDetailForTest(t, agent.ProcessResult{
			Status: agent.ProcessCompleted, ExitCode: &zero,
		})},
	})
	model.addToolCallAt(agent.ToolCall{ID: "read-a", Name: "read", RawArguments: `{"path":"internal/a.go"}`}, startedAt.Add(time.Second))
	model.finishTool(agent.ToolResult{CallID: "read-a"})
	model.addToolCallAt(agent.ToolCall{ID: "read-b", Name: "read", RawArguments: `{"path":"internal/b.go"}`}, startedAt.Add(2*time.Second))
	model.finishTool(agent.ToolResult{CallID: "read-b"})
	model.addToolCallAt(agent.ToolCall{ID: "edit", Name: "edit", RawArguments: `{"path":"main.go"}`}, startedAt.Add(2*time.Second))
	change := agent.FileChange{
		Path: "main.go", Operation: "edited", Additions: 1,
		Hunks: []agent.FileDiffHunk{{Lines: []agent.FileDiffLine{{Kind: "add", NewLine: 1, Text: "new line"}}}},
	}
	detail, err := agent.NewDetail(agent.FileChangeDetailKind, change)
	if err != nil {
		t.Fatal(err)
	}
	model.finishTool(agent.ToolResult{CallID: "edit", Details: []agent.Detail{detail}})
	model.refreshTranscript()

	live := strings.Join(model.transcript.lines, "\n")
	for _, want := range []string{"$ go test ./...", "read  internal/", "edited main.go", "new line"} {
		if !strings.Contains(live, want) {
			t.Fatalf("live compact tail missed %q: %q", want, live)
		}
	}
	if strings.Contains(live, "noisy output") {
		t.Fatalf("live compact Bash exposed successful output: %q", live)
	}

	model.transcript.appendAssistant("first", "Done.")
	model.refreshTranscript()
	collapsed := strings.Join(model.transcript.lines, "\n")
	for _, want := range []string{"Used 4 tools · changed 1 file · ", "ctrl+up for more detail", "Done."} {
		if !strings.Contains(collapsed, want) {
			t.Fatalf("collapsed compact tail missed %q: %q", want, collapsed)
		}
	}
	for _, hidden := range []string{"$ go test", "internal/a.go", "edited main.go", "new line", "noisy output"} {
		if strings.Contains(collapsed, hidden) {
			t.Fatalf("collapsed compact tail kept %q: %q", hidden, collapsed)
		}
	}

	// Work after commentary starts a fresh visible tail, which is committed by
	// the next assistant text independently of the earlier summary.
	model.addToolCallAt(agent.ToolCall{ID: "grep", Name: "grep", RawArguments: `{"pattern":"TODO"}`}, time.Now().Add(-1500*time.Millisecond))
	model.finishTool(agent.ToolResult{CallID: "grep"})
	model.refreshTranscript()
	if live = strings.Join(model.transcript.lines, "\n"); !strings.Contains(live, `grep  "TODO"`) {
		t.Fatalf("new live tail = %q", live)
	}
	model.transcript.appendAssistant("second", "More.")
	model.refreshTranscript()
	if collapsed = strings.Join(model.transcript.lines, "\n"); !strings.Contains(collapsed, "Used 1 tool · ") || strings.Contains(collapsed, `grep  "TODO"`) {
		t.Fatalf("second collapsed tail = %q", collapsed)
	}
	if count := strings.Count(collapsed, "for more detail"); count != 1 {
		t.Fatalf("compact detail hint count = %d in %q", count, collapsed)
	}
}

func TestCompactDisplayFoldsOldestLiveToolsToFitViewport(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.clearTranscript()
	model.displayProfile = DisplayCompact
	model.resize(80, 9)

	for number := 1; number <= 7; number++ {
		id := fmt.Sprintf("bash-%d", number)
		model.addToolCall(agent.ToolCall{
			ID: id, Name: "bash",
			RawArguments: fmt.Sprintf(`{"command":"tool-%d"}`, number),
		})
		model.finishTool(agent.ToolResult{CallID: id})
		model.refreshTranscript()
	}

	live := strings.Join(model.transcript.lines, "\n")
	hiddenPrefix := func(rendered string) int {
		t.Helper()
		hidden := 0
		visible := false
		for number := 1; number <= 7; number++ {
			shown := strings.Contains(rendered, fmt.Sprintf("tool-%d", number))
			if shown {
				visible = true
				continue
			}
			if visible {
				t.Fatalf("tool %d was hidden after a newer visible tool: %q", number, rendered)
			}
			hidden++
		}
		return hidden
	}
	foldedBeforeEditor := hiddenPrefix(live)
	if foldedBeforeEditor <= 0 || foldedBeforeEditor >= 7 || !strings.Contains(live, fmt.Sprintf("Used %d tools", foldedBeforeEditor)) {
		t.Fatalf("rolling summary = %q", live)
	}
	frame := model.inlineFrame()
	if rows := len(frame.transcript) + len(frame.dynamic); rows > model.height {
		t.Fatalf("live frame uses %d rows at height %d", rows, model.height)
	}
	updated, _ := model.Update(tea.PasteMsg{Content: "draft\nsecond line\nthird line"})
	model = updated.(screenModel)
	foldedAfterEditor := hiddenPrefix(strings.Join(model.transcript.lines, "\n"))
	if foldedAfterEditor <= foldedBeforeEditor {
		t.Fatalf("growing editor did not advance rolling summary: before=%d after=%d", foldedBeforeEditor, foldedAfterEditor)
	}

	model.transcript.appendAssistant("answer", "Done.")
	model.refreshTranscript()
	settled := strings.Join(model.transcript.lines, "\n")
	if !strings.Contains(settled, "Used 7 tools") || !strings.Contains(settled, "Done.") {
		t.Fatalf("settled summary = %q", settled)
	}
	for number := 1; number <= 7; number++ {
		if strings.Contains(settled, fmt.Sprintf("tool-%d", number)) {
			t.Fatalf("settled tool %d remained expanded: %q", number, settled)
		}
	}
}

func TestCompactRollingFoldDoesNotClearScrollback(t *testing.T) {
	var output bytes.Buffer
	model, err := newScreenModel(context.Background(), &fakeAgent{
		theme: ThemeLight, displayProfile: DisplayCompact,
	}, Config{}, &output)
	if err != nil {
		t.Fatal(err)
	}
	model.resize(80, 9)
	model.clearTranscript()
	for number := range 12 {
		model.addBlock(screenBlockSystem, fmt.Sprintf("stable history %d", number))
	}
	model.refreshTranscript()
	if err := model.renderer.RenderFrame(model.inlineFrame(), model.width, model.height); err != nil {
		t.Fatal(err)
	}
	model.transcript.presented()
	output.Reset()

	render := func(stage string) {
		t.Helper()
		model.refreshTranscript()
		if err := model.renderer.RenderFrame(model.inlineFrame(), model.width, model.height); err != nil {
			t.Fatal(err)
		}
		model.transcript.presented()
		if strings.Contains(output.String(), ansi.EraseEntireDisplay) {
			t.Fatalf("%s cleared terminal scrollback", stage)
		}
		output.Reset()
	}
	for number := 1; number <= 8; number++ {
		id := fmt.Sprintf("bash-%d", number)
		model.addToolCall(agent.ToolCall{
			ID: id, Name: "bash",
			RawArguments: fmt.Sprintf(`{"command":"tool-%d"}`, number),
		})
		model.finishTool(agent.ToolResult{CallID: id})
		render(id)
	}
	model.transcript.appendAssistant("answer", "Done.")
	render("settled answer")
}

func TestCompactSettledToolsReserveRowsForStreamingAnswer(t *testing.T) {
	var output bytes.Buffer
	model, err := newScreenModel(context.Background(), &fakeAgent{
		theme: ThemeLight, displayProfile: DisplayCompact,
	}, Config{}, &output)
	if err != nil {
		t.Fatal(err)
	}
	model.resize(80, 24)
	model.clearTranscript()
	model.operation = activeOperation{kind: operationTurn, startedAt: time.Now()}
	model.addBlock(screenBlockUser, "Question")
	for number := 1; number <= 8; number++ {
		id := fmt.Sprintf("bash-%d", number)
		model.addToolCall(agent.ToolCall{
			ID: id, Name: "bash",
			RawArguments: fmt.Sprintf(`{"command":"tool-%d"}`, number),
		})
		model.finishTool(agent.ToolResult{CallID: id})
	}
	model.refreshTranscript()
	before := model.inlineFrame()
	if err := model.renderer.RenderFrame(before, model.width, model.height); err != nil {
		t.Fatal(err)
	}
	model.transcript.presented()
	previousRows := len(before.transcript) + len(before.dynamic)
	previousDynamicStart := len(before.transcript)
	output.Reset()

	model.transcript.appendAssistant("answer", "First line")
	model.refreshTranscript()
	frame := model.inlineFrame()
	baseDynamic, _ := model.baseInlineDynamic()
	padding := len(frame.dynamic) - len(baseDynamic)
	if padding <= 0 || len(frame.transcript)+len(frame.dynamic) != previousRows || len(frame.transcript)+padding != previousDynamicStart {
		t.Fatalf("reserved frame: transcript=%d padding=%d dynamic=%d previous rows=%d previous dynamic start=%d",
			len(frame.transcript), padding, len(frame.dynamic), previousRows, previousDynamicStart)
	}
	if err := model.renderer.RenderFrame(frame, model.width, model.height); err != nil {
		t.Fatal(err)
	}
	model.transcript.presented()
	if strings.Contains(output.String(), ansi.EraseEntireDisplay) {
		t.Fatal("settling tools cleared scrollback")
	}
	output.Reset()

	model.transcript.appendAssistant("answer", "\nSecond line")
	model.refreshTranscript()
	next := model.inlineFrame()
	nextBaseDynamic, _ := model.baseInlineDynamic()
	nextPadding := len(next.dynamic) - len(nextBaseDynamic)
	if nextPadding != padding-1 || len(next.transcript)+len(next.dynamic) != previousRows || len(next.transcript)+nextPadding != previousDynamicStart {
		t.Fatalf("consumed reserve: transcript=%d padding=%d rows=%d", len(next.transcript), nextPadding, len(next.transcript)+len(next.dynamic))
	}
	if err := model.renderer.RenderFrame(next, model.width, model.height); err != nil {
		t.Fatal(err)
	}
	model.transcript.presented()
	if strings.Contains(output.String(), ansi.EraseEntireDisplay) {
		t.Fatal("streaming into reserved rows cleared scrollback")
	}

	model.resize(70, 24)
	resized := model.inlineFrame()
	resizedBaseDynamic, _ := model.baseInlineDynamic()
	if padding := len(resized.dynamic) - len(resizedBaseDynamic); padding != 0 {
		t.Fatalf("resized frame kept %d reserved rows", padding)
	}
}

func TestCompactDisplayKeepsFoldedReadStableWhenNewCallArrives(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.clearTranscript()
	model.displayProfile = DisplayCompact
	model.resize(30, 9)

	folded := false
	for number := 1; number <= 12; number++ {
		id := fmt.Sprintf("read-%02d", number)
		model.addToolCall(agent.ToolCall{
			ID: id, Name: "read",
			RawArguments: fmt.Sprintf(`{"path":"internal/file-%02d.go"}`, number),
		})
		model.finishTool(agent.ToolResult{CallID: id})
		model.refreshTranscript()
		if strings.Contains(strings.Join(model.transcript.lines, "\n"), "Used ") {
			folded = true
			break
		}
	}
	if !folded {
		t.Fatalf("read group did not fold: %q", model.transcript.lines)
	}

	model.addToolCall(agent.ToolCall{ID: "read-new", Name: "read", RawArguments: `{"path":"internal/new.go"}`})
	model.finishTool(agent.ToolResult{CallID: "read-new"})
	model.refreshTranscript()
	rendered := strings.Join(model.transcript.lines, "\n")
	if !strings.Contains(rendered, "Used ") || strings.Contains(rendered, "file-01.go") || !strings.Contains(rendered, "new.go") {
		t.Fatalf("folded read changed = %q", rendered)
	}
}

func TestCompactDisplayExpandsRunningAndFailedTools(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.clearTranscript()
	model.displayProfile = DisplayCompact
	startedAt := time.Now().Add(-2 * time.Second)
	model.addToolCallAt(agent.ToolCall{ID: "running", Name: "bash", RawArguments: `{"command":"make check"}`}, startedAt)
	model.addToolCallAt(agent.ToolCall{ID: "failed", Name: "grep", RawArguments: `{"pattern":"needle"}`}, startedAt.Add(time.Second))
	model.finishTool(agent.ToolResult{CallID: "failed", Content: agent.TextContent("permission denied"), Error: true})
	model.transcript.appendAssistant("answer", "Continuing.")
	model.refreshTranscript()

	rendered := strings.Join(model.transcript.lines, "\n")
	for _, want := range []string{"Used 2 tools · ", "$ make check", `grep  "needle": permission denied`, "Continuing."} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("compact active group missed %q: %q", want, rendered)
		}
	}
}

func TestCompactDisplayShortensLiveBashAndDiffs(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.clearTranscript()
	model.displayProfile = DisplayCompact

	longArgument := strings.Repeat("long-argument ", 20)
	model.addToolCall(agent.ToolCall{ID: "bash", Name: "bash", RawArguments: `{"command":"run ` + longArgument + `"}`})
	zero := 0
	model.finishTool(agent.ToolResult{
		CallID:  "bash",
		Content: agent.TextContent("status: completed\nexit_code: 0\n\nfirst output line\nsecond output line\n"),
		Details: []agent.Detail{processDetailForTest(t, agent.ProcessResult{
			Status: agent.ProcessCompleted, ExitCode: &zero,
		})},
	})

	model.addToolCall(agent.ToolCall{ID: "edit", Name: "edit", RawArguments: `{"path":"large.go"}`})
	var diffLines []agent.FileDiffLine
	for line := 1; line <= 20; line++ {
		diffLines = append(diffLines, agent.FileDiffLine{Kind: "add", NewLine: line, Text: fmt.Sprintf("diff-line-%02d", line)})
	}
	change := agent.FileChange{
		Path: "large.go", Operation: "edited", Additions: 20, TotalHunks: 1,
		Hunks: []agent.FileDiffHunk{{Lines: diffLines}},
	}
	detail, err := agent.NewDetail(agent.FileChangeDetailKind, change)
	if err != nil {
		t.Fatal(err)
	}
	model.finishTool(agent.ToolResult{CallID: "edit", Details: []agent.Detail{detail}})
	model.refreshTranscript()

	live := strings.Join(model.transcript.lines, "\n")
	if !strings.Contains(live, "$ run long-argument") || !strings.Contains(live, "…") || strings.Contains(live, "first output line") {
		t.Fatalf("compact Bash display = %q", live)
	}
	if !strings.Contains(live, "diff-line-12") || strings.Contains(live, "diff-line-13") || !strings.Contains(live, "diff preview limited") {
		t.Fatalf("compact diff display = %q", live)
	}
}

func TestCompactDisplayHidesSuccessfulOutputFromProcessBackedTools(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.clearTranscript()
	model.displayProfile = DisplayCompact
	model.addToolCall(agent.ToolCall{ID: "program", Name: "custom_program", RawArguments: `{}`})
	zero := 0
	model.finishTool(agent.ToolResult{
		CallID:  "program",
		Content: agent.TextContent("status: completed\nexit_code: 0\n\nnoisy program output\n"),
		Details: []agent.Detail{processDetailForTest(t, agent.ProcessResult{
			Status: agent.ProcessCompleted, ExitCode: &zero,
		})},
	})
	model.refreshTranscript()

	rendered := strings.Join(model.transcript.lines, "\n")
	if !strings.Contains(rendered, "custom_program") || strings.Contains(rendered, "noisy program output") {
		t.Fatalf("compact process-backed tool = %q", rendered)
	}
}

func TestCompactDisplaySettlesToolTailAtConversationBoundaries(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.clearTranscript()
	model.displayProfile = DisplayCompact

	model.addToolCallAt(agent.ToolCall{ID: "read", Name: "read", RawArguments: `{"path":"before-status.go"}`}, time.Now().Add(-time.Second))
	model.finishTool(agent.ToolResult{CallID: "read"})
	model.addBlock(screenBlockSystem, "context compacted")
	model.addToolCallAt(agent.ToolCall{ID: "grep", Name: "grep", RawArguments: `{"pattern":"needle"}`}, time.Now().Add(-time.Second))
	model.finishTool(agent.ToolResult{CallID: "grep"})
	model.addBlock(screenBlockUser, "queued follow-up")
	model.refreshTranscript()

	rendered := strings.Join(model.transcript.lines, "\n")
	if strings.Count(rendered, "Used 1 tool · ") != 2 || !strings.Contains(rendered, "context compacted") || !strings.Contains(rendered, "queued follow-up") {
		t.Fatalf("compact conversation boundaries = %q", rendered)
	}
	for _, hidden := range []string{"before-status.go", `grep  "needle"`} {
		if strings.Contains(rendered, hidden) {
			t.Fatalf("settled tail kept %q: %q", hidden, rendered)
		}
	}
}

func TestCompactDisplayKeepsToolsSettledAfterDiscardedText(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.clearTranscript()
	model.displayProfile = DisplayCompact
	model.addToolCallAt(agent.ToolCall{ID: "read", Name: "read", RawArguments: `{"path":"answer.go"}`}, time.Now().Add(-time.Second))
	model.finishTool(agent.ToolResult{CallID: "read"})
	model.transcript.appendAssistant("attempt", "partial answer")
	if !model.transcript.discardAttempt("attempt") {
		t.Fatal("visible partial response was not discarded")
	}
	model.refreshTranscript()

	rendered := strings.Join(model.transcript.lines, "\n")
	if !strings.Contains(rendered, "Used 1 tool · ") || strings.Contains(rendered, "answer.go") || strings.Contains(rendered, "partial answer") {
		t.Fatalf("compact discarded response = %q", rendered)
	}
}

func TestCompactDisplayKeepsLiveBashToOneLineOnNarrowScreen(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.clearTranscript()
	model.displayProfile = DisplayCompact
	model.resize(18, 20)
	model.addToolCall(agent.ToolCall{
		ID: "bash", Name: "bash",
		RawArguments: `{"command":"printf one two three four five six seven eight"}`,
	})

	lines := model.renderBlockLinesAt(0, model.transcript.blocks[0])
	if len(lines) != 1 || !strings.Contains(lines[0], "…") {
		t.Fatalf("narrow compact Bash = %#v", lines)
	}

	zero := 0
	model.finishTool(agent.ToolResult{
		CallID:  "bash",
		Content: agent.TextContent("status: completed\n\noutput"),
		Details: []agent.Detail{processDetailForTest(t, agent.ProcessResult{
			Status: agent.ProcessCompleted, ExitCode: &zero, OutputBytes: 112,
		})},
	})
	lines = model.renderBlockLinesAt(0, model.transcript.blocks[0])
	if len(lines) != 1 || !strings.Contains(lines[0], "112 B") {
		t.Fatalf("completed narrow compact Bash = %#v", lines)
	}
}

func TestCollapsedToolDurationAvoidsZero(t *testing.T) {
	if got := formatCollapsedToolDuration(500 * time.Millisecond); got != "<1s" {
		t.Fatalf("subsecond duration = %q", got)
	}
	if got := formatCollapsedToolDuration(3500 * time.Millisecond); got != "3.5s" {
		t.Fatalf("duration = %q", got)
	}
}

func TestCompactToolSummaryUsesMutedMarkerAndNeutralBoldText(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("SK_COLOR", "always")
	model := testScreenModel(t, &fakeAgent{})
	model.clearTranscript()
	model.keymap = newDefaultKeyMapFor("linux")
	for range 11 {
		model.transcript.blocks = append(model.transcript.blocks, screenBlock{
			kind: screenBlockTool,
			tool: &toolBlock{done: true, collapsed: true},
		})
	}

	rendered := strings.Join(model.renderCompactToolSummary(0, 10), "\n")
	if plain := strings.TrimSpace(ansi.Strip(rendered)); plain != "• Used 11 tools  (ctrl+up for more detail)" {
		t.Fatalf("compact tool summary text = %q", plain)
	}
	if !strings.Contains(rendered, model.mutedStyle.Render("•")) ||
		!strings.Contains(rendered, model.summaryStyle.Render("Used 11 tools")) ||
		model.summaryStyle.GetForeground() != lipgloss.NewStyle().GetForeground() || !model.summaryStyle.GetBold() {
		t.Fatalf("compact tool summary = %q", rendered)
	}
}

func TestCompactToolSummaryFallsBackToDisplayCommandWithoutRelativeKeys(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.clearTranscript()
	model.keymap = newDefaultKeyMapFor("darwin")
	model.transcript.blocks = []screenBlock{{
		kind: screenBlockTool,
		tool: &toolBlock{done: true, collapsed: true},
	}}

	rendered := ansi.Strip(strings.Join(model.renderCompactToolSummary(0, 0), "\n"))
	if !strings.Contains(rendered, "(/display for more detail)") {
		t.Fatalf("compact detail fallback = %q", rendered)
	}
	model.keyboard.record(tea.KeyboardEnhancementsMsg{Flags: ansi.KittyDisambiguateEscapeCodes})
	rendered = ansi.Strip(strings.Join(model.renderCompactToolSummary(0, 0), "\n"))
	if !strings.Contains(rendered, "(ctrl+2 for more detail)") {
		t.Fatalf("enhanced compact detail shortcut = %q", rendered)
	}
}

func TestDisplayShortcutHintsMatchAvailableBindings(t *testing.T) {
	linux := testScreenModel(t, &fakeAgent{})
	linux.keymap = newDefaultKeyMapFor("linux")
	if got := linux.displayShortcutHint(); got != "ctrl+up/down adjust" {
		t.Fatalf("legacy Linux display hint = %q", got)
	}
	linux.keyboard.record(tea.KeyboardEnhancementsMsg{Flags: ansi.KittyDisambiguateEscapeCodes})
	if got := linux.displayShortcutHint(); got != "ctrl+1/2/3 select · ctrl+up/down adjust" {
		t.Fatalf("enhanced Linux display hint = %q", got)
	}
	linux.openDisplayPicker()
	if rendered := ansi.Strip(strings.Join(linux.renderPicker(), "\n")); !strings.Contains(rendered, "Anytime: "+linux.displayShortcutHint()) {
		t.Fatalf("display picker omitted shortcuts: %q", rendered)
	}

	macOS := testScreenModel(t, &fakeAgent{})
	macOS.keymap = newDefaultKeyMapFor("darwin")
	if got := macOS.displayShortcutHint(); got != "" {
		t.Fatalf("legacy macOS display hint = %q", got)
	}
	macOS.keyboard.record(tea.KeyboardEnhancementsMsg{Flags: ansi.KittyDisambiguateEscapeCodes})
	if got := macOS.displayShortcutHint(); got != "ctrl+1/2/3 select" {
		t.Fatalf("enhanced macOS display hint = %q", got)
	}
}

func TestCompactDisplayHidesDuplicateTurnFooter(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.clearTranscript()
	model.displayProfile = DisplayCompact
	model.addBlock(screenBlockAssistant, "Done.")
	model.addBlock(screenBlockChangeSummary, "changed 2 files · a.go, b.go")
	model.appendBlock(screenBlock{kind: screenBlockDuration, duration: 3 * time.Second})
	model.refreshTranscript()

	rendered := strings.Join(model.transcript.lines, "\n")
	if !strings.Contains(rendered, "Done.") || strings.Contains(rendered, "changed 2 files") || strings.Contains(rendered, "Worked for") {
		t.Fatalf("compact completed turn = %q", rendered)
	}
}

func TestFullDisplayKeepsIndividualCallsArgumentsAndResults(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.clearTranscript()
	model.displayProfile = DisplayDetailed

	model.addToolCall(agent.ToolCall{ID: "read-a", Name: "read", RawArguments: `{"path":"internal/a.go","offset":10}`})
	model.finishTool(agent.ToolResult{CallID: "read-a", Content: agent.TextContent("first saved result")})
	model.addToolCall(agent.ToolCall{ID: "read-b", Name: "read", RawArguments: `{"path":"internal/b.go","limit":25}`})
	model.finishTool(agent.ToolResult{CallID: "read-b", Content: agent.TextContent("second saved result")})

	longArgument := strings.Repeat("argument-", 30)
	model.addToolCall(agent.ToolCall{ID: "program", Name: "custom_program", RawArguments: `{"value":"` + longArgument + `"}`})
	zero := 0
	model.finishTool(agent.ToolResult{
		CallID:  "program",
		Content: agent.TextContent("status: completed\n\nline-1\nline-2\nline-3\nline-4\nline-5\nline-6\nline-7\nline-8"),
		Details: []agent.Detail{processDetailForTest(t, agent.ProcessResult{Status: agent.ProcessCompleted, ExitCode: &zero})},
	})
	model.refreshTranscript()
	detailed := strings.Join(model.transcript.lines, "\n")
	if !strings.Contains(detailed, "read  internal/ → a.go:10, b.go") || strings.Contains(detailed, "first saved result") {
		t.Fatalf("detailed display lost its summaries: %q", detailed)
	}

	model.displayProfile = DisplayFull
	model.transcript.invalidate()
	model.refreshTranscript()

	rendered := strings.Join(model.transcript.lines, "\n")
	unwrapped := strings.ReplaceAll(rendered, "\n  ", "")
	for _, want := range []string{
		`read  {"path":"internal/a.go","offset":10}`,
		`read  {"path":"internal/b.go","limit":25}`,
		"first saved result",
		"second saved result",
		"line-1",
		"line-8",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("full display missed %q: %q", want, rendered)
		}
	}
	if !strings.Contains(unwrapped, longArgument) {
		t.Fatalf("full display truncated arguments: %q", rendered)
	}
	if strings.Contains(rendered, "internal/ →") || strings.Contains(rendered, "… +") {
		t.Fatalf("full display aggregated or truncated saved tools: %q", rendered)
	}
}

func TestDisplayCommandSwitchesLiveProfile(t *testing.T) {
	fake := &fakeAgent{displayProfile: DisplayDetailed}
	model := testScreenModel(t, fake)
	model.clearTranscript()
	model.keymap = newDefaultKeyMapFor("linux")
	model.addToolCall(agent.ToolCall{ID: "read", Name: "read", RawArguments: `{"path":"internal/command.go"}`})
	model.finishTool(agent.ToolResult{CallID: "read"})
	model.addBlock(screenBlockAssistant, "Done.")
	model.refreshTranscript()
	if rendered := strings.Join(model.transcript.lines, "\n"); !strings.Contains(rendered, "internal/command.go") {
		t.Fatalf("detailed transcript = %q", rendered)
	}

	if _, handled := model.dispatchCommand("/display compact"); !handled {
		t.Fatal("display command was not handled")
	}
	if model.displayProfile != DisplayCompact || fake.displayProfile != DisplayCompact {
		t.Fatalf("display switch: model=%q fake=%q", model.displayProfile, fake.displayProfile)
	}
	model.refreshTranscript()
	if rendered := strings.Join(model.transcript.lines, "\n"); !strings.Contains(rendered, "Used 1 tool") || strings.Contains(rendered, "internal/command.go") {
		t.Fatalf("compact transcript = %q", rendered)
	}
	last := model.transcript.blocks[len(model.transcript.blocks)-1]
	if last.kind != screenBlockSystem || last.text != "display: compact · ctrl+up/down adjust" {
		t.Fatalf("display notice = %#v", last)
	}
	if _, handled := model.dispatchCommand("/display full"); !handled || model.displayProfile != DisplayFull || fake.displayProfile != DisplayFull {
		t.Fatalf("full display switch: handled=%v model=%q fake=%q", handled, model.displayProfile, fake.displayProfile)
	}

	model.dispatchCommand("/display")
	if model.picker.kind != pickerDisplay || model.picker.selectedItem().value != DisplayFull || len(model.picker.items) != 3 {
		t.Fatalf("display picker = %#v", model.picker)
	}
}
