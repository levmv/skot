// Package responses adapts the OpenAI Responses protocol to Skot's
// product-native model items while keeping conversation history stateless.
package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/levmv/skot/agent"
	productlimits "github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/internal/modelhttp"
)

type Authorizer = modelhttp.Authorizer
type AuthorizerFunc = modelhttp.AuthorizerFunc

func BearerToken(token string) Authorizer {
	return modelhttp.BearerToken(token)
}

type Config struct {
	Provider        string
	Model           string
	APIModel        string
	ReasoningEffort string
	Traits          RouteTraits
	BaseURL         string
	HTTPClient      *http.Client
	Authorizer      Authorizer
	Header          http.Header
}

type Backend struct {
	provider           string
	model              string
	apiModel           string
	reasoningEffort    string
	traits             RouteTraits
	endpoint           string
	client             *http.Client
	authorizer         Authorizer
	header             http.Header
	maxRequestBytes    int
	maxCompletionBytes int
}

func New(config Config) (*Backend, error) {
	provider := strings.TrimSpace(config.Provider)
	model := strings.TrimSpace(config.Model)
	apiModel := strings.TrimSpace(config.APIModel)
	if apiModel == "" {
		apiModel = model
	}
	reasoningEffort := strings.ToLower(strings.TrimSpace(config.ReasoningEffort))
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if provider == "" {
		return nil, agent.MarkInvalidRequest(errors.New("provider is required"))
	}
	if model == "" {
		return nil, agent.MarkInvalidRequest(errors.New("model is required"))
	}
	if baseURL == "" {
		return nil, agent.MarkInvalidRequest(errors.New("base URL is required"))
	}
	if err := config.Traits.validate(); err != nil {
		return nil, agent.MarkInvalidRequest(err)
	}
	if config.Authorizer == nil {
		return nil, agent.MarkInvalidRequest(errors.New("authorizer is required"))
	}
	client := config.HTTPClient
	if client == nil {
		client = modelhttp.DefaultClient()
	}
	return &Backend{
		provider: provider, model: model, apiModel: apiModel,
		reasoningEffort: reasoningEffort, traits: config.Traits,
		endpoint: baseURL + "/responses", client: client,
		authorizer: config.Authorizer, header: config.Header.Clone(),
		maxRequestBytes: productlimits.MaxModelRequestBytes, maxCompletionBytes: productlimits.MaxModelCompletionBytes,
	}, nil
}

// ProjectModelItems keeps all runtime-owned items because Responses replays all
// encrypted reasoning in the current provider epoch.
func (backend *Backend) ProjectModelItems(items []agent.Item) []agent.Item {
	return items
}

func (backend *Backend) backendID() string { return BackendID(backend.provider) }

// BackendID returns the stable replay identity used by Responses for a
// provider. Route resolution and the adapter must use this same function.
func BackendID(provider string) string { return "responses." + strings.TrimSpace(provider) }

func (backend *Backend) callReferenceKind() string {
	return "responses." + backend.provider + ".function_call"
}

