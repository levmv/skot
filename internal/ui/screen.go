package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"
	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/app"
)

const (
	transcriptGutter  = 2
	userMarker        = "›"
	resizeInterval    = 75 * time.Millisecond
	transcriptFrame   = 33 * time.Millisecond
	themeQueryTimeout = 150 * time.Millisecond
)

type ConversationAgent interface {
	SessionStatus() agent.SessionStatus
	Run(context.Context, string, agent.EmitFunc) (agent.RunResult, error)
	QueueInput(string) error
	ClaimQueued() (string, bool)
	PopQueued() (string, bool)
	RestoreQueued() []string
	QueuedInputs() []string
	State(context.Context) (agent.State, error)
	ToolStatus(string) ([]agent.Detail, bool)
	Compact(context.Context, int) (agent.ContextCompactedRecord, error)
}

type ShellAgent interface {
	RunShell(context.Context, string) (agent.ToolResult, error)
	RunPrivateShell(context.Context, string) (agent.ToolResult, error)
}

type ConfigurationAgent interface {
	CurrentToolSet() string
	ToolSets() []string
	ToolSetTools(string) []string
	SwitchToolSet(context.Context, string) error
	CurrentModel() string
	ModelChoices() []app.ModelChoice
	SwitchModel(context.Context, string, string) error
	CurrentReasoningEffort() string
	CurrentSandbox() string
	SecuritySummary() string
	SwitchSandbox(context.Context, string) error
	CurrentTheme() string
	SwitchTheme(string) error
}

type CredentialAgent interface {
	ProviderStatuses() ([]ProviderStatus, error)
	Login(context.Context, string, string) error
	Logout(context.Context, string) error
}

type SessionAgent interface {
	SessionID() string
	StartupNotices() []string
	ListSessions() ([]SessionSummary, error)
	ClearSession(context.Context) (string, error)
	ResumeSession(context.Context, string) (string, error)
}

// Agent is the UI composition boundary. The smaller embedded roles keep
// command, conversation, credential, and session dependencies independently
// usable by helpers and alternate frontends.
type Agent interface {
	ConversationAgent
	ShellAgent
	ConfigurationAgent
	CredentialAgent
	SessionAgent
}

type ProviderStatus = app.ProviderStatus

type ModelChoice = app.ModelChoice

type SessionSummary = app.SessionSummary

type Config struct {
	ModelURI        string
	ReasoningEffort string
	Root            string
	ToolSet         string
	Security        string
}

type pickerKind uint8

const (
	pickerNone pickerKind = iota
	pickerModel
	pickerToolSet
	pickerSandbox
	pickerTheme
	pickerLogin
	pickerLogout
	pickerSession
)

type pickerItem struct {
	value       string
	label       string
	description string
	// activeDetail is shown only while the row is selected: facts that inform
	// the choice already being made rather than the choice between rows.
	activeDetail string
	current      bool
	dimmed       bool
	custom       bool
	source       string
	modelURI     string
	efforts      []string
	effortIndex  int
	details      string
}

// pickerNavigation is how a picker is driven beyond the arrow keys. Digits
// belong to short lists whose order is fixed; typing belongs to lists long
// enough that reading them is the slow part. Never both.
type pickerNavigation uint8

const (
	navigationArrows pickerNavigation = iota
	navigationSearch
	navigationNumbers
)

type pickerState struct {
	kind         pickerKind
	items        []pickerItem
	index        int
	query        string
	startupLogin bool
}

func (picker pickerState) active() bool {
	return picker.kind != pickerNone && len(picker.items) != 0
}

type agentEventMsg struct{ event agent.Event }
type transcriptRenderMsg struct{}
type turnTickMsg struct{}

type shellDoneMsg struct {
	result agent.ToolResult
	err    error
}

type sandboxDoneMsg struct {
	policy     string
	summary    string
	concurrent bool
	err        error
}

type compactionDoneMsg struct{ err error }

type agentDoneMsg struct{ err error }

type resumeSessionMsg struct{ idOrPrefix string }
type themeQueryTimeoutMsg struct{ generation uint64 }

