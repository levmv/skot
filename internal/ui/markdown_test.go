package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestMarkdownTableFitsAndReflows(t *testing.T) {
	renderer := newMarkdownRenderer(false, terminalPaletteFor(false))
	markdown := "| Name | Description |\n| --- | --- |\n| alpha | a deliberately long description that must wrap |"
	wide := renderer.renderMarkdownLines(markdown, 80)
	narrow := renderer.renderMarkdownLines(markdown, 32)
	if len(narrow) <= len(wide) {
		t.Fatalf("narrow table did not reflow: wide=%q narrow=%q", wide, narrow)
	}
	for _, line := range narrow {
		if visibleLen(line) > 32 {
			t.Fatalf("line width = %d: %q", visibleLen(line), line)
		}
	}
}

func TestMarkdownStylesHeadingsBoldAndInlineCode(t *testing.T) {
	renderer := newMarkdownRenderer(true, terminalPaletteFor(false))
	lines := renderer.renderMarkdownLines("## Title\nhello **world** and `README.md`", 80)
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		renderer.accentStyle.Bold(true).Render("Title"),
		lipgloss.NewStyle().Bold(true).Render("world"),
		renderer.codeStyle.Render("README.md"),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered markdown missed %q: %q", want, got)
		}
	}
	if strings.Contains(got, "## ") || strings.Contains(got, "**") || strings.Contains(got, "`") {
		t.Fatalf("markdown markers survived: %q", got)
	}
}

func TestMarkdownRemovesFenceLanguage(t *testing.T) {
	renderer := newMarkdownRenderer(false, terminalPaletteFor(false))
	got := strings.Join(renderer.renderMarkdownLines("```json\n{\"count\": 2}\n```", 80), "\n")
	if got != `{"count": 2}` {
		t.Fatalf("code block = %q", got)
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
