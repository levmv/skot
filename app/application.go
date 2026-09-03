package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/levmv/skot/agent"
	productlimits "github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/internal/modelhttp"
	"github.com/levmv/skot/internal/session"
	"github.com/levmv/skot/internal/state"
	"github.com/levmv/skot/internal/toolpolicy"
	"github.com/levmv/skot/model/anthropic"
	"github.com/levmv/skot/model/chatcompletions"
	responsemodel "github.com/levmv/skot/model/responses"
	workspacetools "github.com/levmv/skot/tools"
)

// Application is the concrete, UI-neutral Skot composition root. Its current
// live session owns one agent runtime and its persistence lifetime; clearing or
// resuming replaces that session as a unit. This pre-v1 API is intentionally
// product-specific and is not yet a compatibility promise.
//
// Callers must serialize Close, session replacement, and model or tool
// reconfiguration with Run and with one another. Filesystem policy is the
// exception: SwitchScope and the added and protected path mutations may overlap
// an active Run and affect only subsequently started tool calls and processes.
type Application struct {
	config applicationConfig

	mu           sync.RWMutex
	filesystemMu sync.Mutex
	state        applicationState
}

// applicationConfig is immutable after Open returns. In particular, session
// replacement must not make callers retain a stale runtime, but it does not
// change the stores, catalog, workspace, or runtime policy used to build one.
type applicationConfig struct {
	settings            *state.Store
	interactive         *state.InteractiveStore
	tools               []agent.Tool
	programDeclarations []workspacetools.ProgramTool
	programToolsFile    string
	applicationBuild    agent.BuildSnapshot
	toolSets            toolpolicy.ToolSets
	systemPrompt        string
	root                string
	home                string
	// invocation and settings paths are the filesystem-policy layers a running
	// session does not own: flags last for this run, config.json for every run
	// using that data directory.
	invocationAddedPaths     []string
	invocationProtectedPaths []string
	settingsProtectedPaths   []string
	baseURL                  string
	modelAPI                 modelAPI
	contextWindow            int
	metadataLookup           modelContextLookup
	retryBudget              time.Duration
	streamIdleTimeout        time.Duration
	maxToolIterations        int
	awaitRequiredJobs        bool
	masker                   *secretMasker
}

// applicationState is protected by Application.mu. It contains exactly the
// state that can change after Open, including resources cleared by Close.
type applicationState struct {
	session                 *liveSession
	processes               *workspacetools.ProcessManager
	children                *childSupervisor
	toolSet                 string
	theme                   string
	displayProfile          string
	security                securityState
	additions               *workspacetools.AddedDirectoryPolicy
	protection              *workspacetools.ProtectedPathPolicy
	workspaceAddedPaths     []string
	workspaceProtectedPaths []string
	startupNotices          []string
}

func (application *Application) Run(ctx context.Context, input string, emit agent.EmitFunc) (agent.RunResult, error) {
	runtime, err := application.requireRuntime()
	if err != nil {
		return agent.RunResult{}, err
	}
	result, runErr := runtime.Run(ctx, input, emit)
	application.mu.RLock()
	current := application.state.session
	children := application.state.children
	application.mu.RUnlock()
	currentIsRuntime := current != nil && current.runtime == runtime
	retainedChildren := currentIsRuntime && children != nil && children.HasChildren(runtime.CurrentSessionID())
	if (len(result.DetachedJobs) != 0 || retainedChildren) && currentIsRuntime && current.memory {
		return result, errors.Join(runErr, errors.New("in-memory one-shot created unexpected durable external work"))
	}
	if len(result.DetachedJobs) != 0 || retainedChildren {
		application.mu.Lock()
		if current := application.state.session; current != nil && current.runtime == runtime {
			current.provisional = false
		}
		application.mu.Unlock()
	}
	return result, runErr
}

// SwitchModel selects a route for the live session. api is the protocol the
// caller chose for a route this build does not declare; it is ignored for a
// declared route and for a process-wide protocol override.
func (application *Application) SwitchModel(ctx context.Context, uri, effort, api string) error {
	return application.switchModel(ctx, uri, effort, api, 0, false)
}