type screenModel struct {
	ctx    context.Context
	agent  Agent
	config Config

	composer   composerState
	secret     textinput.Model
	picker     pickerState
	transcript transcriptState

	modelChoices  []ModelChoice
	providers     []ProviderStatus
	loginProvider string
	loginModel    string
	loginEffort   string
	loginReturn   pickerState
	sessionStatus agent.SessionStatus

	width     int
	height    int
	quitting  bool
	operation activeOperation
	sandbox   sandboxSwitchState

	renderer     *inlineRenderer
	renderErr    error
	theme        string
	themePending bool
	themeQuery   uint64
	darkTheme    bool
	useStyle     bool

	mutedStyle      lipgloss.Style
	accentStyle     lipgloss.Style
	errorStyle      lipgloss.Style
	successStyle    lipgloss.Style
	userGutterStyle lipgloss.Style
	markdown        markdownRenderer
}

func CanUseScreen(in io.Reader, out io.Writer) (*os.File, *os.File, bool) {
	inFile, inOK := in.(*os.File)
	outFile, outOK := out.(*os.File)
	if !inOK || !outOK {
		return nil, nil, false
	}
	return inFile, outFile, IsTerminalFile(inFile) && IsTerminalFile(outFile)
}

func RunScreen(ctx context.Context, runtime Agent, config Config, in, out *os.File) (returnErr error) {
	terminalState, err := term.MakeRaw(in.Fd())
	if err != nil {
		return fmt.Errorf("enter terminal raw mode: %w", err)
	}
	defer func() {
		if err := term.Restore(in.Fd(), terminalState); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("restore terminal: %w", err))
		}
	}()

	model, err := newScreenModel(ctx, runtime, config, out)
	if err != nil {
		return err
	}
	defer func() {
		if err := model.renderer.Stop(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("stop terminal renderer: %w", err))
		}
	}()

	width, height, err := term.GetSize(out.Fd())
	if err != nil {
		return fmt.Errorf("get terminal size: %w", err)
	}
	model.resize(width, height)
	model.refreshTranscript()
	if !model.themePending {
		if err := model.renderer.RenderFrame(model.inlineFrame(), width, height); err != nil {
			return fmt.Errorf("render terminal: %w", err)
		}
		model.transcript.presented()
	}

	program := tea.NewProgram(model, tea.WithInput(in), tea.WithOutput(out), tea.WithContext(ctx), tea.WithoutRenderer())
	resizeCtx, stopResize := context.WithCancel(ctx)
	defer stopResize()
	go watchTerminalSize(resizeCtx, program, out, width, height)

	finalModel, err := program.Run()
	if errors.Is(err, tea.ErrInterrupted) {
		return context.Canceled
	}
	if final, ok := finalModel.(screenModel); ok && final.renderErr != nil {
		return final.renderErr
	}
	return err
}

func watchTerminalSize(ctx context.Context, program *tea.Program, out *os.File, width, height int) {
	// WithoutRenderer leaves terminal ownership to Skot, including SIGWINCH
	// handling. Polling also coalesces resize bursts into authoritative redraws.
	ticker := time.NewTicker(resizeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			nextWidth, nextHeight, err := term.GetSize(out.Fd())
			if err != nil || nextWidth <= 0 || nextHeight <= 0 || nextWidth == width && nextHeight == height {
				continue
			}
			width, height = nextWidth, nextHeight
			program.Send(tea.WindowSizeMsg{Width: width, Height: height})
		}
	}
}

func newScreenModel(ctx context.Context, runtime Agent, config Config, out io.Writer) (screenModel, error) {
	composer := newComposerState()
	secret := textinput.New()
	secret.Prompt = ""
	secret.SetWidth(1)
	secret.SetVirtualCursor(false)
	_ = secret.Focus()
	useStyle := shouldUseStyle(out)
	requestedTheme, err := normalizeTheme(runtime.CurrentTheme())
	if err != nil {
		return screenModel{}, err
	}
	darkTheme := requestedTheme == ThemeDark

	m := screenModel{
		ctx:          ctx,
		agent:        runtime,
		config:       config,
		composer:     composer,
		secret:       secret,
		renderer:     newInlineRenderer(out),
		theme:        requestedTheme,
		themePending: requestedTheme == ThemeAuto,
		darkTheme:    darkTheme,
		useStyle:     useStyle,
		modelChoices: runtime.ModelChoices(),
	}
	m.applyTerminalTheme(darkTheme)
	m.syncCommandSuggestions()
	m.addBlock(screenBlockSystem, "Skot · type / for commands, ! for shell")
	if security := strings.TrimSpace(config.Security); security != "" {
		m.addBlock(screenBlockSystem, security)
	}
	if err := m.loadSessionHistory(); err != nil {
		return screenModel{}, fmt.Errorf("load session history: %w", err)
	}
	m.openStartupLoginPicker()
	return m, nil
}

