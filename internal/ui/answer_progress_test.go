package ui

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/model"
)

func TestAnswerProgressPreservesTextAndFinishesInPlace(t *testing.T) {
	terminal := newProgressTestTerminal(60, 12, "$ sk fix the tests")
	now := time.Now()
	progress := newAnswerProgress(terminal, terminal.size, func() time.Time { return now }, "/work")

	progress.Emit(agent.Event{Kind: agent.EventTextDelta, AttemptID: "a1", Text: "Let me run the tests first."})
	mustRedrawProgress(t, progress)
	call := model.ToolCall{ID: "c1", Name: "bash", RawArguments: `{"command":"go test ./..."}`}
	progress.Emit(agent.Event{Kind: agent.EventToolStarted, Call: &call})
	mustRedrawProgress(t, progress)
	if screen := terminal.text(); !strings.Contains(screen, "Let me run the tests first.") || !strings.Contains(screen, "go test ./...") {
		t.Fatalf("progress did not show intermediate text and activity:\n%s", screen)
	}

	change, err := agent.NewDetail(agent.FileChangeDetailKind, agent.FileChange{Path: "main.go", Operation: "edited", Additions: 2})
	if err != nil {
		t.Fatal(err)
	}
	progress.Emit(agent.Event{Kind: agent.EventToolFinished, Call: &call, Result: &model.ToolResult{CallID: "c1", Details: []model.Detail{change}}})
	// A fast local call can start and finish between frames. Keep it visible
	// while the next model attempt has only emitted leading whitespace.
	read := model.ToolCall{ID: "c2", Name: "read", RawArguments: `{"path":"main.go"}`}
	progress.Emit(agent.Event{Kind: agent.EventToolStarted, Call: &read})
	progress.Emit(agent.Event{Kind: agent.EventToolFinished, Result: &model.ToolResult{CallID: read.ID}})
	progress.Emit(agent.Event{Kind: agent.EventModelAttemptStarted, AttemptID: "a2"})
	progress.Emit(agent.Event{Kind: agent.EventTextDelta, AttemptID: "a2", Text: "\n"})
	mustRedrawProgress(t, progress)
	if got := terminal.text(); !strings.Contains(got, "edited main.go") || !strings.Contains(got, "read  main.go") || strings.Contains(got, "Used ") {
		t.Fatalf("completed tools disappeared before the model continued:\n%s", got)
	}
	answer := "## Done\nAll **tests** pass."
	progress.Emit(agent.Event{Kind: agent.EventTextDelta, AttemptID: "a2", Text: strings.TrimSuffix(answer, " pass.")})
	mustRedrawProgress(t, progress)
	if got := terminal.text(); !strings.Contains(got, "Used 2 tools") || !strings.Contains(got, "All tests") || strings.Contains(got, "go test ./...") {
		t.Fatalf("streaming answer did not settle the tool group:\n%s", got)
	}
	// The accepted result can supply a suffix absent from the stream.
	if err := progress.Finish(answer); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"Let me run the tests first.", "Used 2 tools · changed 1 file", "Done", "All tests pass."} {
		if got := terminal.text(); strings.Count(got, want) != 1 {
			t.Fatalf("expected exactly one %q after finish:\n%s", want, got)
		}
	}
	if got := terminal.text(); strings.Contains(got, "Working") || strings.Contains(got, "go test ./...") || strings.Contains(got, "for more detail") {
		t.Fatalf("temporary activity remained after finish:\n%s", got)
	}
	if !terminal.cursorVisible {
		t.Fatal("cursor stayed hidden after finish")
	}
}

