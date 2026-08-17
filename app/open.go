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
	home, err := session.ResolveHome(config.Home)
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
	var interactiveStore *state.InteractiveStore
	var workspaceSettings state.WorkspaceSettings
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
		workspaceSettings = interactiveSettings.Workspace
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

	configuredProtectedPaths := append([]string(nil), settings.ProtectedPaths...)
	configuredProtectedPaths = append(configuredProtectedPaths, config.ProtectedPaths...)
	protection, err := workspacetools.NewProtectedPathPolicy(root, configuredProtectedPaths)
	if err != nil {
		return nil, agent.MarkInvalidRequest(fmt.Errorf("initialize protected paths: %w", err))
	}

	masker := newSecretMasker(settingsStore)
	security := resolveSecurityState(ctx, scope, protection.Paths())
	access, err := workspacetools.NewFilesystemAccess(root, security.EffectiveScope, protection)
	if err != nil {
		return nil, agent.MarkInvalidRequest(fmt.Errorf("initialize filesystem policy: %w", err))
	}
	catalog, _, err := workspacetools.NewWorkspaceToolsWithAccess(access)
	if err != nil {
		return nil, agent.MarkInvalidRequest(fmt.Errorf("initialize workspace tools: %w", err))
	}
	projectInstructions, err := loadInstructions(root, protection)
	if err != nil {
		return nil, agent.MarkInvalidRequest(fmt.Errorf("load project instructions: %w", err))
	}
	instructions := effectiveInstructions(config.SystemPrompt, root, projectInstructions)
	processes, err := workspacetools.NewProcessManagerWithAccess(access, home, "")
	if err != nil {
		return nil, agent.MarkInvalidRequest(fmt.Errorf("initialize process tools: %w", err))
	}
	processes.HideModelEnvironment(credentialEnvironmentNames()...)
	children, err := newChildSupervisor(home, settings.AgentModels, config.AgentModels)
	if err != nil {
		_ = processes.Close()
		return nil, agent.MarkInvalidRequest(err)
	}
	resources := &openResources{processes: processes, children: children}
	catalog = append(catalog, children.tool())
	// Derive this from platform and layout rather than the current scope so a
	// runtime scope switch does not silently change the active tool set.
	builtInOptions := toolpolicy.BuiltInOptions{
		DefaultIncludesLS: landlockProtectedPathsNeedBuiltInLS(workspacetools.BoundaryBackend(), protection.Paths()),
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
		if security.EffectiveScope == workspacetools.ScopeWorkspace {
			toolHome, err = processes.ToolHome()
			if err != nil {
				return resources.fail(agent.MarkInvalidRequest(err))
			}
		}
		security = buildProcessSecurityState(ctx, security, root, toolHome, protection.Paths())
		if err := validateSecurity(security); err != nil {
			return resources.fail(agent.MarkInvalidRequest(err))
		}
	}
	if notice := protectedPathsNotice(security, root, protection.Paths()); notice != "" {
		notices = append(notices, notice)
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
	managedID := ""
	if opened.managed {
		managedID = opened.id
	}
	resources.session = newLiveSession(managedID, nil, opened.journal, opened.managed)
	resources.session.provisional = opened.provisional
	resources.session.memory = opened.memory
	if err != nil {
		return resources.fail(err)
	}
	currentSession := resources.session
	journal := opened.journal
	sessionID := opened.id
	cleanup := resources.fail
	if journal.TailRepaired() {
		notices = append(notices, "repaired an incomplete journal tail; the interrupted final record was discarded")
	}
	if err := agent.Reconcile(ctx, journal); err != nil {
		return cleanup(fmt.Errorf("reconcile session: %w", err))
	}
	runtimeSessionID := sessionID
	var replayedState *agent.State
	if runtimeSessionID == "" || config.Resume {
		records, err := journal.Records(ctx)
		if err != nil {
			return cleanup(fmt.Errorf("read resumed session: %w", err))
		}
		replayed, err := agent.Replay(records)
		if err != nil {
			return cleanup(fmt.Errorf("replay resumed session: %w", err))
		}
		replayedState = &replayed
		if runtimeSessionID == "" {
			runtimeSessionID = replayed.SessionID
		}
	}
	sessionModelSelected := false
	if !config.ModelExplicit {
		if replayedState != nil && replayedState.Selection.Model != "" {
			config.ModelURI = replayedState.Selection.Provider + "/" + replayedState.Selection.Model
			sessionModelSelected = true
			if !config.ReasoningEffortExplicit {
				config.ReasoningEffort = replayedState.Selection.ReasoningEffort
			}
		}
	}
	if !sessionModelSelected {
		notices = append(notices, applyWorkspaceModelPreference(&config, workspaceSettings, root, modelAPIOverride)...)
	}
	initialRoute, err := resolveModelRoute(config.ModelURI, config.ReasoningEffort, modelRouteOverrides{
		BaseURL: config.BaseURL, API: modelAPIOverride, ContextWindow: config.ContextWindow,
	}, modelRouteEnrichment{})
	if err != nil {
		return cleanup(agent.MarkInvalidRequest(err))
	}
	config.ReasoningEffort = initialRoute.ReasoningEffort
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
	if err := children.configure(builder, instructions, config.ModelURI, config.ReasoningEffort); err != nil {
		return cleanup(agent.MarkInvalidRequest(err))
	}
	if err := children.Preload(ctx, runtimeSessionID); err != nil {
		return cleanup(fmt.Errorf("load child agents: %w", err))
	}
	builder.externalWork = applicationExternalWork{
		processes: processExternalWork{processes: processes, await: !config.Interactive}, agents: children,
	}
	runtime, _, err := builder.buildWithRoute(ctx, runtimeBuildParams{
		journal:         journal,
		sessionID:       runtimeSessionID,
		modelURI:        config.ModelURI,
		reasoningEffort: config.ReasoningEffort,
		instructions:    instructions,
		modelOptions: modelBackendOptions{
			requireCredential: !config.Interactive,
		},
		resumedState: replayedState,
	})
	if err != nil {
		return cleanup(err)
	}
	if err := processes.AttachSession(runtimeSessionID); err != nil {
		return cleanup(fmt.Errorf("attach durable jobs: %w", err))
	}
	notices = append(notices, processes.AttachSessionNotices(runtimeSessionID)...)
	currentSession.runtime = runtime
	return &Application{
		config: applicationConfig{
			settings:            settingsStore,
			interactive:         interactiveStore,
			tools:               append([]agent.Tool(nil), catalog...),
			programDeclarations: append([]workspacetools.ProgramTool(nil), builtCatalog.programDeclarations...),
			programToolsFile:    builtCatalog.programToolsFile,
			applicationBuild:    build,
			toolSets:            toolSets,
			systemPrompt:        config.SystemPrompt,
			root:                root,
			home:                home,
			protectedPaths:      protection.Paths(),
			protection:          protection,
			baseURL:             config.BaseURL,
			modelAPI:            modelAPIOverride,
			contextWindow:       config.ContextWindow,
			metadataLookup:      openRouterContextWindow,
			retryBudget:         config.RetryBudget,
			streamIdleTimeout:   config.StreamIdleTimeout,
			maxToolIterations:   config.MaxToolIterations,
			masker:              masker,
			awaitRequiredJobs:   !config.Interactive,
		},
		state: applicationState{
			session:        currentSession,
			processes:      processes,
			children:       children,
			toolSet:        config.ToolSet,
			requestedScope: scope,
			theme:          theme,
			security:       security,
			startupNotices: notices,
		},
	}, nil
}

