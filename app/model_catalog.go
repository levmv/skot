package app

import (
	"strings"

	"github.com/levmv/skot/internal/state"
)

const unknownModelContextWindow = 128 * 1024

// modelSpec is deliberately small. It is a useful first-run catalog, not a
// claim that Skot owns an exhaustive or permanently current provider registry.
type modelSpec struct {
	URI           string
	ContextWindow int
	Estimated     bool
}

type modelContextLookup func(modelID string) (int, error)

var modelCatalog = []modelSpec{
	{URI: "deepseek/deepseek-v4-flash", ContextWindow: 1_000_000},
	{URI: "deepseek/deepseek-v4-pro", ContextWindow: 1_000_000},
	{URI: "openrouter/free"},
	{URI: "openrouter/~x-ai/grok-latest"},
	{URI: "openrouter/~moonshotai/kimi-latest"},
	{URI: "openrouter/~google/gemini-pro-latest"},
	{URI: "openai/gpt-5"},
}

func resolveModelSpec(uri string, store *state.Store, refresh bool) modelSpec {
	return resolveModelSpecWithLookup(uri, store, refresh, openRouterContextWindow)
}

func resolveModelSpecWithLookup(uri string, store *state.Store, refresh bool, lookup modelContextLookup) modelSpec {
	normalized := strings.ToLower(strings.TrimSpace(uri))
	if modelID, ok := strings.CutPrefix(normalized, "openrouter/"); ok {
		modelID = canonicalOpenRouterModelID(modelID)
		cached, cachedOK := cachedModelContext(store, normalized)
		if cachedOK && !refresh && !volatileOpenRouterModel(modelID) {
			return modelSpec{URI: strings.TrimSpace(uri), ContextWindow: cached}
		}
		if window, err := lookup(modelID); err == nil && window > 0 {
			if store != nil {
				_ = store.SetModelContext(normalized, window)
			}
			return modelSpec{URI: strings.TrimSpace(uri), ContextWindow: window}
		}
		if cachedOK {
			return modelSpec{URI: strings.TrimSpace(uri), ContextWindow: cached}
		}
	}
	for _, spec := range modelCatalog {
		if normalized == strings.ToLower(spec.URI) && spec.ContextWindow > 0 {
			spec.URI = strings.TrimSpace(uri)
			return spec
		}
	}
	return modelSpec{URI: strings.TrimSpace(uri), ContextWindow: unknownModelContextWindow, Estimated: true}
}

func canonicalOpenRouterModelID(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if strings.EqualFold(modelID, "free") {
		return "openrouter/free"
	}
	return modelID
}

func volatileOpenRouterModel(modelID string) bool {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	return strings.HasPrefix(modelID, "~") || modelID == "openrouter/free"
}

func cachedModelContext(store *state.Store, uri string) (int, bool) {
	if store == nil {
		return 0, false
	}
	window, ok, err := store.ModelContext(uri)
	return window, ok && err == nil
}

func knownModels(store *state.Store, current string) []string {
	models := make([]string, 0, len(modelCatalog)+4)
	seen := make(map[string]struct{}, cap(models))
	add := func(uri string) {
		uri = strings.TrimSpace(uri)
		key := strings.ToLower(uri)
		if key == "" {
			return
		}
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		models = append(models, uri)
	}
	add(current)
	if store != nil {
		if settings, err := store.Settings(); err == nil {
			add(settings.Model)
			for _, uri := range settings.RecentModels {
				add(uri)
			}
		}
	}
	for _, spec := range modelCatalog {
		add(spec.URI)
	}
	return models
}
