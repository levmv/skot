package app

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/levmv/skot/internal/modelconfig"
	"github.com/levmv/skot/internal/state"
	modelapi "github.com/levmv/skot/model"
)

func TestModelInferenceFollowsOnlyAllowedRedirects(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "private-key")
	t.Setenv("OPENAI_API_KEY", "private-key")
	t.Setenv("ANTHROPIC_API_KEY", "private-key")
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	saveTestCodexTokens(t, store, testCodexTokens())
	for _, test := range []struct {
		uri    string
		api    modelconfig.API
		follow bool
	}{
		{uri: "deepseek/test-model", api: modelconfig.ChatCompletions, follow: true},
		{uri: "openai/test-model", api: modelconfig.Responses, follow: true},
		{uri: "anthropic/test-model", api: modelconfig.AnthropicMessages, follow: true},
		{uri: "openai-codex/gpt-6-astra", api: modelconfig.Responses},
	} {
		for _, sameOrigin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/same-origin=%t", test.uri, sameOrigin), func(t *testing.T) {
				calls := 0
				var body, credentials string
				client := &http.Client{Transport: appRoundTripFunc(func(request *http.Request) (*http.Response, error) {
					calls++
					data, err := io.ReadAll(request.Body)
					if err != nil {
						t.Fatal(err)
					}
					_ = request.Body.Close()
					key := request.Header.Get("Authorization") + request.Header.Get("x-api-key")
					if calls == 1 {
						body, credentials = string(data), key
					} else if request.Method != http.MethodPost || string(data) != body || key != credentials {
						t.Fatal("redirect changed request body or credentials")
					}
					if calls > 1 {
						return codexResponse(http.StatusBadRequest, "target reached"), nil
					}
					response := codexResponse(http.StatusTemporaryRedirect, "redirect")
					location := "https://collector.example.test/collect"
					if sameOrigin {
						location = "/canonical"
					}
					response.Header.Set("Location", location)
					return response, nil
				})}
				route := testResolvedRoute(t, test.uri, "", "", 0)
				route.API = test.api
				backend, _, err := modelconfig.BuildBackend(route, store, modelconfig.BackendOptions{UseEnvironment: true, HTTPClient: client})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := backend.Complete(t.Context(), modelapi.Request{Instructions: "private conversation"}, nil); err == nil {
					t.Fatal("redirect or target's 400 response unexpectedly succeeded")
				}
				want := 1
				if sameOrigin && test.follow {
					want = 2
				}
				if calls != want {
					t.Fatalf("requests = %d, want %d", calls, want)
				}
			})
		}
	}
}

func TestParseModelURIPreservesSlashInModel(t *testing.T) {
	provider, model, err := modelconfig.ParseURI("openrouter/moonshotai/kimi-k3")
	if err != nil {
		t.Fatal(err)
	}
	if provider != "openrouter" || model != "moonshotai/kimi-k3" {
		t.Fatalf("provider/model = %q/%q", provider, model)
	}
}