func (m screenModel) Init() tea.Cmd {
	if m.themePending {
		return queryTerminalTheme(m.themeQuery)
	}
	return nil
}

func queryTerminalTheme(generation uint64) tea.Cmd {
	return tea.Batch(
		tea.RequestBackgroundColor,
		tea.Tick(themeQueryTimeout, func(time.Time) tea.Msg {
			return themeQueryTimeoutMsg{generation: generation}
		}),
	)
}

func (m screenModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	if next.quitting {
		if err := next.renderer.Stop(); err != nil {
			next.renderErr = fmt.Errorf("stop terminal renderer: %w", err)
		}
		return next, cmd
	}
	if next.themePending {
		return next, cmd
	}
	if err := next.renderer.RenderFrame(next.inlineFrame(), next.width, next.height); err != nil {
		next.renderErr = fmt.Errorf("render terminal: %w", err)
		next.quitting = true
		_ = next.renderer.Stop()
		return next, tea.Quit
	}
	next.transcript.presented()
	return next, cmd
}

func (m screenModel) update(msg tea.Msg) (screenModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.BackgroundColorMsg:
		if !m.themePending {
			return m, nil
		}
		m.themePending = false
		m.applyTerminalTheme(msg.IsDark())
		m.refreshTranscript()
		return m, nil
	case themeQueryTimeoutMsg:
		if !m.themePending || msg.generation != m.themeQuery {
			return m, nil
		}
		// OSC 11 is optional and is sometimes filtered by terminal
		// multiplexers. Auto deliberately falls back to the light palette.
		m.themePending = false
		return m, nil
	case tea.WindowSizeMsg:
		if msg.Width > 0 && msg.Height > 0 {
			m.resize(msg.Width, msg.Height)
			m.refreshTranscript()
		}
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case tea.PasteMsg:
		// Bracketed paste never reaches handleKey, so a searchable picker has
		// to claim it here or a pasted model URI lands in the hidden composer.
		if m.picker.active() && pickerNavigationFor(m.picker.kind) == navigationSearch {
			m.picker.appendQuery(msg.Content)
			return m, nil
		}
		return m.updateInput(msg)
	case agentEventMsg:
		m.applyAgentEvent(msg.event)
		if msg.event.Kind == agent.EventTextDelta {
			return m, tea.Batch(waitAgentMsg(m.operation.events), m.scheduleTranscriptRender())
		}
		m.operation.renderPending = false
		m.refreshTranscript()
		return m, waitAgentMsg(m.operation.events)
	case transcriptRenderMsg:
		if !m.operation.renderPending {
			return m, nil
		}
		m.operation.renderPending = false
		m.refreshTranscript()
		return m, nil
	case turnTickMsg:
		m.refreshSessionStatus()
		if m.refreshProcessResults() {
			m.refreshTranscript()
		}
		if !m.operation.isTurn() && !m.operation.isMaintenance() && !m.hasRunningProcesses() {
			return m, nil
		}
		return m, scheduleTurnTick()
	case shellDoneMsg:
		m.refreshSessionStatus()
		m.finishShell(msg.result, msg.err, time.Now())
		m.refreshTranscript()
		return m, nil
	case sandboxDoneMsg:
		m.finishSandboxSwitch(msg)
		m.refreshTranscript()
		return m, nil
	case compactionDoneMsg:
		m.finishCompaction(msg)
		m.refreshTranscript()
		return m, nil
	case resumeSessionMsg:
		m.resumeSession(msg.idOrPrefix)
		m.refreshTranscript()
		return m, nil
	case agentDoneMsg:
		m.refreshSessionStatus()
		m.finishTurnChanges()
		if msg.err != nil && !errors.Is(msg.err, context.Canceled) {
			m.addBlock(screenBlockError, "error: "+msg.err.Error())
		}
		m.finishTurnDuration(time.Now())
		m.operation.clear()
		if input, ok := m.agent.ClaimQueued(); ok {
			m.addBlock(screenBlockUser, input)
			cmd := m.startTurn(input)
			m.refreshTranscript()
			return m, cmd
		}
		m.refreshTranscript()
		return m, nil
	default:
		return m.updateInput(msg)
	}
}

