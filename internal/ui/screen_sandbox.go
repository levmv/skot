package ui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
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
