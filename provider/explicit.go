package provider

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/levmv/skot/internal/modelconfig"
	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/chatcompletions"
	"github.com/levmv/skot/model/responses"
	"github.com/levmv/skot/model/transport"
)

// API selects the wire protocol independently of the connection name.
type API string

const (
	ChatCompletions   = "chat_completions"
	Responses         = "responses"
	AnthropicMessages = "anthropic_messages"
	Gemini            = "gemini"
)

// ExplicitConfig describes a fully specified model. Provider,
// Model, API, BaseURL, and a positive ContextWindow are required.
type ExplicitConfig struct {
	// Provider names the connection for diagnostics and replay ownership. It
	// does not select credentials, aliases, or provider-specific behavior.
	Provider string
	// Model is the exact upstream model ID, without Skot alias resolution.
	Model string
	API   API
	// BaseURL includes the API version prefix, but not the generation path.
	// User information, query parameters, and fragments are not supported.
	BaseURL       string
	ContextWindow int
	// ReasoningEffort is checked by the adapter, not the built-in model catalog.
	// Empty (or "default") leaves the effort unspecified.
	ReasoningEffort string
	// Nil ChatTraits selects reasoning_effort and max_tokens, with no reasoning
	// replay or prompt-cache extension. Supply traits for other compatible dialects.
	ChatTraits      *chatcompletions.RouteTraits
	ResponsesTraits *responses.RouteTraits
	// Anthropic is required for Messages and supplies its default output limit.
	Anthropic *AnthropicOptions
	// ResponseProfile enables extensions such as OpenRouter's USD costs and
	// numeric in-band error codes. Empty uses ordinary protocol semantics.
	ResponseProfile model.ResponseProfile
	// ImageInputUnsupported rejects images locally when the caller knows that
	// the route does not accept them. False does not guarantee image support.
	ImageInputUnsupported bool
	// APIKey uses Bearer, x-api-key, or x-goog-api-key according to API.
	// Authorizer replaces that behavior; specifying both is an error.
	// With neither, Skot adds no authorization. It never looks up credentials.
	APIKey     string
	Authorizer transport.Authorizer
	// Nil uses Skot's built-in transport. A supplied client retains its own
	// connection-error classification; see [transport.Config].
	HTTPClient *http.Client
	Header     http.Header
	// RequestEncoding compresses request bodies; empty sends plain JSON. See
	// [transport.RequestEncoding] before enabling it.
	RequestEncoding transport.RequestEncoding
}

// AnthropicOptions controls Messages extensions and its required output limit.
type AnthropicOptions struct {
	// MaxOutputTokens must be positive. Request.MaxOutputTokens can override it.
	MaxOutputTokens int
	// PromptCache adds cache_control breakpoints to cache the request prefix.
	PromptCache bool
	// DropMismatchedThinking asks the API to drop thinking invalidated by context
	// edits. It enables adaptive thinking and requires the thinking-binding beta.
	DropMismatchedThinking bool
}

// New prepares a model entirely from config. It does not access the catalog,
// environment, credential files, or network. Protocol-specific options for a
// different API are rejected. The returned Model does not need to be closed.
func New(config ExplicitConfig) (*Model, error) {
	api, err := modelconfig.ParseAPI(string(config.API))
	if err != nil {
		return nil, model.MarkInvalidRequest(err)
	}
	if api == "" {
		return nil, model.MarkInvalidRequest(errors.New("model API is required"))
	}
	if config.ContextWindow <= 0 {
		return nil, model.MarkInvalidRequest(errors.New("model context window must be positive"))
	}
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	endpoint, err := url.Parse(baseURL)
	if err != nil || endpoint.Hostname() == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || strings.Contains(baseURL, "#") {
		return nil, model.MarkInvalidRequest(errors.New("base URL must be an absolute HTTP or HTTPS URL without credentials, query, or fragment"))
	}
	if (config.ChatTraits != nil && api != ChatCompletions) ||
		(config.ResponsesTraits != nil && api != Responses) ||
		(config.Anthropic != nil && api != AnthropicMessages) {
		return nil, model.MarkInvalidRequest(errors.New("protocol options do not match the selected model API"))
	}
	effort := strings.ToLower(strings.TrimSpace(config.ReasoningEffort))
	if effort == "default" {
		effort = ""
	}
	route := modelconfig.Route{
		Provider: strings.TrimSpace(config.Provider), Model: strings.TrimSpace(config.Model),
		API: api, BaseURL: baseURL, Header: config.Header,
		ContextWindow: config.ContextWindow, ReasoningEffort: effort,
		ResponseProfile: config.ResponseProfile, ImageInputUnsupported: config.ImageInputUnsupported,
		RequestEncoding: config.RequestEncoding,
	}
	if api == ChatCompletions {
		route.ChatTraits.ReasoningEffort = chatcompletions.ReasoningEffortTopLevel
	}
	if config.ChatTraits != nil {
		route.ChatTraits = *config.ChatTraits
	}
	if config.ResponsesTraits != nil {
		route.ResponsesTraits = *config.ResponsesTraits
	}
	if api == AnthropicMessages {
		if effort != "" {
			return nil, model.MarkInvalidRequest(errors.New("reasoning effort is not supported by the Anthropic Messages adapter"))
		}
		if config.Anthropic == nil || config.Anthropic.MaxOutputTokens <= 0 {
			return nil, model.MarkInvalidRequest(errors.New("a positive default output token limit is required for Anthropic Messages"))
		}
		route.MaxOutputTokens = config.Anthropic.MaxOutputTokens
		route.PromptCache = config.Anthropic.PromptCache
		route.DropMismatchedThinking = config.Anthropic.DropMismatchedThinking
	}
	route.ProviderStateContract = route.StateContract()
	authorizer := config.Authorizer
	key := strings.TrimSpace(config.APIKey)
	if key != "" {
		if authorizer != nil {
			return nil, model.MarkInvalidRequest(errors.New("supply either an API key or an authorizer, not both"))
		}
		switch api {
		case AnthropicMessages:
			authorizer = transport.HeaderToken("x-api-key", key)
		case Gemini:
			authorizer = transport.HeaderToken("x-goog-api-key", key)
		default:
			authorizer = transport.BearerToken(key)
		}
	}
	backend, connection, err := modelconfig.BuildConfiguredBackend(route, config.HTTPClient, authorizer)
	if err != nil {
		return nil, err
	}
	info, err := modelconfig.Info(route)
	if err != nil {
		return nil, model.MarkInvalidRequest(err)
	}
	return &Model{Connection: connection, Backend: backend, Info: info}, nil
}
