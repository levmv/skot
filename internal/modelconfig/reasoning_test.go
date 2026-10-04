package modelconfig

import (
	"slices"
	"testing"

	"github.com/levmv/skot/model/chatcompletions"
)

func TestNormalizeReasoningEffort(t *testing.T) {
	for input, want := range map[string]string{"": "", " default ": "", " HIGH ": "high"} {
		if got, err := NormalizeReasoningEffort("deepseek/model", input); err != nil || got != want {
			t.Fatalf("effort %q = %q, %v", input, got, err)
		}
	}
	if _, err := NormalizeReasoningEffort("deepseek/model", "medium"); err == nil {
		t.Fatal("unsupported effort accepted")
	}
}

// Route declarations, rather than the generic provider fallback, own the
// native DeepSeek vocabulary.
func TestNativeDeepSeekRoutesOfferMaxReasoningEffort(t *testing.T) {
	for _, uri := range []string{"deepseek/deepseek-flash", "deepseek/deepseek-v4-flash", "deepseek/deepseek-v4-pro"} {
		efforts := ReasoningEfforts(uri)
		if !slices.Contains(efforts, "max") || !slices.Contains(efforts, "high") {
			t.Fatalf("%s efforts = %q", uri, efforts)
		}
		if normalized, err := NormalizeReasoningEffort(uri, "max"); err != nil || normalized != "max" {
			t.Fatalf("%s normalize(max) = %q, %v", uri, normalized, err)
		}
	}
}

func TestUndeclaredProviderRouteKeepsTheConservativeFallback(t *testing.T) {
	efforts := ReasoningEfforts("deepseek/deepseek-v9-imaginary")
	if !slices.Equal(efforts, []string{"", "high"}) {
		t.Fatalf("fallback efforts = %q", efforts)
	}
}

func TestNativeDeepSeekRoutesCanDisableThinking(t *testing.T) {
	for _, uri := range []string{"deepseek/deepseek-flash", "deepseek/deepseek-v4-flash", "deepseek/deepseek-v4-pro"} {
		route, err := Resolve(uri, "off", Overrides{}, Enrichment{})
		if err != nil {
			t.Fatalf("%s: %v", uri, err)
		}
		if route.ReasoningEffort != "off" || route.ChatTraits.ReasoningEffort != chatcompletions.ReasoningEffortThinking {
			t.Fatalf("%s route = %#v", uri, route)
		}
	}
	// A gateway route for the same model was not verified for the switch and
	// keeps the plain top-level encoding without the value.
	if _, err := Resolve("opencode-go/deepseek-flash", "off", Overrides{}, Enrichment{}); err == nil {
		t.Fatal("gateway route accepted an unverified off effort")
	}
}

func TestDeepSeekOffDoesNotCrossAProtocolOverride(t *testing.T) {
	if _, err := Resolve("deepseek/deepseek-flash", "off", Overrides{API: Responses}, Enrichment{}); err == nil {
		t.Fatal("Responses override inherited the Chat-only off effort")
	}
	route, err := Resolve("deepseek/deepseek-flash", "high", Overrides{API: Responses}, Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if route.API != Responses || !slices.Equal(route.ReasoningEfforts, []string{"", "high"}) {
		t.Fatalf("Responses override = %#v", route)
	}
}
