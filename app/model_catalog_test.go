package app

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/levmv/skot/internal/state"
)

func TestKnownModelsPrefersCurrentAndRecentBeforeCatalog(t *testing.T) {
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
	models := knownModels(store, "openai/current-model")
	wantPrefix := []string{"openai/current-model", "deepseek/saved-model", "openrouter/recent-model"}
	if len(models) < len(wantPrefix) || !slices.Equal(models[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("models = %#v", models)
	}
	for _, required := range []string{"deepseek/deepseek-v4-flash", "openrouter/free", "openai/gpt-5"} {
		if !slices.Contains(models, required) {
			t.Fatalf("catalog model %q is missing from %#v", required, models)
		}
	}
}

func TestResolveOpenRouterModelContextAndFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/model/moonshotai/kimi-k3" {
			t.Errorf("metadata path = %q", request.URL.Path)
		}
		fmt.Fprint(writer, `{"data":{"context_length":1048576}}`)
	}))
	defer server.Close()

	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lookups := 0
	lookup := func(modelID string) (int, error) {
		lookups++
		return fetchOpenRouterContextWindow(server.Client(), server.URL, modelID)
	}
	spec := resolveModelSpecWithLookup("openrouter/moonshotai/kimi-k3", store, false, lookup)
	if spec.ContextWindow != 1_048_576 || spec.Estimated {
		t.Fatalf("model spec = %#v", spec)
	}
	cached := resolveModelSpecWithLookup("openrouter/moonshotai/kimi-k3", store, false, func(string) (int, error) {
		t.Fatal("cached context triggered a lookup")
		return 0, nil
	})
	if cached.ContextWindow != 1_048_576 || cached.Estimated || lookups != 1 {
		t.Fatalf("cached spec = %#v, lookups=%d", cached, lookups)
	}
	fallback := resolveModelSpecWithLookup("openrouter/example/future", nil, false, func(string) (int, error) {
		return 0, errors.New("offline")
	})
	if fallback.ContextWindow != unknownModelContextWindow || !fallback.Estimated {
		t.Fatalf("fallback spec = %#v", fallback)
	}
	if got := canonicalOpenRouterModelID("free"); got != "openrouter/free" {
		t.Fatalf("canonical free model = %q", got)
	}
}

func TestResolveBuiltInModelContext(t *testing.T) {
	spec := resolveModelSpecWithLookup("deepseek/deepseek-v4-flash", nil, false, func(string) (int, error) {
		t.Fatal("DeepSeek context used OpenRouter lookup")
		return 0, nil
	})
	if spec.ContextWindow != 1_000_000 || spec.Estimated {
		t.Fatalf("model spec = %#v", spec)
	}
}

func TestKnownModelsDeduplicatesCaseInsensitively(t *testing.T) {
	models := knownModels(nil, "DEEPSEEK/deepseek-v4-flash")
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
