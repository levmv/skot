package app

import (
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/codexauth"
	"github.com/levmv/skot/internal/session"
	"github.com/levmv/skot/internal/state"
)

func testCodexTokens() codexauth.Tokens {
	access := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account-1","chatgpt_compute_residency":"eu"}}`)) + ".signature"
	return codexauth.Tokens{AccessToken: access, RefreshToken: "refresh-private", ExpiresAt: time.Now().Add(time.Hour), AccountID: "account-1", Residency: "eu"}
}

func saveTestCodexTokens(t *testing.T, store *state.Store, tokens codexauth.Tokens) {
	t.Helper()
	profile, err := codexProfile(tokens)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateCredential(t.Context(), codexauth.Provider, func(state.CredentialProfile) (state.CredentialProfile, error) { return profile, nil }); err != nil {
		t.Fatal(err)
	}
}

func newCodexTestApp(t *testing.T) *Application {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	saveTestCodexTokens(t, store, testCodexTokens())
	journal, _, err := session.CreateMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	runtime, err := newApplicationTestRuntime(agent.Config{
		Model:   agent.ModelInfo{BackendID: "responses.openai-codex", Provider: codexauth.Provider, Model: "gpt-6-astra"},
		Journal: journal,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Application{
		config: applicationConfig{settings: store, masker: newSecretMasker(store)},
		state:  applicationState{session: newLiveSession("", runtime, nil, false)},
	}
}

func codexResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestCodexSubscriptionUsesOwnEndpointCredentialAndToolCalls(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "separately-billed-key")
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tokens := testCodexTokens()
	saveTestCodexTokens(t, store, tokens)
	client := &http.Client{Transport: appRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != codexauth.BaseURL+"/responses" || request.Header.Get("Authorization") != "Bearer "+tokens.AccessToken || request.Header.Get("ChatGPT-Account-Id") != "account-1" || request.Header.Get("originator") != "skot" || request.Header.Get("session-id") != "session-1" || request.Header.Get("x-openai-internal-codex-residency") != "eu" {
			t.Fatal("subscription request used the wrong destination or credentials")
		}
		var body struct {
			Model          string   `json:"model"`
			Instructions   *string  `json:"instructions"`
			Store          *bool    `json:"store"`
			Stream         bool     `json:"stream"`
			Include        []string `json:"include"`
			PromptCacheKey string   `json:"prompt_cache_key"`
			Tools          []struct {
				Type   string `json:"type"`
				Name   string `json:"name"`
				Strict bool   `json:"strict"`
			} `json:"tools"`
			Input []jsontext.Value `json:"input"`
		}
		if err := json.UnmarshalRead(request.Body, &body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "gpt-6-astra" || body.Instructions == nil || *body.Instructions != "" || body.Store == nil || *body.Store || !body.Stream || len(body.Include) != 1 || body.Include[0] != "reasoning.encrypted_content" || body.PromptCacheKey != "session-1" || len(body.Tools) != 1 || body.Tools[0].Type != "function" || body.Tools[0].Name != "read" || body.Tools[0].Strict || len(body.Input) != 1 {
			t.Fatalf("invalid Codex request: %+v", body)
		}
		return codexResponse(200, `data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"checking file"}],"encrypted_content":"ciphertext"}}

data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read","arguments":"{\"path\":\"README.md\"}","status":"completed"}}

data: {"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":4},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":3},"total_tokens":17}}}

`), nil
	})}
	backend, err := buildModelBackend(testResolvedRoute(t, "openai-codex/gpt-6-astra", "high", "", 0), store, modelBackendOptions{requireCredential: true, httpClient: client})
	if err != nil {
		t.Fatal(err)
	}
	result, err := backend.Complete(t.Context(), agent.ModelRequest{
		SessionID: "session-1", Items: []agent.Item{{Kind: agent.ItemUserText, Text: "inspect README"}},
		Tools: []agent.ToolSpec{{Name: "read", InputSchema: jsontext.Value(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != "tool_calls" || len(result.Items) != 2 || result.Items[0].Kind != agent.ItemReasoning || len(result.Items[0].ProviderData) != 1 || result.Items[1].ToolCall == nil || result.Items[1].ToolCall.Name != "read" || result.Items[1].ToolCall.RawArguments != `{"path":"README.md"}` || result.Usage.CachedInputTokens != 4 || result.Usage.ReasoningTokens != 3 {
		t.Fatalf("Codex response lost tool or reasoning state: %+v", result)
	}
}

func TestCodexNeverFallsBackToAPIKeyOrCustomEndpoint(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "separately-billed-key")
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	route := testResolvedRoute(t, "openai-codex/gpt-6-astra", "", "", 0)
	if _, err := buildModelBackend(route, store, modelBackendOptions{requireCredential: true}); err == nil || !strings.Contains(err.Error(), "/login openai-codex") {
		t.Fatalf("missing subscription error = %v", err)
	}
	backend, err := buildModelBackend(route, store, modelBackendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Complete(t.Context(), agent.ModelRequest{}, nil); err == nil || !strings.Contains(err.Error(), "/login openai-codex") {
		t.Fatalf("missing request credential = %v", err)
	}
	if err := storeProviderCredential(t.Context(), store, codexauth.Provider, "key"); err == nil {
		t.Fatal("accepted an API key as a subscription")
	}
	for _, override := range []modelRouteOverrides{{BaseURL: "https://gateway.example/v1"}, {API: modelAPIChatCompletions}, {API: modelAPIAnthropicMessages}} {
		if _, err := resolveModelRoute(route.URI, "", override, modelRouteEnrichment{}); err == nil {
			t.Fatal("accepted an unsupported subscription override")
		}
	}
	saveTestCodexTokens(t, store, testCodexTokens())
	request, _ := http.NewRequest("POST", "https://gateway.example/responses", nil)
	if err := (codexAuthorizer{store: store}).Authorize(t.Context(), request); err == nil || request.Header.Get("Authorization") != "" {
		t.Fatal("sent ChatGPT credentials to an unrelated endpoint")
	}
	if err := deleteProviderCredential(t.Context(), store, codexauth.Provider); err != nil {
		t.Fatal(err)
	}
	if _, source, err := credentialForProvider(store, codexauth.Provider); err != nil || source != "none" {
		t.Fatalf("logout status = %s, %v", source, err)
	}
	if token, _, _ := credentialForProvider(store, "openai"); token != "separately-billed-key" {
		t.Fatal("subscription logout changed the API credential")
	}
}

func TestCodexRefreshIsSharedAcrossStoresAndMasksRotatedTokens(t *testing.T) {
	home := t.TempDir()
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	expired := testCodexTokens()
	expired.AccessToken = "old-private-access"
	expired.ExpiresAt = time.Now().Add(-time.Hour)
	saveTestCodexTokens(t, store, expired)
	if err := store.SetAPIKey(t.Context(), "deepseek", "other-key"); err != nil {
		t.Fatal(err)
	}
	masker := newSecretMasker(store)
	var calls atomic.Int32
	refreshed := testCodexTokens()
	refreshed.RefreshToken = "rotated-private-refresh"
	client := &http.Client{Transport: appRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.URL.String() != "https://auth.openai.com/oauth/token" {
			t.Error("wrong refresh destination")
		}
		if err := request.ParseForm(); err != nil {
			return nil, err
		}
		if request.Form.Get("refresh_token") != expired.RefreshToken {
			t.Error("reused an obsolete refresh token")
		}
		body, _ := json.Marshal(map[string]any{"access_token": refreshed.AccessToken, "refresh_token": refreshed.RefreshToken, "expires_in": 3600})
		return codexResponse(200, string(body)), nil
	})}
	var group sync.WaitGroup
	for range 8 {
		other, err := state.Open(home)
		if err != nil {
			t.Fatal(err)
		}
		group.Go(func() {
			request, _ := http.NewRequest("POST", codexauth.BaseURL+"/responses", nil)
			err := (codexAuthorizer{store: other, client: client, masker: masker}).Authorize(t.Context(), request)
			if err != nil {
				t.Error(err)
				return
			}
			if request.Header.Get("Authorization") != "Bearer "+refreshed.AccessToken {
				t.Error("request used an expired token")
			}
		})
	}
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls.Load())
	}
	saved, err := storedCodexTokens(store)
	if err != nil || saved.RefreshToken != refreshed.RefreshToken || saved.NeedsRefresh() {
		t.Fatal("rotated credentials were not saved")
	}
	for _, secret := range []string{expired.AccessToken, expired.RefreshToken, refreshed.AccessToken, refreshed.RefreshToken} {
		if masker.Redact(secret) != "[REDACTED]" {
			t.Fatal("an OAuth secret is missing from redaction")
		}
	}
	if value, ok, err := store.APIKey("deepseek"); err != nil || !ok || value != "other-key" {
		t.Fatal("refresh overwrote another provider credential")
	}
	for _, name := range []string{"auth.json", "auth.lock"} {
		info, err := os.Stat(filepath.Join(home, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s is not private", name)
		}
	}
}

func TestCodexRefreshKeepsTemporaryFailuresRetryable(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tokens := testCodexTokens()
	tokens.ExpiresAt = time.Now().Add(-time.Hour)
	saveTestCodexTokens(t, store, tokens)
	for _, status := range []int{0, http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusBadRequest} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client := &http.Client{Transport: appRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.String() != "https://auth.openai.com/oauth/token" {
					t.Fatal("model request was sent without refreshing the expired token")
				}
				if status == 0 {
					return nil, errors.New("connection reset")
				}
				response := codexResponse(status, "private-token-details")
				response.Header.Set("Retry-After", "5")
				return response, nil
			})}
			backend, err := buildModelBackend(testResolvedRoute(t, "openai-codex/gpt-6-astra", "", "", 0), store, modelBackendOptions{httpClient: client})
			if err != nil {
				t.Fatal(err)
			}
			_, err = backend.Complete(t.Context(), agent.ModelRequest{}, nil)
			retryable := status != http.StatusBadRequest
			if err == nil || errors.Is(err, agent.ErrProviderFailure) != retryable || errors.Is(err, agent.ErrInvalidRequest) == retryable || strings.Contains(err.Error(), "private-token-details") {
				t.Fatalf("refresh error = %v, want retryable = %t", err, retryable)
			}
			if status != 0 && retryable {
				details, ok := errors.AsType[*agent.ProviderError](err)
				if !ok || !details.Retryable || details.RetryAfter != 5*time.Second {
					t.Fatalf("lost token endpoint retry policy: %v", err)
				}
			}
		})
	}
}