// SwitchModelWithContextWindow selects a route with optional context metadata.
// An undeclared route whose window cannot be discovered requires a positive
// value.
func (application *Application) SwitchModelWithContextWindow(ctx context.Context, uri, effort, api string, contextWindow int) error {
	return application.switchModel(ctx, uri, effort, api, contextWindow, true)
}

func (application *Application) switchModel(ctx context.Context, uri, effort, api string, contextWindow int, requireContext bool) error {
	runtime, err := application.requireRuntime()
	if err != nil {
		return err
	}
	if contextWindow < 0 {
		return agent.MarkInvalidRequest(errors.New("model context window cannot be negative"))
	}
	// A protocol supplied for this switch is user input and must be rejected
	// when it is not one Skot implements; a protocol remembered by another
	// build is tolerated and simply stops applying.
	if _, err := parseModelAPI(api); err != nil {
		return agent.MarkInvalidRequest(err)
	}
	selectionAPI := string(selectionModelAPI(uri, api))
	selectionContextWindow := 0
	if application.config.contextWindow == 0 {
		selectionContextWindow = selectionModelContextWindow(uri, contextWindow)
	}
	overrides := modelRouteOverrides{
		BaseURL: application.config.baseURL, API: application.config.modelAPI, ContextWindow: application.config.contextWindow,
	}.withSelection(uri, selectionAPI, selectionContextWindow)
	route, err := resolveModelRoute(uri, effort, overrides, modelRouteEnrichment{})
	if err != nil {
		return agent.MarkInvalidRequest(err)
	}
	activated := false
	_, declared := catalogModelSpec(uri)
	if requireContext && contextWindow == 0 && !declared && route.ContextWindowEstimated {
		route, err = activateModelRoute(ctx, uri, effort, overrides,
			savedModelContextFromInfo(runtime.CurrentModelInfo()), application.config.metadataLookup)
		if err != nil {
			return agent.MarkInvalidRequest(err)
		}
		activated = true
		if route.ContextWindowEstimated {
			return agent.MarkInvalidRequest(&ModelContextWindowRequiredError{URI: uri})
		}
	}
	currentModel := runtime.CurrentModel()
	currentEffort := runtime.CurrentReasoningEffort()
	currentInfo := runtime.CurrentModelInfo()
	sameProtocol := selectionAPI == "" || modelAPIFromBackendID(currentInfo.BackendID) == route.API
	sameContext := (selectionContextWindow == 0 && !activated) || currentInfo.ContextWindow == route.ContextWindow
	if strings.EqualFold(route.URI, currentModel) && route.ReasoningEffort == currentEffort && sameProtocol && sameContext {
		return application.persistInteractivePreference("model", func(preferences *state.InteractiveStore) error {
			return preferences.SetModelSelectionWithContext(currentModel, currentEffort, selectionAPI, selectionContextWindow)
		})
	}
	if !activated {
		route, err = activateModelRoute(ctx, uri, effort, overrides,
			savedModelContextFromInfo(currentInfo), application.config.metadataLookup)
		if err != nil {
			return agent.MarkInvalidRequest(err)
		}
	}
	modelInfo, err := modelInfoForRoute(route)
	if err != nil {
		return agent.MarkInvalidRequest(err)
	}
	backend, err := buildModelBackend(route, application.config.settings, modelBackendOptions{requireCredential: true})
	if err != nil {
		return err
	}
	if err := runtime.SwitchModel(ctx, modelInfo, backend); err != nil {
		return err
	}
	application.mu.RLock()
	children := application.state.children
	application.mu.RUnlock()
	if children != nil {
		children.setModelSelection(runtime.CurrentModel(), runtime.CurrentReasoningEffort(), selectionAPI, selectionContextWindow)
	}
	return application.persistInteractivePreference("model", func(preferences *state.InteractiveStore) error {
		return preferences.SetModelSelectionWithContext(runtime.CurrentModel(), runtime.CurrentReasoningEffort(), selectionAPI, selectionContextWindow)
	})
}

func (application *Application) CurrentReasoningEffort() string {
	runtime := application.runtimeOrNil()
	if runtime == nil {
		return ""
	}
	return runtime.CurrentReasoningEffort()
}

func (application *Application) CurrentTheme() string {
	application.mu.RLock()
	defer application.mu.RUnlock()
	return application.state.theme
}