func (m screenModel) View() tea.View { return tea.NewView("") }

func (m *screenModel) resize(width, height int) {
	m.width = width
	m.height = height
	m.composer.resize(m.contentWidth(), height)
	m.secret.SetWidth(m.contentWidth())
}

func (m screenModel) inlineFrame() inlineFrame {
	dynamic := []string{m.workingLine()}
	editorDynamicStart := -1
	if m.operation.isMaintenance() {
		elapsed := formatTurnDuration(time.Since(m.operation.startedAt))
		dynamic = append(dynamic, strings.Repeat(" ", transcriptGutter)+m.mutedStyle.Render(m.operation.label()+" ("+elapsed+" · esc to interrupt)"))
	} else if m.picker.active() {
		dynamic = append(dynamic, m.renderPicker()...)
	} else {
		if queued := m.queuedLine(); queued != "" {
			dynamic = append(dynamic, queued)
		}
		editorDynamicStart = len(dynamic)
		dynamic = append(dynamic, m.markedEditorLines()...)
		dynamic = append(dynamic, m.renderCommandSuggestions()...)
	}
	dynamic = append(dynamic, "", strings.Repeat(" ", transcriptGutter)+truncateANSI(m.footerLine(), m.contentWidth()))

	var cursor *tea.Cursor
	if editorDynamicStart >= 0 {
		if editorCursor := m.editorCursor(); editorCursor != nil {
			copy := *editorCursor
			copy.Position.X += transcriptGutter
			copy.Position.Y += len(m.transcript.lines) + editorDynamicStart
			cursor = &copy
		}
	}
	return inlineFrame{
		transcript:          m.transcript.lines,
		dynamic:             dynamic,
		cursor:              cursor,
		transcriptChanged:   m.transcript.dirty,
		transcriptDirtyFrom: m.transcript.dirtyFrom,
	}
}

func (m screenModel) markedEditorLines() []string {
	lines := strings.Split(strings.TrimSuffix(m.editorView(), "\n"), "\n")
	for index, line := range lines {
		marker := " "
		if index == 0 {
			marker = userMarker
		}
		lines[index] = marker + strings.Repeat(" ", transcriptGutter-1) + line
	}
	return lines
}

func (m screenModel) editorView() string {
	if m.loginProvider != "" {
		return m.secret.View()
	}
	return m.composer.view()
}

func (m screenModel) editorCursor() *tea.Cursor {
	if m.loginProvider != "" {
		return m.secret.Cursor()
	}
	return m.composer.cursor()
}

func (m screenModel) workingLine() string {
	if !m.operation.isTurn() {
		return ""
	}
	elapsed := formatTurnDuration(time.Since(m.operation.startedAt))
	return strings.Repeat(" ", transcriptGutter) + m.mutedStyle.Render("Working ("+elapsed+" · esc to interrupt)")
}

func (m screenModel) queuedLine() string {
	queued := m.agent.QueuedInputs()
	if len(queued) == 0 {
		return ""
	}
	text := "queued: " + compactSingleLine(queued[len(queued)-1], 120)
	if len(queued) > 1 {
		text = fmt.Sprintf("queued %d · latest: %s", len(queued), compactSingleLine(queued[len(queued)-1], 120))
	}
	return m.marked(" ", truncateANSI(m.mutedStyle.Render(text), m.contentWidth()))
}

func (m screenModel) contentWidth() int { return max(1, max(20, m.width)-transcriptGutter-1) }
