package ui

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/levmv/skot/agent"
)

func renderPlainMarkdown(source string, options markdownOptions) string {
	renderer := newMarkdownRenderer(true, terminalPaletteFor(false))
	return ansi.Strip(strings.Join(renderer.render(source, options), "\n"))
}

func TestMarkdownRendersModelOutput(t *testing.T) {
	for _, test := range []struct {
		name, source, want string
		width              int
	}{
		{"emphasis", "*Word* and _word_, **bold**, __strong__, ***both***, ~~gone~~", "Word and word, bold, strong, both, gone", 0},
		{"literal delimiters", "snake_case_name, 2 * 3 * 4, *.go, ~5 min", "snake_case_name, 2 * 3 * 4, *.go, ~5 min", 0},
		{"code spans", "`a*b*c` and ``x ` y``", "a*b*c and x ` y", 0},
		{"escapes and references", `\*not emphasis\* &amp; &rarr;`, "*not emphasis* & →", 0},
		{"unclosed markers", "**not closed and `tick", "**not closed and `tick", 0},
		{
			"links",
			"[docs](https://example.com/docs), [https://go.dev](https://go.dev), <https://a.dev>, https://b.dev/x_(y).",
			"docs (https://example.com/docs), https://go.dev, https://a.dev, https://b.dev/x_(y).",
			0,
		},
		{"url in link text", "[see https://a.dev/x](https://a.dev/y)", "see https://a.dev/x (https://a.dev/y)", 0},
		{"headings", "# One\n### Three ###", "One\nThree", 0},
		{
			"lists",
			"- one\n  - nested\n- [x] done\n- [ ] todo\n\n3. three\n4. four",
			"• one\n  ◦ nested\n☑ done\n☐ todo\n\n3. three\n4. four",
			0,
		},
		{"fence in list item", "1. Run:\n   ```sh\n   make check\n   ```\n2. Done", "1. Run:\n   make check\n2. Done", 0},
		{"quote", "> quoted **text**\n> - item\n\nafter", "│ quoted text\n│ • item\n\nafter", 0},
		{"quote paragraphs", "> one\n>\n> two", "│ one\n│\n│ two", 0},
		{"tabs", "name\t`a\tb`\n```\n\tindented\n```", "name    a    b\n    indented", 0},
		{"rule", "above\n\n---\n\nbelow", "above\n\n" + strings.Repeat("─", 20) + "\n\nbelow", 20},
		{"line breaks", "first\nsecond <br> third<br>fourth", "first\nsecond\nthird\nfourth", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := renderPlainMarkdown(test.source, markdownOptions{width: test.width}); got != test.want {
				t.Fatalf("rendered:\n%s\nwant:\n%s", got, test.want)
			}
		})
	}
}

func TestMarkdownStylesSpans(t *testing.T) {
	renderer := newMarkdownRenderer(true, terminalPaletteFor(false))
	got := strings.Join(renderer.render("## Title\n**bold** *it* `code` [docs](https://example.com)", markdownOptions{width: 80}), "\n")
	for _, want := range []string{
		renderer.paint("Title", mdAccent|mdBold),
		renderer.paint("bold", mdBold),
		renderer.paint("it", mdItalic),
		renderer.paint("code", mdCode),
		renderer.paintLink("docs", mdLink, "https://example.com"),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered markdown missed %q: %q", want, got)
		}
	}
}

func TestMarkdownWrapsUnderListAndQuoteMarkers(t *testing.T) {
	got := renderPlainMarkdown("- alpha bravo charlie delta\n> echo foxtrot golf hotel", markdownOptions{width: 16})
	if want := "• alpha bravo\n  charlie delta\n│ echo foxtrot\n│ golf hotel"; got != want {
		t.Fatalf("wrapped:\n%s\nwant:\n%s", got, want)
	}
}

