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

func TestProtocolEndpointsRespectAPIAndBaseURLOverrides(t *testing.T) {
	for _, test := range []struct {
		name      string
		provider  state.ProviderConfig
		overrides Overrides
		wantAPI   API
		wantURL   string
	}{
		{
			name: "declared protocol", wantAPI: AnthropicMessages,
			wantURL: "https://opencode.ai/inference/go/anthropic/v1",
		},
		{
			name: "invocation protocol", overrides: Overrides{API: Responses}, wantAPI: Responses,
			wantURL: "https://opencode.ai/inference/go/openai/v1",
		},
		{
			name: "configured provider protocol", provider: state.ProviderConfig{API: new("gemini")}, wantAPI: Gemini,
			wantURL: "https://opencode.ai/inference/go/google/v1beta",
		},
		{
			name: "configured model protocol",
			provider: state.ProviderConfig{Models: map[string]state.ModelConfig{
				"claude-haiku-5-5": {API: new("gemini")},
			}},
			wantAPI: Gemini, wantURL: "https://opencode.ai/inference/go/google/v1beta",
		},
		{
			name: "configured endpoint", provider: state.ProviderConfig{BaseURL: new("https://gateway.example/v1/")},
			wantAPI: AnthropicMessages, wantURL: "https://gateway.example/v1",
		},
		{
			name:     "explicit default endpoint also wins",
			provider: state.ProviderConfig{BaseURL: new("https://opencode.ai/inference/go/openai/v1")},
			wantAPI:  AnthropicMessages, wantURL: "https://opencode.ai/inference/go/openai/v1",
		},
		{
			name: "invocation endpoint", overrides: Overrides{BaseURL: "https://invocation.example/v1/"},
			wantAPI: AnthropicMessages, wantURL: "https://invocation.example/v1",
		},
		{
			name:      "invocation overrides configuration",
			provider:  state.ProviderConfig{BaseURL: new("https://gateway.example/v1"), API: new("gemini")},
			overrides: Overrides{BaseURL: "https://invocation.example/v1", API: Responses},
			wantAPI:   Responses, wantURL: "https://invocation.example/v1",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry, err := NewRegistry(map[string]state.ProviderConfig{"opencode-go": test.provider})
			if err != nil {
				t.Fatal(err)
			}
			test.overrides.Registry = registry
			route, err := Resolve("opencode-go/claude-haiku-5-5", "", test.overrides, Enrichment{})
			if err != nil {
				t.Fatal(err)
			}
			if route.API != test.wantAPI || route.BaseURL != test.wantURL {
				t.Fatalf("route uses %s at %s, want %s at %s", route.API, route.BaseURL, test.wantAPI, test.wantURL)
			}
		})
	}
}
