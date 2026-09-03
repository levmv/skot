package app

import (
	"errors"
	"fmt"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/toolpolicy"
	workspacetools "github.com/levmv/skot/tools"
)

const (
	// DefaultModelURI is Skot's product default when no CLI, environment, or
	// persisted selection overrides it.
	DefaultModelURI = "deepseek/deepseek-v4-flash"

	ToolSetDefault  = toolpolicy.ToolSetDefault
	ToolSetEdit     = toolpolicy.ToolSetEdit
	ToolSetReadOnly = toolpolicy.ToolSetReadOnly
	ToolSetNone     = toolpolicy.ToolSetNone

	ScopeWorkspace = workspacetools.ScopeWorkspace
	ScopeMachine   = workspacetools.ScopeMachine

	// Defaults bound one logical model request, not a full agent run. A new
	// budget starts after every successful model response.
	DefaultRetryBudget       = 15 * time.Minute
	DefaultStreamIdleTimeout = 5 * time.Minute
)

// Config describes one concrete Skot application instance. Explicit flags
// distinguish CLI/env choices from values that may be restored from settings.
type Config struct {
	// Version identifies the host application build. VCS provenance is obtained
	// from Go build information when available.
	Version                 string
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
	// SystemPromptExplicit affects an empty SystemPrompt: false selects the
	// built-in and project instructions, while true selects no instructions.
	SystemPromptExplicit bool
	// ToolsFile is a strict JSON catalog of executable-backed tools. Empty uses
	// tools.json in Home; a missing file is an empty catalog.
	ToolsFile string
	// ConfigureTools receives the complete standard catalog and returns the
	// complete catalog to expose through tool sets. It may add, remove, replace,
	// or modify any tool. Open validates and takes an owned copy of the result;
	// returning nil deliberately selects an empty catalog. Resources captured
	// by added or replacement tools remain owned by the caller. A newly added
	// tool is model-visible only when a tool set also names it.
	ConfigureTools func([]agent.Tool) ([]agent.Tool, error)
	// ToolSets adds named exact tool lists and completely replaces built-in
	// tool sets with the same case-insensitive name. Values from Config override
	// definitions loaded from the settings file.
	ToolSets        map[string][]string
	ToolSet         string
	ToolSetExplicit bool
	// AgentModels adds model URIs which the opt-in agent tool may select for a
	// child. Omitting a model always inherits the parent's current selection.
	AgentModels   []string
	Scope         string
	ScopeExplicit bool
	// AddedPaths extends workspace scope with model-accessible directories.
	// Relative paths are resolved from Root.
	AddedPaths []string
	// ProtectedPaths adds model-inaccessible paths to those loaded from
	// config.json. Relative paths are resolved from Root.
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
	ToolService   bool
}

// ModelChoice is one locally known route presented to frontends. Unavailable
// choices are descriptive only and must not appear as ordinary selections.
type ModelChoice struct {
	URI      string
	Name     string
	Protocol string
	// ProtocolExplicit marks a route whose protocol the user chose, because
	// this build does not describe it. It is a fact the user owns and can act
	// on, unlike the reviewed protocol of a declared route.
	ProtocolExplicit       bool
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

// PreferenceNotPersistedError reports a live configuration value whose
// interactive preference could not be saved. Frontends should refresh
// from the live Application state and present the error as a partial-success
// notice rather than pretending the runtime change failed.
type PreferenceNotPersistedError struct {
	Setting string
	Err     error
}

func (failure *PreferenceNotPersistedError) Error() string {
	return fmt.Sprintf("%s is active for this session but was not saved: %v", failure.Setting, failure.Err)
}

func (failure *PreferenceNotPersistedError) Unwrap() error { return failure.Err }

func IsPreferenceNotPersisted(err error) bool {
	_, ok := errors.AsType[*PreferenceNotPersistedError](err)
	return ok
}
