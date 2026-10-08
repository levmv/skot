package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/modelconfig"
	"github.com/levmv/skot/internal/toolpolicy"
	"github.com/levmv/skot/model"
	workspacetools "github.com/levmv/skot/tools"
)

// runtimeBuilder contains the resolved application-owned dependencies shared
// by initial, cleared, and resumed session runtimes. Session identity and model
// selection remain per-build inputs.
type runtimeBuilder struct {
	baseURL           string
	modelAPI          modelconfig.API
	contextWindow     int
	credentials       modelconfig.CredentialStore
	masker            *secretMasker
	metadataLookup    modelconfig.ContextWindowLookup
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
	// modelSelectionAPI is the protocol chosen for a route this build does not
	// declare. A session being reopened supplies its own, so a route selected
	// that way stays usable across resume.
	modelSelectionAPI string
	// selectionContext is metadata supplied for an undeclared route. A resumed
	// session obtains its effective window from the recorded runtime policy.
	selectionContext int
	instructions     string
	modelOptions     modelconfig.BackendOptions
	resumedState     *agent.State
	// knownModel may only describe the same saved selection being reopened, or
	// the current Runtime selection carried through ClearSession. It permits an
	// inspectable Runtime when that exact route no longer resolves.
	knownModel *model.Info
}

func (builder runtimeBuilder) build(ctx context.Context, params runtimeBuildParams) (*agent.Runtime, error) {
	route, err := builder.activateRoute(ctx, params)
	if err != nil {
		return nil, model.MarkInvalidRequest(err)
	}
	modelInfo, backend, err := builder.modelForRoute(route, params.modelOptions)
	if err != nil {
		return nil, err
	}
	return builder.newRuntime(params, modelInfo, backend)
}

func (builder runtimeBuilder) buildRestored(ctx context.Context, params runtimeBuildParams) (*agent.Runtime, error) {
	modelInfo, backend, err := builder.resolveRestored(ctx, params)
	if err != nil {
		return nil, err
	}
	return builder.newRuntime(params, modelInfo, backend)
}

func (builder runtimeBuilder) resolveRestored(ctx context.Context, params runtimeBuildParams) (model.Info, model.Backend, error) {
	route, err := builder.activateRoute(ctx, params)
	if err == nil {
		return builder.modelForRoute(route, params.modelOptions)
	}
	if ctx.Err() != nil {
		return model.Info{}, nil, ctx.Err()
	}
	if params.knownModel != nil && modelInfoMatchesURI(*params.knownModel, params.modelURI) {
		// A route which resolves with its default effort is still available: the
		// original failure was validation of the requested effort, not a reason to
		// silently fall back to the saved descriptor.
		if _, routeErr := modelconfig.Resolve(params.modelURI, "", builder.modelOverrides(params), modelconfig.Enrichment{}); routeErr == nil {
			return model.Info{}, nil, model.MarkInvalidRequest(err)
		}
		return *params.knownModel, nil, nil
	}
	return model.Info{}, nil, model.MarkInvalidRequest(err)
}

func (builder runtimeBuilder) activateRoute(ctx context.Context, params runtimeBuildParams) (modelconfig.Route, error) {
	return modelconfig.Activate(
		ctx, params.modelURI, params.reasoningEffort, builder.modelOverrides(params),
		savedContextWindowFromState(params.resumedState), builder.metadataLookup,
	)
}

func (builder runtimeBuilder) modelOverrides(params runtimeBuildParams) modelconfig.Overrides {
	overrides := modelconfig.Overrides{BaseURL: builder.baseURL, API: builder.modelAPI, ContextWindow: builder.contextWindow}
	return overrides.WithSelection(params.modelURI, params.selectionAPI(), params.selectionContextWindow())
}

