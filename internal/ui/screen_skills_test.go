package ui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/skills"
)

func TestSkillReferencesRenderFromCapturedSelections(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("SK_COLOR", "always")
	const quoted = `$"/some dir/other/SKILL.md"`
	text := "проверь по $review, " + quoted + " и $обзор. Затем $unknown; `$review` — пример."
	fake := &fakeAgent{prepareInput: func(text string) (agent.Input, error) {
		return agent.Input{Text: text, Instructions: []agent.Instruction{
			{Key: "/original/review/SKILL.md", Name: "review", Text: "Review the changes."},
			{Key: "/some dir/other/SKILL.md", Name: "other", Text: "Other instructions."},
			{Key: "/original/обзор/SKILL.md", Name: "обзор", Text: "Проверь изменения."},
		}}, nil
	}}
	m := testScreenModel(t, fake)
	m.resize(240, 24)
	m.composer.setValue(text)
	m, _ = m.submitInput()
	block := m.transcript.blocks[len(m.transcript.blocks)-1]
	// Check coloring without wrapping, then check layout separately at both widths.
	rendered := strings.Join(m.renderBlockLines(block), "\n")
	for _, token := range []string{"$review", quoted, "$обзор"} {
		if !strings.Contains(rendered, m.accentStyle.Render(token)) {
			t.Fatalf("selected token %q is not highlighted: %q", token, rendered)
		}
	}
	if strings.Contains(rendered, m.accentStyle.Render("$unknown")) || !strings.Contains(rendered, "`$review`") {
		t.Fatal("ordinary dollar text was styled as a selected skill")
	}
	for _, profile := range []string{DisplayCompact, DisplayDetailed, DisplayFull} {
		m.displayProfile = profile
		m.resize(240, 24)
		plain := ansi.Strip(strings.Join(m.renderBlockLines(block), "\n"))
		if (strings.Contains(plain, "/original/review/SKILL.md") && strings.Contains(plain, "≈")) != (profile != DisplayCompact) {
			t.Fatalf("unexpected source details for %s: %q", profile, plain)
		}
		m.resize(20, 24)
		for _, line := range m.renderBlockLines(block) {
			if visibleLen(line) > m.width {
				t.Fatalf("%s skill details exceed terminal width: %q", profile, line)
			}
		}
	}
}

func TestSkillSearchAttachesWithoutSendingAndPreservesDraft(t *testing.T) {
	entries := []skills.Entry{
		{Path: "/project/review/SKILL.md", Source: skills.Source{Label: "project"}, Metadata: skills.Metadata{Name: "review", Description: "Inspect changes", UserInvocable: true}},
		{Path: "/user/review/SKILL.md", Source: skills.Source{Label: "user"}, Metadata: skills.Metadata{Name: "review", Description: "Personal review", UserInvocable: true}},
	}
	for _, test := range []struct {
		name        string
		draft       string
		cursor      string
		want        string
		wantCursor  string
		alternative bool
		key         rune
	}{
		{"alternative source", "сделай ревью по $rev, затем проверь тесты", "сделай ревью по $rev", `сделай ревью по $"/user/review/SKILL.md", затем проверь тесты`, `сделай ревью по $"/user/review/SKILL.md"`, true, tea.KeyEnter},
		{"punctuation", "use $revjunk, then check tests", "use $rev", "use $review, then check tests", "use $review", false, tea.KeyEnter},
		{"whitespace", "use $rev\u2003then check tests", "use $rev", "use $review\u2003then check tests", "use $review\u2003", false, tea.KeyEnter},
		{"end of input", "use $rev", "use $rev", "use $review ", "use $review ", false, tea.KeyEnter},
		{"tab at bare dollar", "use $", "use $", "use $review ", "use $review ", false, tea.KeyTab},
		{"tab at full name", "use $review", "use $review", "use $review ", "use $review ", false, tea.KeyTab},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := skills.Catalog{Entries: entries[:1]}
			if test.alternative {
				catalog.Entries = entries
			}
			m := testScreenModel(t, &fakeAgent{skillCatalog: catalog})
			m.resize(58, 24)
			m.composer.setValueAt(test.draft, len(test.cursor))
			m.syncCommandSuggestions()
			if !m.skillSearch.visible() || len(m.skillSearch.matches) != len(catalog.Entries) {
				t.Fatalf("search = %#v", m.skillSearch)
			}
			if test.alternative {
				m, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
				if !strings.Contains(strings.Join(m.renderSkillSuggestions(), "\n"), "$review uses /project/review/SKILL.md") {
					t.Fatal("alternative source does not identify the package selected by name")
				}
			}
			for _, line := range m.renderSkillSuggestions() {
				if visibleLen(line) > m.width {
					t.Fatalf("skill suggestion exceeds terminal: %q", line)
				}
			}
			m, command := m.handleKey(tea.KeyPressMsg{Code: test.key})
			if command != nil || m.operation.isTurn() || m.composer.value() != test.want || m.skillSearch.visible() {
				t.Fatalf("selection sent or lost draft: %q, %#v", m.composer.value(), m.operation)
			}
			if m.composer.cursorOffset() != len(test.wantCursor) {
				t.Fatalf("cursor = %d", m.composer.cursorOffset())
			}
		})
	}
}