func (application *Application) SwitchTheme(value string) error {
	theme, err := state.NormalizeTheme(value)
	if err != nil {
		return err
	}
	application.mu.Lock()
	if application.state.session == nil {
		application.mu.Unlock()
		return errors.New("application is closed")
	}
	if theme != application.state.theme {
		application.state.theme = theme
	}
	application.mu.Unlock()
	return application.persistInteractivePreference("theme", func(preferences *state.InteractiveStore) error {
		return preferences.SetThemeSelection(theme)
	})
}

func (application *Application) CurrentDisplayProfile() string {
	application.mu.RLock()
	defer application.mu.RUnlock()
	return application.state.displayProfile
}

func (application *Application) SwitchDisplayProfile(value string) error {
	profile, err := state.NormalizeDisplayProfile(value)
	if err != nil {
		return err
	}
	application.mu.Lock()
	if application.state.session == nil {
		application.mu.Unlock()
		return errors.New("application is closed")
	}
	application.state.displayProfile = profile
	application.mu.Unlock()
	return application.persistInteractivePreference("display", func(preferences *state.InteractiveStore) error {
		return preferences.SetDisplaySelection(profile)
	})
}

func (application *Application) persistInteractivePreference(setting string, persist func(*state.InteractiveStore) error) error {
	preferences := application.config.interactive
	if preferences == nil {
		return nil
	}
	if err := persist(preferences); err != nil {
		return &PreferenceNotPersistedError{Setting: setting, Err: err}
	}
	return nil
}

func (application *Application) ModelChoices() []ModelChoice {
	overrides := modelRouteOverrides{
		BaseURL: application.config.baseURL, API: application.config.modelAPI, ContextWindow: application.config.contextWindow,
	}
	runtime := application.runtimeOrNil()
	if runtime == nil {
		return modelChoices(application.config.interactive, "", "", overrides)
	}
	info := runtime.CurrentModelInfo()
	current := runtime.CurrentModel()
	// The live session already proves which protocol the current route speaks,
	// so it stays selectable even when no stored selection describes it.
	choices := modelChoices(application.config.interactive, current, string(modelAPIFromBackendID(info.BackendID)), overrides)
	for index := range choices {
		if strings.EqualFold(choices[index].URI, current) {
			if protocol := modelAPIFromBackendID(info.BackendID); protocol != "" {
				choices[index].Protocol = string(protocol)
			}
			choices[index].ContextWindow = info.ContextWindow
			choices[index].ContextWindowEstimated = info.ContextWindowEstimated
			break
		}
	}
	return choices
}

func (application *Application) CurrentModel() string {
	runtime := application.runtimeOrNil()
	if runtime == nil {
		return ""
	}
	return runtime.CurrentModel()
}

func (application *Application) State(ctx context.Context) (agent.State, error) {
	runtime, err := application.requireRuntime()
	if err != nil {
		return agent.State{}, err
	}
	return runtime.State(ctx)
}

func (application *Application) QueueInput(input string) error {
	runtime, err := application.requireRuntime()
	if err != nil {
		return err
	}
	return runtime.QueueInput(input)
}

func (application *Application) ClaimQueued() (string, bool) {
	runtime := application.runtimeOrNil()
	if runtime == nil {
		return "", false
	}
	return runtime.ClaimQueued()
}

func (application *Application) PopQueued() (string, bool) {
	runtime := application.runtimeOrNil()
	if runtime == nil {
		return "", false
	}
	return runtime.PopQueued()
}

func (application *Application) QueuedInputs() []string {
	runtime := application.runtimeOrNil()
	if runtime == nil {
		return nil
	}
	return runtime.QueuedInputs()
}

func (application *Application) RunShell(ctx context.Context, command string) (agent.ToolResult, error) {
	runtime, err := application.requireRuntime()
	if err != nil {
		return agent.ToolResult{}, err
	}
	return runtime.RunShell(ctx, command)
}

func (application *Application) RunPrivateShell(ctx context.Context, command string) (agent.ToolResult, error) {
	runtime, err := application.requireRuntime()
	if err != nil {
		return agent.ToolResult{}, err
	}
	return runtime.RunPrivateShell(ctx, command)
}

