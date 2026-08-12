package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/session"
	"github.com/levmv/skot/internal/state"
	"github.com/levmv/skot/internal/toolpolicy"
	"github.com/levmv/skot/model/chatcompletions"
	workspacetools "github.com/levmv/skot/tools"
)

// Application is the concrete, UI-neutral Skot composition root. Its current
// live session owns one agent runtime and its persistence lifetime; clearing or
// resuming replaces that session as a unit. This pre-v1 API is intentionally
// product-specific and is not yet a compatibility promise.
type Application struct {
	config applicationConfig

	mu        sync.RWMutex
	sandboxMu sync.Mutex
	state     applicationState
}

// applicationConfig is immutable after Open returns. In particular, session
// replacement must not make callers retain a stale runtime, but it does not
// change the stores, catalog, workspace, or runtime policy used to build one.
type applicationConfig struct {
	settings          *state.Store
	tools             []agent.Tool
	programTools      []agent.ProgramToolSnapshot
	profiles          toolpolicy.Profiles
	systemPrompt      string
	root              string
	home              string
	toolHome          string
	protectedPaths    []string
	protection        *workspacetools.ProtectedPathPolicy
	baseURL           string
	contextWindow     int
	retryBudget       time.Duration
	streamIdleTimeout time.Duration
	maxToolIterations int
	awaitRequiredJobs bool
	masker            *secretMasker
}

// applicationState is protected by Application.mu. It contains exactly the
// state that can change after Open, including resources cleared by Close.
type applicationState struct {
	session          *liveSession
	processes        *workspacetools.ProcessManager
	profile          string
	requestedSandbox string
	security         securityState
	startupNotices   []string
	temporaryDir     string
}

func (application *Application) Run(ctx context.Context, input string, emit agent.EmitFunc) (agent.RunResult, error) {
	runtime, err := application.requireRuntime()
	if err != nil {
		return agent.RunResult{}, err
	}
	uri := runtime.CurrentModel()
	provider, _, err := parseModelURI(uri)
	if err != nil {
		return agent.RunResult{}, err
	}
	spec, _ := modelProviderSpec(provider)
	baseURL := strings.TrimSpace(application.config.baseURL)
	settings := application.config.settings
	if baseURL == "" && !spec.credentialless {
		token, _, err := credentialForProvider(settings, provider)
		if err != nil {
			return agent.RunResult{}, err
		}
		if token == "" {
			return agent.RunResult{}, agent.MarkInvalidRequest(missingProviderCredentialError(provider, uri))
		}
	}
	return runtime.Run(ctx, input, emit)
}

func (application *Application) SwitchModel(ctx context.Context, uri, effort string) error {
	baseURL := application.config.baseURL
	contextWindow := application.config.contextWindow
	effort, err := normalizeReasoningEffort(uri, effort)
	if err != nil {
		return agent.MarkInvalidRequest(err)
	}
	model, err := buildModelBackend(uri, effort, baseURL, contextWindow, application.config.settings, modelBackendOptions{
		requireCredential: true,
		refreshContext:    true,
	})
	if err != nil {
		return err
	}
	runtime, err := application.requireRuntime()
	if err != nil {
		return err
	}
	if err := runtime.SwitchModel(ctx, model); err != nil {
		return err
	}
	if err := application.config.settings.SetDefaultModelSelection(runtime.CurrentModel(), runtime.CurrentReasoningEffort()); err != nil {
		return fmt.Errorf("save model selection: %w", err)
	}
	return nil
}

func (application *Application) CurrentReasoningEffort() string {
	runtime := application.runtimeOrNil()
	if runtime == nil {
		return ""
	}
	return runtime.CurrentReasoningEffort()
}

func (application *Application) ReasoningEfforts(uri string) []string {
	return reasoningEffortsForModel(uri)
}