func (backend *Backend) Complete(ctx context.Context, request agent.ModelRequest, emit func(agent.ModelStreamEvent)) (result agent.ModelResponse, returnErr error) {
	wireRequest, err := backend.buildRequest(request)
	if err != nil {
		return agent.ModelResponse{}, agent.MarkInvalidRequest(err)
	}
	body, err := modelhttp.MarshalRequestJSON(wireRequest)
	if err != nil {
		return agent.ModelResponse{}, agent.MarkInvalidRequest(fmt.Errorf("encode Responses request: %w", err))
	}
	if len(body) > backend.maxRequestBytes {
		return agent.ModelResponse{}, agent.MarkInvalidRequest(fmt.Errorf("%w: responses request is %d bytes, limit is %d", agent.ErrModelRequestTooLarge, len(body), backend.maxRequestBytes))
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, backend.endpoint, bytes.NewReader(body))
	if err != nil {
		return agent.ModelResponse{}, agent.MarkInvalidRequest(fmt.Errorf("create Responses request: %w", err))
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	for name, values := range backend.header {
		for _, value := range values {
			httpRequest.Header.Add(name, value)
		}
	}
	if err := backend.authorizer.Authorize(ctx, httpRequest); err != nil {
		return agent.ModelResponse{}, agent.MarkInvalidRequest(fmt.Errorf("authorize %s request: %w", backend.provider, err))
	}
	defer func() { returnErr = agent.MarkProviderFailure(returnErr) }()

	response, err := backend.client.Do(httpRequest)
	if err != nil {
		return agent.ModelResponse{}, fmt.Errorf("%s Responses request: %w", backend.provider, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return agent.ModelResponse{}, modelhttp.DecodeProviderError(backend.provider, backend.model, "Responses API", response)
	}

	stream := modelhttp.OpenEventStream(ctx, response.Body, request.StreamIdleTimeout)
	defer stream.Close()
	var text, reasoning strings.Builder
	completionBytes := 0
	for {
		payload, readErr := stream.Next()
		if errors.Is(readErr, io.EOF) {
			return agent.ModelResponse{}, fmt.Errorf("%s Responses stream ended before a terminal event", backend.provider)
		}
		if errors.Is(readErr, modelhttp.ErrEventTooLarge) || len(payload) > backend.maxCompletionBytes-completionBytes {
			return partialStreamResponse(text.String(), reasoning.String()), nil
		}
		if readErr != nil {
			return agent.ModelResponse{}, fmt.Errorf("read %s Responses stream: %w", backend.provider, readErr)
		}
		completionBytes += len(payload)
		var event streamEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			return agent.ModelResponse{}, fmt.Errorf("decode %s Responses stream event: %w", backend.provider, err)
		}
		switch event.Type {
		case "response.output_text.delta", "response.refusal.delta":
			text.WriteString(event.Delta)
			emitModelEvent(emit, agent.EventTextDelta, event.Delta)
		case "response.reasoning_summary_text.delta":
			reasoning.WriteString(event.Delta)
			emitModelEvent(emit, agent.EventReasoningSummaryDelta, event.Delta)
		case "response.completed", "response.incomplete":
			if event.Response == nil {
				return agent.ModelResponse{}, fmt.Errorf("%s Responses terminal event has no response", backend.provider)
			}
			eventStatus := strings.TrimPrefix(event.Type, "response.")
			if event.Response.Status == "" {
				event.Response.Status = eventStatus
			} else if event.Response.Status != eventStatus {
				return agent.ModelResponse{}, fmt.Errorf(
					"%s Responses terminal event %q carries status %q",
					backend.provider, event.Type, event.Response.Status,
				)
			}
			return backend.parseResponse(*event.Response)
		case "response.failed":
			if event.Response != nil && event.Response.Error != nil {
				return agent.ModelResponse{}, modelhttp.NewProviderEnvelopeError(backend.provider, backend.model, event.Response.Error)
			}
			return agent.ModelResponse{}, fmt.Errorf("%s Responses API failed", backend.provider)
		case "error":
			if event.Error != nil {
				return agent.ModelResponse{}, modelhttp.NewProviderEnvelopeError(backend.provider, backend.model, event.Error)
			}
			return agent.ModelResponse{}, modelhttp.NewProviderEnvelopeError(backend.provider, backend.model, &apiError{Message: event.Message, Code: event.Code})
		}
	}
}

func partialStreamResponse(text, reasoning string) agent.ModelResponse {
	items := make([]agent.Item, 0, 2)
	if reasoning != "" {
		items = append(items, agent.Item{Kind: agent.ItemReasoning, Text: reasoning})
	}
	if text != "" {
		items = append(items, agent.Item{Kind: agent.ItemAssistantText, Text: text})
	}
	return agent.ModelResponse{Items: items, StopReason: agent.StopReasonOutputLimit}
}

func emitModelEvent(emit func(agent.ModelStreamEvent), kind agent.EventKind, text string) {
	if emit != nil && text != "" {
		emit(agent.ModelStreamEvent{Kind: kind, Text: text})
	}
}