func (application *Application) ToolStatus(id string) ([]agent.Detail, bool) {
	runtime := application.runtimeOrNil()
	if runtime == nil {
		return nil, false
	}
	return runtime.ToolStatus(id)
}

func (application *Application) CurrentToolSet() string {
	application.mu.RLock()
	defer application.mu.RUnlock()
	return application.state.toolSet
}

func (application *Application) ToolSets() []string {
	return application.config.toolSets.Names()
}

func (application *Application) ToolSetTools(toolSet string) []string {
	return application.config.toolSets.ToolNames(toolSet)
}

func (application *Application) SwitchToolSet(ctx context.Context, value string) error {
	runtime, err := application.requireRuntime()
	if err != nil {
		return err
	}
	toolSets := application.config.toolSets
	application.mu.RLock()
	currentToolSet := application.state.toolSet
	memorySession := application.state.session != nil && application.state.session.memory
	processes := application.state.processes
	application.mu.RUnlock()
	toolSet, err := toolSets.Normalize(value)
	if err != nil {
		return err
	}
	if toolSet == currentToolSet {
		return application.persistInteractivePreference("tool set", func(preferences *state.InteractiveStore) error {
			return preferences.SetToolSetSelection(toolSet)
		})
	}
	if memorySession && !toolSetSupportsMemorySession(toolSets, toolSet) {
		return errors.New("cannot enable process or external-work tools in an in-memory one-shot session")
	}
	selected, programSnapshots, err := bindToolSetTools(
		application.config.tools, toolSets, toolSet,
		application.config.programDeclarations, application.config.programToolsFile, processes,
	)
	if err != nil {
		return err
	}
	if err := runtime.SetToolsWithProgramTools(ctx, selected, toolSet, programSnapshots); err != nil {
		return err
	}
	application.mu.Lock()
	application.state.toolSet = toolSet
	application.mu.Unlock()
	return application.persistInteractivePreference("tool set", func(preferences *state.InteractiveStore) error {
		return preferences.SetToolSetSelection(toolSet)
	})
}

func (application *Application) SessionStatus() agent.SessionStatus {
	runtime := application.runtimeOrNil()
	if runtime == nil {
		return agent.SessionStatus{}
	}
	return runtime.SessionStatus()
}

func (application *Application) Compact(ctx context.Context) (agent.ContextCompactedRecord, error) {
	runtime, err := application.requireRuntime()
	if err != nil {
		return agent.ContextCompactedRecord{}, err
	}
	return runtime.Compact(ctx)
}

func (application *Application) CurrentScope() string {
	application.mu.RLock()
	defer application.mu.RUnlock()
	return string(application.state.security.Scope)
}

func modelInfoForRoute(route resolvedModelRoute) (agent.ModelInfo, error) {
	var backendID string
	switch route.API {
	case modelAPIChatCompletions:
		backendID = chatcompletions.BackendID(route.Provider)
	case modelAPIResponses:
		backendID = responsemodel.BackendID(route.Provider)
	case modelAPIAnthropicMessages:
		backendID = anthropic.BackendID(route.Provider)
	default:
		return agent.ModelInfo{}, fmt.Errorf("unsupported model API %q", route.API)
	}
	return agent.ModelInfo{
		BackendID: backendID, Provider: route.Provider, Model: route.Model,
		ReasoningEffort: route.ReasoningEffort, ProviderStateContract: route.ProviderStateContract,
		ImageInputUnsupported: route.ImageInputUnsupported,
		ContextWindow:         route.ContextWindow, ContextWindowEstimated: route.ContextWindowEstimated,
		MaxRequestBytes: productlimits.MaxModelRequestBytes, MaxCompletionBytes: productlimits.MaxModelCompletionBytes,
		Endpoint: modelhttp.PublicEndpoint(route.BaseURL),
	}, nil
}

