package app

import (
	"strings"
	"testing"
)

func TestParseModelURIPreservesSlashInModel(t *testing.T) {
	provider, model, err := parseModelURI("openrouter/moonshotai/kimi-k3")
	if err != nil {
		t.Fatal(err)
	}
	if provider != "openrouter" || model != "moonshotai/kimi-k3" {
		t.Fatalf("provider/model = %q/%q", provider, model)
	}
}

func TestOllamaProviderUsesLocalOpenAICompatibilityEndpoint(t *testing.T) {
	spec, err := modelProviderSpec(" OLLAMA ")
	if err != nil {
		t.Fatal(err)
	}
	if spec.baseURL != "http://localhost:11434/v1" || !spec.credentialless || spec.defaultAPI != modelAPIChatCompletions {
		t.Fatalf("Ollama provider = %#v", spec)
	}
}

func TestModelAPIUsesProviderDefaultUnlessModelOverridesIt(t *testing.T) {
	route, err := resolveModelRoute("deepseek/deepseek-v4-flash", "", modelRouteOverrides{}, modelRouteEnrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if route.API != modelAPIChatCompletions {
		t.Fatalf("default model API = %q", route.API)
	}
	overridden, err := resolveModelRoute("deepseek/deepseek-v4-flash", "", modelRouteOverrides{API: modelAPIResponses}, modelRouteEnrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if overridden.API != modelAPIResponses || overridden.Compatibility != modelCompatibilityUnverified {
		t.Fatalf("overridden route = %#v", overridden)
	}
}

func TestMixedProtocolProviderDoesNotGuessUnknownModelAPI(t *testing.T) {
	if _, err := resolveModelRoute("opencode-go/future-model", "", modelRouteOverrides{}, modelRouteEnrichment{}); err == nil ||
		!strings.Contains(err.Error(), "no reviewed protocol declaration") || !strings.Contains(err.Error(), "-model-api") {
		t.Fatalf("unknown mixed-protocol route error = %v", err)
	}
	route, err := resolveModelRoute("opencode-go/future-model", "", modelRouteOverrides{API: modelAPIResponses}, modelRouteEnrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if route.API != modelAPIResponses || route.Compatibility != modelCompatibilityUnverified {
		t.Fatalf("explicit mixed-protocol route = %#v", route)
	}
	if _, err := resolveModelRoute("opencode-go/future-model", "high", modelRouteOverrides{API: modelAPIAnthropicMessages}, modelRouteEnrichment{}); err == nil ||
		!strings.Contains(err.Error(), "reasoning effort") {
		t.Fatalf("explicit Anthropic reasoning error = %v", err)
	}
}

func TestModelCatalogInvariants(t *testing.T) {
	for provider, spec := range modelProviderCatalog {
		if spec.defaultAPI == "" || !knownModelAPI(spec.defaultAPI) {
			t.Errorf("provider %q default API = %q", provider, spec.defaultAPI)
		}
	}
	seen := make(map[string]struct{}, len(modelCatalog))
	for _, spec := range modelCatalog {
		if strings.TrimSpace(spec.Name) == "" {
			t.Errorf("catalog URI %q has no display name", spec.URI)
		}
		provider, _, err := parseModelURI(spec.URI)
		if err != nil {
			t.Errorf("catalog URI %q: %v", spec.URI, err)
			continue
		}
		if _, err := modelProviderSpec(provider); err != nil {
			t.Errorf("catalog URI %q: %v", spec.URI, err)
		}
		if spec.API != "" && !knownModelAPI(spec.API) {
			t.Errorf("catalog URI %q API = %q", spec.URI, spec.API)
		}
		if spec.MaxOutputTokens < 0 || (spec.MaxOutputTokens > 0 && spec.API != modelAPIAnthropicMessages) {
			t.Errorf("catalog URI %q max output tokens/API = %d/%q", spec.URI, spec.MaxOutputTokens, spec.API)
		}
		switch spec.Compatibility {
		case modelCompatibilitySupported, modelCompatibilityUnverified, modelCompatibilityUnsupported:
		default:
			t.Errorf("catalog URI %q compatibility = %q", spec.URI, spec.Compatibility)
		}
		key := strings.ToLower(strings.TrimSpace(spec.URI))
		if _, duplicate := seen[key]; duplicate {
			t.Errorf("duplicate catalog URI %q", spec.URI)
		}
		seen[key] = struct{}{}
		overrides := modelRouteOverrides{}
		if spec.Compatibility == modelCompatibilityUnsupported {
			overrides.API = spec.API
			if overrides.API == "" {
				providerSpec, providerErr := modelProviderSpec(provider)
				if providerErr != nil {
					continue
				}
				overrides.API = providerSpec.defaultAPI
			}
		}
		route, err := resolveModelRoute(spec.URI, "", overrides, modelRouteEnrichment{})
		if err != nil {
			t.Errorf("resolve catalog URI %q: %v", spec.URI, err)
			continue
		}
		if spec.Compatibility == modelCompatibilityUnsupported && route.Compatibility != modelCompatibilityUnverified {
			t.Errorf("explicit override for unsupported catalog URI %q has compatibility %q", spec.URI, route.Compatibility)
		}
		if implementedModelAPI(route.API) {
			backend, err := buildModelBackend(route, nil, modelBackendOptions{})
			if err != nil {
				t.Errorf("build catalog URI %q: %v", spec.URI, err)
			} else if backend.Info().ProviderStateContract != route.ProviderStateContract {
				t.Errorf("catalog URI %q state contract = %q, route has %q", spec.URI, backend.Info().ProviderStateContract, route.ProviderStateContract)
			}
		}
	}
}

func TestUnsupportedRouteErrorDoesNotAssumeItsAdapterIsMissing(t *testing.T) {
	original := modelCatalog
	modelCatalog = append(append([]modelSpec(nil), original...), modelSpec{
		URI: "ollama/known-incompatible", Name: "Known Incompatible", API: modelAPIChatCompletions,
		Compatibility: modelCompatibilityUnsupported,
	})
	t.Cleanup(func() { modelCatalog = original })

	_, err := resolveModelRoute("ollama/known-incompatible", "", modelRouteOverrides{}, modelRouteEnrichment{})
	if err == nil || !strings.Contains(err.Error(), "unsupported by Skot") || strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("unsupported implemented route error = %v", err)
	}
}

func TestOpenCodeGoKnownAnthropicRouteDoesNotFallBackToChatCompletions(t *testing.T) {
	route, err := resolveModelRoute("opencode-go/minimax-m3", "", modelRouteOverrides{}, modelRouteEnrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if route.API != modelAPIAnthropicMessages || route.Compatibility != modelCompatibilitySupported ||
		route.ContextWindow != 1_000_000 || route.MaxOutputTokens != 131_072 || len(route.ReasoningEfforts) != 1 || route.ReasoningEfforts[0] != "" {
		t.Fatalf("Anthropic route = %#v", route)
	}
	redundantOverride, err := resolveModelRoute("opencode-go/minimax-m3", "", modelRouteOverrides{
		API: modelAPIAnthropicMessages,
	}, modelRouteEnrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if redundantOverride.MaxOutputTokens != 131_072 || redundantOverride.Compatibility != modelCompatibilityUnverified {
		t.Fatalf("redundantly overridden Anthropic route = %#v", redundantOverride)
	}
	custom, err := resolveModelRoute("opencode-go/minimax-m3", "", modelRouteOverrides{
		BaseURL: "https://gateway.example/v1",
	}, modelRouteEnrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if custom.MaxOutputTokens != 0 || custom.ContextWindow != unknownModelContextWindow || !custom.ContextWindowEstimated {
		t.Fatalf("custom Anthropic route = %#v", custom)
	}
}

func TestBuildModelBackendRejectsUnknownModelAPI(t *testing.T) {
	original := modelCatalog
	modelCatalog = append(append([]modelSpec(nil), original...), modelSpec{URI: "ollama/future-test", API: modelAPI("future")})
	t.Cleanup(func() { modelCatalog = original })

	want := `unsupported model API "future"`
	_, err := resolveModelRoute("ollama/future-test", "", modelRouteOverrides{ContextWindow: 128_000}, modelRouteEnrichment{})
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("resolve error = %v, want containing %q", err, want)
	}
}

func TestBuildModelBackendSelectsAnthropicAdapter(t *testing.T) {
	route, err := resolveModelRoute("ollama/messages-test", "", modelRouteOverrides{
		API: modelAPIAnthropicMessages, ContextWindow: 128_000,
	}, modelRouteEnrichment{})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := buildModelBackend(route, nil, modelBackendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if info := backend.Info(); info.Backend != "anthropic_messages.ollama" || info.ProviderStateContract != "anthropic_messages.thinking_replay.v1" {
		t.Fatalf("Anthropic backend info = %#v", info)
	}
}

func TestBuildModelBackendSelectsResponsesAdapter(t *testing.T) {
	route, err := resolveModelRoute("ollama/responses-test", "", modelRouteOverrides{
		API: modelAPIResponses, ContextWindow: 128_000,
	}, modelRouteEnrichment{})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := buildModelBackend(route, nil, modelBackendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if info := backend.Info(); info.Backend != "responses.ollama" || info.ProviderStateContract != "responses.manual_history.v1" {
		t.Fatalf("Responses backend info = %#v", info)
	}
}
