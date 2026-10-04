// Package responses adapts the OpenAI Responses protocol to Skot's
// product-native model items while keeping conversation history stateless.
package responses

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"

	productlimits "github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/internal/modelhttp"
	modelapi "github.com/levmv/skot/model"
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
		return nil, modelapi.MarkInvalidRequest(errors.New("provider is required"))
	}
	if model == "" {
		return nil, modelapi.MarkInvalidRequest(errors.New("model is required"))
	}
	if baseURL == "" {
		return nil, modelapi.MarkInvalidRequest(errors.New("base URL is required"))
	}
	if err := config.Traits.validate(); err != nil {
		return nil, modelapi.MarkInvalidRequest(err)
	}
	if config.Authorizer == nil {
		return nil, modelapi.MarkInvalidRequest(errors.New("authorizer is required"))
	}
	client := modelhttp.ModelClient(config.HTTPClient)
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
func (backend *Backend) ProjectModelItems(items []modelapi.Item) []modelapi.Item {
	return items
}

func (backend *Backend) backendID() string { return BackendID(backend.provider) }

// BackendID returns the stable replay identity used by Responses for a
// provider. Route resolution and the adapter must use this same function.
func BackendID(provider string) string { return "responses." + strings.TrimSpace(provider) }

func (backend *Backend) callReferenceKind() string {
	return "responses." + backend.provider + ".function_call"
}

