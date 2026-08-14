package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/levmv/skot/internal/state"
	"github.com/levmv/skot/model/chatcompletions"
)

func TestKnownModelURIsPreferCurrentAndRecentBeforeCatalog(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDefaultModel("openrouter/recent-model"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDefaultModel("deepseek/saved-model"); err != nil {
		t.Fatal(err)
	}
	models := knownModelURIs(store, "openai/current-model")
	wantPrefix := []string{"openai/current-model", "deepseek/saved-model", "openrouter/recent-model"}
	if len(models) < len(wantPrefix) || !slices.Equal(models[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("models = %#v", models)
	}
	for _, required := range []string{"deepseek/deepseek-v4-flash", "openrouter/free"} {
		if !slices.Contains(models, required) {
			t.Fatalf("catalog model %q is missing from %#v", required, models)
		}
	}
}

func TestResolveModelRouteAppliesReviewedFactsAndExplicitOverrides(t *testing.T) {
	route, err := resolveModelRoute("deepseek/deepseek-v4-flash", "high", modelRouteOverrides{}, modelRouteEnrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if route.API != modelAPIChatCompletions || route.ContextWindow != 1_000_000 || route.ContextWindowEstimated ||
		route.Compatibility != modelCompatibilitySupported || route.ChatTraits.ReasoningReplay != chatcompletions.ReasoningReplayToolTurns ||
		route.ProviderStateContract == "" {
		t.Fatalf("resolved route = %#v", route)
	}

	overridden, err := resolveModelRoute("deepseek/deepseek-v4-flash", "high", modelRouteOverrides{
		BaseURL: "https://gateway.example/v1", API: modelAPIResponses,
	}, modelRouteEnrichment{ContextWindow: 2_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if overridden.API != modelAPIResponses || overridden.BaseURL != "https://gateway.example/v1" ||
		overridden.ContextWindow != unknownModelContextWindow || !overridden.ContextWindowEstimated ||
		overridden.Compatibility != modelCompatibilityUnverified || overridden.ChatTraits.PromptCacheKey ||
		overridden.ProviderStateContract != "responses.manual_history.v1" {
		t.Fatalf("overridden route = %#v", overridden)
	}

	explicitContext, err := resolveModelRoute("deepseek/deepseek-v4-flash", "", modelRouteOverrides{
		BaseURL: "https://gateway.example/v1", ContextWindow: 64_000,
	}, modelRouteEnrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if explicitContext.API != modelAPIChatCompletions || explicitContext.ContextWindow != 64_000 || explicitContext.ContextWindowEstimated ||
		explicitContext.ChatTraits.ReasoningReplay != "" || explicitContext.ProviderStateContract != "" {
		t.Fatalf("explicit context route = %#v", explicitContext)
	}
	strictGateway, err := resolveModelRoute("openai/example-model", "", modelRouteOverrides{BaseURL: "https://gateway.example/v1"}, modelRouteEnrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if strictGateway.API != modelAPIChatCompletions || strictGateway.ChatTraits.PromptCacheKey {
		t.Fatalf("strict gateway route = %#v", strictGateway)
	}
}

func TestActivateOpenRouterRouteEnrichesAndPureResolutionPreservesProtocol(t *testing.T) {
	original := modelCatalog
	modelCatalog = append(append([]modelSpec(nil), original...), modelSpec{
		URI: "openrouter/moonshotai/kimi-k3", Compatibility: modelCompatibilitySupported,
	})
	t.Cleanup(func() { modelCatalog = original })

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/model/moonshotai/kimi-k3" {
			t.Errorf("metadata path = %q", request.URL.Path)
		}
		fmt.Fprint(writer, `{"data":{"context_length":1048576}}`)
	}))
	defer server.Close()
	lookup := func(ctx context.Context, modelID string) (int, error) {
		return fetchOpenRouterContextWindow(ctx, server.Client(), server.URL, modelID)
	}
	route, err := activateModelRoute(context.Background(), "openrouter/moonshotai/kimi-k3", "", modelRouteOverrides{}, nil, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if route.ContextWindow != 1_048_576 || route.ContextWindowEstimated || route.API != modelAPIChatCompletions {
		t.Fatalf("activated route = %#v", route)
	}
	modelCatalog[len(modelCatalog)-1].API = modelAPIResponses
	resolved, err := resolveModelRoute("openrouter/moonshotai/kimi-k3", "", modelRouteOverrides{}, modelRouteEnrichment{ContextWindow: 1_048_576})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ContextWindow != 1_048_576 || resolved.API != modelAPIResponses {
		t.Fatalf("resolved enriched route = %#v", resolved)
	}
}

func TestActivateOpenRouterRouteFallsBackToMatchingSavedContext(t *testing.T) {
	offline := func(context.Context, string) (int, error) { return 0, errors.New("offline") }
	saved := &savedModelContext{
		URI: "openrouter/example/future", Endpoint: "https://openrouter.ai/api/v1", Window: 256_000,
	}
	route, err := activateModelRoute(context.Background(), "openrouter/example/future", "", modelRouteOverrides{}, saved, offline)
	if err != nil {
		t.Fatal(err)
	}
	if route.ContextWindow != 256_000 || route.ContextWindowEstimated {
		t.Fatalf("saved fallback route = %#v", route)
	}

	saved.Endpoint = "https://different.example/v1"
	route, err = activateModelRoute(context.Background(), "openrouter/example/future", "", modelRouteOverrides{}, saved, offline)
	if err != nil {
		t.Fatal(err)
	}
	if route.ContextWindow != unknownModelContextWindow || !route.ContextWindowEstimated {
		t.Fatalf("mismatched fallback route = %#v", route)
	}
}

func TestActivateCustomOpenRouterEndpointDoesNotPerformPublicLookup(t *testing.T) {
	lookups := 0
	route, err := activateModelRoute(context.Background(), "openrouter/example/future", "", modelRouteOverrides{
		BaseURL: "https://gateway.example/v1",
	}, nil, func(context.Context, string) (int, error) {
		lookups++
		return 512_000, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if lookups != 0 || route.ContextWindow != unknownModelContextWindow || !route.ContextWindowEstimated {
		t.Fatalf("custom route/lookups = %#v/%d", route, lookups)
	}
}

func TestActivateOpenRouterResponsesRouteUsesProtocolIndependentEnrichment(t *testing.T) {
	lookups := 0
	route, err := activateModelRoute(context.Background(), "openrouter/example/future", "", modelRouteOverrides{
		API: modelAPIResponses,
	}, nil, func(context.Context, string) (int, error) {
		lookups++
		return 512_000, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if lookups != 1 || route.API != modelAPIResponses || route.ContextWindow != 512_000 || route.ContextWindowEstimated {
		t.Fatalf("route/lookups = %#v/%d", route, lookups)
	}
}

func TestKnownModelURIsDeduplicateCaseInsensitively(t *testing.T) {
	models := knownModelURIs(nil, "DEEPSEEK/deepseek-v4-flash")
	count := 0
	for _, model := range models {
		if model == "DEEPSEEK/deepseek-v4-flash" || model == "deepseek/deepseek-v4-flash" {
			count++
		}
	}
	if count != 1 || models[0] != "DEEPSEEK/deepseek-v4-flash" {
		t.Fatalf("models = %#v", models)
	}
}

func TestModelChoicesExposeRunnableRouteFactsAndMarkUnsupportedRoutesUnavailable(t *testing.T) {
	choices := modelChoices(nil, "opencode-go/gpt-5.6-luna", modelRouteOverrides{})
	var luna *ModelChoice
	var minimax *ModelChoice
	for index := range choices {
		choice := &choices[index]
		if choice.URI == "opencode-go/gpt-5.6-luna" {
			luna = choice
		}
		if choice.URI == "opencode-go/minimax-m3" {
			minimax = choice
		}
	}
	if luna == nil || luna.Name != "OpenCode Go · GPT 5.6 Luna" || luna.Protocol != "responses" ||
		luna.Compatibility != "supported" || luna.ContextWindow != 922_000 || luna.ContextWindowEstimated ||
		!slices.Equal(luna.ReasoningEfforts, []string{"", "none", "low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("Luna choice = %#v", luna)
	}
	glmIndex := slices.IndexFunc(choices, func(choice ModelChoice) bool { return choice.URI == "opencode-go/glm-5.2" })
	if glmIndex < 0 {
		t.Fatal("GLM choice is missing")
	}
	if choices[glmIndex].ContextWindow != 1_000_000 || choices[glmIndex].ContextWindowEstimated {
		t.Fatalf("GLM choice = %#v", choices[glmIndex])
	}
	for _, uri := range []string{
		"opencode-go/gpt-5.6-luna",
		"opencode-go/deepseek-v4-flash",
		"opencode-go/deepseek-v4-pro",
		"opencode-go/kimi-k3",
		"opencode-go/glm-5.2",
	} {
		choiceIndex := slices.IndexFunc(choices, func(choice ModelChoice) bool { return choice.URI == uri })
		if choiceIndex < 0 {
			t.Fatalf("supported OpenCode Go choice %q is missing", uri)
		}
		if choices[choiceIndex].Compatibility != "supported" || choices[choiceIndex].Unavailable {
			t.Fatalf("supported OpenCode Go choice %q = %#v", uri, choices[choiceIndex])
		}
	}
	if minimax == nil || minimax.Name != "OpenCode Go · MiniMax M3" || minimax.Protocol != "anthropic_messages" ||
		minimax.Compatibility != "unsupported" || !minimax.Unavailable {
		t.Fatalf("MiniMax choice = %#v", minimax)
	}
}

func TestModelChoicesSurfaceRecentRoutesWhichNoLongerResolve(t *testing.T) {
	choices := modelChoices(nil, "opencode-go/removed-model", modelRouteOverrides{})
	if len(choices) == 0 || choices[0].URI != "opencode-go/removed-model" || !choices[0].Unavailable || choices[0].Compatibility != "" ||
		!strings.Contains(choices[0].UnavailableReason, "no reviewed protocol declaration") {
		t.Fatalf("removed recent choice = %#v", choices)
	}
}

func TestModelChoicesApplyGlobalProtocolOverrideHonestly(t *testing.T) {
	choices := modelChoices(nil, "", modelRouteOverrides{API: modelAPIResponses})
	wanted := map[string]bool{
		"deepseek/deepseek-v4-flash": false,
		"opencode-go/gpt-5.6-luna":   false,
	}
	for _, choice := range choices {
		if _, exists := wanted[choice.URI]; !exists {
			continue
		}
		if choice.Protocol != "responses" || choice.Compatibility != "unverified" || choice.Unavailable {
			t.Fatalf("overridden choice = %#v", choice)
		}
		wanted[choice.URI] = true
	}
	for uri, found := range wanted {
		if !found {
			t.Fatalf("overridden choice %q is missing", uri)
		}
	}
}
