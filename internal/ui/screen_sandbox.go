package ui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/levmv/skot/app"
)

type scopeSwitchState struct {
	pending   bool
	startedAt time.Time
	cancel    context.CancelFunc
	// previous is the scope in force when the switch started, kept so the
	// result can name where it came from once the async switch lands.
	previous string
	// prompt and typed belong to a path collected from the input line. They
	// restore that prompt when the path is refused, so a correction costs a
	// keystroke instead of opening /scope again.
	prompt filesystemPathRow
	typed  string
}

func (m *screenModel) startScopeSwitch(scope string) tea.Cmd {
	if m.scope.pending {
		m.addBlock(screenBlockError, "scope: a switch is already in progress")
		return nil
	}
	operationCtx, cancel := context.WithCancel(m.ctx)
	previous := m.agent.CurrentScope()
	m.scope = scopeSwitchState{pending: true, startedAt: time.Now(), cancel: cancel, previous: previous}
	return func() tea.Msg {
		err := m.agent.SwitchScope(operationCtx, scope)
		current := m.agent.CurrentScope()
		notice := ""
		if (err == nil || preferenceAppliedDespiteError(err)) && current != previous {
			notice = m.agent.ScopeNotice()
		}
		return scopeDoneMsg{
			scope:   current,
			summary: m.agent.ScopeSummary(),
			notice:  notice,
			err:     err,
		}
	}
}

func (m *screenModel) finishScopeSwitch(message scopeDoneMsg) {
	if m.scope.cancel != nil {
		m.scope.cancel()
	}
	previous, prompt, typed := m.scope.previous, m.scope.prompt, m.scope.typed
	m.scope = scopeSwitchState{}
	if message.err != nil && !preferenceAppliedDespiteError(message.err) {
		m.addBlock(screenBlockError, "scope: "+message.err.Error())
		if prompt != notFilesystemPath {
			// The path was refused, not the prompt: keep collecting one so the
			// reason and the next attempt stay in the same place.
			m.reopenPathPrompt(prompt, typed)
			return
		}
		m.refreshScopePicker()
		return
	}
	text := message.change
	if text == "" {
		text = formatSettingChange("filesystem scope", previous, message.scope)
		if detail := scopeSummaryDetail(message.scope, message.summary); detail != "" {
			text += "\n" + detail
		}
	}
	if message.notice != "" {
		text += "\nwarning: " + message.notice
	}
	m.addBlock(screenBlockScopeChange, text)
	if message.err != nil {
		m.addBlock(screenBlockError, "scope: "+message.err.Error())
	}
	if prompt != notFilesystemPath {
		// A path typed from the menu belongs to the menu: show it there.
		m.openScopePickerAt(prompt)
		return
	}
	m.refreshScopePicker()
}

// startFilesystemPathAddition remembers one typed path for this workspace.
func (m *screenModel) startFilesystemPathAddition(kind filesystemPathRow, value string) tea.Cmd {
	subject, apply := "added directory", m.agent.AddDirectory
	if kind == protectedPathRow {
		subject, apply = "protected path", m.agent.ProtectPath
	}
	return m.startFilesystemPathChange(subject+": "+value, value, apply, kind)
}

// startFilesystemPathRemoval drops one remembered path. The prompt it returns
// to is none: a removal is driven from the menu, which stays open.
func (m *screenModel) startFilesystemPathRemoval(item pickerItem) tea.Cmd {
	subject, apply := "added directory", m.agent.RemoveAddedDirectory
	if item.filesystemPath == protectedPathRow {
		subject, apply = "protected path", m.agent.UnprotectPath
	}
	return m.startFilesystemPathChange("removed "+subject+": "+item.value, item.value, apply, notFilesystemPath)
}

// startFilesystemPathChange runs one path mutation. It shares the scope switch
// slot with every other change to the live policy: the same transaction, the
// same process-boundary check, and therefore the same result message.
func (m *screenModel) startFilesystemPathChange(change, value string, apply func(context.Context, string) error, prompt filesystemPathRow) tea.Cmd {
	if m.scope.pending {
		m.addBlock(screenBlockError, "scope: a filesystem change is already in progress")
		m.refreshTranscript()
		return nil
	}
	operationCtx, cancel := context.WithCancel(m.ctx)
	m.scope = scopeSwitchState{
		pending: true, startedAt: time.Now(), cancel: cancel, previous: m.agent.CurrentScope(),
		prompt: prompt, typed: value,
	}
	return func() tea.Msg {
		err := apply(operationCtx, value)
		notice := ""
		if (err == nil || preferenceAppliedDespiteError(err)) && prompt != notFilesystemPath {
			// Protecting a path can introduce a process-boundary cost, while adding
			// a directory can make an existing external protected path relevant.
			// Either change is the useful moment to explain it; later starts are not.
			notice = m.agent.ScopeNotice()
		}
		return scopeDoneMsg{
			scope:   m.agent.CurrentScope(),
			summary: m.agent.ScopeSummary(),
			notice:  notice,
			err:     err,
			change:  change,
		}
	}
}

