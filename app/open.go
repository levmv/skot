package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/session"
	"github.com/levmv/skot/internal/state"
	"github.com/levmv/skot/internal/toolpolicy"
	workspacetools "github.com/levmv/skot/tools"
)

// Open assembles one concrete Skot application. It owns all returned session,
// process, and temporary resources until Application.Close.
func Open(ctx context.Context, config Config) (*Application, error) {
	build := currentBuildSnapshot(config.Version)
	if config.RetryBudget < 0 {
		return nil, agent.MarkInvalidRequest(errors.New("retry budget cannot be negative"))
	}
	if config.StreamIdleTimeout < 0 {
		return nil, agent.MarkInvalidRequest(errors.New("stream idle timeout cannot be negative"))
	}
	if config.MaxToolIterations < -1 {
		return nil, agent.MarkInvalidRequest(errors.New("max tool iterations must be positive or -1 for unlimited"))
	}
	if config.ContextWindow < 0 {
		return nil, agent.MarkInvalidRequest(errors.New("model context window cannot be negative"))
	}
	config.RetryBudget = effectiveRetryBudget(config.RetryBudget)
	config.StreamIdleTimeout = effectiveStreamIdleTimeout(config.StreamIdleTimeout)
	config.MaxToolIterations = effectiveMaxToolIterations(config.MaxToolIterations)
	if config.Resume && strings.TrimSpace(config.JournalPath) != "" {
		return nil, agent.MarkInvalidRequest(errors.New("resume cannot be combined with a journal path"))
	}
	if config.Resume && config.SaveSession {
		return nil, agent.MarkInvalidRequest(errors.New("resume cannot be combined with save session"))
	}
	modelAPIOverride, err := parseModelAPI(config.ModelAPI)
	if err != nil {
		return nil, agent.MarkInvalidRequest(err)
	}
	home, err := ResolveHome(config.Home)
	if err != nil {
		return nil, err
	}
	config.Home = home
	settingsStore, err := state.Open(home)
	if err != nil {
		return nil, fmt.Errorf("initialize Skot data: %w", err)
	}
	settings, err := settingsStore.Settings()
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	root, err := workspacetools.ResolveWorkspaceRoot(config.Root)
	if err != nil {
		return nil, agent.MarkInvalidRequest(fmt.Errorf("initialize workspace: %w", err))
	}
	var notices []string
	theme := state.ThemeAuto
	displayProfile := state.DisplayCompact
	var interactiveStore *state.InteractiveStore
	var workspaceSettings state.WorkspaceSettings
	var lastModel state.ModelPreference
	workspaceToolSet := false
	if config.Interactive {
		legacyKeys, err := settingsStore.LegacyInteractiveKeys()
		if err != nil {
			return nil, fmt.Errorf("inspect legacy settings: %w", err)
		}
		if len(legacyKeys) != 0 {
			notices = append(notices, "config.json contains ignored legacy interactive settings: "+strings.Join(legacyKeys, ", ")+"; remove them and choose the preferences again")
		}
		interactiveStore, err = state.OpenInteractive(home, root)
		if err != nil {
			return nil, fmt.Errorf("initialize interactive state: %w", err)
		}
		interactiveSettings, err := interactiveStore.Settings()
		if err != nil {
			return nil, fmt.Errorf("load interactive state: %w", err)
		}
		notices = append(notices, interactiveSettings.Notices...)
		if interactiveSettings.Theme != "" {
			theme = interactiveSettings.Theme
		}
		if interactiveSettings.Display != "" {
			displayProfile = interactiveSettings.Display
		}
		workspaceSettings = interactiveSettings.Workspace
		lastModel = interactiveSettings.LastModel()
		if !config.ToolSetExplicit && workspaceSettings.ToolSet != "" {
			config.ToolSet = workspaceSettings.ToolSet
			workspaceToolSet = true
		}
		if !config.ScopeExplicit && workspaceSettings.Scope != "" {
			config.Scope = workspaceSettings.Scope
		}
	}
	if strings.TrimSpace(config.ModelURI) == "" {
		config.ModelURI = DefaultModelURI
	}
	scope, err := workspacetools.NormalizeScope(config.Scope)
	if err != nil {
		return nil, agent.MarkInvalidRequest(err)
	}
	config.Scope = string(scope)

	layers, pathNotices, err := resolveFilesystemLayers(root, config.AddedPaths, config.ProtectedPaths, settings.ProtectedPaths, workspaceSettings)
	if err != nil {
		return nil, agent.MarkInvalidRequest(err)
	}
	notices = append(notices, pathNotices...)

	masker := newSecretMasker(settingsStore)
	security := newSecurityState(scope, layers.additions.Paths(), layers.protection.Paths())
	access, err := workspacetools.NewFilesystemAccess(root, security.Scope, layers.additions, layers.protection)
	if err != nil {
		return nil, agent.MarkInvalidRequest(fmt.Errorf("initialize filesystem policy: %w", err))
	}
	catalog, _, err := workspacetools.NewWorkspaceToolsWithAccess(access)
	if err != nil {
		return nil, agent.MarkInvalidRequest(fmt.Errorf("initialize workspace tools: %w", err))
	}
	projectInstructions, err := loadInstructions(root, layers.protection)
	if err != nil {
		return nil, agent.MarkInvalidRequest(fmt.Errorf("load project instructions: %w", err))
	}
	instructions := effectiveInstructions(config.SystemPrompt, root, projectInstructions)
	processes, err := workspacetools.NewProcessManagerWithAccess(access, home, "")
	if err != nil {
		return nil, agent.MarkInvalidRequest(fmt.Errorf("initialize process tools: %w", err))
	}
	resources := &openResources{processes: processes}
	processes.HideModelEnvironment(credentialEnvironmentNames()...)
	children, err := newChildSupervisor(home, settings.AgentModels, config.AgentModels)
	if err != nil {
		return resources.fail(agent.MarkInvalidRequest(err))
	}
	resources.children = children
	catalog = append(catalog, children.tool())
	// Derive this from platform and layout rather than the current scope so a
	// runtime scope switch does not silently change the active tool set.
	builtInOptions := toolpolicy.BuiltInOptions{
		DefaultIncludesLS: landlockProtectedPathsNeedBuiltInLS(workspacetools.BoundaryBackend(), layers.protection.Paths()),
	}
	builtCatalog, err := buildToolCatalog(config, settings, settingsStore, masker, catalog, processes, builtInOptions)
	if err != nil {
		return resources.fail(agent.MarkInvalidRequest(err))
	}
	catalog = builtCatalog.tools
	toolSets := builtCatalog.toolSets
	selectedToolSet, err := toolSets.Normalize(config.ToolSet)
	if err != nil && workspaceToolSet {
		notices = append(notices, fmt.Sprintf("invalid workspace tool_set %q for %s; ignored", config.ToolSet, root))
		selectedToolSet, err = toolSets.Normalize(ToolSetDefault)
	}
	if err != nil {
		return resources.fail(agent.MarkInvalidRequest(err))
	}
	config.ToolSet = selectedToolSet
	if config.Interactive || toolSetNeedsProcessBoundary(toolSets, builtCatalog.programDeclarations, config.ToolSet) {
		toolHome := ""
		if security.Scope == workspacetools.ScopeWorkspace {
			toolHome, err = processes.ToolHome()
			if err != nil {
				return resources.fail(agent.MarkInvalidRequest(err))
			}
		}
		security = buildProcessSecurityState(ctx, security, root, toolHome)
		if err := validateSecurity(security); err != nil {
			return resources.fail(agent.MarkInvalidRequest(err))
		}
	}
	catalog, programSnapshots, err := bindProgramToolsForSet(
		catalog, toolSets, config.ToolSet, builtCatalog.programDeclarations,
		builtCatalog.programToolsFile, processes,
	)
	if err != nil {
		return resources.fail(agent.MarkInvalidRequest(err))
	}

	memorySession := freshHeadlessMemorySession(config, toolSets)
	opened, err := openInitialSession(config, home, root, memorySession)
	if err != nil {
		return resources.fail(err)
	}
	managedID := ""
	if opened.managed {
		managedID = opened.id
	}
	resources.session = newLiveSession(managedID, nil, opened.journal, opened.managed)
	resources.session.provisional = opened.provisional
	resources.session.memory = opened.memory
	currentSession := resources.session
	journal := opened.journal
	sessionID := opened.id
	sessionState, _, err := agent.Reconcile(ctx, journal)
	if err != nil {
		return resources.fail(fmt.Errorf("reconcile session: %w", err))
	}
	runtimeSessionID := sessionID
	var resumedState *agent.State
	if runtimeSessionID == "" || config.Resume {
		resumedState = &sessionState
		if runtimeSessionID == "" {
			runtimeSessionID = sessionState.SessionID
		}
	}
	sessionModelSelected := false
	if !config.ModelExplicit {
		if resumedState != nil && resumedState.Selection.Model != "" {
			config.ModelURI = resumedState.Selection.Provider + "/" + resumedState.Selection.Model
			sessionModelSelected = true
			if !config.ReasoningEffortExplicit {
				config.ReasoningEffort = resumedState.Selection.ReasoningEffort
			}
		}
	}
	modelSelectionAPI := ""
	if !sessionModelSelected {
		rememberedAPI, rememberedNotices := applyRememberedModelPreference(&config, workspaceSettings, lastModel, root, modelAPIOverride)
		modelSelectionAPI = rememberedAPI
		notices = append(notices, rememberedNotices...)
	}
	var knownModel *agent.ModelInfo
	if resumedState != nil {
		if modelInfo, ok := restoredModelInfo(*resumedState, config.ModelURI); ok {
			knownModel = &modelInfo
		}
	}
	builder := runtimeBuilder{
		baseURL:           config.BaseURL,
		modelAPI:          modelAPIOverride,
		contextWindow:     config.ContextWindow,
		credentials:       settingsStore,
		metadataLookup:    openRouterContextWindow,
		tools:             catalog,
		programTools:      programSnapshots,
		applicationBuild:  build,
		toolSets:          toolSets,
		toolSet:           config.ToolSet,
		processes:         processes,
		workspace:         root,
		requestPolicy:     modelRequestPolicy(config.RetryBudget, config.StreamIdleTimeout),
		maxToolIterations: config.MaxToolIterations,
		scope:             security.snapshot(),
		awaitRequiredJobs: !config.Interactive,
		sanitize:          masker.Redact,
	}
	buildParams := runtimeBuildParams{
		journal:           journal,
		sessionID:         runtimeSessionID,
		modelURI:          config.ModelURI,
		reasoningEffort:   config.ReasoningEffort,
		modelSelectionAPI: modelSelectionAPI,
		instructions:      instructions,
		modelOptions: modelBackendOptions{
			requireCredential: !config.Interactive,
		},
		resumedState: resumedState,
		knownModel:   knownModel,
	}
	modelInfo, backend, err := builder.resolveRestored(ctx, buildParams)
	if err != nil {
		return resources.fail(err)
	}
	config.ReasoningEffort = modelInfo.ReasoningEffort
	if err := children.configure(builder, instructions, config.ModelURI, config.ReasoningEffort, buildParams.selectionAPI()); err != nil {
		return resources.fail(agent.MarkInvalidRequest(err))
	}
	if err := children.Preload(ctx, runtimeSessionID); err != nil {
		return resources.fail(fmt.Errorf("load child agents: %w", err))
	}
	builder.externalWork = applicationExternalWork{
		processes: processExternalWork{processes: processes, await: !config.Interactive}, agents: children,
	}
	runtime, err := builder.newRuntime(buildParams, modelInfo, backend)
	if err != nil {
		return resources.fail(err)
	}
	if err := processes.AttachSession(runtimeSessionID); err != nil {
		return resources.fail(fmt.Errorf("attach durable jobs: %w", err))
	}
	notices = append(notices, processes.AttachSessionNotices(runtimeSessionID)...)
	currentSession.runtime = runtime
	return &Application{
		config: applicationConfig{
			settings:                 settingsStore,
			interactive:              interactiveStore,
			tools:                    append([]agent.Tool(nil), catalog...),
			programDeclarations:      append([]workspacetools.ProgramTool(nil), builtCatalog.programDeclarations...),
			programToolsFile:         builtCatalog.programToolsFile,
			applicationBuild:         build,
			toolSets:                 toolSets,
			systemPrompt:             config.SystemPrompt,
			root:                     root,
			home:                     home,
			invocationAddedPaths:     layers.invocationAdded,
			invocationProtectedPaths: layers.invocationProtected,
			settingsProtectedPaths:   layers.settingsProtected,
			baseURL:                  config.BaseURL,
			modelAPI:                 modelAPIOverride,
			contextWindow:            config.ContextWindow,
			metadataLookup:           openRouterContextWindow,
			retryBudget:              config.RetryBudget,
			streamIdleTimeout:        config.StreamIdleTimeout,
			maxToolIterations:        config.MaxToolIterations,
			masker:                   masker,
			awaitRequiredJobs:        !config.Interactive,
		},
		state: applicationState{
			session:                 currentSession,
			processes:               processes,
			children:                children,
			toolSet:                 config.ToolSet,
			theme:                   theme,
			displayProfile:          displayProfile,
			security:                security,
			additions:               layers.additions,
			protection:              layers.protection,
			workspaceAddedPaths:     layers.workspaceAdded,
			workspaceProtectedPaths: layers.workspaceProtected,
			startupNotices:          notices,
		},
	}, nil
}