func buildModelBackend(route resolvedModelRoute, credentials *state.Store, options modelBackendOptions) (agent.Backend, error) {
	if !implementedModelAPI(route.API) {
		return nil, agent.MarkInvalidRequest(fmt.Errorf("unsupported model API %q", route.API))
	}
	if options.requireCredential && !route.CustomEndpoint && !route.Credentialless {
		token, _, err := credentialForProvider(credentials, route.Provider)
		if err != nil {
			return nil, err
		}
		if token == "" {
			return nil, agent.MarkInvalidRequest(missingProviderCredentialError(route.Provider, route.URI))
		}
	}
	var authorizer modelhttp.Authorizer = storedBearerAuthorizer{
		store: credentials, provider: route.Provider, modelURI: route.URI, allowMissing: route.CustomEndpoint,
	}
	if route.Credentialless {
		// Ollama ignores the token, while OpenAI-compatible clients conventionally
		// send a non-empty placeholder.
		authorizer = modelhttp.BearerToken(route.Provider)
	}
	var backend agent.Backend
	var err error
	switch route.API {
	case modelAPIChatCompletions:
		backend, err = chatcompletions.New(chatcompletions.Config{
			Provider: route.Provider, Model: route.Model, APIModel: route.APIModel,
			ReasoningEffort: route.ReasoningEffort, Traits: route.ChatTraits,
			BaseURL: route.BaseURL, HTTPClient: options.httpClient, Authorizer: authorizer, Header: route.Header,
		})
	case modelAPIResponses:
		backend, err = responsemodel.New(responsemodel.Config{
			Provider: route.Provider, Model: route.Model, APIModel: route.APIModel,
			ReasoningEffort: route.ReasoningEffort, Traits: route.ResponsesTraits,
			BaseURL: route.BaseURL, HTTPClient: options.httpClient, Authorizer: authorizer, Header: route.Header,
		})
	case modelAPIAnthropicMessages:
		var apiKeyAuthorizer modelhttp.Authorizer = storedAPIKeyAuthorizer{
			store: credentials, provider: route.Provider, modelURI: route.URI, allowMissing: route.CustomEndpoint,
		}
		if route.Credentialless {
			apiKeyAuthorizer = anthropic.APIKey(route.Provider)
		}
		backend, err = anthropic.New(anthropic.Config{
			Provider: route.Provider, Model: route.Model, APIModel: route.APIModel,
			MaxTokens: route.MaxOutputTokens, PromptCache: route.PromptCache,
			BaseURL: route.BaseURL, HTTPClient: options.httpClient, Authorizer: apiKeyAuthorizer, Header: route.Header,
		})
	default:
		return nil, agent.MarkInvalidRequest(fmt.Errorf("unsupported model API %q", route.API))
	}
	if err != nil {
		return nil, fmt.Errorf("initialize model: %w", err)
	}
	return addRouteDiagnostics(backend, route), nil
}

func (application *Application) ProviderStatuses() ([]ProviderStatus, error) {
	return providerStatuses(application.config.settings)
}

func (application *Application) Login(ctx context.Context, provider, token string) error {
	masker := application.config.masker
	if masker == nil {
		return errors.New("application is closed")
	}
	if err := application.updateCredential(ctx, provider, func(settings *state.Store, normalizedProvider string) error {
		return storeProviderCredential(settings, normalizedProvider, token)
	}); err != nil {
		return err
	}
	masker.Add(token)
	return nil
}

func (application *Application) Logout(ctx context.Context, provider string) error {
	return application.updateCredential(ctx, provider, deleteProviderCredential)
}

