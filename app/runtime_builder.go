package app

import (
	"fmt"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/state"
	"github.com/levmv/skot/internal/toolpolicy"
	workspacetools "github.com/levmv/skot/tools"
)

type modelBackendOptions struct {
	requireCredential bool
	refreshContext    bool
}

// runtimeBuilder contains the resolved application-owned dependencies shared
// by initial, cleared, and resumed session runtimes. Session identity and model
// selection remain per-build inputs.
type runtimeBuilder struct {
	baseURL           string
	contextWindow     int
	credentials       *state.Store
	tools             []agent.Tool
	programTools      []agent.ProgramToolSnapshot
	profiles          toolpolicy.Profiles
	profile           string
	processes         *workspacetools.ProcessManager
	workspace         string
	requestPolicy     agent.ModelRequestPolicy
	maxToolIterations int
	sandbox           agent.SandboxSnapshot
	awaitRequiredJobs bool
	sanitize          func(string) string
	externalWork      agent.ExternalWork
}

type runtimeBuildParams struct {
	journal         agent.Journal
	sessionID       string
	modelURI        string
	reasoningEffort string
	instructions    string
	modelOptions    modelBackendOptions
}

func (builder runtimeBuilder) build(params runtimeBuildParams) (*agent.Runtime, error) {
	model, err := buildModelBackend(
		params.modelURI, params.reasoningEffort, builder.baseURL, builder.contextWindow,
		builder.credentials, params.modelOptions,
	)
	if err != nil {
		return nil, err
	}
	selectedTools, err := profileTools(builder.profiles, builder.tools, builder.credentials, builder.profile)
	if err != nil {
		return nil, fmt.Errorf("select profile tools: %w", err)
	}
	externalWork := builder.externalWork
	if externalWork == nil {
		externalWork = processExternalWork{processes: builder.processes, await: builder.awaitRequiredJobs}
	}
	runtime, err := agent.New(agent.Config{
		Model:             model,
		Journal:           params.journal,
		Tools:             selectedTools,
		Instructions:      params.instructions,
		SessionID:         params.sessionID,
		Workspace:         builder.workspace,
		RequestPolicy:     builder.requestPolicy,
		MaxToolIterations: builder.maxToolIterations,
		UserShell:         builder.processes.RunShell,
		ExternalWork:      externalWork,
		Sanitize:          builder.sanitize,
		Metadata: agent.ConfigurationMetadata{
			ToolProfile: builder.profile, Sandbox: builder.sandbox, AwaitRequiredJobs: builder.awaitRequiredJobs,
			ProgramTools: builder.programTools,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("initialize agent runtime: %w", err)
	}
	return runtime, nil
}
