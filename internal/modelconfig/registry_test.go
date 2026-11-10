package modelconfig

import (
	"slices"
	"testing"

	"github.com/levmv/skot/internal/state"
)

func TestRegistryOverlaysOnlySpecifiedFields(t *testing.T) {
	const uri = "deepseek/deepseek-flash"
	native, _ := Resolve(uri, "", Overrides{}, Enrichment{})
	first := map[string]state.ProviderConfig{"deepseek": {Models: map[string]state.ModelConfig{
		"deepseek-flash": {Name: new("My Flash"), ContextWindow: new(500_000)},
	}}}
	second := map[string]state.ProviderConfig{"deepseek": {Models: map[string]state.ModelConfig{
		"deepseek-flash": {ContextWindow: new(700_000), ImageInput: new(false)},
	}}}
	registry, err := NewRegistry(first, second)
	if err != nil {
		t.Fatal(err)
	}
	// Construction owns the definitions, just as separate applications own their settings.
	*second["deepseek"].Models["deepseek-flash"].ContextWindow = 1
	overrides := (Overrides{Registry: registry}).WithSelection(uri, "responses", 100_000)
	route, err := Resolve(uri, "", overrides, Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if route.ContextWindow != 700_000 || !route.ImageInputUnsupported || route.API != native.API ||
		route.BaseURL != native.BaseURL || !slices.Equal(route.ReasoningEfforts, native.ReasoningEfforts) {
		t.Fatalf("partial override lost inherited fields: %+v", route)
	}
	spec, _ := registry.CatalogSpec(uri)
	if spec.Name != "My Flash" {
		t.Fatalf("earlier layer's name lost: %+v", spec)
	}
	other, _ := Resolve(uri, "", Overrides{}, Enrichment{})
	if other.ContextWindow != native.ContextWindow || other.ImageInputUnsupported != native.ImageInputUnsupported {
		t.Fatal("configuration changed the built-in catalog")
	}
	explicit := Overrides{Registry: registry, API: Responses, ContextWindow: 800_000}.WithSelection(uri, "chat_completions", 200_000)
	route, err = Resolve(uri, "", explicit, Enrichment{})
	if err != nil || route.API != Responses || route.ContextWindow != 800_000 {
		t.Fatalf("explicit invocation must win: %+v, %v", route, err)
	}
}

func TestRegistryRequiresUsableProviderDefinitions(t *testing.T) {
	for _, test := range []struct {
		name      string
		providers map[string]state.ProviderConfig
	}{
		{"missing endpoint", map[string]state.ProviderConfig{"router": {API: new("chat_completions")}}},
		{"missing protocol", map[string]state.ProviderConfig{"router": {BaseURL: new("https://router.example/v1")}}},
		{"invalid context", map[string]state.ProviderConfig{"openai": {Models: map[string]state.ModelConfig{"test": {ContextWindow: new(0)}}}}},
		{"tool service name", map[string]state.ProviderConfig{"keenable": {BaseURL: new("https://router.example/v1"), API: new("responses")}}},
		{"OAuth endpoint", map[string]state.ProviderConfig{"openai-codex": {BaseURL: new("https://router.example/v1")}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewRegistry(test.providers); err == nil {
				t.Fatal("accepted unusable provider configuration")
			}
		})
	}
}