func TestMarkdownTableFitsAndReflows(t *testing.T) {
	renderer := newMarkdownRenderer(false, terminalPaletteFor(false))
	markdown := "| Name | Description |\n| --- | --- |\n| alpha | a deliberately long description that must wrap |"
	wide := renderer.render(markdown, markdownOptions{width: 80})
	narrow := renderer.render(markdown, markdownOptions{width: 32})
	if len(narrow) <= len(wide) {
		t.Fatalf("narrow table did not reflow: wide=%q narrow=%q", wide, narrow)
	}
	for _, line := range narrow {
		if visibleLen(line) > 32 {
			t.Fatalf("line width = %d: %q", visibleLen(line), line)
		}
	}
}

func TestMarkdownTableAlignsCells(t *testing.T) {
	got := renderPlainMarkdown("| Name | Count | Note |\n| :--- | ---: | :---: |\n| `a|b` | 7 | ok |", markdownOptions{width: 80})
	want := "┌──────┬───────┬──────┐\n" +
		"│ Name │ Count │ Note │\n" +
		"├──────┼───────┼──────┤\n" +
		"│ a|b  │     7 │  ok  │\n" +
		"└──────┴───────┴──────┘"
	if got != want {
		t.Fatalf("table:\n%s\nwant:\n%s", got, want)
	}
}

func TestMarkdownRemovesFenceLanguage(t *testing.T) {
	renderer := newMarkdownRenderer(false, terminalPaletteFor(false))
	got := strings.Join(renderer.render("```json\n{\"count\": 2}\n```", markdownOptions{width: 80}), "\n")
	if got != `{"count": 2}` {
		t.Fatalf("code block = %q", got)
	}
}

func TestMarkdownStreamingNeverFlashesSyntax(t *testing.T) {
	const answer = "## Plan\n\n" +
		"We fix **three** issues and one *possible* one in `markdown.go`; snake_case and *.go stay literal.\n\n" +
		"1. **Leak** in `run`:\n   ```go\n   defer close(done)\n   ```\n" +
		"2. ~~Old~~ new behaviour, see [docs](https://example.com/docs) and <https://go.dev>.\n\n" +
		"- [x] done\n- nested:\n  - item &amp; more\n\n" +
		"> Quoted *text*\n\n---\n\n" +
		"Status [WIP]: arr[0] is **_both_**, see ![logo](https://example.com/logo.png).\n\n" +
		"Escaped \\*stars\\*. Bare https://example.com/x."
	plain := func(source string, streaming bool) []string {
		return strings.Split(renderPlainMarkdown(source, markdownOptions{streaming: streaming}), "\n")
	}
	final := plain(answer, false)
	for end := range len(answer) + 1 {
		if end < len(answer) && !utf8.RuneStart(answer[end]) {
			continue
		}
		// Every frame must read as the start of the final answer: syntax can
		// be held back, but never shown and then taken away.
		frame := plain(answer[:end], true)
		ok := len(frame) <= len(final)
		for index := 0; ok && index < len(frame); index++ {
			if index == len(frame)-1 {
				ok = strings.HasPrefix(final[index], frame[index])
			} else {
				ok = frame[index] == final[index]
			}
		}
		if !ok {
			t.Fatalf("frame for %q:\n%s\nis not a prefix of:\n%s", answer[:end], strings.Join(frame, "\n"), strings.Join(final, "\n"))
		}
	}
}

func TestMarkdownStreamingKeepsFinishedLines(t *testing.T) {
	// Spans left open on a finished line are literal; only the line still
	// being written renders them as if closed.
	got := renderPlainMarkdown("A lone ` and *star\nnext **line", markdownOptions{streaming: true})
	if want := "A lone ` and *star\nnext line"; got != want {
		t.Fatalf("streamed:\n%s\nwant:\n%s", got, want)
	}
}

func TestMarkdownStreamingPreservesTextBeforePendingSyntax(t *testing.T) {
	for _, pending := range []string{"<https://example", "<br", "<br /", "&am", "\\"} {
		t.Run(pending, func(t *testing.T) {
			got := renderPlainMarkdown("See "+pending, markdownOptions{streaming: true})
			if got != "See " {
				t.Fatalf("pending syntax changed visible text: %q", got)
			}
		})
	}
}

