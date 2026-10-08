// Package provider configures model connections and backends, including protocol
// selection and authentication. Use Model.Connection for raw protocol requests,
// or Model.Backend for item-based calls. Each call makes one generation attempt;
// the caller manages retries, tool execution, and conversation history.
package provider

import (
	"context"
	"net/http"
	"time"

	"github.com/levmv/skot/internal/modelconfig"
	"github.com/levmv/skot/internal/state"
	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/transport"
)

// Config selects a provider/model using the same catalog and protocol defaults
// as the Skot application. URI is required; no application preferences are read.
type Config struct {
	URI             string
	ReasoningEffort string
	// API overrides the route's protocol: chat_completions, responses, or
	// anthropic_messages. Usually the model catalog supplies it.
	API           string
	BaseURL       string
	ContextWindow int
	// APIKey takes precedence over environment and stored keys. For ChatGPT
	// subscriptions, supply Credentials containing an OAuth login instead.
	APIKey string
	// Credentials is authoritative when supplied: environment variables are
	// ignored, even when the store has no key for the selected provider.
	// With nil, API keys are read from provider environment variables.
	Credentials CredentialStore
	// HTTPClient is borrowed for generation, token refresh, and model metadata.
	HTTPClient *http.Client
}

// Model provides a resolved route for either raw protocol requests through
// Connection or item-based calls through Backend. Pass Backend and Info to agent.New.
type Model struct {
	Connection *transport.Connection
	Backend    model.Backend
	Info       model.Info
}

// Open resolves a model and prepares its connection and backend. It may fetch OpenRouter
// context-window metadata. The returned Model does not need to be closed.
func Open(ctx context.Context, config Config) (*Model, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	api, err := modelconfig.ParseAPI(config.API)
	if err != nil {
		return nil, model.MarkInvalidRequest(err)
	}
	lookup := modelconfig.OpenRouterContextWindow
	if config.HTTPClient != nil {
		lookup = func(ctx context.Context, modelID string) (int, error) {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			return modelconfig.FetchOpenRouterContextWindow(ctx, config.HTTPClient, modelconfig.OpenRouterModelsURL, modelID)
		}
	}
	route, err := modelconfig.Activate(ctx, config.URI, config.ReasoningEffort, modelconfig.Overrides{
		API: api, BaseURL: config.BaseURL, ContextWindow: config.ContextWindow,
	}, nil, lookup)
	if err != nil {
		return nil, model.MarkInvalidRequest(err)
	}
	info, err := modelconfig.Info(route)
	if err != nil {
		return nil, model.MarkInvalidRequest(err)
	}
	backend, connection, err := modelconfig.BuildBackend(route, config.Credentials, modelconfig.BackendOptions{
		RequireCredential: true, HTTPClient: config.HTTPClient, APIKey: config.APIKey,
		UseEnvironment: config.Credentials == nil,
	})
	if err != nil {
		return nil, err
	}
	return &Model{Connection: connection, Backend: backend, Info: info}, nil
}

// Credential is an opaque provider credential. Preserve its payload unchanged
// when storing it; Skot interprets it and supplies replacements on OAuth refresh.
type Credential = state.CredentialProfile

// CredentialStore may be implemented by the host application's storage.
// Credential returns an owned snapshot, or a zero Credential when absent.
// UpdateCredential must serialize updates to the same provider and persist the
// callback's result before returning success. Cancellation may stop waiting for
// the lock, but must not discard a successful callback's result: an OAuth refresh
// may already have rotated the token. Do not retry a successful callback.
type CredentialStore interface {
	Credential(provider string) (Credential, error)
	UpdateCredential(context.Context, string, func(Credential) (Credential, error)) error
}

// OpenCredentials opens Skot's file credential store, sharing logins and refreshed
// tokens with Skot. Empty home selects ~/.skot; pass a custom Skot home explicitly.
func OpenCredentials(home string) (CredentialStore, error) {
	home, err := state.ResolveHome(home)
	if err != nil {
		return nil, err
	}
	store, err := state.OpenCredentials(home)
	if err != nil {
		return nil, err
	}
	return store, nil
}
