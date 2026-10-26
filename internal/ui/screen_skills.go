package ui

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/levmv/skot/app"
	"github.com/levmv/skot/skills"
)

type skillSearchState struct {
	active    bool
	dismissed string
	catalog   skills.Catalog
	matches   []skills.Entry
	index     int
	start     int
	end       int
	query     string
}

func (search skillSearchState) visible() bool {
	return search.active && len(search.matches) != 0
}

func (m *screenModel) syncSkillSuggestions() {
	search := &m.skillSearch
	value := m.composer.value()
	if value != search.dismissed {
		search.dismissed = ""
	}
	start, end, query, ok := app.SkillCompletion(value, m.composer.cursorOffset())
	if !ok || search.dismissed == value || m.loginProvider != "" || m.pathPrompt != notFilesystemPath || m.modelContextSelection.uri != "" || m.picker.active() || m.maintenanceOperation().isMaintenance() {
		search.active = false
		return
	}
	if !search.active {
		search.catalog = m.agent.Skills()
	}
	if search.query != query || !search.active {
		search.index = 0
	}
	search.active, search.start, search.end, search.query = true, start, end, query
	search.matches = nil
	for _, entry := range search.catalog.Entries {
		if !entry.Metadata.UserInvocable {
			continue
		}
		if strings.HasPrefix(entry.Metadata.Name, query) {
			search.matches = append(search.matches, entry)
		}
	}
	search.index = min(search.index, max(0, len(search.matches)-1))
}

func (m *screenModel) moveSkillSuggestion(delta int) {
	search := &m.skillSearch
	if len(search.matches) > 0 {
		search.index = (search.index + delta + len(search.matches)) % len(search.matches)
	}
}

func (m *screenModel) acceptSkillSuggestion() {
	search := &m.skillSearch
	if len(search.matches) == 0 {
		return
	}
	entry := search.matches[search.index]
	reference := app.SkillReference(search.catalog, entry)
	value := m.composer.value()
	suffix := value[search.end:]
	if suffix == "" {
		reference += " "
	}
	updated := value[:search.start] + reference + suffix
	position := search.start + len(reference)
	if next, width := utf8.DecodeRuneInString(suffix); unicode.IsSpace(next) {
		position += width
	}
	m.composer.setValueAt(updated, position)
	search.active = false
	search.dismissed = updated
	m.syncCommandSuggestions()
}

func (m screenModel) renderSkillSuggestions() []string {
	search := m.skillSearch
	if !search.visible() {
		return nil
	}
	selected := search.matches[search.index]
	var override string
	if preferred, err := search.catalog.Resolve(selected.Metadata.Name); err == nil && preferred.Path != selected.Path {
		override = "$" + selected.Metadata.Name + " uses " + preferred.Path
	}
	reserved := 6
	if len(search.catalog.Diagnostics) != 0 {
		reserved++
	}
	if override != "" {
		reserved++
	}
	limit := min(6, max(1, m.height-m.composer.height()-reserved), len(search.matches))
	start := min(max(0, search.index-limit/2), len(search.matches)-limit)
	var lines []string
	for index := start; index < start+limit; index++ {
		entry := search.matches[index]
		label := entry.Metadata.Name + " · " + entry.Metadata.Description
		label = compactSingleLine(label, m.contentWidth())
		marker := " "
		if index == search.index {
			marker = userMarker
			label = m.accentStyle.Render(label)
		}
		lines = append(lines, m.marked(marker, label))
	}
	detail := selected.Path
	if len(selected.Metadata.UnhandledFields) != 0 {
		detail += " · unsupported metadata: " + strings.Join(selected.Metadata.UnhandledFields, ", ")
	}
	lines = append(lines, m.marked(" ", m.mutedStyle.Render(compactSingleLine(detail, m.contentWidth()))))
	if override != "" {
		lines = append(lines, m.marked(" ", m.mutedStyle.Render(compactSingleLine(override, m.contentWidth()))))
	}
	if len(search.catalog.Diagnostics) != 0 {
		message := fmt.Sprintf("%d source errors (/skills)", len(search.catalog.Diagnostics))
		lines = append(lines, m.marked(" ", m.warningStyle.Render(compactSingleLine(message, m.contentWidth()))))
	}
	return lines
}

func runSkillsCommand(m *screenModel, _ string, args []string) tea.Cmd {
	if len(args) != 0 {
		if args[0] != "clear" {
			m.addBlock(screenBlockError, "usage: /skills [clear]")
		} else if err := m.agent.ClearSkills(m.ctx); err != nil {
			m.addBlock(screenBlockError, err.Error())
		} else {
			m.addBlock(screenBlockSystem, "skills cleared")
			m.refreshSessionStatus()
		}
		return nil
	}
	catalog := m.agent.Skills()
	for _, diagnostic := range catalog.Diagnostics {
		m.addBlock(screenBlockError, diagnostic.Path+": "+diagnostic.Err.Error())
	}
	m.skillSearch = skillSearchState{active: true, catalog: catalog}
	m.composer.setValue("$")
	m.composer.moveToEnd()
	m.syncCommandSuggestions()
	if !m.skillSearch.visible() && len(catalog.Diagnostics) == 0 {
		m.addBlock(screenBlockSystem, "no skills available · add <name>/SKILL.md to .agents/skills or $SK_HOME/skills")
	}
	return nil
}