func TestCodexUsageLimitsAreNotRetriedAsTemporaryRateLimits(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	saveTestCodexTokens(t, store, testCodexTokens())
	for _, test := range []struct {
		signal string
		kind   agent.ProviderErrorKind
	}{
		{"usage_limit_reached", agent.ProviderErrorQuota}, {"usage_not_included", agent.ProviderErrorSubscription},
	} {
		for _, transport := range []string{"HTTP", "response.done"} {
			t.Run(test.signal+"/"+transport, func(t *testing.T) {
				client := &http.Client{Transport: appRoundTripFunc(func(*http.Request) (*http.Response, error) {
					if transport == "response.done" {
						return codexResponse(200, fmt.Sprintf("data: {\"type\":\"response.done\",\"response\":{\"status\":\"failed\",\"error\":{\"type\":%q,\"message\":\"allowance unavailable\"}}}\n\n", test.signal)), nil
					}
					return codexResponse(429, fmt.Sprintf(`{"error":{"type":%q,"message":"allowance unavailable"}}`, test.signal)), nil
				})}
				backend, err := buildModelBackend(testResolvedRoute(t, "openai-codex/gpt-6-astra", "", "", 0), store, modelBackendOptions{httpClient: client})
				if err != nil {
					t.Fatal(err)
				}
				_, err = backend.Complete(t.Context(), agent.ModelRequest{}, nil)
				classified, ok := errors.AsType[*agent.ProviderError](err)
				if !ok || classified.Kind != test.kind || classified.Retryable {
					t.Fatalf("limit error = %v", err)
				}
			})
		}
	}
}

