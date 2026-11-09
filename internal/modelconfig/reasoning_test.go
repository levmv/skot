package modelconfig

import (
	"slices"
	"testing"

	"github.com/levmv/skot/model/chatcompletions"
)

func TestResolveNormalizesReasoningEffort(t *testing.T) {
	for input, want := range map[string]string{"": "", " default ": "", " HIGH ": "high"} {
		route, err := Resolve("deepseek/deepseek-v4-flash", input, Overrides{}, Enrichment{})
		if err != nil || route.ReasoningEffort != want {
			t.Fatalf("effort %q = %q, %v", input, route.ReasoningEffort, err)
		}
	}
}

// Route declarations, rather than the generic provider fallback, own the
// native DeepSeek vocabulary.
func TestNativeDeepSeekRoutesOfferMaxReasoningEffort(t *testing.T) {
	for _, uri := range []string{"deepseek/deepseek-flash", "deepseek/deepseek-v4-flash", "deepseek/deepseek-v4-pro"} {
		route, err := Resolve(uri, "max", Overrides{}, Enrichment{})
		if err != nil || route.ReasoningEffort != "max" {
			t.Fatalf("%s effort = %q, %v", uri, route.ReasoningEffort, err)
		}
		if !slices.Contains(route.ReasoningEfforts, "max") || !slices.Contains(route.ReasoningEfforts, "high") {
			t.Fatalf("%s efforts = %q", uri, route.ReasoningEfforts)
		}
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
