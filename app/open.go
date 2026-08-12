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
	workspacetools "github.com/levmv/skot/tools"
)

// Open assembles one concrete Skot application. It owns all returned session,
// process, and temporary resources until Application.Close.
func Open(ctx context.Context, config Config) (*Application, error) {
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
	home, err := session.ResolveHome(config.Home)
	if err != nil {
		return nil, err
	}
	config.Home = home
	toolHomeRoot, err := workspacetools.DefaultToolHomeRoot()
	if err != nil {
		return nil, agent.MarkInvalidRequest(err)
	}
	settingsStore, err := state.Open(home)
	if err != nil {
		return nil, fmt.Errorf("initialize local state: %w", err)
	}
	settings, err := settingsStore.Settings()
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	if !config.ModelExplicit && strings.TrimSpace(settings.Model) != "" {
		config.ModelURI = settings.Model
		if !config.ReasoningEffortExplicit {
			config.ReasoningEffort = settings.ReasoningEffort
		}
	}
	if !config.ProfileExplicit && strings.TrimSpace(settings.Profile) != "" {
		config.Profile = settings.Profile
	}
	if !config.SandboxExplicit && strings.TrimSpace(settings.Sandbox) != "" {
		config.Sandbox = settings.Sandbox
	}
	if strings.TrimSpace(config.ModelURI) == "" {
		config.ModelURI = DefaultModelURI
	}
	config.Sandbox, err = workspacetools.NormalizeSandboxPolicy(config.Sandbox)
	if err != nil {
		return nil, agent.MarkInvalidRequest(err)
	}

	configuredProtectedPaths := append([]string(nil), settings.ProtectedPaths...)
	configuredProtectedPaths = append(configuredProtectedPaths, config.ProtectedPaths...)
	configuredProtectedPaths = append([]string{home}, configuredProtectedPaths...)
	protection, err := workspacetools.NewProtectedPathPolicy(config.Root, configuredProtectedPaths, false)
	if err != nil {
		return nil, agent.MarkInvalidRequest(fmt.Errorf("initialize protected paths: %w", err))
	}

	masker := newSecretMasker(settingsStore)
	catalog, root, err := workspacetools.NewWorkspaceToolsWithProtection(config.Root, protection)
	if err != nil {
		return nil, agent.MarkInvalidRequest(fmt.Errorf("initialize workspace tools: %w", err))
	}
	toolHome := workspacetools.WorkspaceToolHome(toolHomeRoot, root)
	security := buildSecurityStateWithToolHome(ctx, config.Sandbox, root, home, toolHome, protection.Paths())
	if err := validateSecurity(security); err != nil {
		return nil, agent.MarkInvalidRequest(fmt.Errorf("%w; choose -sandbox off explicitly to run without it", err))
	}
	var notices []string
	protection.SetEnabled(security.EffectivePolicy != workspacetools.SandboxOff)
	projectInstructions, err := loadInstructions(root, protection)
	if err != nil {
		return nil, agent.MarkInvalidRequest(fmt.Errorf("load project instructions: %w", err))
	}
	instructions := effectiveInstructions(config.SystemPrompt, projectInstructions)
	processes, err := workspacetools.NewProcessManager(root, home, toolHomeRoot, security.EffectivePolicy, protection)
	if err != nil {
		return nil, agent.MarkInvalidRequest(fmt.Errorf("initialize process tools: %w", err))
	}
	processes.HideModelEnvironment(credentialEnvironmentNames()...)
	resources := &openResources{processes: processes}
	builtCatalog, err := buildToolCatalog(config, settings, settingsStore, masker, catalog, processes)
	if err != nil {
		return resources.fail(agent.MarkInvalidRequest(err))
	}
	catalog = builtCatalog.tools
	profiles := builtCatalog.profiles
	programSnapshots := builtCatalog.programSnapshots
	config.Profile = builtCatalog.profile

	opened, err := openInitialSession(config, home, root)
	resources.session = newLiveSession(opened.id, nil, opened.journal, opened.managed)
	resources.session.provisional = opened.provisional
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
	if err := processes.AttachSession(sessionID); err != nil {
		return cleanup(fmt.Errorf("attach durable jobs: %w", err))
	}
	notices = append(notices, processes.AttachSessionNotices(sessionID)...)
	if config.Resume && !config.ModelExplicit {
		records, err := journal.Records(ctx)
		if err != nil {
			return cleanup(fmt.Errorf("read resumed session: %w", err))
		}
		replayed, err := agent.Replay(records)
		if err != nil {
			return cleanup(fmt.Errorf("replay resumed session: %w", err))
		}
		if replayed.Selection.Provider != "" && replayed.Selection.Model != "" {
			config.ModelURI = replayed.Selection.Provider + "/" + replayed.Selection.Model
			if !config.ReasoningEffortExplicit {
				config.ReasoningEffort = replayed.Selection.ReasoningEffort
			}
		}
	}
	config.ReasoningEffort, err = normalizeReasoningEffort(config.ModelURI, config.ReasoningEffort)
	if err != nil {
		return cleanup(agent.MarkInvalidRequest(err))
	}
	builder := runtimeBuilder{
		baseURL:           config.BaseURL,
		contextWindow:     config.ContextWindow,
		credentials:       settingsStore,
		tools:             catalog,
		programTools:      programSnapshots,
		profiles:          profiles,
		profile:           config.Profile,
		processes:         processes,
		workspace:         root,
		requestPolicy:     modelRequestPolicy(config.RetryBudget, config.StreamIdleTimeout),
		maxToolIterations: config.MaxToolIterations,
		sandbox:           security.snapshot(),
		awaitRequiredJobs: !config.Interactive,
		sanitize:          masker.Redact,
	}
	runtime, err := builder.build(runtimeBuildParams{
		journal:         journal,
		sessionID:       sessionID,
		modelURI:        config.ModelURI,
		reasoningEffort: config.ReasoningEffort,
		instructions:    instructions,
		modelOptions: modelBackendOptions{
			requireCredential: !config.Interactive,
		},
	})
	if err != nil {
		return cleanup(err)
	}
	currentSession.runtime = runtime
	return &Application{
		config: applicationConfig{
			settings:          settingsStore,
			tools:             append([]agent.Tool(nil), catalog...),
			programTools:      append([]agent.ProgramToolSnapshot(nil), programSnapshots...),
			profiles:          profiles,
			systemPrompt:      config.SystemPrompt,
			root:              root,
			home:              home,
			toolHome:          toolHome,
			protectedPaths:    protection.Paths(),
			protection:        protection,
			baseURL:           config.BaseURL,
			contextWindow:     config.ContextWindow,
			retryBudget:       config.RetryBudget,
			streamIdleTimeout: config.StreamIdleTimeout,
			maxToolIterations: config.MaxToolIterations,
			masker:            masker,
			awaitRequiredJobs: !config.Interactive,
		},
		state: applicationState{
			session:          currentSession,
			processes:        processes,
			profile:          config.Profile,
			requestedSandbox: config.Sandbox,
			security:         security,
			startupNotices:   notices,
		},
	}, nil
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