func TestAnswerProgressStreamsLongTextIntoStableScrollback(t *testing.T) {
	// Start at the bottom row, as after Enter in a full shell terminal.
	history := strings.Repeat("earlier output\n", 6) + "$ sk explain"
	terminal := newProgressTestTerminal(30, 8, history)
	progress := newAnswerProgress(terminal, terminal.size, time.Now, "/work")
	var answer strings.Builder
	var previous []string
	for line := range 40 {
		chunk := "line " + strconv.Itoa(line) + "\n"
		answer.WriteString(chunk)
		progress.Emit(agent.Event{Kind: agent.EventTextDelta, AttemptID: "a1", Text: chunk})
		mustRedrawProgress(t, progress)
		if len(terminal.scrollback) < len(previous) || strings.Join(terminal.scrollback[:len(previous)], "\n") != strings.Join(previous, "\n") {
			t.Fatalf("streaming changed existing scrollback: %q", terminal.scrollback)
		}
		if strings.Contains(strings.Join(terminal.scrollback, "\n"), "Working") {
			t.Fatalf("temporary status escaped into scrollback: %q", terminal.scrollback)
		}
		previous = append([]string(nil), terminal.scrollback...)
	}
	if len(terminal.scrollback) == 0 || !strings.Contains(strings.Join(terminal.scrollback, "\n"), "line 0") {
		t.Fatal("the beginning of the answer was not available in scrollback before completion")
	}
	if err := progress.Finish(answer.String()); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(terminal.text(), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "•"))
		if line != "" {
			lines = append(lines, line)
		}
	}
	if got, want := strings.Join(lines, "\n"), history+"\n"+strings.TrimSuffix(answer.String(), "\n"); got != want {
		t.Fatalf("streaming lost, reordered or duplicated output:\ngot: %s\nwant: %s", got, want)
	}
	if strings.Contains(terminal.writes.String(), "\x1b[2J") || strings.Contains(terminal.writes.String(), "\x1b[3J") {
		t.Fatal("one-shot output cleared the terminal or scrollback")
	}
	for _, mode := range []string{ansi.RequestKittyKeyboard, ansi.SetModifyOtherKeys2, ansi.SetModeBracketedPaste, ansi.ResetModifyOtherKeys, ansi.ResetModeBracketedPaste} {
		if strings.Contains(terminal.writes.String(), mode) {
			t.Fatalf("output-only run changed input modes or queried the terminal: %q", mode)
		}
	}
}

func TestAnswerProgressInterruptedBeforeFirstText(t *testing.T) {
	terminal := newProgressTestTerminal(80, 12, "$ sk review")
	progress := newAnswerProgress(terminal, terminal.size, time.Now, "/work")
	mustRedrawProgress(t, progress)
	if !strings.Contains(terminal.text(), "Working") {
		t.Fatal("missing initial status")
	}
	if err := progress.Finish(""); err != nil {
		t.Fatal(err)
	}
	if got := terminal.text(); got != "$ sk review" || !terminal.cursorVisible {
		t.Fatalf("interrupted run left transient output or hid the cursor: %q", got)
	}
}

func TestAnswerProgressWrapsWithoutLosingText(t *testing.T) {
	const answer = "alpha bravo charlie delta echo foxtrot golf hotel india"
	// Exercise a normal pane, a narrow pane that still fits words, and one
	// too narrow for either the gutter or an intact word.
	for _, width := range []int{24, 12, 1} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			terminal := newProgressTestTerminal(width, 12, "")
			progress := newAnswerProgress(terminal, terminal.size, time.Now, "/work")
			for _, word := range strings.Fields(answer) {
				progress.Emit(agent.Event{Kind: agent.EventTextDelta, AttemptID: "a1", Text: word + " "})
				mustRedrawProgress(t, progress)
			}
			if err := progress.Finish(answer); err != nil {
				t.Fatal(err)
			}
			var words []string
			for _, line := range strings.Split(terminal.text(), "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				if width > 1 && ((!strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "• ")) || visibleLen(line) >= width) {
					t.Fatalf("missing output margin: %q", line)
				}
				words = append(words, strings.Fields(strings.TrimPrefix(line, "•"))...)
			}
			got, want := strings.Join(words, " "), answer
			if width == 1 {
				got, want = strings.Join(words, ""), strings.ReplaceAll(answer, " ", "")
			}
			if got != want {
				t.Fatalf("wrapped output = %q, want %q", got, want)
			}
		})
	}
}

