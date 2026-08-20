package app

import (
	"context"
	"strings"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/modelhttp"
)

type savedModelContext struct {
	URI       string
	Endpoint  string
	Window    int
	Estimated bool
}

func savedModelContextFromState(state *agent.State) *savedModelContext {
	if state == nil || state.Configured == nil || state.Configured.RuntimePolicy.ContextWindow <= 0 ||
		strings.TrimSpace(state.Selection.Provider) == "" || strings.TrimSpace(state.Selection.Model) == "" {
		return nil
	}
	return &savedModelContext{
		URI:       strings.ToLower(strings.TrimSpace(state.Selection.Provider)) + "/" + strings.TrimSpace(state.Selection.Model),
		Endpoint:  strings.TrimSpace(state.Configured.Environment.Endpoint),
		Window:    state.Configured.RuntimePolicy.ContextWindow,
		Estimated: state.Configured.RuntimePolicy.ContextWindowEstimated,
	}
}

func savedModelContextFromInfo(info agent.ModelInfo) *savedModelContext {
	if info.ContextWindow <= 0 || strings.TrimSpace(info.Provider) == "" || strings.TrimSpace(info.Model) == "" {
		return nil
	}
	return &savedModelContext{
		URI:       strings.ToLower(strings.TrimSpace(info.Provider)) + "/" + strings.TrimSpace(info.Model),
		Endpoint:  strings.TrimSpace(info.Endpoint),
		Window:    info.ContextWindow,
		Estimated: info.ContextWindowEstimated,
	}
}

// activateModelRoute owns the narrow online enrichment which must happen above
// the pure resolver and backend constructors.
func activateModelRoute(ctx context.Context, uri, effort string, overrides modelRouteOverrides, saved *savedModelContext, lookup modelContextLookup) (resolvedModelRoute, error) {
	route, err := resolveModelRoute(uri, effort, overrides, modelRouteEnrichment{})
	if err != nil {
		return resolvedModelRoute{}, err
	}
	if !implementedModelAPI(route.API) || route.Provider != "openrouter" || route.CustomEndpoint || overrides.ContextWindow > 0 || !route.ContextWindowEstimated {
		return route, nil
	}
	if lookup == nil {
		return route, nil
	}
	window, lookupErr := lookup(ctx, route.APIModel)
	if lookupErr == nil && window > 0 {
		return resolveModelRoute(uri, effort, overrides, modelRouteEnrichment{ContextWindow: window})
	}
	if err := ctx.Err(); err != nil {
		return resolvedModelRoute{}, err
	}
	if saved != nil && saved.Window > 0 && saved.URI == route.URI && saved.Endpoint != "" && saved.Endpoint == modelhttp.PublicEndpoint(route.BaseURL) {
		return resolveModelRoute(uri, effort, overrides, modelRouteEnrichment{
			ContextWindow: saved.Window, ContextWindowEstimated: saved.Estimated,
		})
	}
	return route, nil
}
