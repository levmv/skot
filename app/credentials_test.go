package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/levmv/skot/internal/codexauth"
	"github.com/levmv/skot/internal/modelconfig"
	"github.com/levmv/skot/internal/state"
	"github.com/levmv/skot/model"
)

func TestCredentialChangesCanCancelWhileAnotherProcessRefreshes(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	for _, action := range []string{"login", "logout"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				application := newCodexTestApp(t)
				store := application.config.settings
				if err := store.SetAPIKey(t.Context(), "deepseek", "existing-key"); err != nil {
					t.Fatal(err)
				}
				locked, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
				go func() {
					finished <- store.UpdateCredential(t.Context(), codexauth.Provider, func(profile state.CredentialProfile) (state.CredentialProfile, error) {
						close(locked)
						<-release
						return profile, nil
					})
				}()
				defer func() { close(release); <-finished }()
				<-locked
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				result := make(chan error, 1)
				go func() {
					if action == "login" {
						result <- application.Login(ctx, "deepseek", "replacement-key")
					} else {
						result <- application.Logout(ctx, codexauth.Provider)
					}
				}()
				// Wait until the mutation is blocked on the held lock, then cancel.
				synctest.Wait()
				cancel()
				synctest.Wait()
				select {
				case err := <-result:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancelled %s = %v", action, err)
					}
				default:
					t.Fatal("credential change ignored cancellation while waiting for the lock")
				}
				if token, ok, err := store.APIKey("deepseek"); err != nil || !ok || token != "existing-key" {
					t.Fatal("cancelled login changed the stored key")
				}
				if tokens, err := modelconfig.StoredCodexTokens(store); err != nil || !tokens.Valid() {
					t.Fatal("cancelled logout removed the subscription")
				}
			})
		})
	}
}

func TestStoredCredentialIsUsedAndEnvironmentOverridesIt(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "")
	if err := storeProviderCredential(t.Context(), store, " OpenAI ", "stored-key"); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, "https://example.test", nil)
	authorizer := modelconfig.BearerAuthorizer{UseEnvironment: true, Store: store, Provider: "openai", ModelURI: "openai/test"}
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
	if err := deleteProviderCredential(t.Context(), store, "openai"); err == nil || !strings.Contains(err.Error(), "environment override") {
		t.Fatalf("environment logout error = %v", err)
	}
}

func TestOpenCodeGoCredentialUsesSubscriptionEnvironmentAndLoginURL(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "subscription-key")
	token, source, err := modelconfig.CredentialForProvider(nil, "opencode-go", true)
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
	token, source, err := modelconfig.CredentialForProvider(nil, "Anthropic", true)
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

func TestModelCanBeBuiltWithoutCredentialForInteractiveLogin(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	if _, err := modelconfig.BuildBackend(testResolvedRoute(t, "deepseek/test", "", "", 0), store, modelconfig.BackendOptions{UseEnvironment: true}); err != nil {
		t.Fatalf("interactive model build: %v", err)
	}
	if _, err := modelconfig.BuildBackend(testResolvedRoute(t, "deepseek/test", "", "", 0), store, modelconfig.BackendOptions{UseEnvironment: true, RequireCredential: true}); err == nil || !strings.Contains(err.Error(), "/login deepseek") || !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("one-shot missing credential error = %v", err)
	}
	if _, err := modelconfig.BuildBackend(testResolvedRoute(t, "deepseek/test", "", "https://gateway.example/v1", 0), store, modelconfig.BackendOptions{UseEnvironment: true, RequireCredential: true}); err != nil {
		t.Fatalf("custom endpoint model build: %v", err)
	}
}

func TestModelInfoUsesAutomaticContextUnlessOverridden(t *testing.T) {
	automatic, err := modelconfig.Info(testResolvedRoute(t, "deepseek/deepseek-v4-flash", "", "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if got := automatic.ContextWindow; got != 1_000_000 {
		t.Fatalf("automatic context window = %d", got)
	}
	if automatic.ProviderStateContract == "" {
		t.Fatalf("automatic model info = %#v", automatic)
	}
	overridden, err := modelconfig.Info(testResolvedRoute(t, "deepseek/deepseek-v4-flash", "", "https://gateway.example/v1", 64_000))
	if err != nil {
		t.Fatal(err)
	}
	if got := overridden.ContextWindow; got != 64_000 {
		t.Fatalf("overridden context window = %d", got)
	}
	custom, err := modelconfig.Info(testResolvedRoute(t, "deepseek/deepseek-v4-flash", "", "https://gateway.example/v1", 0))
	if err != nil {
		t.Fatal(err)
	}
	if got := custom.ContextWindow; got != modelconfig.FallbackContextWindow {
		t.Fatalf("custom endpoint fallback window = %d", got)
	}
}

func testResolvedRoute(t *testing.T, uri, effort, baseURL string, contextWindow int) modelconfig.Route {
	t.Helper()
	route, err := modelconfig.Resolve(uri, effort, modelconfig.Overrides{BaseURL: baseURL, ContextWindow: contextWindow}, modelconfig.Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func TestProviderStatusesReportCredentialSource(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "environment")
	t.Setenv("OPENROUTER_API_KEY", "")
	if err := store.SetAPIKey(t.Context(), "deepseek", "stored"); err != nil {
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