func TestMarkdownContinuationPreservesInlineText(t *testing.T) {
	for _, source := range []string{
		"Use *.go and snake_case.",
		"First &amp; second &#x2192; third.",
		"alpha\tbeta\tgamma",
		"Use `alpha\tbeta` here.",
		"See <https://example.com>.",
		"See [docs](https://example.com).",
		"Status [WIP] and arr[0].",
		"See ![logo](https://example.com/logo.png).",
		"Use **_both_** here.",
		"Пример: \\*текст\\* и **код**.",
	} {
		t.Run(source, func(t *testing.T) {
			want := renderPlainMarkdown(source, markdownOptions{})
			for end := range len(source) + 1 {
				if end < len(source) && !utf8.RuneStart(source[end]) {
					continue
				}
				prefix := source[:end]
				printed := renderPlainMarkdown(prefix, markdownOptions{streaming: true})
				continued := renderPlainMarkdown(source, markdownOptions{from: markdownShown(prefix)})
				if got := printed + continued; got != want {
					t.Fatalf("continuation after %q:\ngot:  %q\nwant: %q", prefix, got, want)
				}
			}
		})
	}
}

func TestMarkdownStreamingShowsTableRowsWhole(t *testing.T) {
	const table = "Results:\n\n| Key | Value |\n| --- | --- |\n| alpha | one |\n| beta | two |\n"
	for end := range len(table) + 1 {
		frame := renderPlainMarkdown(table[:end], markdownOptions{streaming: true})
		if strings.ContainsAny(frame, "|-") {
			t.Fatalf("frame for %q shows raw table syntax:\n%s", table[:end], frame)
		}
		if strings.Contains(frame, "alph") != strings.Contains(frame, "alpha │ one") ||
			strings.Contains(frame, "bet") != strings.Contains(frame, "beta  │ two") {
			t.Fatalf("frame for %q shows a partial row:\n%s", table[:end], frame)
		}
	}
	if got := renderPlainMarkdown(table, markdownOptions{}); !strings.Contains(got, "│ beta  │ two   │") {
		t.Fatalf("final table:\n%s", got)
	}
}

func TestMarkdownResumesInsideSpans(t *testing.T) {
	renderer := newMarkdownRenderer(true, terminalPaletteFor(false))
	source := "Intro line\n- item with **bold text** after\n"
	lines := renderer.render(source, markdownOptions{from: strings.Index(source, "ld text")})
	got := strings.Join(lines, "\n")
	if plain := ansi.Strip(got); plain != "  ld text after" {
		t.Fatalf("continuation = %q", plain)
	}
	if !strings.Contains(got, renderer.paint("ld text", mdBold)) {
		t.Fatalf("continuation lost emphasis context: %q", got)
	}
}

func TestAssistantBlockUsesMarkdownRenderer(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.markdown.useStyle = true
	lines := strings.Join(model.renderAssistantBlock("## Title\n**bold**"), "\n")
	if strings.Contains(lines, "## ") || strings.Contains(lines, "**") {
		t.Fatalf("assistant markdown was not rendered: %q", lines)
	}
	if !strings.Contains(lines, "•") || !strings.Contains(lines, "Title") {
		t.Fatalf("assistant block = %q", lines)
	}
}

func TestInterruptedTurnShowsHeldBackMarkdown(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.applyAgentEvent(agent.Event{Kind: agent.EventTextDelta, AttemptID: "a1", Text: "Summary:\n| Key | Value |"})
	model.refreshTranscript()
	if transcript := strings.Join(model.transcript.lines, "\n"); !strings.Contains(transcript, "Summary:") || strings.Contains(transcript, "Key") {
		t.Fatalf("streaming showed a possible table header as text:\n%s", transcript)
	}
	model, _ = model.update(agentDoneMsg{err: context.Canceled})
	if transcript := strings.Join(model.transcript.lines, "\n"); !strings.Contains(transcript, "| Key | Value |") {
		t.Fatalf("interrupted turn hid streamed text:\n%s", transcript)
	}
}
