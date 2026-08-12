package ui

import (
	"bytes"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type countingBuffer struct {
	bytes.Buffer
	writes int
}

func TestInlineRendererRequestsBaseLayoutKeys(t *testing.T) {
	var output bytes.Buffer
	renderer := newInlineRenderer(&output)
	if err := renderer.RenderFrame(inlineFrame{dynamic: []string{"draft"}}, 80, 24); err != nil {
		t.Fatal(err)
	}
	flags := ansi.KittyDisambiguateEscapeCodes | ansi.KittyReportAlternateKeys
	if !strings.Contains(output.String(), ansi.KittyKeyboard(flags, 1)) {
		t.Fatalf("renderer startup sequence = %q", output.String())
	}
}

func (b *countingBuffer) Write(value []byte) (int, error) {
	b.writes++
	return b.Buffer.Write(value)
}

func (b *countingBuffer) WriteString(value string) (int, error) {
	b.writes++
	return b.Buffer.WriteString(value)
}

func (b *countingBuffer) reset() {
	b.Buffer.Reset()
	b.writes = 0
}

func TestInlineRendererFirstFrameDoesNotClearExistingTerminalHistory(t *testing.T) {
	var output bytes.Buffer
	renderer := newInlineRenderer(&output)
	view := tea.NewView("session history\n\n›")
	view.Cursor = tea.NewCursor(2, 2)

	if err := renderer.Render(view, 80, 24); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if strings.Contains(got, ansi.EraseEntireScreen) || strings.Contains(got, ansi.EraseEntireDisplay) {
		t.Fatalf("first frame cleared existing terminal history: %q", got)
	}
	if strings.Count(got, "session history") != 1 {
		t.Fatalf("first frame = %q", got)
	}
	for _, forbidden := range []string{"\x1b[?47h", "\x1b[?1047h", "\x1b[?1049h", "\x1b[?1000h", "\x1b[?1002h", "\x1b[?1003h", "\x1b[?1006h"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("first frame enabled alternate screen or mouse reporting %q: %q", forbidden, got)
		}
	}
}

func TestInlineRendererResizePurgesAndReplaysOneSourceBackedFrame(t *testing.T) {
	var output bytes.Buffer
	renderer := newInlineRenderer(&output)
	wide := tea.NewView("resumed session\n\n› hey\n\n• answer")
	wide.Cursor = tea.NewCursor(2, 4)
	if err := renderer.Render(wide, 80, 24); err != nil {
		t.Fatal(err)
	}

	output.Reset()
	narrow := tea.NewView("resumed\nsession\n\n› hey\n\n• answer")
	narrow.Cursor = tea.NewCursor(2, 5)
	if err := renderer.Render(narrow, 40, 24); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, ansi.EraseEntireScreen) || !strings.Contains(got, ansi.EraseEntireDisplay) {
		t.Fatalf("resize did not purge before replay: %q", got)
	}
	if strings.Count(got, "› hey") != 1 || strings.Count(got, "• answer") != 1 {
		t.Fatalf("resize replayed duplicate source blocks: %q", got)
	}
}

func TestInlineRendererUpdatesVisibleSuffixWithoutReplayingPrefix(t *testing.T) {
	var output bytes.Buffer
	renderer := newInlineRenderer(&output)
	first := tea.NewView("unchanged prefix\nold tail\n›")
	first.Cursor = tea.NewCursor(1, 2)
	if err := renderer.Render(first, 80, 24); err != nil {
		t.Fatal(err)
	}

	output.Reset()
	next := tea.NewView("unchanged prefix\nnew tail\n›")
	next.Cursor = tea.NewCursor(1, 2)
	if err := renderer.Render(next, 80, 24); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if strings.Contains(got, "unchanged prefix") || !strings.Contains(got, "new tail") {
		t.Fatalf("differential frame = %q", got)
	}
	if strings.Contains(got, ansi.EraseEntireDisplay) {
		t.Fatalf("visible suffix update purged scrollback: %q", got)
	}
}

