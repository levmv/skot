package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/state"
)

func TestStoredCredentialIsUsedAndEnvironmentOverridesIt(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "")
	if err := storeProviderCredential(store, " OpenAI ", "stored-key"); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, "https://example.test", nil)
	authorizer := storedBearerAuthorizer{store: store, provider: "openai", modelURI: "openai/test"}
	if err := authorizer.Authorize(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer stored-key" {
		t.Fatalf("stored authorization = %q", got)
	}

	t.Setenv("OPENAI_API_KEY", "environment-key")
	request.Header.Del("Authorization")
	if err := authorizer.Authorize(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer environment-key" {
		t.Fatalf("environment authorization = %q", got)
	}
	if err := deleteProviderCredential(store, "openai"); err == nil || !strings.Contains(err.Error(), "environment override") {
		t.Fatalf("environment logout error = %v", err)
	}
}

func TestOpenCodeGoCredentialUsesSubscriptionEnvironmentAndLoginURL(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "subscription-key")
	token, source, err := credentialForProvider(nil, "opencode-go")
	if err != nil || token != "subscription-key" || source != "environment override" {
		t.Fatalf("credential = %q/%q, %v", token, source, err)
	}
	statuses, err := providerStatuses(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range statuses {
		if status.Name == "opencode-go" {
			if status.Source != "environment override" || status.Description != "OpenCode Go subscription" || status.CredentialURL != "https://opencode.ai/auth" {
				t.Fatalf("OpenCode Go status = %#v", status)
			}
			return
		}
	}
	t.Fatal("OpenCode Go credential status is missing")
}

func TestAnthropicCredentialUsesNativeEnvironmentAndLoginURL(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "native-key")
	token, source, err := credentialForProvider(nil, "Anthropic")
	if err != nil || token != "native-key" || source != "environment override" {
		t.Fatalf("credential = %q/%q, %v", token, source, err)
	}
	statuses, err := providerStatuses(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range statuses {
		if status.Name == "anthropic" {
			if status.Source != "environment override" || status.Description != "model provider" ||
				status.CredentialURL != "https://platform.claude.com/settings/keys" {
				t.Fatalf("Anthropic status = %#v", status)
			}
			return
		}
	}
	t.Fatal("Anthropic credential status is missing")
}

func TestStoredAPIKeyAuthorizerUsesNativeMessagesHeader(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "subscription-key")
	request, _ := http.NewRequest(http.MethodPost, "https://example.test/messages", nil)
	authorizer := storedAPIKeyAuthorizer{provider: "opencode-go", modelURI: "opencode-go/minimax-m3"}
	if err := authorizer.Authorize(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := request.Header.Get("x-api-key"); got != "subscription-key" {
		t.Fatalf("x-api-key = %q", got)
	}
	if got := request.Header.Get("Authorization"); got != "" {
		t.Fatalf("unexpected Authorization = %q", got)
	}
}

func TestModelCanBeBuiltWithoutCredentialForInteractiveLogin(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	if _, err := buildModelBackend(testResolvedRoute(t, "deepseek/test", "", "", 0), store, modelBackendOptions{}); err != nil {
		t.Fatalf("interactive model build: %v", err)
	}
	if _, err := buildModelBackend(testResolvedRoute(t, "deepseek/test", "", "", 0), store, modelBackendOptions{requireCredential: true}); err == nil || !strings.Contains(err.Error(), "/login deepseek") || !errors.Is(err, agent.ErrInvalidRequest) {
		t.Fatalf("one-shot missing credential error = %v", err)
	}
	if _, err := buildModelBackend(testResolvedRoute(t, "deepseek/test", "", "https://gateway.example/v1", 0), store, modelBackendOptions{requireCredential: true}); err != nil {
		t.Fatalf("custom endpoint model build: %v", err)
	}
}

func TestOllamaModelNeverRequiresStoredCredential(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backend, err := buildModelBackend(testResolvedRoute(t, "ollama/qwen3:8b", "", "", 0), store, modelBackendOptions{requireCredential: true})
	if err != nil {
		t.Fatal(err)
	}
	info := backend.Info()
	if info.Provider != "ollama" || info.Model != "qwen3:8b" || info.Endpoint != "http://localhost:11434/v1" || !info.ContextWindowEstimated {
		t.Fatalf("Ollama model info = %#v", info)
	}
}

func TestBuildModelBackendUsesAutomaticContextUnlessOverridden(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	automatic, err := buildModelBackend(testResolvedRoute(t, "deepseek/deepseek-v4-flash", "", "", 0), store, modelBackendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := automatic.Info().ContextWindow; got != 1_000_000 {
		t.Fatalf("automatic context window = %d", got)
	}
	if automatic.Info().ProviderStateContract == "" {
		t.Fatalf("automatic model info = %#v", automatic.Info())
	}
	overridden, err := buildModelBackend(testResolvedRoute(t, "deepseek/deepseek-v4-flash", "", "https://gateway.example/v1", 64_000), store, modelBackendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := overridden.Info().ContextWindow; got != 64_000 {
		t.Fatalf("overridden context window = %d", got)
	}
	custom, err := buildModelBackend(testResolvedRoute(t, "deepseek/deepseek-v4-flash", "", "https://gateway.example/v1", 0), store, modelBackendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := custom.Info().ContextWindow; got != unknownModelContextWindow {
		t.Fatalf("custom endpoint fallback window = %d", got)
	}
}

func testResolvedRoute(t *testing.T, uri, effort, baseURL string, contextWindow int) resolvedModelRoute {
	t.Helper()
	route, err := resolveModelRoute(uri, effort, modelRouteOverrides{BaseURL: baseURL, ContextWindow: contextWindow}, modelRouteEnrichment{})
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func TestOptionalStoredAuthorizerSendsConfiguredKeyButAllowsNone(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	authorizer := storedBearerAuthorizer{
		store:        store,
		provider:     "deepseek",
		modelURI:     "deepseek/test",
		allowMissing: true,
	}
	request, _ := http.NewRequest(http.MethodPost, "https://gateway.example/v1", nil)
	if err := authorizer.Authorize(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := request.Header.Get("Authorization"); got != "" {
		t.Fatalf("authorization without key = %q", got)
	}
	if err := store.SetAPIKey("deepseek", "proxy-key"); err != nil {
		t.Fatal(err)
	}
	if err := authorizer.Authorize(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer proxy-key" {
		t.Fatalf("configured authorization = %q", got)
	}
}

func TestProviderStatusesReportCredentialSource(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "environment")
	t.Setenv("OPENROUTER_API_KEY", "")
	if err := store.SetAPIKey("deepseek", "stored"); err != nil {
		t.Fatal(err)
	}
	statuses, err := providerStatuses(store)
	if err != nil {
		t.Fatal(err)
	}
	sources := make(map[string]string)
	for _, status := range statuses {
		sources[status.Name] = status.Source
	}
	if sources["deepseek"] != "auth store" || sources["openai"] != "environment override" || sources["openrouter"] != "none" {
		t.Fatalf("provider sources = %#v", sources)
	}
}
