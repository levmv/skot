// Package responses adapts the OpenAI Responses protocol to Skot's
// product-native model items while keeping conversation history stateless.
package responses

import (
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
	"github.com/levmv/skot/model/transport"
)

type Config struct {
	Connection      *transport.Connection
	ReasoningEffort string
	Traits          RouteTraits
}

type Backend struct {
	connection         *transport.Connection
	provider           string
	apiModel           string
	reasoningEffort    string
	traits             RouteTraits
	maxRequestBytes    int
	maxCompletionBytes int
}

func New(config Config) (*Backend, error) {
	if config.Connection == nil || config.Connection.API() != "responses" {
		return nil, modelapi.MarkInvalidRequest(errors.New("a responses connection is required"))
	}
	provider := config.Connection.Provider()
	apiModel := config.Connection.APIModel()
	reasoningEffort := strings.ToLower(strings.TrimSpace(config.ReasoningEffort))
	if err := config.Traits.validate(); err != nil {
		return nil, modelapi.MarkInvalidRequest(err)
	}
	return &Backend{
		connection: config.Connection,
		provider:   provider, apiModel: apiModel,
		reasoningEffort: reasoningEffort, traits: config.Traits,
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
	requestStarted := false
	defer func() {
		if requestStarted && !errors.Is(returnErr, modelapi.ErrRequestNotSent) {
			returnErr = modelapi.MarkProviderFailure(returnErr)
		} else {
			returnErr = modelapi.MarkRequestNotSent(returnErr)
		}
	}()
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
	header := http.Header{"Accept": {"text/event-stream"}}
	if request.SessionID != "" {
		header.Set("X-Session-ID", request.SessionID)
	}
	requestStarted = true
	response, err := backend.connection.Post(ctx, body, header)
	if response != nil {
		usage.SetRequestID(response.Header)
	}
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		return modelapi.Response{}, err
	}
	stream := transport.OpenEventStream(ctx, response.Body, transport.StreamOptions{IdleTimeout: request.StreamIdleTimeout})
	defer stream.Close()
	var text, reasoning strings.Builder
	completedOutput := make(map[int]jsontext.Value)
	completionBytes := 0
	for {
		received, readErr := stream.Next()
		payload := received.Data
		if errors.Is(readErr, io.EOF) {
			return modelapi.Response{}, fmt.Errorf("%s Responses stream ended before a terminal event", backend.provider)
		}
		if errors.Is(readErr, transport.ErrEventTooLarge) || len(payload) > backend.maxCompletionBytes-completionBytes {
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
				return modelapi.Response{}, modelhttp.NewProviderEnvelopeError(backend.provider, backend.apiModel, event.Response.Error)
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
				return modelapi.Response{}, modelhttp.NewProviderEnvelopeError(backend.provider, backend.apiModel, event.Response.Error)
			}
			return modelapi.Response{}, fmt.Errorf("%s Responses API failed", backend.provider)
		case "error":
			if event.Error != nil {
				return modelapi.Response{}, modelhttp.NewProviderEnvelopeError(backend.provider, backend.apiModel, event.Error)
			}
			return modelapi.Response{}, modelhttp.NewProviderEnvelopeError(backend.provider, backend.apiModel, &apiError{Message: event.Message, Code: event.Code})
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
