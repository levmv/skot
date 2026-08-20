package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
)

func TestFileChangeDetailRendersFocusedDiff(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.addToolCall(agent.ToolCall{ID: "call-1", Name: "edit", RawArguments: `{"path":"note.txt"}`})
	change := agent.FileChange{
		Type: agent.FileChangeDetailKind, Path: "note.txt", Operation: "edited",
		Additions: 1, Deletions: 1, TotalHunks: 1,
		Hunks: []agent.FileDiffHunk{{
			OldStart: 1, OldLines: 2, NewStart: 1, NewLines: 2,
			Lines: []agent.FileDiffLine{
				{Kind: "context", OldLine: 1, NewLine: 1, Text: "alpha"},
				{Kind: "delete", OldLine: 2, Text: "beta"},
				{Kind: "add", NewLine: 2, Text: "gamma"},
			},
		}},
	}
	detail, err := agent.NewDetail(agent.FileChangeDetailKind, change)
	if err != nil {
		t.Fatal(err)
	}
	model.finishTool(agent.ToolResult{CallID: "call-1", Details: []agent.Detail{detail}})

	block := model.transcript.blocks[len(model.transcript.blocks)-1]
	if block.tool == nil || block.tool.fileChange == nil || block.text != "edited  note.txt" {
		t.Fatalf("file change block = %#v", block)
	}
	rendered := strings.Join(model.renderBlockLines(block), "\n")
	want := "  edited note.txt (+1 −1)\n" +
		"  1    alpha\n" +
		"  2 −  beta\n" +
		"  2 +  gamma"
	if rendered != want {
		t.Fatalf("rendered diff = %q, want %q", rendered, want)
	}
	if len(model.operation.changedPaths) != 1 || model.operation.changedPaths[0] != "note.txt" {
		t.Fatalf("changed paths = %#v", model.operation.changedPaths)
	}
}

func TestFileChangeSeparatesHunksWithoutMachineHeader(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	change := agent.FileChange{
		Additions: 2,
		Deletions: 2,
		Hunks: []agent.FileDiffHunk{
			{Lines: []agent.FileDiffLine{
				{Kind: "delete", OldLine: 2, Text: "old near start"},
				{Kind: "add", NewLine: 2, Text: "new near start"},
			}},
			{Lines: []agent.FileDiffLine{
				{Kind: "delete", OldLine: 12, Text: "old near end"},
				{Kind: "add", NewLine: 12, Text: "new near end"},
			}},
		},
	}

	lines := model.renderFileChangeLines("edited  note.txt", change, " ")
	rendered := strings.Join(lines, "\n")
	if strings.Contains(rendered, "@@") {
		t.Fatalf("machine hunk header survived: %q", rendered)
	}
	if count := strings.Count(rendered, "⋮"); count != 1 {
		t.Fatalf("hunk separators = %d: %q", count, rendered)
	}
	numberColumn := strings.Index(lines[1], "2")
	separatorColumn := strings.Index(lines[3], "⋮")
	if numberColumn < 0 || separatorColumn != numberColumn {
		t.Fatalf("separator column = %d, number column = %d: %q", separatorColumn, numberColumn, rendered)
	}
}

func TestFileChangeWrapUsesHangingContentIndent(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.resize(24, 24)
	lines := model.renderFileDiffLine(agent.FileDiffLine{
		Kind: "add", NewLine: 123, Text: "abcdefghijklmnopqrstuv",
	}, 3)

	if len(lines) < 2 {
		t.Fatalf("long diff line did not wrap: %q", lines)
	}
	firstContentColumn := strings.Index(lines[0], "a")
	continuationColumn := len(lines[1]) - len(strings.TrimLeft(lines[1], " "))
	if firstContentColumn < 0 || continuationColumn != firstContentColumn {
		t.Fatalf("continuation column = %d, content column = %d: %q", continuationColumn, firstContentColumn, lines)
	}
	if strings.Contains(lines[1], "+") || strings.Contains(lines[1], "123") {
		t.Fatalf("continuation repeated the gutter: %q", lines)
	}
}

func TestFileChangeStylesOnlyDiffSignsAndDeletedContent(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.useStyle = true
	model.applyTerminalTheme(true)

	deleted := strings.Join(model.renderFileDiffLine(agent.FileDiffLine{
		Kind: "delete", OldLine: 7, Text: "old text",
	}, 1), "\n")
	if !strings.Contains(deleted, model.mutedStyle.Render("7")) ||
		!strings.Contains(deleted, model.errorStyle.Render("−")) ||
		!strings.Contains(deleted, model.mutedStyle.Render("old text")) {
		t.Fatalf("deleted line styles = %q", deleted)
	}

	added := strings.Join(model.renderFileDiffLine(agent.FileDiffLine{
		Kind: "add", NewLine: 7, Text: "new text",
	}, 1), "\n")
	if !strings.Contains(added, model.mutedStyle.Render("7")) ||
		!strings.Contains(added, model.successStyle.Render("+")) ||
		strings.Contains(added, model.successStyle.Render("new text")) {
		t.Fatalf("added line styles = %q", added)
	}
}

func TestCompletedToolShowsElapsedTime(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.addToolCallAt(agent.ToolCall{ID: "call-1", Name: "read", RawArguments: `{"path":"main.go"}`}, time.Now().Add(-1500*time.Millisecond))
	model.finishTool(agent.ToolResult{CallID: "call-1"})

	block := model.transcript.blocks[len(model.transcript.blocks)-1]
	if block.tool == nil || !block.tool.done || block.tool.elapsed < time.Second {
		t.Fatalf("tool timing = %#v", block)
	}
	if rendered := strings.Join(model.renderBlockLines(block), "\n"); !strings.Contains(rendered, "1.5s") {
		t.Fatalf("rendered tool timing = %q", rendered)
	}
}

func TestTurnCompletionAddsChangeSummaryAndDuration(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	finishedAt := time.Now()
	model.operation = activeOperation{
		kind: operationTurn, startedAt: finishedAt.Add(-3 * time.Second),
		changedPaths: []string{"a.go", "internal/b.go", "internal/c.go"},
	}
	model.finishTurnChanges()
	model.finishTurnDuration(finishedAt)

	if got := model.transcript.blocks[len(model.transcript.blocks)-2].text; got != "changed 3 files · a.go · internal/ → b.go, c.go" {
		t.Fatalf("change summary = %q", got)
	}
	last := model.transcript.blocks[len(model.transcript.blocks)-1]
	if last.kind != screenBlockDuration || !strings.Contains(model.renderDurationLine(last.duration), "Worked for 3s") {
		t.Fatalf("duration block = %#v", last)
	}
}
