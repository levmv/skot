package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/levmv/skot/internal/codexauth"
	"github.com/levmv/skot/internal/modelconfig"
	"github.com/levmv/skot/internal/state"
)

func providerStatuses(store modelconfig.CredentialStore, registry *modelconfig.Registry) ([]ProviderStatus, error) {
	statuses := make([]ProviderStatus, 0, len(modelconfig.CredentialCatalog))
	for _, spec := range registry.CredentialSpecs() {
		_, source, err := registry.CredentialForProvider(store, spec.Name, true)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, ProviderStatus{
			Name:          spec.Name,
			Source:        source,
			Environment:   spec.Environment,
			Description:   spec.Description,
			CredentialURL: spec.CredentialURL,
			ToolService:   spec.Capabilities&modelconfig.CredentialModel == 0,
			BrowserLogin:  spec.Name == codexauth.Provider,
		})
	}
	return statuses, nil
}

func storeProviderCredential(ctx context.Context, store *state.Store, provider, token string, registry *modelconfig.Registry) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == codexauth.Provider {
		return errors.New("openai-codex requires browser login; use /login openai-codex")
	}
	if !registry.KnownCredentialProvider(provider) {
		return fmt.Errorf("unsupported login provider %q", provider)
	}
	if spec, err := registry.Provider(provider); err == nil && spec.AuthNone {
		return fmt.Errorf("provider %q does not use an API key", provider)
	}
	if store == nil {
		return errors.New("auth store is unavailable")
	}
	if strings.TrimSpace(token) == "" {
		return errors.New("API key is required")
	}
	return store.SetAPIKey(ctx, provider, token)
}

func deleteProviderCredential(ctx context.Context, store *state.Store, provider string, registry *modelconfig.Registry) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !registry.KnownCredentialProvider(provider) {
		return fmt.Errorf("unsupported logout provider %q", provider)
	}
	if strings.TrimSpace(os.Getenv(registry.ProviderEnvironment(provider))) != "" {
		return fmt.Errorf("%s is supplied by an environment override; unset %s to log out", provider, registry.ProviderEnvironment(provider))
	}
	if store == nil {
		return errors.New("auth store is unavailable")
	}
	if provider == codexauth.Provider {
		return store.UpdateCredential(ctx, provider, func(state.CredentialProfile) (state.CredentialProfile, error) {
			return state.CredentialProfile{}, nil
		})
	}
	return store.DeleteAPIKey(ctx, provider)
}
