package modelconfig

import (
	"context"

	"github.com/levmv/skot/internal/modelhttp"
)

type SavedContextWindow struct {
	URI       string
	Endpoint  string
	Window    int
	Estimated bool
}

// Activate owns the narrow online enrichment which must happen above
// the pure resolver and backend constructors.
func Activate(ctx context.Context, uri, effort string, overrides Overrides, saved *SavedContextWindow, lookup ContextWindowLookup) (Route, error) {
	route, err := Resolve(uri, effort, overrides, Enrichment{})
	if err != nil {
		return Route{}, err
	}
	if !KnownAPI(route.API) || route.Provider != "openrouter" || route.CustomEndpoint || overrides.ContextWindow > 0 || !route.ContextWindowEstimated {
		return route, nil
	}
	if lookup == nil {
		return route, nil
	}
	window, lookupErr := lookup(ctx, route.APIModel)
	if lookupErr == nil && window > 0 {
		return Resolve(uri, effort, overrides, Enrichment{ContextWindow: window})
	}
	if err := ctx.Err(); err != nil {
		return Route{}, err
	}
	if saved != nil && saved.Window > 0 && saved.URI == route.URI && saved.Endpoint != "" && saved.Endpoint == modelhttp.PublicEndpoint(route.BaseURL) {
		return Resolve(uri, effort, overrides, Enrichment{
			ContextWindow: saved.Window, ContextWindowEstimated: saved.Estimated,
		})
	}
	return route, nil
}