// scopeSummaryDetail removes the headline already carried by the transition and
// gives each fact its own line: a switch result is read, not scanned, and one
// long line of lists is where the reading stops. ScopeSummary itself stays one
// line, since startup shows it next to the banner.
func scopeSummaryDetail(scope, summary string) string {
	summary = strings.TrimSpace(summary)
	headline := "scope: " + strings.TrimSpace(scope)
	if summary == headline {
		return ""
	}
	if detail, ok := strings.CutPrefix(summary, headline+" · "); ok {
		return strings.ReplaceAll(strings.TrimSpace(detail), " · ", "\n")
	}
	return strings.ReplaceAll(summary, " · ", "\n")
}

func (m *screenModel) openScopePicker() {
	items := m.scopePickerRows()
	m.openPicker(pickerScope, items, markCurrentPickerItem(items, m.agent.CurrentScope()))
}

// refreshScopePicker rebuilds an open scope picker after the policy changed, so
// a removed path leaves the list without closing the menu.
func (m *screenModel) refreshScopePicker() {
	if m.picker.kind != pickerScope {
		return
	}
	items := m.scopePickerRows()
	m.picker.items = items
	m.picker.index = min(max(0, m.picker.index), len(items)-1)
}

// scopePickerRows lists the scopes first, so their digits and the muscle memory
// of the two top rows survive however many paths follow.
func (m screenModel) scopePickerRows() []pickerItem {
	items := append([]pickerItem(nil), scopePickerItems...)
	added, protected := m.agent.FilesystemPaths()
	items = append(items, m.filesystemPathSection(added, addedDirectoryRow, "-add-dir",
		"+ add a directory", "reachable in workspace scope")...)
	items = append(items, m.filesystemPathSection(protected, protectedPathRow, "-protect-path",
		"+ protect a path", "hidden from the model")...)
	return items
}

// filesystemPathSection is one list and the row which adds to it. The add row
// closes the section so an empty list still costs a single line.
func (m screenModel) filesystemPathSection(paths []FilesystemPath, kind filesystemPathRow, flag, label, description string) []pickerItem {
	rows := m.filesystemPathRows(paths, kind, flag)
	return append(rows, pickerItem{
		label: label, description: description, dividerBefore: len(rows) == 0,
		filesystemPath: kind, addRow: true,
	})
}

// filesystemPathRows describes only what departs from the ordinary row: one
// remembered for this workspace says nothing, because the section it sits in
// already says what it is.
func (m screenModel) filesystemPathRows(paths []FilesystemPath, kind filesystemPathRow, flag string) []pickerItem {
	rows := make([]pickerItem, 0, len(paths))
	for index, entry := range paths {
		row := pickerItem{
			value:          entry.Path,
			label:          displayToolPath(m.config.Root, entry.Path),
			dividerBefore:  index == 0,
			filesystemPath: kind,
			dimmed:         entry.Origin != app.FilesystemPathRemembered,
		}
		switch entry.Origin {
		case app.FilesystemPathRemembered:
			row.activeDetail = "(d or backspace to remove)"
		case app.FilesystemPathInvocation:
			row.description = flag + " this run"
		case app.FilesystemPathSettings:
			row.description = "config.json"
		}
		rows = append(rows, row)
	}
	return rows
}

// openPathPrompt turns the input line into a path field for one list. What it
// is collecting is named by pathPromptLine, next to the line itself.
func (m *screenModel) openPathPrompt(kind filesystemPathRow) {
	m.pathPrompt = kind
	m.pathCompletion.reset()
	m.composer.reset()
	m.syncCommandSuggestions()
}

// pathPromptLine names what the input line is collecting. It lives in the
// dynamic area rather than the transcript: it belongs to the prompt, and a
// transcript line would pile up every time the prompt is opened.
func (m screenModel) pathPromptLine() string {
	if m.pathPrompt == notFilesystemPath {
		return ""
	}
	subject := "directory to add"
	if m.pathPrompt == protectedPathRow {
		subject = "path to protect"
	}
	return strings.Repeat(" ", transcriptGutter) +
		m.mutedStyle.Render(subject+" · tab completes · esc returns to /scope")
}

// reopenPathPrompt puts a refused path back in the input line, ready to edit.
func (m *screenModel) reopenPathPrompt(kind filesystemPathRow, typed string) {
	m.pathPrompt = kind
	m.composer.setValue(typed)
	m.composer.cursorEnd()
	m.syncCommandSuggestions()
}

// openScopePickerAt returns to the menu with the section which was just edited
// under the cursor, so another path costs one keystroke and the new row is in
// view.
func (m *screenModel) openScopePickerAt(kind filesystemPathRow) {
	m.openScopePicker()
	for index, item := range m.picker.items {
		if item.addRow && item.filesystemPath == kind {
			m.picker.index = index
			break
		}
	}
}

// closePathPrompt returns to the menu the prompt was opened from.
func (m *screenModel) closePathPrompt() {
	m.pathPrompt = notFilesystemPath
	m.pathCompletion.reset()
	m.composer.reset()
	m.openScopePicker()
}
