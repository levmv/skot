package modelconfig

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/levmv/skot/internal/codexauth"
	"github.com/levmv/skot/internal/state"
)

// CredentialStore keeps credential payloads opaque to their persistence layer.
// UpdateCredential serializes updates to a provider, including token refresh.
type CredentialStore interface {
	Credential(provider string) (state.CredentialProfile, error)
	UpdateCredential(context.Context, string, func(state.CredentialProfile) (state.CredentialProfile, error)) error
}

type CredentialSpec struct {
	Name          string
	Environment   string
	Description   string
	CredentialURL string
	Capabilities  CredentialCapability
}

type CredentialCapability uint8

const (
	CredentialModel CredentialCapability = 1 << iota
	CredentialWebSearch
	CredentialWebFetch
)

var CredentialCatalog = []CredentialSpec{
	{Name: "deepseek", Environment: "DEEPSEEK_API_KEY", Description: "model provider", CredentialURL: "https://platform.deepseek.com/api_keys", Capabilities: CredentialModel},
	{Name: "openrouter", Environment: "OPENROUTER_API_KEY", Description: "model provider", CredentialURL: "https://openrouter.ai/settings/keys", Capabilities: CredentialModel},
	{Name: "openai", Environment: "OPENAI_API_KEY", Description: "model provider", CredentialURL: "https://platform.openai.com/api-keys", Capabilities: CredentialModel},
	{Name: codexauth.Provider, Description: "ChatGPT subscription", Capabilities: CredentialModel},
	{Name: "anthropic", Environment: "ANTHROPIC_API_KEY", Description: "model provider", CredentialURL: "https://platform.claude.com/settings/keys", Capabilities: CredentialModel},
	{Name: "opencode-go", Environment: "OPENCODE_API_KEY", Description: "OpenCode Go subscription", CredentialURL: "https://opencode.ai/auth", Capabilities: CredentialModel},
	{Name: "keenable", Environment: "KEENABLE_API_KEY", Description: "web search and fetch", CredentialURL: "https://app.keenable.ai", Capabilities: CredentialWebSearch | CredentialWebFetch},
	{Name: "tavily", Environment: "TAVILY_API_KEY", Description: "web search", CredentialURL: "https://app.tavily.com", Capabilities: CredentialWebSearch},
	{Name: "firecrawl", Environment: "FIRECRAWL_API_KEY", Description: "web fetch", CredentialURL: "https://www.firecrawl.dev/app/api-keys", Capabilities: CredentialWebFetch},
	{Name: "exa", Environment: "EXA_API_KEY", Description: "web search and fetch", CredentialURL: "https://dashboard.exa.ai/api-keys", Capabilities: CredentialWebSearch | CredentialWebFetch},
}

type BearerAuthorizer struct {
	Store          CredentialStore
	Provider       string
	ModelURI       string
	AllowMissing   bool
	APIKey         string
	UseEnvironment bool
}

func (authorizer BearerAuthorizer) Authorize(_ context.Context, request *http.Request) error {
	token, err := authorizer.token()
	if err != nil {
		return err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	setProviderSessionHeader(request, authorizer.Provider)
	return nil
}

type APIKeyAuthorizer BearerAuthorizer

func (authorizer APIKeyAuthorizer) Authorize(_ context.Context, request *http.Request) error {
	token, err := BearerAuthorizer(authorizer).token()
	if err != nil {
		return err
	}
	if token != "" {
		request.Header.Set("x-api-key", token)
	}
	setProviderSessionHeader(request, authorizer.Provider)
	return nil
}

func setProviderSessionHeader(request *http.Request, provider string) {
	// OpenCode uses this header for routing and prompt caching across all APIs.
	// https://opencode.ai/docs/go/#supported-clients
	if provider == "opencode-go" {
		if sessionID := request.Header.Get("X-Session-ID"); sessionID != "" {
			request.Header.Set("x-opencode-session", sessionID)
		}
	}
}

func (authorizer BearerAuthorizer) token() (string, error) {
	if token := strings.TrimSpace(authorizer.APIKey); token != "" {
		return token, nil
	}
	token, _, err := CredentialForProvider(authorizer.Store, authorizer.Provider, authorizer.UseEnvironment)
	if err != nil {
		return "", err
	}
	if token == "" && !authorizer.AllowMissing {
		return "", missingProviderCredentialError(authorizer.Provider, authorizer.ModelURI)
	}
	return token, nil
}

func CredentialForProvider(store CredentialStore, provider string, useEnvironment bool) (token, source string, err error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == codexauth.Provider {
		tokens, err := StoredCodexTokens(store)
		if errors.Is(err, ErrInvalidCodexCredentials) {
			// Reauthorization must remain available for an incomplete profile.
			return "", "none", nil
		}
		if err != nil {
			return "", "", err
		}
		if tokens.Valid() {
			return tokens.AccessToken, "auth store", nil
		}
		return "", "none", nil
	}
	if useEnvironment {
		if token := strings.TrimSpace(os.Getenv(ProviderEnvironment(provider))); token != "" {
			return token, "environment override", nil
		}
	}
	if store == nil {
		return "", "none", nil
	}
	profile, err := store.Credential(provider)
	if err != nil {
		return "", "", err
	}
	if profile.Kind == "api_key" && strings.EqualFold(strings.TrimSpace(profile.Provider), provider) {
		var payload struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(profile.Payload, &payload); err != nil {
			return "", "", fmt.Errorf("decode %s API key profile: %w", provider, err)
		}
		if token := strings.TrimSpace(payload.Token); token != "" {
			return token, "auth store", nil
		}
	}
	return "", "none", nil
}

func ProviderEnvironment(provider string) string {
	if spec, ok := credentialProviderSpec(provider); ok {
		return spec.Environment
	}
	return ""
}

func credentialProviderSpec(provider string) (CredentialSpec, bool) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	for _, spec := range CredentialCatalog {
		if spec.Name == provider {
			return spec, true
		}
	}
	return CredentialSpec{}, false
}

func CredentialEnvironmentNames() []string {
	names := make([]string, 0, len(CredentialCatalog))
	for _, spec := range CredentialCatalog {
		if spec.Environment != "" {
			names = append(names, spec.Environment)
		}
	}
	return names
}

func missingProviderCredentialError(provider, modelURI string) error {
	if provider == codexauth.Provider {
		return fmt.Errorf("ChatGPT login is unavailable for model %q; supply OAuth credentials (in Skot, use /login openai-codex)", modelURI)
	}
	return fmt.Errorf(
		"%s API key is unavailable for model %q; supply an API key or credentials (in Skot, set %s or use /login %s)",
		provider, modelURI, ProviderEnvironment(provider), provider,
	)
}

func KnownCredentialProvider(provider string) bool {
	_, exists := credentialProviderSpec(provider)
	return exists
}