func TestInlineRendererCommitsChangedContentAndCursorTogether(t *testing.T) {
	var output countingBuffer
	renderer := newInlineRenderer(&output)
	first := tea.NewView("history\n› draft\n\nfooter")
	first.Cursor = tea.NewCursor(7, 1)
	if err := renderer.Render(first, 80, 24); err != nil {
		t.Fatal(err)
	}

	output.reset()
	next := tea.NewView("history\n› drafts\n\nfooter")
	next.Cursor = tea.NewCursor(8, 1)
	if err := renderer.Render(next, 80, 24); err != nil {
		t.Fatal(err)
	}
	if output.writes != 1 {
		t.Fatalf("changed frame used %d terminal writes, want 1: %q", output.writes, output.String())
	}
	got := output.String()
	cursorPosition := strings.LastIndex(got, ansi.CursorHorizontalAbsolute(9))
	syncEnd := strings.LastIndex(got, ansi.ResetModeSynchronizedOutput)
	if cursorPosition < 0 || syncEnd < 0 || cursorPosition > syncEnd {
		t.Fatalf("cursor position was not committed inside synchronized output: %q", got)
	}
}

func TestInlineRendererReusesUnchangedTranscript(t *testing.T) {
	var output bytes.Buffer
	renderer := newInlineRenderer(&output)
	first := inlineFrame{
		transcript:        []string{"stable history"},
		dynamic:           []string{"draft"},
		transcriptChanged: true,
	}
	if err := renderer.RenderFrame(first, 80, 24); err != nil {
		t.Fatal(err)
	}

	output.Reset()
	next := inlineFrame{
		transcript: []string{"stable history"},
		dynamic:    []string{"updated draft"},
	}
	if err := renderer.RenderFrame(next, 80, 24); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if strings.Contains(got, "stable history") || !strings.Contains(got, "updated draft") {
		t.Fatalf("dynamic update replayed the transcript: %q", got)
	}
}

func TestInlineRendererStartsTranscriptDiffAtDirtyLine(t *testing.T) {
	var output bytes.Buffer
	renderer := newInlineRenderer(&output)
	first := inlineFrame{
		transcript:        []string{"stable prefix", "old tail"},
		dynamic:           []string{"draft"},
		transcriptChanged: true,
	}
	if err := renderer.RenderFrame(first, 80, 24); err != nil {
		t.Fatal(err)
	}

	output.Reset()
	next := inlineFrame{
		transcript:          []string{"stable prefix", "new tail"},
		dynamic:             []string{"draft"},
		transcriptChanged:   true,
		transcriptDirtyFrom: 1,
	}
	if err := renderer.RenderFrame(next, 80, 24); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if strings.Contains(got, "stable prefix") || !strings.Contains(got, "new tail") {
		t.Fatalf("dirty transcript tail replayed its stable prefix: %q", got)
	}
}

func TestInlineRendererPurgesWhenChangedContentIsAboveViewport(t *testing.T) {
	var output bytes.Buffer
	renderer := newInlineRenderer(&output)
	lines := make([]string, 30)
	for index := range lines {
		lines[index] = "line"
	}
	first := tea.NewView(strings.Join(lines, "\n"))
	first.Cursor = tea.NewCursor(0, len(lines)-1)
	if err := renderer.Render(first, 80, 10); err != nil {
		t.Fatal(err)
	}

	output.Reset()
	lines[0] = "changed above viewport"
	next := tea.NewView(strings.Join(lines, "\n"))
	next.Cursor = tea.NewCursor(0, len(lines)-1)
	if err := renderer.Render(next, 80, 10); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), ansi.EraseEntireDisplay) {
		t.Fatalf("off-screen source change was not rebuilt: %q", output.String())
	}
}

func TestInlineRendererInvalidatePurgesBeforeReplacingTranscript(t *testing.T) {
	var output bytes.Buffer
	renderer := newInlineRenderer(&output)
	oldSession := tea.NewView("old session\n\n› old message")
	oldSession.Cursor = tea.NewCursor(0, 2)
	if err := renderer.Render(oldSession, 80, 24); err != nil {
		t.Fatal(err)
	}

	output.Reset()
	renderer.Invalidate()
	newSession := tea.NewView("resumed new session\n\n› hey")
	newSession.Cursor = tea.NewCursor(0, 2)
	if err := renderer.Render(newSession, 80, 24); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, ansi.EraseEntireDisplay) {
		t.Fatalf("invalidated frame did not purge old scrollback: %q", got)
	}
	if strings.Contains(got, "old message") || strings.Count(got, "› hey") != 1 {
		t.Fatalf("invalidated frame mixed transcripts: %q", got)
	}
}

