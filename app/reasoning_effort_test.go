package app

import (
	"slices"
	"testing"

	"github.com/levmv/skot/model/chatcompletions"
)

func TestNormalizeReasoningEffort(t *testing.T) {
	for input, want := range map[string]string{"": "", " default ": "", " HIGH ": "high"} {
		if got, err := normalizeReasoningEffort("deepseek/model", input); err != nil || got != want {
			t.Fatalf("effort %q = %q, %v", input, got, err)
		}
	}
	if _, err := normalizeReasoningEffort("deepseek/model", "medium"); err == nil {
		t.Fatal("unsupported effort accepted")
	}
}

// Route declarations, rather than the generic provider fallback, own the
// native V4 vocabulary.
func TestNativeDeepSeekRoutesOfferMaxReasoningEffort(t *testing.T) {
	for _, uri := range []string{"deepseek/deepseek-v4-flash", "deepseek/deepseek-v4-pro"} {
		efforts := reasoningEffortsForModel(uri)
		if !slices.Contains(efforts, "max") || !slices.Contains(efforts, "high") {
			t.Fatalf("%s efforts = %q", uri, efforts)
		}
		if normalized, err := normalizeReasoningEffort(uri, "max"); err != nil || normalized != "max" {
			t.Fatalf("%s normalize(max) = %q, %v", uri, normalized, err)
		}
	}
}

func TestUndeclaredProviderRouteKeepsTheConservativeFallback(t *testing.T) {
	efforts := reasoningEffortsForModel("deepseek/deepseek-v9-imaginary")
	if !slices.Equal(efforts, []string{"", "high"}) {
		t.Fatalf("fallback efforts = %q", efforts)
	}
}

func TestNativeDeepSeekRoutesCanDisableThinking(t *testing.T) {
	for _, uri := range []string{"deepseek/deepseek-v4-flash", "deepseek/deepseek-v4-pro"} {
		route, err := resolveModelRoute(uri, "off", modelRouteOverrides{}, modelRouteEnrichment{})
		if err != nil {
			t.Fatalf("%s: %v", uri, err)
		}
		if route.ReasoningEffort != "off" || route.ChatTraits.ReasoningEffort != chatcompletions.ReasoningEffortThinking {
			t.Fatalf("%s route = %#v", uri, route)
		}
	}
	// A gateway route for the same model was not verified for the switch and
	// keeps the plain top-level encoding without the value.
	if _, err := resolveModelRoute("opencode-go/deepseek-v4-flash", "off", modelRouteOverrides{}, modelRouteEnrichment{}); err == nil {
		t.Fatal("gateway route accepted an unverified off effort")
	}
}

func TestDeepSeekOffDoesNotCrossAProtocolOverride(t *testing.T) {
	if _, err := resolveModelRoute("deepseek/deepseek-v4-flash", "off", modelRouteOverrides{API: modelAPIResponses}, modelRouteEnrichment{}); err == nil {
		t.Fatal("Responses override inherited the Chat-only off effort")
	}
	route, err := resolveModelRoute("deepseek/deepseek-v4-flash", "high", modelRouteOverrides{API: modelAPIResponses}, modelRouteEnrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if route.API != modelAPIResponses || !slices.Equal(route.ReasoningEfforts, []string{"", "high"}) {
		t.Fatalf("Responses override = %#v", route)
	}
}