func (backend *Backend) Complete(ctx context.Context, request modelapi.Request, emit func(modelapi.StreamEvent)) (result modelapi.Response, returnErr error) {
	var usage modelhttp.UsageAccumulator
	defer usage.Attach(&result)
	wireRequest, err := backend.buildRequest(request)
	if err != nil {
		return modelapi.Response{}, modelapi.MarkInvalidRequest(err)
	}
	body, err := modelhttp.MarshalRequestJSON(wireRequest)
	if err != nil {
		return modelapi.Response{}, modelapi.MarkInvalidRequest(fmt.Errorf("encode Responses request: %w", err))
	}
	if len(body) > backend.maxRequestBytes {
		return modelapi.Response{}, modelapi.MarkInvalidRequest(fmt.Errorf("%w: responses request is %d bytes, limit is %d", modelapi.ErrModelRequestTooLarge, len(body), backend.maxRequestBytes))
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, backend.endpoint, bytes.NewReader(body))
	if err != nil {
		return modelapi.Response{}, modelapi.MarkInvalidRequest(fmt.Errorf("create Responses request: %w", err))
	}
	modelhttp.SetRequestHeaders(httpRequest.Header, backend.header, request.SessionID)
	if err := backend.authorizer.Authorize(ctx, httpRequest); err != nil {
		err = fmt.Errorf("authorize %s request: %w", backend.provider, err)
		// Refreshing credentials can fail transiently without invalidating them.
		if errors.Is(err, modelapi.ErrProviderFailure) {
			return modelapi.Response{}, err
		}
		return modelapi.Response{}, modelapi.MarkInvalidRequest(err)
	}
	defer func() { returnErr = modelapi.MarkProviderFailure(returnErr) }()

	response, err := backend.client.Do(httpRequest)
	if err != nil {
		return modelapi.Response{}, fmt.Errorf("%s Responses request: %w", backend.provider, err)
	}
	defer response.Body.Close()
	usage.SetRequestID(response.Header)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return modelapi.Response{}, modelhttp.DecodeProviderError(backend.provider, backend.model, "Responses API", response)
	}

	stream := modelhttp.OpenEventStream(ctx, response.Body, request.StreamIdleTimeout)
	defer stream.Close()
	var text, reasoning strings.Builder
	completedOutput := make(map[int]jsontext.Value)
	completionBytes := 0
	for {
		payload, readErr := stream.Next()
		if errors.Is(readErr, io.EOF) {
			return modelapi.Response{}, fmt.Errorf("%s Responses stream ended before a terminal event", backend.provider)
		}
		if errors.Is(readErr, modelhttp.ErrEventTooLarge) || len(payload) > backend.maxCompletionBytes-completionBytes {
			return partialStreamResponse(text.String(), reasoning.String()), nil
		}
		if readErr != nil {
			return modelapi.Response{}, fmt.Errorf("read %s Responses stream: %w", backend.provider, readErr)
		}
		completionBytes += len(payload)
		var event streamEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			return modelapi.Response{}, fmt.Errorf("decode %s Responses stream event: %w", backend.provider, err)
		}
		if event.Response != nil {
			if event.Response.ID != "" {
				usage.Snapshot.ResponseID = event.Response.ID
			}
			if event.Response.Model != "" {
				usage.Snapshot.Model = event.Response.Model
			}
			if event.Response.Provider != "" {
				usage.Snapshot.Provider = event.Response.Provider
			}
			terminal := event.Type == "response.completed" || event.Type == "response.incomplete" || event.Type == "response.done" || event.Type == "response.failed"
			if err := usage.Observe(event.Response.Usage, "responses", backend.provider, terminal); err != nil {
				return modelapi.Response{}, err
			}
		}
		switch event.Type {
		case "response.output_item.done":
			if event.OutputIndex == nil || *event.OutputIndex < 0 || len(event.Item) == 0 {
				return modelapi.Response{}, fmt.Errorf("%s Responses output item is missing its index or content", backend.provider)
			}
			completedOutput[*event.OutputIndex] = event.Item
		case "response.output_text.delta", "response.refusal.delta":
			text.WriteString(event.Delta)
			emitModelEvent(emit, modelapi.EventTextDelta, event.Delta)
		case "response.reasoning_summary_text.delta":
			reasoning.WriteString(event.Delta)
			emitModelEvent(emit, modelapi.EventReasoningSummaryDelta, event.Delta)
		case "response.completed", "response.incomplete", "response.done":
			if event.Response == nil {
				return modelapi.Response{}, fmt.Errorf("%s Responses terminal event has no response", backend.provider)
			}
			if event.Response.Error != nil {
				return modelapi.Response{}, modelhttp.NewProviderEnvelopeError(backend.provider, backend.model, event.Response.Error)
			}
			eventStatus := strings.TrimPrefix(event.Type, "response.")
			if eventStatus == "done" {
				eventStatus = event.Response.Status
				if eventStatus == "" {
					eventStatus = "completed"
				}
				if eventStatus != "completed" && eventStatus != "incomplete" {
					return modelapi.Response{}, fmt.Errorf("%s Responses terminal event has unsupported status %q", backend.provider, eventStatus)
				}
			}
			if event.Response.Status == "" {
				event.Response.Status = eventStatus
			} else if event.Response.Status != eventStatus {
				return modelapi.Response{}, fmt.Errorf(
					"%s Responses terminal event %q carries status %q",
					backend.provider, event.Type, event.Response.Status,
				)
			}
			// Codex sends completed items individually and can leave output empty
			// in the terminal response. Keep their provider order even when tools
			// finish out of order, and prefer a full terminal snapshot when present.
			if len(event.Response.Output) == 0 {
				for _, index := range slices.Sorted(maps.Keys(completedOutput)) {
					event.Response.Output = append(event.Response.Output, completedOutput[index])
				}
			}
			return backend.parseResponse(*event.Response)
		case "response.failed":
			if event.Response != nil && event.Response.Error != nil {
				return modelapi.Response{}, modelhttp.NewProviderEnvelopeError(backend.provider, backend.model, event.Response.Error)
			}
			return modelapi.Response{}, fmt.Errorf("%s Responses API failed", backend.provider)
		case "error":
			if event.Error != nil {
				return modelapi.Response{}, modelhttp.NewProviderEnvelopeError(backend.provider, backend.model, event.Error)
			}
			return modelapi.Response{}, modelhttp.NewProviderEnvelopeError(backend.provider, backend.model, &apiError{Message: event.Message, Code: event.Code})
		}
	}
}

func partialStreamResponse(text, reasoning string) modelapi.Response {
	items := make([]modelapi.Item, 0, 2)
	if reasoning != "" {
		items = append(items, modelapi.Item{Kind: modelapi.ItemReasoning, Text: reasoning})
	}
	if text != "" {
		items = append(items, modelapi.Item{Kind: modelapi.ItemAssistantText, Text: text})
	}
	return modelapi.Response{Items: items, StopReason: modelapi.StopReasonOutputLimit}
}

func emitModelEvent(emit func(modelapi.StreamEvent), kind modelapi.StreamEventKind, text string) {
	if emit != nil && text != "" {
		emit(modelapi.StreamEvent{Kind: kind, Text: text})
	}
}