func TestAnswerProgressResizePreservesMarkdownContext(t *testing.T) {
	const code = "```python\nif True:\n    # important comment\n    pattern = '**'\n```\n## Done"
	for _, test := range []struct{ name, prefix, want string }{
		{"opening fence", "```py", "if True:\n    # important comment\n    pattern = '**'\nDone"},
		{"inside code", "```python\nif True:\n", "if True:\n    # important comment\n    pattern = '**'\nDone"},
		{"partial code line", "```python\nif True:\n    # important comment\n    pattern = ", "if True:\n    # important comment\n    pattern =\n '**'\nDone"},
	} {
		t.Run(test.name, func(t *testing.T) {
			terminal := newProgressTestTerminal(80, 12, "")
			progress := newAnswerProgress(terminal, terminal.size, time.Now, "/work")
			progress.Emit(agent.Event{Kind: agent.EventTextDelta, AttemptID: "a1", Text: test.prefix})
			mustRedrawProgress(t, progress)
			terminal.resize(70, 12, true)
			mustRedrawProgress(t, progress)
			progress.Emit(agent.Event{Kind: agent.EventTextDelta, AttemptID: "a1", Text: code[len(test.prefix):]})
			if err := progress.Finish(code); err != nil {
				t.Fatal(err)
			}
			var lines []string
			for _, line := range strings.Split(terminal.text(), "\n") {
				// Remove the UI gutter and block padding, retaining code indentation.
				if strings.HasPrefix(line, "• ") {
					line = strings.TrimPrefix(line, "• ")
				} else {
					line = strings.TrimPrefix(line, "  ")
				}
				if line != "" {
					lines = append(lines, line)
				}
			}
			if got := strings.Join(lines, "\n"); got != test.want {
				t.Fatalf("resize changed code or replayed text:\ngot: %q\nwant: %q", got, test.want)
			}
		})
	}
}

func TestAnswerProgressResizeKeepsHeldBackMarkdown(t *testing.T) {
	const answer = "Results:\n| Key | Value |\n| --- | --- |\n| alpha | one |\n"
	terminal := newProgressTestTerminal(60, 12, "")
	progress := newAnswerProgress(terminal, terminal.size, time.Now, "/work")
	// The header line is held back until its delimiter row arrives.
	progress.Emit(agent.Event{Kind: agent.EventTextDelta, AttemptID: "a1", Text: "Results:\n| Key | Value |\n"})
	mustRedrawProgress(t, progress)
	terminal.resize(50, 12, true)
	mustRedrawProgress(t, progress)
	progress.Emit(agent.Event{Kind: agent.EventTextDelta, AttemptID: "a1", Text: "| --- | --- |\n| alpha | one |\n"})
	if err := progress.Finish(answer); err != nil {
		t.Fatal(err)
	}
	got := terminal.text()
	if strings.Count(got, "Results:") != 1 || strings.Count(got, "Key") != 1 || !strings.Contains(got, "│ alpha │ one   │") || strings.Contains(got, "|") {
		t.Fatalf("resize lost or replayed held-back Markdown:\n%s", got)
	}
}

func TestAnswerProgressResizePreservesInlineText(t *testing.T) {
	for _, test := range []struct{ name, prefix, answer string }{
		{"glob", "Use *", "Use *.go"},
		{"reference", "First &amp; sec", "First &amp; second"},
		{"pending reference", "First &am", "First &amp; second"},
		{"tab", "alpha\tbe", "alpha\tbeta"},
		{"inline code tab", "Use `alpha\tbe", "Use `alpha\tbeta`"},
		{"fenced code tab", "```\nalpha\tbe", "```\nalpha\tbeta\n```"},
	} {
		t.Run(test.name, func(t *testing.T) {
			terminal := newProgressTestTerminal(80, 12, "")
			progress := newAnswerProgress(terminal, terminal.size, time.Now, "/work")
			progress.Emit(agent.Event{Kind: agent.EventTextDelta, AttemptID: "a1", Text: test.prefix})
			mustRedrawProgress(t, progress)
			terminal.resize(70, 12, true)
			mustRedrawProgress(t, progress)
			progress.Emit(agent.Event{Kind: agent.EventTextDelta, AttemptID: "a1", Text: test.answer[len(test.prefix):]})
			if err := progress.Finish(test.answer); err != nil {
				t.Fatal(err)
			}
			// A committed partial line resumes on a new terminal row. Ignore
			// that wrap point and the gutter, but retain every text character.
			got := strings.Join(strings.Fields(strings.ReplaceAll(terminal.text(), "•", "")), "")
			want := strings.Join(strings.Fields(renderPlainMarkdown(test.answer, markdownOptions{})), "")
			if got != want {
				t.Fatalf("resize lost or replayed text:\ngot:  %q\nwant: %q", got, want)
			}
		})
	}
}