func applyWorkspaceModelPreference(config *Config, workspace state.WorkspaceSettings, root string, api modelAPI) []string {
	if config.ModelExplicit || strings.TrimSpace(workspace.Model) == "" {
		return nil
	}
	effort := config.ReasoningEffort
	if !config.ReasoningEffortExplicit && workspace.ReasoningEffort != nil {
		effort = *workspace.ReasoningEffort
	}
	if config.ReasoningEffortExplicit {
		config.ModelURI = workspace.Model
		return nil
	}
	overrides := modelRouteOverrides{BaseURL: config.BaseURL, API: api, ContextWindow: config.ContextWindow}
	if _, err := resolveModelRoute(workspace.Model, effort, overrides, modelRouteEnrichment{}); err == nil {
		config.ModelURI = workspace.Model
		config.ReasoningEffort = effort
		return nil
	} else if workspace.ReasoningEffort != nil {
		if _, fallbackErr := resolveModelRoute(workspace.Model, "", overrides, modelRouteEnrichment{}); fallbackErr == nil {
			config.ModelURI = workspace.Model
			config.ReasoningEffort = ""
			return []string{fmt.Sprintf("invalid workspace reasoning_effort %q for %s; ignored: %v", effort, root, err)}
		}
		return []string{fmt.Sprintf("invalid workspace model preference %q for %s; ignored: %v", workspace.Model, root, err)}
	} else {
		return []string{fmt.Sprintf("invalid workspace model preference %q for %s; ignored: %v", workspace.Model, root, err)}
	}
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

func ResolveHome(value string) (string, error) { return session.ResolveHome(value) }

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
