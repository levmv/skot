package app

import (
	"context"
	"fmt"
	"net/http"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/state"
	"github.com/levmv/skot/internal/toolpolicy"
	workspacetools "github.com/levmv/skot/tools"
)

type modelBackendOptions struct {
	requireCredential bool
	httpClient        *http.Client
}

// runtimeBuilder contains the resolved application-owned dependencies shared
// by initial, cleared, and resumed session runtimes. Session identity and model
// selection remain per-build inputs.
type runtimeBuilder struct {
	baseURL           string
	modelAPI          modelAPI
	contextWindow     int
	credentials       *state.Store
	metadataLookup    modelContextLookup
	tools             []agent.Tool
	programTools      []agent.ProgramToolSnapshot
	applicationBuild  agent.BuildSnapshot
	toolSets          toolpolicy.ToolSets
	toolSet           string
	processes         *workspacetools.ProcessManager
	workspace         string
	requestPolicy     agent.ModelRequestPolicy
	maxToolIterations int
	scope             agent.ScopeSnapshot
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
	resumedState    *agent.State
}

func (builder runtimeBuilder) build(ctx context.Context, params runtimeBuildParams) (*agent.Runtime, error) {
	runtime, _, err := builder.buildWithRoute(ctx, params)
	return runtime, err
}

func (builder runtimeBuilder) buildWithRoute(ctx context.Context, params runtimeBuildParams) (*agent.Runtime, resolvedModelRoute, error) {
	route, err := activateModelRoute(ctx, params.modelURI, params.reasoningEffort, modelRouteOverrides{
		BaseURL: builder.baseURL, API: builder.modelAPI, ContextWindow: builder.contextWindow,
	}, savedModelContextFromState(params.resumedState), builder.metadataLookup)
	if err != nil {
		return nil, resolvedModelRoute{}, agent.MarkInvalidRequest(err)
	}
	model, err := buildModelBackend(route, builder.credentials, params.modelOptions)
	if err != nil {
		return nil, resolvedModelRoute{}, err
	}
	selectedTools, err := toolSetTools(builder.toolSets, builder.tools, builder.credentials, builder.toolSet)
	if err != nil {
		return nil, resolvedModelRoute{}, fmt.Errorf("select tools for tool set: %w", err)
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
		ExternalWork:      builder.externalWork,
		Sanitize:          builder.sanitize,
		Metadata: agent.ConfigurationMetadata{
			ToolSet: builder.toolSet, Scope: builder.scope, AwaitRequiredJobs: builder.awaitRequiredJobs,
			Build: builder.applicationBuild, ProgramTools: builder.programTools,
		},
	})
	if err != nil {
		return nil, resolvedModelRoute{}, fmt.Errorf("initialize agent runtime: %w", err)
	}
	return runtime, route, nil
}