func TestAnswerProgressPreservesTableCellsInNarrowPanes(t *testing.T) {
	const table = "| Aaa | Bbb | Ccc | Ddd |\n| --- | --- | --- | --- |\n| one | two | six | last |\n"
	for _, width := range []int{60, 24, 12, 1} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			terminal := newProgressTestTerminal(width, 20, "")
			progress := newAnswerProgress(terminal, terminal.size, time.Now, "/work")
			if err := progress.Finish(table); err != nil {
				t.Fatal(err)
			}
			// Ignore borders, gutter and wrap points while preserving the
			// letters of every cell in reading order.
			got := strings.Map(func(char rune) rune {
				if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' {
					return char
				}
				return -1
			}, terminal.text())
			if want := "AaaBbbCccDddonetwosixlast"; got != want {
				t.Fatalf("table lost or reordered cells:\ngot:  %q\nwant: %q\n%s", got, want, terminal.text())
			}
		})
	}
}

func TestAnswerProgressKeepsRejectedToolVisible(t *testing.T) {
	terminal := newProgressTestTerminal(90, 20, "$ sk review")
	progress := newAnswerProgress(terminal, terminal.size, time.Now, "/work")
	call := model.ToolCall{ID: "rejected", Name: "bash", RawArguments: `{"command":"cat private.txt"}`}
	progress.Emit(agent.Event{Kind: agent.EventToolRejected, Call: &call, Result: &model.ToolResult{
		CallID: call.ID, Error: true, Content: model.TextContent("permission denied"),
	}})
	progress.Emit(agent.Event{Kind: agent.EventTextDelta, AttemptID: "next", Text: "I will use the other files."})
	mustRedrawProgress(t, progress)
	for _, want := range []string{"Used 1 tool", "private.txt", "permission denied"} {
		if screen := terminal.text(); !strings.Contains(screen, want) {
			t.Fatalf("rejected tool was hidden: %s\n%s", want, screen)
		}
	}
	if err := progress.Finish(""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(terminal.text(), "Used 1 tool") {
		t.Fatal("rejected call was absent from the final tool count")
	}
}

func TestAnswerProgressFoldsLongToolGroupToFit(t *testing.T) {
	terminal := newProgressTestTerminal(70, 9, "$ sk review")
	progress := newAnswerProgress(terminal, terminal.size, time.Now, "/work")
	for index := range 8 {
		name := "tool-" + strconv.Itoa(index)
		call := model.ToolCall{ID: name, Name: "bash", RawArguments: `{"command":"` + name + `"}`}
		progress.Emit(agent.Event{Kind: agent.EventToolStarted, Call: &call})
		progress.Emit(agent.Event{Kind: agent.EventToolFinished, Call: &call, Result: &model.ToolResult{CallID: call.ID}})
		mustRedrawProgress(t, progress)
	}
	if screen := terminal.text(); !strings.Contains(screen, "Used ") || !strings.Contains(screen, "tool-7") {
		t.Fatalf("long group lost its summary or newest call:\n%s", screen)
	}
	for _, line := range terminal.scrollback {
		if strings.Contains(line, "tool-") || strings.Contains(line, "Used ") {
			t.Fatalf("mutable tool rows escaped into scrollback: %q", terminal.scrollback)
		}
	}
	if err := progress.Finish("Review complete."); err != nil {
		t.Fatal(err)
	}
	if screen := terminal.text(); strings.Contains(screen, "tool-") || !strings.Contains(screen, "Used 8 tools") || strings.Count(screen, "Review complete.") != 1 {
		t.Fatalf("completed run left transient calls behind:\n%s", screen)
	}
}

func TestAnswerProgressResizePreservesHistoryAndPendingText(t *testing.T) {
	for _, reflow := range []bool{false, true} {
		t.Run("reflow="+strconv.FormatBool(reflow), func(t *testing.T) {
			terminal := newProgressTestTerminal(64, 12, "$ sk explain")
			progress := newAnswerProgress(terminal, terminal.size, time.Now, "/work")
			var answer strings.Builder
			emit := func(index int) {
				chunk := "record-" + strconv.Itoa(index) + ": alpha bravo charlie delta echo foxtrot\n"
				if !reflow {
					// A terminal without reflow clips wide lines itself; keep old
					// rows short enough to distinguish that from lost app output.
					chunk = "record-" + strconv.Itoa(index) + ": data\n"
				}
				answer.WriteString(chunk)
				progress.Emit(agent.Event{Kind: agent.EventTextDelta, AttemptID: "a1", Text: chunk})
			}
			for index := range 20 {
				emit(index)
				mustRedrawProgress(t, progress)
			}
			// Resizing alone must not print another copy, even on repeated ticks.
			for _, size := range [][2]int{{25, 9}, {80, 16}} {
				terminal.resize(size[0], size[1], reflow)
				before := terminal.writes.Len()
				mustRedrawProgress(t, progress)
				mustRedrawProgress(t, progress)
				if strings.Contains(terminal.writes.String()[before:], "record-") {
					t.Fatal("resize replayed already displayed source")
				}
			}
			// Events can arrive after the last frame but before a resize tick.
			emit(20)
			terminal.resize(26, 10, reflow)
			mustRedrawProgress(t, progress)
			for index := 21; index < 25; index++ {
				emit(index)
				mustRedrawProgress(t, progress)
			}
			// Finish must handle a resize even without another progress tick.
			emit(25)
			terminal.resize(40, 8, reflow)
			if err := progress.Finish(answer.String()); err != nil {
				t.Fatal(err)
			}
			got := terminal.text()
			// Native reflow may split words. Compare all content in order,
			// ignoring only wrapping whitespace and the assistant gutter.
			content := strings.Join(strings.Fields(strings.ReplaceAll(got, "•", "")), "")
			want := strings.Join(strings.Fields("$ sk explain\n"+answer.String()), "")
			if content != want {
				t.Fatalf("resize lost, reordered or duplicated text:\ngot: %s\nwant: %s", content, want)
			}
			if strings.Count(got, "$ sk explain") != 1 || strings.Contains(got, "Working") || !terminal.cursorVisible {
				t.Fatalf("resize damaged shell history or left transient state:\n%s", got)
			}
			if strings.Contains(terminal.writes.String(), "\x1b[2J") || strings.Contains(terminal.writes.String(), "\x1b[3J") {
				t.Fatal("resize cleared the terminal or scrollback")
			}
		})
	}
}

func TestAnswerProgressFinishPreservesHistoryAndShowsChangedAnswer(t *testing.T) {
	terminal := newProgressTestTerminal(40, 6, "$ sk explain")
	progress := newAnswerProgress(terminal, terminal.size, time.Now, "/work")
	for index := range 20 {
		chunk := "section-" + strconv.Itoa(index) + "\n"
		progress.Emit(agent.Event{Kind: agent.EventTextDelta, AttemptID: "a1", Text: chunk})
		mustRedrawProgress(t, progress)
	}
	// A completed response can add content missing from all streamed deltas.
	final := "Do not delete this file."
	before := terminal.text()
	if err := progress.Finish(final); err != nil {
		t.Fatal(err)
	}
	got := terminal.text()
	printed := strings.TrimRight(strings.Split(before, "  Working")[0], "\n ")
	if !strings.HasPrefix(got, printed) || strings.Count(got, final) != 1 || !strings.Contains(got, "Final answer:") || strings.Contains(got, "Working") {
		t.Fatalf("finish lost printed history or the accepted answer:\nbefore: %s\nafter: %s", before, got)
	}
}

func TestAnswerProgressFinishReconcilesUnprintedText(t *testing.T) {
	terminal := newProgressTestTerminal(80, 12, "")
	progress := newAnswerProgress(terminal, terminal.size, time.Now, "/work")
	progress.Emit(agent.Event{Kind: agent.EventTextDelta, AttemptID: "a1", Text: "delete this file."})
	if err := progress.Finish("Do not delete this file."); err != nil {
		t.Fatal(err)
	}
	got := terminal.text()
	if !strings.Contains(got, "Do not delete this file.") || strings.Count(got, "delete this file.") != 1 || strings.Contains(got, "Final answer:") {
		t.Fatalf("finish did not reconcile text before its first frame: %s", got)
	}
}

func TestAnswerProgressLateMarkdownDoesNotReplayHistory(t *testing.T) {
	terminal := newProgressTestTerminal(70, 8, "$ sk explain")
	progress := newAnswerProgress(terminal, terminal.size, time.Now, "/work")
	text := "| Key | Value |\n| --- | --- |\n"
	for index := range 15 {
		text += "| entry-" + strconv.Itoa(index) + " | x |\n"
	}
	progress.Emit(agent.Event{Kind: agent.EventTextDelta, AttemptID: "a1", Text: text})
	mustRedrawProgress(t, progress)
	// This changes table column widths in rows already in scrollback.
	tail := "| new-entry | a much longer value than the previous rows |\n"
	progress.Emit(agent.Event{Kind: agent.EventTextDelta, AttemptID: "a1", Text: tail})
	mustRedrawProgress(t, progress)
	if err := progress.Finish(text + tail); err != nil {
		t.Fatal(err)
	}
	got := terminal.text()
	for index := range 15 {
		label := "entry-" + strconv.Itoa(index) + " "
		if strings.Count(got, label) != 1 {
			t.Fatalf("late Markdown lost or duplicated %s:\n%s", label, got)
		}
	}
	if strings.Count(got, "new-entry") != 1 || !strings.Contains(got, "a much longer value than the previous rows") || strings.Contains(got, "Working") {
		t.Fatalf("late Markdown lost new content or retained progress:\n%s", got)
	}
}

func TestAnswerProgressToolsContinueAfterResize(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run("failed="+strconv.FormatBool(failed), func(t *testing.T) {
			terminal := newProgressTestTerminal(80, 10, "$ sk review")
			progress := newAnswerProgress(terminal, terminal.size, time.Now, "/work")
			call := model.ToolCall{ID: "old", Name: "bash", RawArguments: `{"command":"old-command"}`}
			progress.Emit(agent.Event{Kind: agent.EventToolStarted, Call: &call})
			mustRedrawProgress(t, progress)
			terminal.resize(34, 6, true)
			mustRedrawProgress(t, progress)
			progress.Emit(agent.Event{Kind: agent.EventToolFinished, Result: &model.ToolResult{CallID: call.ID, Error: failed, Content: model.TextContent("outcome")}})
			mustRedrawProgress(t, progress)
			want := "bash completed."
			if failed {
				want = "bash failed: outcome"
			}
			if got := terminal.text(); strings.Count(got, "old-command") != 1 || strings.Count(got, want) != 1 {
				t.Fatalf("lost or replayed the tool across resize:\n%s", got)
			}
			call = model.ToolCall{ID: "new", Name: "bash", RawArguments: `{"command":"new-command"}`}
			progress.Emit(agent.Event{Kind: agent.EventToolStarted, Call: &call})
			progress.Emit(agent.Event{Kind: agent.EventToolFinished, Result: &model.ToolResult{CallID: call.ID}})
			mustRedrawProgress(t, progress)
			if !strings.Contains(terminal.text(), "new-command") {
				t.Fatal("new tool was not shown after resize")
			}
			progress.Emit(agent.Event{Kind: agent.EventTextDelta, AttemptID: "next", Text: "Done."})
			if err := progress.Finish("Done."); err != nil {
				t.Fatal(err)
			}
			if got := terminal.text(); !strings.Contains(got, "Used 1 tool") || strings.Contains(got, "new-command") || strings.Contains(got, "Working") {
				t.Fatalf("new tool group did not fold normally after resize:\n%s", got)
			}
		})
	}
}