// applyRememberedModelPreference resolves the interactive model fallback. A
// workspace record is a deliberate choice for that workspace and wins; the
// shared last selection only fills in for a workspace which has never made one.
func applyRememberedModelPreference(config *Config, workspace state.WorkspaceSettings, last state.ModelPreference, root string, api modelAPI) (string, []string) {
	if config.ModelExplicit {
		return "", nil
	}
	if strings.TrimSpace(workspace.Model) != "" {
		preference := state.ModelPreference{
			Model: workspace.Model, ReasoningEffort: workspace.ReasoningEffort, ModelAPI: workspace.ModelAPI,
		}
		return applyModelPreference(config, preference, "workspace", root, api)
	}
	return applyModelPreference(config, last, "remembered", "", api)
}

// kind and root name the invalid value in a notice: a workspace preference is
// reported with the path whose record must be corrected.
func applyModelPreference(config *Config, preference state.ModelPreference, kind, root string, api modelAPI) (string, []string) {
	if strings.TrimSpace(preference.Model) == "" {
		return "", nil
	}
	location := ""
	if root != "" {
		location = " for " + root
	}
	effort := config.ReasoningEffort
	if !config.ReasoningEffortExplicit && preference.ReasoningEffort != nil {
		effort = *preference.ReasoningEffort
	}
	selectionAPI := string(selectionModelAPI(preference.Model, preference.ModelAPI))
	if config.ReasoningEffortExplicit {
		config.ModelURI = preference.Model
		return selectionAPI, nil
	}
	overrides := modelRouteOverrides{
		BaseURL: config.BaseURL, API: api, ContextWindow: config.ContextWindow,
	}.withSelection(preference.Model, selectionAPI)
	_, err := resolveModelRoute(preference.Model, effort, overrides, modelRouteEnrichment{})
	if err == nil {
		config.ModelURI = preference.Model
		config.ReasoningEffort = effort
		return selectionAPI, nil
	}
	// A remembered effort the route rejects must not disqualify the model itself.
	if preference.ReasoningEffort != nil {
		if _, fallbackErr := resolveModelRoute(preference.Model, "", overrides, modelRouteEnrichment{}); fallbackErr == nil {
			config.ModelURI = preference.Model
			config.ReasoningEffort = ""
			return selectionAPI, []string{fmt.Sprintf("invalid %s reasoning_effort %q%s; ignored: %v", kind, effort, location, err)}
		}
	}
	return "", []string{fmt.Sprintf("invalid %s model preference %q%s; ignored: %v", kind, preference.Model, location, err)}
}

func modelRequestPolicy(retryBudget, streamIdleTimeout time.Duration) agent.ModelRequestPolicy {
	return agent.ModelRequestPolicy{
		MaxAttempts:       -1,
		RetryBudget:       retryBudget,
		BaseDelay:         time.Second,
		MaxDelay:          time.Minute,
		StreamIdleTimeout: streamIdleTimeout,
	}
}

func effectiveRetryBudget(value time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return DefaultRetryBudget
}

func effectiveMaxToolIterations(value int) int {
	if value == 0 {
		return agent.DefaultMaxToolIterations
	}
	return value
}

func effectiveStreamIdleTimeout(value time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return DefaultStreamIdleTimeout
}

func (application *Application) Root() string {
	return application.config.root
}

func (application *Application) StartupNotices() []string {
	application.mu.RLock()
	defer application.mu.RUnlock()
	return append([]string(nil), application.state.startupNotices...)
}

func (application *Application) ShortSessionID() string {
	return ShortSessionID(application.SessionID())
}

func ShortSessionID(id string) string { return session.ShortID(strings.TrimSpace(id)) }
