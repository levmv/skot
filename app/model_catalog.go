package app

import (
	"fmt"
	"strings"

	"github.com/levmv/skot/internal/modelconfig"
	"github.com/levmv/skot/internal/state"
)

// modelSelection is one known route together with metadata its last deliberate
// selection carried. Zero values mean the route describes itself.
type modelSelection struct {
	URI           string
	API           string
	ContextWindow int
}

func knownModelSelections(store *state.InteractiveStore, current, currentAPI string) []modelSelection {
	var stored []modelSelection
	if store != nil {
		if settings, err := store.Settings(); err == nil {
			stored = append(stored, modelSelection{
				URI: settings.Workspace.Model, API: settings.Workspace.ModelAPI,
				ContextWindow: settings.Workspace.ContextWindow,
			})
			for _, selection := range settings.ModelHistory {
				stored = append(stored, modelSelection{
					URI: selection.Model, API: selection.ModelAPI, ContextWindow: selection.ContextWindow,
				})
			}
		}
	}
	selections := make([]modelSelection, 0, len(modelconfig.Catalog)+len(stored)+1)
	seen := make(map[string]struct{}, cap(selections))
	add := func(uri, api string, contextWindow int) {
		uri = strings.TrimSpace(uri)
		key := strings.ToLower(uri)
		if key == "" {
			return
		}
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		selections = append(selections, modelSelection{URI: uri, API: api, ContextWindow: contextWindow})
	}
	add(current, currentAPI, 0)
	for _, selection := range stored {
		add(selection.URI, selection.API, selection.ContextWindow)
	}
	for _, spec := range modelconfig.Catalog {
		add(spec.URI, "", 0)
	}
	return selections
}

func modelChoices(store *state.InteractiveStore, current, currentAPI string, overrides modelconfig.Overrides) []ModelChoice {
	choices := make([]ModelChoice, 0, len(modelconfig.Catalog)+4)
	for _, selection := range knownModelSelections(store, current, currentAPI) {
		uri := selection.URI
		declaration, _ := modelconfig.CatalogSpec(uri)
		selected := overrides.WithSelection(uri, selection.API, selection.ContextWindow)
		explicitProtocol := selected.API != "" && overrides.API == ""
		route, err := modelconfig.Resolve(uri, "", selected, modelconfig.Enrichment{})
		if err != nil {
			api := declaration.API
			if api == "" {
				if provider, _, parseErr := modelconfig.ParseURI(uri); parseErr == nil {
					if providerSpec, providerErr := modelconfig.Provider(provider); providerErr == nil {
						api = providerSpec.DefaultAPI
					}
				}
			}
			contextEstimated := declaration.ContextWindow <= 0
			choices = append(choices, ModelChoice{
				URI: uri, Name: declaration.Name, Protocol: string(api),
				ContextWindow: declaration.ContextWindow, ContextWindowEstimated: contextEstimated,
				ReasoningEfforts: append([]string(nil), declaration.ReasoningEfforts...),
				Unavailable:      true, UnavailableReason: err.Error(),
			})
			continue
		}
		if !modelconfig.KnownAPI(route.API) {
			choices = append(choices, ModelChoice{
				URI: uri, Name: declaration.Name, Protocol: string(route.API),
				ContextWindow: route.ContextWindow, ContextWindowEstimated: route.ContextWindowEstimated,
				ReasoningEfforts: append([]string(nil), route.ReasoningEfforts...), Unavailable: true,
				UnavailableReason: fmt.Sprintf("model API %q is not implemented", route.API),
			})
			continue
		}
		choices = append(choices, ModelChoice{
			URI: uri, Name: declaration.Name, Protocol: string(route.API),
			ProtocolExplicit: explicitProtocol,
			ContextWindow:    route.ContextWindow, ContextWindowEstimated: route.ContextWindowEstimated,
			ReasoningEfforts: append([]string(nil), route.ReasoningEfforts...),
		})
	}
	return choices
}