func mustRedrawProgress(t *testing.T, progress *AnswerProgress) {
	t.Helper()
	if err := progress.redraw(); err != nil {
		t.Fatal(err)
	}
}

// progressTestTerminal models the parts of a terminal the progress region
// relies on: autowrap, scrolling into scrollback, CR, LF with the tty's
// newline translation, cursor up, and erase below. Other escapes are ignored.
type progressTestTerminal struct {
	width, height     int
	scrollback        []string
	scrollbackWrapped []bool
	wrapped           []bool
	screen            [][]rune
	row, col          int
	cursorVisible     bool
	writes            strings.Builder
}

func newProgressTestTerminal(width, height int, prompt string) *progressTestTerminal {
	terminal := &progressTestTerminal{width: width, height: height, screen: make([][]rune, height), wrapped: make([]bool, height), cursorVisible: true}
	_, _ = terminal.Write([]byte(prompt + "\n"))
	return terminal
}

func (t *progressTestTerminal) size() (int, int) { return t.width, t.height }

func (t *progressTestTerminal) Write(data []byte) (int, error) {
	t.writes.Write(data)
	text := []rune(string(data))
	for index := 0; index < len(text); index++ {
		switch r := text[index]; r {
		case '\r':
			t.col = 0
		case '\n':
			t.wrapped[t.row] = false
			t.col = 0
			t.lineFeed()
		case '\x1b':
			index = t.escape(text, index)
		default:
			if t.col >= t.width {
				t.wrapped[t.row] = true
				t.col = 0
				t.lineFeed()
			}
			line := t.screen[t.row]
			for len(line) <= t.col {
				line = append(line, ' ')
			}
			line[t.col] = r
			t.screen[t.row] = line
			t.col++
		}
	}
	return len(data), nil
}