func (application *Application) KnownModels() []string {
	return knownModels(application.config.settings, application.CurrentModel())
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

func (application *Application) RestoreQueued() []string {
	runtime := application.runtimeOrNil()
	if runtime == nil {
		return nil
	}
	return runtime.RestoreQueued()
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

func (application *Application) CurrentProfile() string {
	application.mu.RLock()
	defer application.mu.RUnlock()
	return application.state.profile
}

func (application *Application) Profiles() []string {
	return application.config.profiles.Names()
}

func (application *Application) SwitchProfile(ctx context.Context, value string) error {
	runtime, err := application.requireRuntime()
	if err != nil {
		return err
	}
	catalog := append([]agent.Tool(nil), application.config.tools...)
	profiles := application.config.profiles
	settings := application.config.settings
	application.mu.RLock()
	oldProfile := application.state.profile
	application.mu.RUnlock()
	profile, err := profiles.Normalize(value)
	if err != nil {
		return err
	}
	selected, err := profileTools(profiles, catalog, settings, profile)
	if err != nil {
		return err
	}
	if err := runtime.SetTools(ctx, selected, profile); err != nil {
		return err
	}
	if err := settings.SetDefaultProfile(profile); err != nil {
		previous, selectErr := profileTools(profiles, catalog, settings, oldProfile)
		rollbackErr := selectErr
		if selectErr == nil {
			rollbackErr = runtime.SetTools(context.WithoutCancel(ctx), previous, oldProfile)
		}
		if rollbackErr != nil {
			rollbackErr = fmt.Errorf("restore previous tool profile: %w", rollbackErr)
		}
		return errors.Join(fmt.Errorf("save profile: %w", err), rollbackErr)
	}
	application.mu.Lock()
	if application.state.session == nil || application.state.session.runtime != runtime {
		application.mu.Unlock()
		return errors.New("runtime changed while switching profile")
	}
	application.state.profile = profile
	application.mu.Unlock()
	return nil
}

func (application *Application) ContextReport(ctx context.Context) (agent.ContextReport, error) {
	runtime, err := application.requireRuntime()
	if err != nil {
		return agent.ContextReport{}, err
	}
	return runtime.ContextReport(ctx)
}

func (application *Application) Compact(ctx context.Context, keepRecent int) (agent.ContextCompactedRecord, error) {
	runtime, err := application.requireRuntime()
	if err != nil {
		return agent.ContextCompactedRecord{}, err
	}
	return runtime.Compact(ctx, keepRecent)
}

func (application *Application) CurrentSandbox() string {
	application.mu.RLock()
	defer application.mu.RUnlock()
	return application.state.requestedSandbox
}

func buildModelBackend(uri, reasoningEffort, baseURL string, contextWindow int, credentials *state.Store, options modelBackendOptions) (agent.Model, error) {
	provider, model, err := parseModelURI(uri)
	if err != nil {
		return nil, agent.MarkInvalidRequest(err)
	}
	spec, err := modelProviderSpec(provider)
	if err != nil {
		return nil, agent.MarkInvalidRequest(err)
	}
	baseURL = strings.TrimSpace(baseURL)
	customEndpoint := baseURL != ""
	if customEndpoint {
		spec.baseURL = baseURL
	}
	if options.requireCredential && !customEndpoint && !spec.credentialless {
		token, _, err := credentialForProvider(credentials, provider)
		if err != nil {
			return nil, err
		}
		if token == "" {
			return nil, agent.MarkInvalidRequest(missingProviderCredentialError(provider, uri))
		}
	}
	contextWindowEstimated := false
	if contextWindow == 0 {
		if customEndpoint {
			// A compatible endpoint may serve a model with the same URI but a
			// different limit. Do not mistake public-provider metadata for a
			// guarantee; -context-window remains the explicit override.
			contextWindow = unknownModelContextWindow
			contextWindowEstimated = true
		} else {
			modelSpec := resolveModelSpec(uri, credentials, options.refreshContext)
			contextWindow = modelSpec.ContextWindow
			contextWindowEstimated = modelSpec.Estimated
		}
	}
	apiModel := model
	if provider == "openrouter" {
		apiModel = canonicalOpenRouterModelID(model)
	}
	var authorizer chatcompletions.Authorizer = storedBearerAuthorizer{
		store: credentials, provider: provider, modelURI: uri, allowMissing: customEndpoint,
	}
	if spec.credentialless {
		// Ollama ignores the token, while OpenAI-compatible clients conventionally
		// send a non-empty placeholder.
		authorizer = chatcompletions.BearerToken(provider)
	}
	backend, err := chatcompletions.New(chatcompletions.Config{
		Provider:               provider,
		Model:                  model,
		APIModel:               apiModel,
		ReasoningEffort:        reasoningEffort,
		ContextWindow:          contextWindow,
		ContextWindowEstimated: contextWindowEstimated,
		BaseURL:                spec.baseURL,
		Authorizer:             authorizer,
		Header:                 spec.header,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize model: %w", err)
	}
	return backend, nil
}

func (application *Application) ProviderStatuses() ([]ProviderStatus, error) {
	return providerStatuses(application.config.settings)
}

func (application *Application) Login(ctx context.Context, provider, token string) error {
	masker := application.config.masker
	if masker == nil {
		return errors.New("application is closed")
	}
	if err := application.updateCredential(ctx, provider, "login", func(settings *state.Store, normalizedProvider string) error {
		return storeProviderCredential(settings, normalizedProvider, token)
	}); err != nil {
		return err
	}
	masker.Add(token)
	return nil
}

func (application *Application) Logout(ctx context.Context, provider string) error {
	return application.updateCredential(ctx, provider, "logout", deleteProviderCredential)
}

func (application *Application) updateCredential(ctx context.Context, provider, operation string, mutate func(*state.Store, string) error) error {
	if _, err := application.requireRuntime(); err != nil {
		return err
	}
	settings := application.config.settings
	if settings == nil {
		return errors.New("application is closed")
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	previousAvailable, err := workspacetools.WebSearchAvailable(webCredentialLookup(settings))
	if err != nil {
		return err
	}
	previousToken, previousStored, err := settings.APIKey(provider)
	if err != nil {
		return err
	}
	if err := mutate(settings, provider); err != nil {
		return err
	}
	rollback := func(cause error) error {
		return errors.Join(cause, restoreStoredCredential(settings, provider, previousToken, previousStored))
	}
	nextAvailable, err := workspacetools.WebSearchAvailable(webCredentialLookup(settings))
	if err != nil {
		return rollback(err)
	}
	if previousAvailable != nextAvailable {
		if err := application.reloadActiveProfileTools(ctx); err != nil {
			return rollback(fmt.Errorf("reload tools after %s: %w", operation, err))
		}
	}
	return nil
}

func (application *Application) reloadActiveProfileTools(ctx context.Context) error {
	runtime, err := application.requireRuntime()
	if err != nil {
		return err
	}
	tools := append([]agent.Tool(nil), application.config.tools...)
	profiles, settings := application.config.profiles, application.config.settings
	application.mu.RLock()
	profile := application.state.profile
	application.mu.RUnlock()
	selected, err := profileTools(profiles, tools, settings, profile)
	if err != nil {
		return err
	}
	return runtime.SetTools(ctx, selected, profile)
}

func restoreStoredCredential(store *state.Store, provider, token string, existed bool) error {
	if existed {
		return store.SetAPIKey(provider, token)
	}
	return store.DeleteAPIKey(provider)
}

func (application *Application) SessionID() string {
	application.mu.RLock()
	defer application.mu.RUnlock()
	if application.state.session == nil {
		return ""
	}
	return application.state.session.id
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
	if err := application.installSession(ctx, journal, id, application.CurrentModel(), application.CurrentReasoningEffort()); err != nil {
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
	if err := agent.Reconcile(ctx, journal); err != nil {
		_ = journal.Close()
		return "", fmt.Errorf("reconcile session: %w", err)
	}
	records, err := journal.Records(ctx)
	if err != nil {
		_ = journal.Close()
		return "", fmt.Errorf("read session: %w", err)
	}
	replayed, err := agent.Replay(records)
	if err != nil {
		_ = journal.Close()
		return "", fmt.Errorf("replay session: %w", err)
	}
	modelURI := application.CurrentModel()
	reasoningEffort := application.CurrentReasoningEffort()
	if replayed.Selection.Model != "" {
		provider := replayed.Selection.Provider
		if provider == "" {
			provider = replayed.Selection.Backend
		}
		modelURI = provider + "/" + replayed.Selection.Model
		reasoningEffort = replayed.Selection.ReasoningEffort
	}
	if err := application.installSession(ctx, journal, summary.ID, modelURI, reasoningEffort); err != nil {
		_ = journal.Close()
		return "", err
	}
	return summary.ID, nil
}

func (application *Application) installSession(ctx context.Context, journal *session.Store, id, modelURI, reasoningEffort string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	baseURL, contextWindow := application.config.baseURL, application.config.contextWindow
	retryBudget, streamIdleTimeout := application.config.retryBudget, application.config.streamIdleTimeout
	maxToolIterations := application.config.maxToolIterations
	settings, masker := application.config.settings, application.config.masker
	root, systemPrompt := application.config.root, application.config.systemPrompt
	tools := append([]agent.Tool(nil), application.config.tools...)
	programTools := append([]agent.ProgramToolSnapshot(nil), application.config.programTools...)
	profiles := application.config.profiles
	awaitRequiredJobs := application.config.awaitRequiredJobs
	application.mu.RLock()
	processes := application.state.processes
	security := application.state.security
	profile := application.state.profile
	currentSession := application.state.session
	application.mu.RUnlock()
	if currentSession == nil || settings == nil || processes == nil {
		return errors.New("application is closed")
	}
	projectInstructions, err := loadInstructions(root, application.config.protection)
	if err != nil {
		return fmt.Errorf("load project instructions: %w", err)
	}
	instructions := effectiveInstructions(systemPrompt, projectInstructions)
	if err := processes.AttachSession(id); err != nil {
		return fmt.Errorf("attach durable jobs: %w", err)
	}
	attachNotices := processes.AttachSessionNotices(id)
	builder := runtimeBuilder{
		baseURL:           baseURL,
		contextWindow:     contextWindow,
		credentials:       settings,
		tools:             tools,
		programTools:      programTools,
		profiles:          profiles,
		profile:           profile,
		processes:         processes,
		workspace:         root,
		requestPolicy:     modelRequestPolicy(retryBudget, streamIdleTimeout),
		maxToolIterations: maxToolIterations,
		sandbox:           security.snapshot(),
		awaitRequiredJobs: awaitRequiredJobs,
		sanitize:          masker.Redact,
	}
	runtime, err := builder.build(runtimeBuildParams{
		journal: journal, sessionID: id, modelURI: modelURI, reasoningEffort: reasoningEffort, instructions: instructions,
	})
	if err != nil {
		return err
	}
	nextSession := newLiveSession(id, runtime, journal, true)
	if err := processes.CloseSession(currentSession.id); err != nil {
		return fmt.Errorf("stop previous session jobs: %w", err)
	}

	application.mu.Lock()
	if application.state.session != currentSession {
		application.mu.Unlock()
		return errors.New("runtime changed while switching session")
	}
	application.state.session = nextSession
	application.state.startupNotices = append(application.state.startupNotices, attachNotices...)
	application.mu.Unlock()
	_ = currentSession.close()
	return nil
}

func (application *Application) Close() error {
	application.mu.Lock()
	currentSession := application.state.session
	processes, temporaryDir := application.state.processes, application.state.temporaryDir
	application.state.session = nil
	application.state.processes = nil
	application.state.temporaryDir = ""
	application.mu.Unlock()
	sessionErr := currentSession.close()
	var processErr error
	if processes != nil {
		processErr = processes.Close()
	}
	var temporaryErr error
	if temporaryDir != "" {
		temporaryErr = os.RemoveAll(temporaryDir)
	}
	return errors.Join(sessionErr, processErr, temporaryErr)
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

func (application *Application) SecuritySummary() string {
	application.mu.RLock()
	security, processes := application.state.security, application.state.processes
	application.mu.RUnlock()
	summary := security.Summary()
	if processes == nil {
		return summary
	}
	policies := processes.RunningSandboxPolicies()
	var retained []string
	for _, policy := range []string{workspacetools.SandboxWorkspace, workspacetools.SandboxMasked, workspacetools.SandboxOff} {
		if count := policies[policy]; count > 0 && policy != security.EffectivePolicy {
			retained = append(retained, fmt.Sprintf("%s (%d)", policy, count))
		}
	}
	if len(retained) != 0 {
		summary += " · running processes retain launch policy: " + strings.Join(retained, ", ")
	}
	return summary
}

func (application *Application) SwitchSandbox(ctx context.Context, value string) error {
	application.sandboxMu.Lock()
	defer application.sandboxMu.Unlock()

	policy, err := workspacetools.NormalizeSandboxPolicy(value)
	if err != nil {
		return err
	}
	application.mu.RLock()
	if policy == application.state.requestedSandbox {
		application.mu.RUnlock()
		return nil
	}
	processes := application.state.processes
	currentSession := application.state.session
	oldPolicy := application.state.requestedSandbox
	application.mu.RUnlock()
	settings := application.config.settings
	root := application.config.root
	home := application.config.home
	toolHome := application.config.toolHome
	protectedPaths := application.config.protectedPaths
	if processes == nil || settings == nil || currentSession == nil || currentSession.runtime == nil {
		return fmt.Errorf("application is closed")
	}
	runtime := currentSession.runtime

	security := buildSecurityStateWithToolHome(ctx, policy, root, home, toolHome, protectedPaths)
	if err := validateSecurity(security); err != nil {
		return fmt.Errorf("cannot switch sandbox: %w", err)
	}

	application.mu.Lock()
	defer application.mu.Unlock()
	if application.state.processes != processes || application.state.session != currentSession {
		return fmt.Errorf("runtime changed while switching sandbox")
	}
	if err := settings.SetDefaultSandbox(policy); err != nil {
		return fmt.Errorf("save sandbox policy: %w", err)
	}
	if err := processes.SetSandboxAfter(security.EffectivePolicy, func() error {
		return runtime.SetSandboxSnapshot(ctx, security.snapshot())
	}); err != nil {
		settingsErr := settings.SetDefaultSandbox(oldPolicy)
		return errors.Join(err, settingsErr)
	}
	application.state.requestedSandbox = policy
	application.state.security = security
	return nil
}