func TestSkillSearchVisibilityAndSubmission(t *testing.T) {
	fake := &fakeAgent{skillCatalog: skills.Catalog{Entries: []skills.Entry{
		{Path: "/project/review/SKILL.md", Source: skills.Source{Label: "project"}, Metadata: skills.Metadata{Name: "review", Description: "Inspect the path", UserInvocable: true}},
	}}}
	m, err := newScreenModel(context.Background(), fake, Config{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"explain $rev", "explain $reviewz", "explain $review"} {
		m.composer.setValue(text)
		m.composer.moveToEnd()
		m.syncCommandSuggestions()
		wantVisible := text != "explain $reviewz"
		if m.skillSearch.visible() != wantVisible || (len(m.renderSkillSuggestions()) > 0) != wantVisible {
			t.Fatalf("unexpected suggestion visibility for %q", text)
		}
	}
	m.composer.setValue("$обзор\n$")
	m.composer.moveToEnd()
	m.syncCommandSuggestions()
	if !m.skillSearch.visible() {
		t.Fatal("cannot choose a skill after an unknown reference")
	}
	m, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.skillSearch.visible() || m.composer.value() != "$обзор\n$" {
		t.Fatal("escape did not close just the search")
	}
	m.operation.kind = operationTurn
	for index, text := range []string{"$unknown", "почему пустой $path", "что значит $pro", "why $view", "цена 100 $", "сделай ревью $review"} {
		m.composer.setValue(text)
		m.composer.moveToEnd()
		m.syncCommandSuggestions()
		m, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
		if len(fake.queued) != index+1 || fake.queued[index].Text != text || m.composer.value() != "" {
			t.Fatalf("completion prevented sending ordinary input %q", text)
		}
	}
}

func TestSkillSubmissionKeepsCapturedContextInQueueAndErrorsInEditor(t *testing.T) {
	fake := &fakeAgent{prepareInput: func(text string) (agent.Input, error) {
		if strings.Contains(text, "broken") {
			return agent.Input{}, errors.New("broken skill")
		}
		return agent.Input{Text: text, Instructions: []agent.Instruction{{Text: "Captured instructions."}}}, nil
	}}
	m, err := newScreenModel(context.Background(), fake, Config{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	m.operation.kind = operationTurn
	m.composer.setValue("$review check")
	m, _ = m.submitInput()
	prepared, ok := fake.ClaimQueuedInput()
	if !ok || len(prepared.Instructions) != 1 || prepared.Instructions[0].Text != "Captured instructions." || prepared.Text != "$review check" {
		t.Fatalf("queued input = %#v", prepared)
	}
	m.composer.setValue("$broken check")
	m, _ = m.submitInput()
	if m.composer.value() != "$broken check" || len(fake.queued) != 0 {
		t.Fatal("failed attachment lost the draft or entered the queue")
	}
}

func TestSkillsClearDoesNotStartAModelTurn(t *testing.T) {
	fake := &fakeAgent{}
	m := testScreenModel(t, fake)
	command, handled := m.dispatchCommand("/skills clear")
	if !handled || command != nil || !fake.skillsCleared || m.operation.isTurn() || m.skillSearch.visible() {
		t.Fatal("skills clear did not clear attachments directly")
	}
}