func (t *progressTestTerminal) escape(text []rune, index int) int {
	if index+1 >= len(text) || text[index+1] != '[' {
		return index + 1
	}
	end := index + 2
	for end < len(text) && (text[end] < 0x40 || text[end] > 0x7e) {
		end++
	}
	if end >= len(text) {
		return end
	}
	params := string(text[index+2 : end])
	switch text[end] {
	case 'A':
		count := 1
		if params != "" {
			count, _ = strconv.Atoi(params)
		}
		t.row = max(0, t.row-count)
	case 'B':
		count := 1
		if params != "" {
			count, _ = strconv.Atoi(params)
		}
		t.row = min(t.height-1, t.row+count)
	case 'G':
		column, _ := strconv.Atoi(params)
		t.col = max(0, column-1)
	case 'H':
		t.row, t.col = 0, 0
	case 'K':
		if params == "2" {
			t.screen[t.row] = nil
		} else if params == "" || params == "0" {
			t.screen[t.row] = t.screen[t.row][:min(t.col, len(t.screen[t.row]))]
		}
	case 'J':
		if params == "2" {
			t.screen = make([][]rune, t.height)
		}
		if params == "3" {
			t.scrollback = nil
		}
		if params == "" || params == "0" {
			t.screen[t.row] = t.screen[t.row][:min(t.col, len(t.screen[t.row]))]
			for row := t.row + 1; row < t.height; row++ {
				t.screen[row] = nil
			}
		}
	case 'h', 'l':
		if params == "?25" {
			t.cursorVisible = text[end] == 'h'
		}
	}
	return end
}

