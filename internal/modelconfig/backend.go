package modelconfig

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	productlimits "github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/internal/modelhttp"
	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/anthropic"
	"github.com/levmv/skot/model/chatcompletions"
	responsemodel "github.com/levmv/skot/model/responses"
	"github.com/levmv/skot/model/transport"
)

type BackendOptions struct {
	RequireCredential bool
	UseEnvironment    bool
	HTTPClient        *http.Client
	Masker            interface{ Add(string) }
	APIKey            string
}

func Info(route Route) (model.Info, error) {
	var backendID string
	switch route.API {
	case ChatCompletions:
		backendID = chatcompletions.BackendID(route.Provider)
	case Responses:
		backendID = responsemodel.BackendID(route.Provider)
	case AnthropicMessages:
		backendID = anthropic.BackendID(route.Provider)
	default:
		return model.Info{}, fmt.Errorf("unsupported model API %q", route.API)
	}
	return model.Info{
		BackendID: backendID, Provider: route.Provider, Model: route.Model,
		ReasoningEffort: route.ReasoningEffort, ProviderStateContract: route.ProviderStateContract,
		ImageInputUnsupported: route.ImageInputUnsupported,
		ContextWindow:         route.ContextWindow, ContextWindowEstimated: route.ContextWindowEstimated,
		MaxRequestBytes: productlimits.MaxModelRequestBytes, MaxCompletionBytes: productlimits.MaxModelCompletionBytes,
		Endpoint: modelhttp.PublicEndpoint(route.BaseURL),
	}, nil
}

func BuildBackend(route Route, credentials CredentialStore, options BackendOptions) (model.Backend, *transport.Connection, error) {
	options.APIKey = strings.TrimSpace(options.APIKey)
	if route.Provider == "openai-codex" && options.APIKey != "" {
		return nil, nil, model.MarkInvalidRequest(errors.New("openai-codex requires OAuth credentials, not an API key"))
	}
	if !KnownAPI(route.API) {
		return nil, nil, model.MarkInvalidRequest(fmt.Errorf("unsupported model API %q", route.API))
	}
	if options.RequireCredential && options.APIKey == "" && !route.CustomEndpoint && !route.Credentialless {
		token, _, err := CredentialForProvider(credentials, route.Provider, options.UseEnvironment)
		if err != nil {
			return nil, nil, err
		}
		if token == "" {
			return nil, nil, model.MarkInvalidRequest(missingProviderCredentialError(route.Provider, route.URI))
		}
	}
	var authorizer transport.Authorizer = BearerAuthorizer{
		Store: credentials, Provider: route.Provider, ModelURI: route.URI, AllowMissing: route.CustomEndpoint || route.Credentialless, APIKey: options.APIKey,
		UseEnvironment: options.UseEnvironment,
	}
	if route.Provider == "openai-codex" {
		if route.CustomEndpoint || route.API != Responses {
			return nil, nil, model.MarkInvalidRequest(errors.New("openai-codex requires the ChatGPT Codex endpoint and Responses API"))
		}
		authorizer = CodexAuthorizer{Store: credentials, ModelURI: route.URI, Client: options.HTTPClient, Masker: options.Masker}
		options.HTTPClient = CodexHTTPClient(options.HTTPClient)
	}
	path := "/chat/completions"
	switch route.API {
	case Responses:
		path = "/responses"
	case AnthropicMessages:
		path = "/messages"
		authorizer = APIKeyAuthorizer{
			Store: credentials, Provider: route.Provider, ModelURI: route.URI, AllowMissing: route.CustomEndpoint || route.Credentialless, APIKey: options.APIKey,
			UseEnvironment: options.UseEnvironment,
		}
	}
	apiModel := route.APIModel
	if apiModel == "" {
		apiModel = route.Model
	}
	connection, err := transport.New(transport.Config{
		API: string(route.API), Provider: route.Provider, APIModel: apiModel,
		Endpoint:   strings.TrimRight(strings.TrimSpace(route.BaseURL), "/") + path,
		HTTPClient: options.HTTPClient, Authorizer: authorizer, Header: route.Header,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("initialize connection: %w", err)
	}
	var backend model.Backend
	switch route.API {
	case ChatCompletions:
		backend, err = chatcompletions.New(chatcompletions.Config{
			Connection:      connection,
			ReasoningEffort: route.ReasoningEffort, Traits: route.ChatTraits,
		})
	case Responses:
		backend, err = responsemodel.New(responsemodel.Config{
			Connection:      connection,
			ReasoningEffort: route.ReasoningEffort, Traits: route.ResponsesTraits,
		})
	case AnthropicMessages:
		backend, err = anthropic.New(anthropic.Config{
			Connection: connection,
			MaxTokens:  route.MaxOutputTokens, PromptCache: route.PromptCache,
			DropMismatchedThinking: route.DropMismatchedThinking,
		})
	default:
		return nil, nil, model.MarkInvalidRequest(fmt.Errorf("unsupported model API %q", route.API))
	}
	if err != nil {
		return nil, nil, fmt.Errorf("initialize model: %w", err)
	}
	return addRouteDiagnostics(backend, route), connection, nil
}
