// Package chatcompletions provides an item-based Backend and a raw-response
// Observer for Chat Completions. Both support one choice (n=1).
package chatcompletions

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
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

func (backend *Backend) backendID() string {
	return BackendID(backend.provider)
}

// BackendID returns the stable replay identity used by Chat Completions for a
// provider. Route resolution and the adapter must use this same function.
func BackendID(provider string) string { return "chat_completions." + strings.TrimSpace(provider) }

func New(config Config) (*Backend, error) {
	if config.Connection == nil || config.Connection.API() != "chat_completions" {
		return nil, modelapi.MarkInvalidRequest(errors.New("a chat_completions connection is required"))
	}
	provider := config.Connection.Provider()
	apiModel := config.Connection.APIModel()
	reasoningEffort := strings.ToLower(strings.TrimSpace(config.ReasoningEffort))
	if err := config.Traits.validate(reasoningEffort); err != nil {
		return nil, modelapi.MarkInvalidRequest(err)
	}
	return &Backend{
		connection:         config.Connection,
		provider:           provider,
		apiModel:           apiModel,
		reasoningEffort:    reasoningEffort,
		traits:             config.Traits,
		maxRequestBytes:    productlimits.MaxModelRequestBytes,
		maxCompletionBytes: productlimits.MaxModelCompletionBytes,
	}, nil
}

func (backend *Backend) Complete(ctx context.Context, request modelapi.Request, emit func(modelapi.StreamEvent)) (result modelapi.Response, returnErr error) {
	observer := NewObserver(backend.connection)
	defer func() { result.Usage = observer.Usage() }()
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
		return modelapi.Response{}, modelapi.MarkInvalidRequest(fmt.Errorf("encode chat completion request: %w", err))
	}
	if len(body) > backend.maxRequestBytes {
		return modelapi.Response{}, modelapi.MarkInvalidRequest(fmt.Errorf("%w: chat completion request is %d bytes, limit is %d", modelapi.ErrModelRequestTooLarge, len(body), backend.maxRequestBytes))
	}
	header := http.Header{"Accept": {"text/event-stream"}}
	if request.SessionID != "" {
		header.Set("X-Session-ID", request.SessionID)
	}
	requestStarted = true
	response, err := backend.connection.Post(ctx, body, header)
	if response != nil {
		observer.ObserveHeader(response.Header)
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
	var calls toolCallAccumulator
	var stopReason string
	completionBytes := 0
	limited := false
	for {
		event, err := stream.Next()
		payload := event.Data
		if errors.Is(err, io.EOF) {
			if err := observer.End(stream.SawDone()); err != nil {
				return modelapi.Response{}, err
			}
			if stopReason == "" {
				// Some compatible gateways use the legacy sentinel as their only
				// terminal signal. Keep accepting it, but do not let an empty value
				// escape the adapter's closed completion-reason vocabulary.
				stopReason = "stop"
			}
			break
		}
		if errors.Is(err, transport.ErrEventTooLarge) {
			limited = true
			break
		}
		if err != nil {
			return modelapi.Response{}, fmt.Errorf("read %s stream: %w", backend.provider, err)
		}
		if len(payload) > backend.maxCompletionBytes-completionBytes {
			limited = true
			break
		}
		completionBytes += len(payload)
		var chunk streamChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			return modelapi.Response{}, fmt.Errorf("decode %s stream chunk: %w", backend.provider, err)
		}
		// Reject a failed generation before accepting output or tool calls;
		// later usage or size limits must not hide the error.
		if err := observer.observeChunk(chunk); err != nil {
			return modelapi.Response{}, err
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			if choice.FinishReason != "" && choice.FinishReason != "null" {
				normalized, err := backend.normalizeFinishReason(choice.FinishReason)
				if err != nil {
					if native := strings.TrimSpace(choice.NativeFinishReason); native != "" {
						err = fmt.Errorf("%w (native_finish_reason %q)", err, native)
					}
					return modelapi.Response{}, err
				}
				stopReason = normalized
			}
			if delta := choice.Delta.reasoningText(); delta != "" {
				reasoning.WriteString(delta)
				emitModelEvent(emit, modelapi.EventReasoningSummaryDelta, delta)
			}
			if choice.Delta.Content != "" {
				text.WriteString(choice.Delta.Content)
				emitModelEvent(emit, modelapi.EventTextDelta, choice.Delta.Content)
			}
			if err := calls.merge(choice.Delta.ToolCalls); err != nil {
				return modelapi.Response{}, fmt.Errorf("decode %s tool calls: %w", backend.provider, err)
			}
		}
	}

	if limited {
		stopReason = modelapi.StopReasonOutputLimit
	}
	items := make([]modelapi.Item, 0, 2+len(calls.calls))
	if reasoning.Len() != 0 {
		items = append(items, modelapi.Item{Kind: modelapi.ItemReasoning, Text: reasoning.String()})
	}
	if text.Len() != 0 {
		items = append(items, modelapi.Item{Kind: modelapi.ItemAssistantText, Text: text.String()})
	}
	// Tool arguments can be truncated when a response stops early.
	if !modelapi.IsIncompleteStopReason(stopReason) {
		for _, call := range calls.snapshot() {
			if strings.TrimSpace(call.Function.Name) == "" {
				return modelapi.Response{}, errors.New("chat completion returned a tool call without a name")
			}
			arguments, err := modelapi.NormalizeToolArguments(call.Function.Arguments)
			if err != nil {
				return modelapi.Response{}, fmt.Errorf("chat completion returned invalid arguments for tool %q: %w", call.Function.Name, err)
			}
			toolCall := modelapi.ToolCall{Name: call.Function.Name, RawArguments: arguments}
			if call.ID != "" {
				data, _ := json.Marshal(call.ID, json.Deterministic(true))
				toolCall.ProviderReferences = []modelapi.ProviderReference{{
					Kind: backend.callIDReferenceKind(),
					Data: data,
				}}
			}
			items = append(items, modelapi.Item{Kind: modelapi.ItemToolCall, ToolCall: &toolCall})
		}
	}
	// Empty output is valid for an incomplete provider stop, which the runtime
	// reports as an incomplete run rather than a transport failure.
	if len(items) == 0 && !modelapi.IsIncompleteStopReason(stopReason) {
		return modelapi.Response{}, errors.New("chat completion returned no output items")
	}
	return modelapi.Response{Items: items, StopReason: stopReason}, nil
}

func emitModelEvent(emit func(modelapi.StreamEvent), kind modelapi.StreamEventKind, text string) {
	if emit != nil {
		emit(modelapi.StreamEvent{Kind: kind, Text: text})
	}
}

func (backend *Backend) callIDReferenceKind() string {
	return "chat_completions." + backend.provider + ".call_id"
}