func TestModelAPIUsesProviderDefaultUnlessModelOverridesIt(t *testing.T) {
	route, err := modelconfig.Resolve("deepseek/deepseek-v4-flash", "", modelconfig.Overrides{}, modelconfig.Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if route.API != modelconfig.ChatCompletions {
		t.Fatalf("default model API = %q", route.API)
	}
	overridden, err := modelconfig.Resolve("deepseek/deepseek-v4-flash", "", modelconfig.Overrides{API: modelconfig.Responses}, modelconfig.Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if overridden.API != modelconfig.Responses || overridden.Compatibility != modelconfig.Unverified {
		t.Fatalf("overridden route = %#v", overridden)
	}
}

func TestMixedProtocolProviderDoesNotGuessUnknownModelAPI(t *testing.T) {
	if _, err := modelconfig.Resolve("opencode-go/future-model", "", modelconfig.Overrides{}, modelconfig.Enrichment{}); !modelapi.IsAPIRequired(err) {
		t.Fatalf("unknown mixed-protocol route error = %v", err)
	}
	route, err := modelconfig.Resolve("opencode-go/future-model", "", modelconfig.Overrides{API: modelconfig.Responses}, modelconfig.Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if route.API != modelconfig.Responses || route.Compatibility != modelconfig.Unverified {
		t.Fatalf("explicit mixed-protocol route = %#v", route)
	}
	if _, err := modelconfig.Resolve("opencode-go/future-model", "high", modelconfig.Overrides{API: modelconfig.AnthropicMessages}, modelconfig.Enrichment{}); err == nil ||
		!strings.Contains(err.Error(), "reasoning effort") {
		t.Fatalf("explicit Anthropic reasoning error = %v", err)
	}
}

func TestModelCatalogInvariants(t *testing.T) {
	for provider, spec := range modelconfig.Providers {
		if spec.DefaultAPI == "" || !modelconfig.KnownAPI(spec.DefaultAPI) {
			t.Errorf("provider %q default API = %q", provider, spec.DefaultAPI)
		}
	}
	seen := make(map[string]struct{}, len(modelconfig.Catalog))
	for _, spec := range modelconfig.Catalog {
		if strings.TrimSpace(spec.Name) == "" {
			t.Errorf("catalog URI %q has no display name", spec.URI)
		}
		provider, _, err := modelconfig.ParseURI(spec.URI)
		if err != nil {
			t.Errorf("catalog URI %q: %v", spec.URI, err)
			continue
		}
		if _, err := modelconfig.Provider(provider); err != nil {
			t.Errorf("catalog URI %q: %v", spec.URI, err)
		}
		if spec.API != "" && !modelconfig.KnownAPI(spec.API) {
			t.Errorf("catalog URI %q API = %q", spec.URI, spec.API)
		}
		if spec.MaxOutputTokens < 0 || (spec.MaxOutputTokens > 0 && spec.API != modelconfig.AnthropicMessages) {
			t.Errorf("catalog URI %q max output tokens/API = %d/%q", spec.URI, spec.MaxOutputTokens, spec.API)
		}
		switch spec.Compatibility {
		case "", modelconfig.Supported, modelconfig.Unverified, modelconfig.Unsupported:
		default:
			t.Errorf("catalog URI %q compatibility = %q", spec.URI, spec.Compatibility)
		}
		key := strings.ToLower(strings.TrimSpace(spec.URI))
		if _, duplicate := seen[key]; duplicate {
			t.Errorf("duplicate catalog URI %q", spec.URI)
		}
		seen[key] = struct{}{}
		overrides := modelconfig.Overrides{}
		if spec.Compatibility == modelconfig.Unsupported {
			overrides.API = spec.API
			if overrides.API == "" {
				providerSpec, providerErr := modelconfig.Provider(provider)
				if providerErr != nil {
					continue
				}
				overrides.API = providerSpec.DefaultAPI
			}
		}
		route, err := modelconfig.Resolve(spec.URI, "", overrides, modelconfig.Enrichment{})
		if err != nil {
			t.Errorf("resolve catalog URI %q: %v", spec.URI, err)
			continue
		}
		if spec.Compatibility == modelconfig.Unsupported && route.Compatibility != modelconfig.Unverified {
			t.Errorf("explicit override for unsupported catalog URI %q has compatibility %q", spec.URI, route.Compatibility)
		}
		if modelconfig.KnownAPI(route.API) {
			_, _, err := modelconfig.BuildBackend(route, nil, modelconfig.BackendOptions{UseEnvironment: true})
			if err != nil {
				t.Errorf("build catalog URI %q: %v", spec.URI, err)
			}

		}
	}
}

func TestOpenCodeGoKnownAnthropicRouteDoesNotFallBackToChatCompletions(t *testing.T) {
	route, err := modelconfig.Resolve("opencode-go/minimax-m3", "", modelconfig.Overrides{}, modelconfig.Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	// The subscription endpoint caches on its own, so Skot places no breakpoints
	// of its own and leaves the protocol budget to it.
	if route.API != modelconfig.AnthropicMessages || route.Compatibility != modelconfig.Supported ||
		route.ContextWindow != 1_000_000 || route.MaxOutputTokens != 131_072 || route.PromptCache ||
		len(route.ReasoningEfforts) != 1 || route.ReasoningEfforts[0] != "" {
		t.Fatalf("Anthropic route = %#v", route)
	}
	redundantOverride, err := modelconfig.Resolve("opencode-go/minimax-m3", "", modelconfig.Overrides{
		API: modelconfig.AnthropicMessages,
	}, modelconfig.Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if redundantOverride.MaxOutputTokens != 131_072 || redundantOverride.Compatibility != modelconfig.Unverified {
		t.Fatalf("redundantly overridden Anthropic route = %#v", redundantOverride)
	}
	custom, err := modelconfig.Resolve("opencode-go/minimax-m3", "", modelconfig.Overrides{
		BaseURL: "https://gateway.example/v1",
	}, modelconfig.Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if custom.MaxOutputTokens != 0 || custom.ContextWindow != modelconfig.FallbackContextWindow || !custom.ContextWindowEstimated {
		t.Fatalf("custom Anthropic route = %#v", custom)
	}
}

func TestAnthropicProviderRoutesThroughNativeMessages(t *testing.T) {
	route, err := modelconfig.Resolve("anthropic/claude-opus-5", "", modelconfig.Overrides{}, modelconfig.Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if route.API != modelconfig.AnthropicMessages || route.BaseURL != "https://api.anthropic.com/v1" ||
		route.Compatibility != modelconfig.Supported || route.ContextWindow != 1_000_000 ||
		route.ContextWindowEstimated || route.MaxOutputTokens != 128_000 || !route.PromptCache {
		t.Fatalf("Anthropic route = %#v", route)
	}
	// Undeclared models stay usable on the provider default protocol; only the
	// reviewed route facts are withheld. Caching belongs to the endpoint rather
	// than the model, so it survives.
	undeclared, err := modelconfig.Resolve("anthropic/claude-unreleased", "", modelconfig.Overrides{}, modelconfig.Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if undeclared.API != modelconfig.AnthropicMessages || undeclared.Compatibility != modelconfig.Unverified ||
		undeclared.MaxOutputTokens != 0 || !undeclared.ContextWindowEstimated || !undeclared.PromptCache {
		t.Fatalf("undeclared Anthropic route = %#v", undeclared)
	}
	custom, err := modelconfig.Resolve("anthropic/claude-opus-5", "", modelconfig.Overrides{
		BaseURL: "https://gateway.example/v1",
	}, modelconfig.Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if custom.BaseURL != "https://gateway.example/v1" || custom.API != modelconfig.AnthropicMessages ||
		custom.MaxOutputTokens != 0 || custom.ContextWindow != modelconfig.FallbackContextWindow ||
		!custom.ContextWindowEstimated || custom.PromptCache {
		t.Fatalf("custom Anthropic route = %#v", custom)
	}
}

func TestSelectionProtocolResolvesUndeclaredRouteAndYieldsToDeclarations(t *testing.T) {
	overrides := modelconfig.Overrides{}.WithSelection("opencode-go/future-model", "responses", 0)
	route, err := modelconfig.Resolve("opencode-go/future-model", "", overrides, modelconfig.Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if route.API != modelconfig.Responses || route.Compatibility != modelconfig.Unverified {
		t.Fatalf("selected route = %#v", route)
	}
	// A reviewed declaration owns the protocol of its route, so a protocol
	// remembered while the route was undeclared must not survive it.
	declared := modelconfig.Overrides{}.WithSelection("opencode-go/minimax-m3", "chat_completions", 0)
	route, err = modelconfig.Resolve("opencode-go/minimax-m3", "", declared, modelconfig.Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if route.API != modelconfig.AnthropicMessages || route.Compatibility != modelconfig.Supported {
		t.Fatalf("declared route = %#v", route)
	}
	// The process-wide override stays the stronger instruction.
	forced := modelconfig.Overrides{API: modelconfig.ChatCompletions}.WithSelection("opencode-go/future-model", "responses", 0)
	route, err = modelconfig.Resolve("opencode-go/future-model", "", forced, modelconfig.Enrichment{})
	if err != nil || route.API != modelconfig.ChatCompletions {
		t.Fatalf("forced route = %#v, err = %v", route, err)
	}
}
