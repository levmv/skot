package app

import (
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/toolpolicy"
	workspacetools "github.com/levmv/skot/tools"
)

const (
	// DefaultModelURI is Skot's product default when no CLI, environment, or
	// persisted selection overrides it.
	DefaultModelURI = "deepseek/deepseek-v4-flash"

	ProfileFull     = toolpolicy.ProfileFull
	ProfileEdit     = toolpolicy.ProfileEdit
	ProfileReadOnly = toolpolicy.ProfileReadOnly

	SandboxAuto      = workspacetools.SandboxAuto
	SandboxWorkspace = workspacetools.SandboxWorkspace
	SandboxMasked    = workspacetools.SandboxMasked
	SandboxOff       = workspacetools.SandboxOff

	// Defaults bound one logical model request, not a full agent run. A new
	// budget starts after every successful model response.
	DefaultRetryBudget       = 15 * time.Minute
	DefaultStreamIdleTimeout = 5 * time.Minute
)

// Config describes one concrete Skot application instance. Explicit flags
// distinguish CLI/env choices from values that may be restored from settings.
type Config struct {
	Home                    string
	Root                    string
	ModelURI                string
	ReasoningEffort         string
	ModelAPI                string
	ModelExplicit           bool
	ReasoningEffortExplicit bool
	BaseURL                 string
	ContextWindow           int
	RetryBudget             time.Duration
	StreamIdleTimeout       time.Duration
	MaxToolIterations       int
	SystemPrompt            string
	// ToolsFile is a strict JSON catalog of executable-backed tools. Empty uses
	// tools.json in Home; a missing file is an empty catalog.
	ToolsFile string
	// ConfigureTools receives the complete standard catalog and returns the
	// complete catalog to expose through profiles. It may add, remove, replace,
	// or modify any tool. Open validates and takes an owned copy of the result;
	// returning nil deliberately selects an empty catalog. Resources captured
	// by added or replacement tools remain owned by the caller. A newly added
	// tool is model-visible only when a profile also names it.
	ConfigureTools func([]agent.Tool) ([]agent.Tool, error)
	// Profiles adds named exact tool lists and completely replaces built-in
	// profiles with the same case-insensitive name. Values from Config override
	// definitions loaded from the settings file.
	Profiles        map[string][]string
	Profile         string
	ProfileExplicit bool
	// AgentModels adds model URIs which the opt-in agent tool may select for a
	// child. Omitting a model always inherits the parent's current selection.
	AgentModels     []string
	Sandbox         string
	SandboxExplicit bool
	// ProtectedPaths adds model-inaccessible paths to those loaded from
	// config.json. Relative paths are resolved from Root; Skot Home is always
	// protected while the sandbox is workspace or masked.
	ProtectedPaths []string
	JournalPath    string
	Resume         bool
	ResumePrefix   string
	SaveSession    bool
	// Interactive selects frontend-oriented lifecycle policy: credentials may
	// be supplied after startup and required background work is not awaited by
	// Run.
	Interactive bool
}

type ProviderStatus struct {
	Name          string
	Source        string
	Description   string
	CredentialURL string
}

// ModelChoice is one locally known route presented to frontends. Unavailable
// choices are descriptive only and must not appear as ordinary selections.
type ModelChoice struct {
	URI                    string
	Name                   string
	Protocol               string
	ContextWindow          int
	ContextWindowEstimated bool
	ReasoningEfforts       []string
	Unavailable            bool
	UnavailableReason      string
}

type SessionSummary struct {
	ID        string
	Title     string
	UpdatedAt time.Time
}