func (application *Application) updateCredential(ctx context.Context, provider string, mutate func(*state.Store, string) error) error {
	if _, err := application.requireRuntime(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	settings := application.config.settings
	if settings == nil {
		return errors.New("application is closed")
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	return mutate(settings, provider)
}

func (application *Application) SessionID() string {
	application.mu.RLock()
	defer application.mu.RUnlock()
	if application.state.session == nil || application.state.session.provisional {
		return ""
	}
	return application.state.session.managedID
}

// HasUserTurn reports whether the current session has received a submitted
// user input. Empty interactive sessions are not resumable after Close.
func (application *Application) HasUserTurn() bool {
	application.mu.RLock()
	current := application.state.session
	application.mu.RUnlock()
	return current != nil && current.journal != nil && current.journal.HasUserTurn()
}

func (application *Application) ListSessions() ([]SessionSummary, error) {
	home, root := application.config.home, application.config.root
	summaries, err := session.List(home, root)
	if err != nil {
		return nil, err
	}
	result := make([]SessionSummary, 0, len(summaries))
	for _, summary := range summaries {
		result = append(result, SessionSummary{
			ID: summary.ID, Title: summary.Title, UpdatedAt: summary.UpdatedAt,
		})
	}
	return result, nil
}

func (application *Application) ClearSession(ctx context.Context) (string, error) {
	home := application.config.home
	journal, id, err := session.Create(home)
	if err != nil {
		return "", err
	}
	if err := application.installSession(ctx, journal, id, application.CurrentModel(), application.CurrentReasoningEffort(), nil); err != nil {
		_ = journal.ClosePruningEmpty()
		return "", err
	}
	return id, nil
}

func (application *Application) ResumeSession(ctx context.Context, idOrPrefix string) (string, error) {
	home, root := application.config.home, application.config.root
	summary, err := session.Resolve(home, root, idOrPrefix)
	if err != nil {
		return "", err
	}
	journal, err := session.OpenManaged(home, summary.ID)
	if err != nil {
		return "", err
	}
	sessionState, _, err := agent.Reconcile(ctx, journal)
	if err != nil {
		_ = journal.Close()
		return "", fmt.Errorf("reconcile session: %w", err)
	}
	modelURI := application.CurrentModel()
	reasoningEffort := application.CurrentReasoningEffort()
	if sessionState.Selection.Model != "" {
		modelURI = sessionState.Selection.Provider + "/" + sessionState.Selection.Model
		reasoningEffort = sessionState.Selection.ReasoningEffort
	}
	if err := application.installSession(ctx, journal, summary.ID, modelURI, reasoningEffort, &sessionState); err != nil {
		_ = journal.Close()
		return "", err
	}
	return summary.ID, nil
}

func (application *Application) installSession(ctx context.Context, journal *session.Store, id, modelURI, reasoningEffort string, resumedState *agent.State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	baseURL, contextWindow := application.config.baseURL, application.config.contextWindow
	retryBudget, streamIdleTimeout := application.config.retryBudget, application.config.streamIdleTimeout
	maxToolIterations := application.config.maxToolIterations
	settings, masker := application.config.settings, application.config.masker
	root, systemPrompt := application.config.root, application.config.systemPrompt
	tools := append([]agent.Tool(nil), application.config.tools...)
	toolSets := application.config.toolSets
	awaitRequiredJobs := application.config.awaitRequiredJobs
	application.mu.RLock()
	processes := application.state.processes
	children := application.state.children
	security := application.state.security
	protection := application.state.protection
	toolSet := application.state.toolSet
	currentSession := application.state.session
	var currentRuntime *agent.Runtime
	if currentSession != nil {
		currentRuntime = currentSession.runtime
	}
	application.mu.RUnlock()
	if currentSession == nil || settings == nil || processes == nil {
		return errors.New("application is closed")
	}
	if currentRuntime == nil {
		return errors.New("current session runtime is unavailable")
	}
	currentRuntimeID := currentRuntime.CurrentSessionID()
	var knownModel *agent.ModelInfo
	if resumedState != nil {
		if modelInfo, ok := restoredModelInfo(*resumedState, modelURI); ok {
			knownModel = &modelInfo
		}
	} else {
		modelInfo := currentRuntime.CurrentModelInfo()
		if modelInfoMatchesURI(modelInfo, modelURI) {
			knownModel = &modelInfo
		}
	}
	tools, programTools, err := bindProgramToolsForSet(
		tools, toolSets, toolSet, application.config.programDeclarations,
		application.config.programToolsFile, processes,
	)
	if err != nil {
		return err
	}
	projectInstructions, err := loadInstructions(root, protection)
	if err != nil {
		return fmt.Errorf("load project instructions: %w", err)
	}
	instructions := effectiveInstructions(systemPrompt, root, projectInstructions)
	builder := runtimeBuilder{
		baseURL:           baseURL,
		modelAPI:          application.config.modelAPI,
		contextWindow:     contextWindow,
		credentials:       settings,
		metadataLookup:    application.config.metadataLookup,
		tools:             tools,
		programTools:      programTools,
		applicationBuild:  application.config.applicationBuild,
		toolSets:          toolSets,
		toolSet:           toolSet,
		processes:         processes,
		workspace:         root,
		requestPolicy:     modelRequestPolicy(retryBudget, streamIdleTimeout),
		maxToolIterations: maxToolIterations,
		scope:             security.snapshot(),
		awaitRequiredJobs: awaitRequiredJobs,
		sanitize:          masker.Redact,
		externalWork: applicationExternalWork{
			processes: processExternalWork{processes: processes, await: awaitRequiredJobs}, agents: children,
		},
	}
	// The session being replaced proves the protocol of the route it runs, so a
	// route this build does not declare survives clearing and resuming.
	selectionAPI := ""
	selectionContextWindow := 0
	if current := application.runtimeOrNil(); current != nil {
		info := current.CurrentModelInfo()
		if modelInfoMatchesURI(info, modelURI) {
			selectionAPI = string(modelAPIFromBackendID(info.BackendID))
			if !info.ContextWindowEstimated {
				selectionContextWindow = selectionModelContextWindow(modelURI, info.ContextWindow)
			}
		}
	}
	params := runtimeBuildParams{
		journal: journal, sessionID: id, modelURI: modelURI, reasoningEffort: reasoningEffort,
		modelSelectionAPI: selectionAPI, selectionContext: selectionContextWindow,
		instructions: instructions,
		resumedState: resumedState, knownModel: knownModel,
	}
	runtime, err := builder.buildRestored(ctx, params)
	if err != nil {
		return err
	}
	nextSession := newLiveSession(id, runtime, journal, true)
	if children != nil {
		if err := children.PreloadSession(ctx, id, instructions, security.snapshot()); err != nil {
			return fmt.Errorf("load child agents: %w", err)
		}
	}
	// AttachSession reports only failures which happen before adoption. Invalid
	// individual jobs become notices, so child preloading is the only work which
	// needs compensation when this call fails.
	if err := processes.AttachSession(id); err != nil {
		if children != nil {
			_ = children.ReleaseParent(id)
		}
		return fmt.Errorf("attach durable jobs: %w", err)
	}
	attachNotices := processes.AttachSessionNotices(id)
	application.mu.Lock()
	application.state.session = nextSession
	application.state.startupNotices = append(application.state.startupNotices, attachNotices...)
	if children != nil {
		children.setSessionDefaults(runtime.CurrentModel(), runtime.CurrentReasoningEffort(), params.selectionAPI(),
			params.selectionContextWindow(), instructions, security.snapshot())
	}
	application.mu.Unlock()

	var retireErr error
	if err := processes.CloseSession(currentRuntimeID); err != nil {
		retireErr = errors.Join(retireErr, fmt.Errorf("stop previous session jobs: %w", err))
	}
	if children != nil {
		retireErr = errors.Join(retireErr, children.ReleaseParent(currentRuntimeID))
	}
	retireErr = errors.Join(retireErr, currentSession.close())
	if retireErr != nil {
		detail := retireErr.Error()
		if application.config.masker != nil {
			detail = application.config.masker.Redact(detail)
		}
		notice := "switched session but could not fully release the previous session: " + detail
		application.mu.Lock()
		application.state.startupNotices = append(application.state.startupNotices, notice)
		application.mu.Unlock()
	}
	return nil
}

func (application *Application) Close() error {
	application.mu.Lock()
	currentSession := application.state.session
	processes := application.state.processes
	children := application.state.children
	application.state.session = nil
	application.state.processes = nil
	application.state.children = nil
	application.mu.Unlock()
	childErr := children.Close()
	sessionErr := currentSession.close()
	var processErr error
	if processes != nil {
		processErr = processes.Close()
	}
	return errors.Join(childErr, sessionErr, processErr)
}

func (application *Application) requireRuntime() (*agent.Runtime, error) {
	application.mu.RLock()
	currentSession := application.state.session
	application.mu.RUnlock()
	if currentSession == nil || currentSession.runtime == nil {
		return nil, errors.New("application is closed")
	}
	return currentSession.runtime, nil
}

func (application *Application) runtimeOrNil() *agent.Runtime {
	application.mu.RLock()
	defer application.mu.RUnlock()
	if application.state.session == nil {
		return nil
	}
	return application.state.session.runtime
}

func (application *Application) ScopeSummary() string {
	application.mu.RLock()
	security, processes := application.state.security, application.state.processes
	application.mu.RUnlock()
	summary := security.Summary()
	if processes == nil {
		return summary
	}
	scopes := processes.RunningScopes()
	var retained []string
	for _, scope := range []workspacetools.Scope{workspacetools.ScopeWorkspace, workspacetools.ScopeMachine} {
		if count := scopes[scope]; count > 0 && scope != security.Scope {
			retained = append(retained, fmt.Sprintf("%s (%d)", scope, count))
		}
	}
	if len(retained) != 0 {
		summary += " · running processes retain launch scope: " + strings.Join(retained, ", ")
	}
	return summary
}

// ScopeNotice describes a current filesystem-boundary limitation immediately
// after an interactive policy change. It is too noisy for ScopeSummary and for
// repeated startup output.
func (application *Application) ScopeNotice() string {
	application.mu.RLock()
	security := application.state.security
	application.mu.RUnlock()
	return protectedPathsNotice(security, application.config.root)
}

func (application *Application) SwitchScope(ctx context.Context, value string) error {
	application.filesystemMu.Lock()
	defer application.filesystemMu.Unlock()

	scope, err := workspacetools.NormalizeScope(value)
	if err != nil {
		return err
	}
	application.mu.RLock()
	if scope == application.state.security.Scope {
		application.mu.RUnlock()
		return application.persistInteractivePreference("filesystem scope", func(preferences *state.InteractiveStore) error {
			return preferences.SetScopeSelection(string(scope))
		})
	}
	additions := application.state.additions
	protection := application.state.protection
	application.mu.RUnlock()
	if err := application.applyFilesystemPolicy(ctx, scope, additions, protection); err != nil {
		return err
	}
	return application.persistInteractivePreference("filesystem scope", func(preferences *state.InteractiveStore) error {
		return preferences.SetScopeSelection(string(scope))
	})
}

// applyFilesystemPolicy switches the complete live policy as one transaction.
// filesystemMu must be held by the caller so future path mutations share the
// same ordering as scope changes.
func (application *Application) applyFilesystemPolicy(ctx context.Context, scope workspacetools.Scope, additions *workspacetools.AddedDirectoryPolicy, protection *workspacetools.ProtectedPathPolicy) error {
	application.mu.RLock()
	processes := application.state.processes
	currentSession := application.state.session
	application.mu.RUnlock()
	root := application.config.root
	if processes == nil || currentSession == nil || currentSession.runtime == nil {
		return fmt.Errorf("application is closed")
	}
	runtime := currentSession.runtime

	security := newSecurityState(scope, additions.Paths(), protection.Paths())
	// An interactive session may enable process tools later, so keep its process
	// boundary validated even while the active tool set does not need one.
	needsProcessBoundary := application.config.interactive != nil ||
		toolSetNeedsProcessBoundary(application.config.toolSets, application.config.programDeclarations, application.CurrentToolSet())
	if needsProcessBoundary {
		toolHome := ""
		if security.Scope == workspacetools.ScopeWorkspace {
			var err error
			toolHome, err = processes.ToolHome()
			if err != nil {
				return fmt.Errorf("cannot apply filesystem policy: %w", err)
			}
		}
		security = buildProcessSecurityState(ctx, security, root, toolHome)
	}
	if err := validateSecurity(security); err != nil {
		return fmt.Errorf("cannot apply filesystem policy: %w", err)
	}

	application.mu.Lock()
	if application.state.processes != processes || application.state.session != currentSession {
		application.mu.Unlock()
		return fmt.Errorf("runtime changed while applying the filesystem policy")
	}
	if err := processes.SetFilesystemPolicyAfter(security.Scope, additions, protection, func() error {
		previous := application.state.security.snapshot()
		if err := runtime.SetScopeSnapshot(ctx, security.snapshot()); err != nil {
			return err
		}
		if children := application.state.children; children != nil {
			if err := children.setScopeSnapshot(ctx, security.snapshot()); err != nil {
				return errors.Join(err, runtime.SetScopeSnapshot(context.WithoutCancel(ctx), previous))
			}
		}
		return nil
	}); err != nil {
		application.mu.Unlock()
		return err
	}
	application.state.security = security
	application.state.additions = additions
	application.state.protection = protection
	application.mu.Unlock()
	return nil
}
