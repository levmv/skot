package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/levmv/skot/internal/state"
)

type providerCredentialSpec struct {
	name          string
	environment   string
	description   string
	credentialURL string
	capabilities  credentialCapability
}

type credentialCapability uint8

const (
	credentialModel credentialCapability = 1 << iota
	credentialWebSearch
	credentialWebFetch
)

var providerCredentialCatalog = []providerCredentialSpec{
	{name: "deepseek", environment: "DEEPSEEK_API_KEY", description: "model provider", credentialURL: "https://platform.deepseek.com/api_keys", capabilities: credentialModel},
	{name: "openrouter", environment: "OPENROUTER_API_KEY", description: "model provider", credentialURL: "https://openrouter.ai/settings/keys", capabilities: credentialModel},
	{name: "openai", environment: "OPENAI_API_KEY", description: "model provider", credentialURL: "https://platform.openai.com/api-keys", capabilities: credentialModel},
	{name: "anthropic", environment: "ANTHROPIC_API_KEY", description: "model provider", credentialURL: "https://platform.claude.com/settings/keys", capabilities: credentialModel},
	{name: "opencode-go", environment: "OPENCODE_API_KEY", description: "OpenCode Go subscription", credentialURL: "https://opencode.ai/auth", capabilities: credentialModel},
	{name: "tavily", environment: "TAVILY_API_KEY", description: "web search", credentialURL: "https://app.tavily.com", capabilities: credentialWebSearch},
	{name: "firecrawl", environment: "FIRECRAWL_API_KEY", description: "web fetch fallback", credentialURL: "https://www.firecrawl.dev/app/api-keys", capabilities: credentialWebFetch},
	{name: "exa", environment: "EXA_API_KEY", description: "web search and fetch", credentialURL: "https://dashboard.exa.ai/api-keys", capabilities: credentialWebSearch | credentialWebFetch},
}

type storedBearerAuthorizer struct {
	store        *state.Store
	provider     string
	modelURI     string
	allowMissing bool
}

func (authorizer storedBearerAuthorizer) Authorize(_ context.Context, request *http.Request) error {
	token, err := storedRequestCredential(authorizer.store, authorizer.provider, authorizer.modelURI, authorizer.allowMissing)
	if err != nil {
		return err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

type storedAPIKeyAuthorizer storedBearerAuthorizer

func (authorizer storedAPIKeyAuthorizer) Authorize(_ context.Context, request *http.Request) error {
	token, err := storedRequestCredential(authorizer.store, authorizer.provider, authorizer.modelURI, authorizer.allowMissing)
	if err != nil {
		return err
	}
	if token != "" {
		request.Header.Set("x-api-key", token)
	}
	return nil
}

func storedRequestCredential(store *state.Store, provider, modelURI string, allowMissing bool) (string, error) {
	token, _, err := credentialForProvider(store, provider)
	if err != nil {
		return "", err
	}
	if token == "" && !allowMissing {
		return "", missingProviderCredentialError(provider, modelURI)
	}
	return token, nil
}

func credentialForProvider(store *state.Store, provider string) (token, source string, err error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if token := strings.TrimSpace(os.Getenv(providerEnvironment(provider))); token != "" {
		return token, "environment override", nil
	}
	if store == nil {
		return "", "none", nil
	}
	token, ok, err := store.APIKey(provider)
	if err != nil {
		return "", "", err
	}
	if ok {
		return token, "auth store", nil
	}
	return "", "none", nil
}

func providerEnvironment(provider string) string {
	if spec, ok := credentialProviderSpec(provider); ok {
		return spec.environment
	}
	return ""
}

func credentialProviderSpec(provider string) (providerCredentialSpec, bool) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	for _, spec := range providerCredentialCatalog {
		if spec.name == provider {
			return spec, true
		}
	}
	return providerCredentialSpec{}, false
}

func credentialEnvironmentNames() []string {
	names := make([]string, 0, len(providerCredentialCatalog))
	for _, spec := range providerCredentialCatalog {
		names = append(names, spec.environment)
	}
	return names
}

func missingProviderCredentialError(provider, modelURI string) error {
	return fmt.Errorf(
		"%s API key is unavailable for model %q; set %s or start interactive Skot and use /login %s",
		provider, modelURI, providerEnvironment(provider), provider,
	)
}

func providerStatuses(store *state.Store) ([]ProviderStatus, error) {
	statuses := make([]ProviderStatus, 0, len(providerCredentialCatalog))
	for _, spec := range providerCredentialCatalog {
		_, source, err := credentialForProvider(store, spec.name)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, ProviderStatus{
			Name:          spec.name,
			Source:        source,
			Description:   spec.description,
			CredentialURL: spec.credentialURL,
		})
	}
	return statuses, nil
}

func storeProviderCredential(store *state.Store, provider, token string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !knownCredentialProvider(provider) {
		return fmt.Errorf("unsupported login provider %q", provider)
	}
	if strings.TrimSpace(os.Getenv(providerEnvironment(provider))) != "" {
		return fmt.Errorf("%s is supplied by an environment override; unset %s to replace it", provider, providerEnvironment(provider))
	}
	if store == nil {
		return errors.New("auth store is unavailable")
	}
	if strings.TrimSpace(token) == "" {
		return errors.New("API key is required")
	}
	return store.SetAPIKey(provider, token)
}

func deleteProviderCredential(store *state.Store, provider string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !knownCredentialProvider(provider) {
		return fmt.Errorf("unsupported logout provider %q", provider)
	}
	if strings.TrimSpace(os.Getenv(providerEnvironment(provider))) != "" {
		return fmt.Errorf("%s is supplied by an environment override; unset %s to log out", provider, providerEnvironment(provider))
	}
	if store == nil {
		return errors.New("auth store is unavailable")
	}
	return store.DeleteAPIKey(provider)
}

func knownCredentialProvider(provider string) bool {
	_, exists := credentialProviderSpec(provider)
	return exists
}