func TestInlineRendererAppendsNewFrameWithoutClearingPreservedTranscript(t *testing.T) {
	var output bytes.Buffer
	renderer := newInlineRenderer(&output)
	oldSession := tea.NewView("old transcript\nold answer\nworking\n› command\n\nfooter")
	oldSession.Cursor = tea.NewCursor(2, 3)
	if err := renderer.Render(oldSession, 80, 24); err != nil {
		t.Fatal(err)
	}

	output.Reset()
	renderer.AppendFrameAfter(2)
	resumed := tea.NewView("resumed session\n\n› hey\n\nfooter")
	resumed.Cursor = tea.NewCursor(2, 2)
	if err := renderer.Render(resumed, 80, 24); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if strings.Contains(got, ansi.EraseEntireScreen) || strings.Contains(got, ansi.EraseEntireDisplay) {
		t.Fatalf("resume cleared terminal history: %q", got)
	}
	if strings.Contains(got, "old transcript") || strings.Contains(got, "old answer") {
		t.Fatalf("resume replayed the preserved prefix: %q", got)
	}
	if strings.Count(got, "resumed session") != 1 || strings.Count(got, "› hey") != 1 {
		t.Fatalf("resume did not append exactly one new frame: %q", got)
	}
	if !strings.Contains(got, ansi.EraseEntireLine) {
		t.Fatalf("resume left the old interactive suffix managed: %q", got)
	}
}

func TestInlineRendererResizeAfterAppendReplaysOnlyCurrentFrame(t *testing.T) {
	var output bytes.Buffer
	renderer := newInlineRenderer(&output)
	oldSession := tea.NewView("old transcript\nold answer\nworking\n›\n\nfooter")
	oldSession.Cursor = tea.NewCursor(1, 3)
	if err := renderer.Render(oldSession, 80, 24); err != nil {
		t.Fatal(err)
	}
	renderer.AppendFrameAfter(2)
	resumed := tea.NewView("resumed session\n\n› hey\n\nwide table")
	resumed.Cursor = tea.NewCursor(2, 2)
	if err := renderer.Render(resumed, 80, 24); err != nil {
		t.Fatal(err)
	}

	output.Reset()
	narrow := tea.NewView("resumed\nsession\n\n› hey\n\nnarrow\ntable")
	narrow.Cursor = tea.NewCursor(2, 3)
	if err := renderer.Render(narrow, 40, 24); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, ansi.EraseEntireDisplay) {
		t.Fatalf("resize did not rebuild the active frame: %q", got)
	}
	if strings.Contains(got, "old transcript") || strings.Contains(got, "old answer") {
		t.Fatalf("resize resurrected the previous session: %q", got)
	}
	if strings.Count(got, "› hey") != 1 {
		t.Fatalf("resize duplicated the active session: %q", got)
	}
}

func TestInlineRendererStopClearsDynamicSuffix(t *testing.T) {
	var output bytes.Buffer
	renderer := newInlineRenderer(&output)
	frame := inlineFrame{
		transcript:        []string{"kept transcript"},
		dynamic:           []string{"working", "› draft", "", "footer"},
		cursor:            tea.NewCursor(2, 2),
		transcriptChanged: true,
	}
	if err := renderer.RenderFrame(frame, 80, 24); err != nil {
		t.Fatal(err)
	}

	output.Reset()
	if err := renderer.Stop(); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if count := strings.Count(got, ansi.EraseEntireLine); count != len(frame.dynamic) {
		t.Fatalf("stop cleared %d lines, want %d: %q", count, len(frame.dynamic), got)
	}
	if strings.Contains(got, ansi.EraseEntireDisplay) || strings.Contains(got, ansi.EraseEntireScreen) {
		t.Fatalf("stop purged transcript or scrollback: %q", got)
	}
}