// selectionAPI prefers the protocol the caller chose. A session which already
// ran on the same route records the protocol it used, and reopening it must not
// depend on that choice still being remembered elsewhere.
func (params runtimeBuildParams) selectionAPI() string {
	if strings.TrimSpace(params.modelSelectionAPI) != "" {
		return params.modelSelectionAPI
	}
	if params.resumedState == nil {
		return ""
	}
	selection := params.resumedState.Selection
	saved := strings.TrimSpace(selection.Provider) + "/" + strings.TrimSpace(selection.Model)
	if !strings.EqualFold(strings.TrimSpace(params.modelURI), saved) {
		return ""
	}
	return string(modelconfig.APIFromBackendID(selection.Backend))
}

func (params runtimeBuildParams) selectionContextWindow() int {
	if params.selectionContext > 0 {
		return params.selectionContext
	}
	if params.resumedState == nil || params.resumedState.Configured == nil ||
		params.resumedState.Configured.RuntimePolicy.ContextWindowEstimated {
		return 0
	}
	selection := params.resumedState.Selection
	saved := strings.TrimSpace(selection.Provider) + "/" + strings.TrimSpace(selection.Model)
	if !strings.EqualFold(strings.TrimSpace(params.modelURI), saved) {
		return 0
	}
	return max(0, params.resumedState.Configured.RuntimePolicy.ContextWindow)
}

func (builder runtimeBuilder) modelForRoute(route modelconfig.Route, options modelconfig.BackendOptions) (model.Info, model.Backend, error) {
	modelInfo, err := modelconfig.Info(route)
	if err != nil {
		return model.Info{}, nil, model.MarkInvalidRequest(err)
	}
	options.Masker = builder.masker
	options.UseEnvironment = true
	backend, _, err := modelconfig.BuildBackend(route, builder.credentials, options)
	if err != nil {
		return model.Info{}, nil, err
	}
	return modelInfo, backend, nil
}

func restoredModelInfo(state agent.State, modelURI string) (model.Info, bool) {
	selection := state.Selection
	if strings.TrimSpace(selection.Backend) == "" || strings.TrimSpace(selection.Provider) == "" || strings.TrimSpace(selection.Model) == "" {
		return model.Info{}, false
	}
	savedURI := strings.TrimSpace(selection.Provider) + "/" + strings.TrimSpace(selection.Model)
	if !strings.EqualFold(strings.TrimSpace(modelURI), savedURI) {
		return model.Info{}, false
	}
	info := model.Info{
		BackendID: selection.Backend, Provider: selection.Provider, Model: selection.Model,
		ReasoningEffort: selection.ReasoningEffort, ProviderStateContract: selection.ProviderStateContract,
	}
	if state.Configured != nil {
		info.ImageInputUnsupported = state.Configured.RuntimePolicy.ImageInputUnsupported
		info.ContextWindow = state.Configured.RuntimePolicy.ContextWindow
		info.ContextWindowEstimated = state.Configured.RuntimePolicy.ContextWindowEstimated
		info.MaxRequestBytes = state.Configured.RuntimePolicy.MaxRequestBytes
		info.MaxCompletionBytes = state.Configured.RuntimePolicy.MaxCompletionBytes
		info.Endpoint = state.Configured.Environment.Endpoint
	}
	return info, true
}

func modelInfoMatchesURI(info model.Info, modelURI string) bool {
	if strings.TrimSpace(info.BackendID) == "" || strings.TrimSpace(info.Provider) == "" || strings.TrimSpace(info.Model) == "" {
		return false
	}
	want := strings.TrimSpace(info.Provider) + "/" + strings.TrimSpace(info.Model)
	return strings.EqualFold(strings.TrimSpace(modelURI), want)
}

func (builder runtimeBuilder) newRuntime(params runtimeBuildParams, modelInfo model.Info, backend model.Backend) (*agent.Runtime, error) {
	selectedTools, err := builder.toolSets.Tools(builder.tools, builder.toolSet)
	if err != nil {
		return nil, fmt.Errorf("select tools for tool set: %w", err)
	}
	runtime, err := agent.New(agent.Config{
		Model:             modelInfo,
		Backend:           backend,
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
		return nil, fmt.Errorf("initialize agent runtime: %w", err)
	}
	return runtime, nil
}