func TestIncompleteCodexCredentialCanBeReauthorizedAndCancelled(t *testing.T) {
	home := t.TempDir()
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	profile := state.CredentialProfile{Provider: codexauth.Provider, Kind: "oauth", Payload: jsontext.Value(`{"access_token":"incomplete-token"}`)}
	if err := store.UpdateCredential(t.Context(), codexauth.Provider, func(state.CredentialProfile) (state.CredentialProfile, error) { return profile, nil }); err != nil {
		t.Fatal(err)
	}
	profile, err = store.Credential(codexauth.Provider)
	if err != nil {
		t.Fatal(err)
	}
	application, err := Open(t.Context(), Config{Home: home, Root: t.TempDir(), ModelURI: "openai-codex/gpt-6-astra", Interactive: true, ToolSet: "none"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	statuses, err := application.ProviderStatuses()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, status := range statuses {
		if status.Name == codexauth.Provider {
			found = true
			if status.Source != "none" || !status.BrowserLogin {
				t.Fatal("incomplete credentials did not offer browser login")
			}
		}
	}
	if !found {
		t.Fatal("ChatGPT login is missing")
	}
	login, err := application.BeginBrowserLogin(t.Context(), codexauth.Provider)
	if err != nil {
		t.Fatal(err)
	}
	login.Close()
	if err := login.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled login = %v", err)
	}
	if err := login.Complete(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled login could be saved: %v", err)
	}
	saved, err := store.Credential(codexauth.Provider)
	if err != nil || string(saved.Payload) != string(profile.Payload) {
		t.Fatal("cancelled login changed existing credentials")
	}
	if err := application.Logout(t.Context(), codexauth.Provider); err != nil {
		t.Fatal(err)
	}
}