func (t *progressTestTerminal) lineFeed() {
	if t.row < t.height-1 {
		t.row++
		return
	}
	t.scrollback = append(t.scrollback, string(t.screen[0]))
	t.scrollbackWrapped = append(t.scrollbackWrapped, t.wrapped[0])
	t.screen = append(t.screen[1:], nil)
	t.wrapped = append(t.wrapped[1:], false)
}

func (t *progressTestTerminal) text() string {
	lines := append([]string(nil), t.scrollback...)
	for _, line := range t.screen {
		lines = append(lines, string(line))
	}
	for index := range lines {
		lines[index] = strings.TrimRight(lines[index], " ")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// resize models both native reflow (joining soft wraps, retaining hard breaks)
// and terminals that clip rows. The cursor follows its character in the line;
// height changes can move earlier rows into or out of scrollback.
func (t *progressTestTerminal) resize(width, height int, reflow bool) {
	oldLines := append([]string(nil), t.scrollback...)
	oldWrapped := append(append([]bool(nil), t.scrollbackWrapped...), t.wrapped...)
	for _, row := range t.screen {
		oldLines = append(oldLines, string(row))
	}
	oldCursor := len(t.scrollback) + t.row
	var lines []string
	var wrapped []bool
	var logical []rune
	cursorRow, cursorCol, cursorOffset := 0, 0, -1
	for index, line := range oldLines {
		if index == oldCursor {
			cursorOffset = len(logical) + t.col
		}
		logical = append(logical, []rune(line)...)
		if reflow && oldWrapped[index] {
			continue
		}
		count := 1
		if reflow {
			count = max(1, (len(logical)+width-1)/width)
		}
		if cursorOffset >= 0 {
			cursorRow = len(lines)
			cursorCol = min(cursorOffset, width-1)
			if reflow {
				cursorRow += cursorOffset / width
				cursorCol = cursorOffset % width
			}
			cursorOffset = -1
		}
		for part := range count {
			start := part * width
			lines = append(lines, string(logical[min(start, len(logical)):min(start+width, len(logical))]))
			wrapped = append(wrapped, part < count-1)
		}
		logical = nil
	}
	for len(lines) > cursorRow+1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
		wrapped = wrapped[:len(wrapped)-1]
	}
	start := min(cursorRow, max(0, len(lines)-height))
	t.scrollback = append([]string(nil), lines[:start]...)
	t.scrollbackWrapped = append([]bool(nil), wrapped[:start]...)
	t.screen, t.wrapped = make([][]rune, height), make([]bool, height)
	for index := start; index < min(len(lines), start+height); index++ {
		t.screen[index-start] = []rune(lines[index])
		t.wrapped[index-start] = wrapped[index]
	}
	t.width, t.height, t.row, t.col = width, height, cursorRow-start, cursorCol
}
