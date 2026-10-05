// Package chatcompletions adapts the OpenAI-compatible Chat Completions
// protocol to Skot's product-native model items.
package chatcompletions

import (
	"bytes"
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

func (backend *Backend) backendID() string {
	return BackendID(backend.provider)
}

// BackendID returns the stable replay identity used by Chat Completions for a
// provider. Route resolution and the adapter must use this same function.
func BackendID(provider string) string { return "chat_completions." + strings.TrimSpace(provider) }

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
	if err := config.Traits.validate(reasoningEffort); err != nil {
		return nil, modelapi.MarkInvalidRequest(err)
	}
	if config.Authorizer == nil {
		return nil, modelapi.MarkInvalidRequest(errors.New("authorizer is required"))
	}
	client := modelhttp.ModelClient(config.HTTPClient)
	return &Backend{
		provider:           provider,
		model:              model,
		apiModel:           apiModel,
		reasoningEffort:    reasoningEffort,
		traits:             config.Traits,
		endpoint:           baseURL + "/chat/completions",
		client:             client,
		authorizer:         config.Authorizer,
		header:             config.Header.Clone(),
		maxRequestBytes:    productlimits.MaxModelRequestBytes,
		maxCompletionBytes: productlimits.MaxModelCompletionBytes,
	}, nil
}

func (backend *Backend) Complete(ctx context.Context, request modelapi.Request, emit func(modelapi.StreamEvent)) (result modelapi.Response, returnErr error) {
	var usage modelhttp.UsageAccumulator
	defer usage.Attach(&result)
	requestStarted := false
	defer func() {
		if requestStarted {
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
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, backend.endpoint, bytes.NewReader(body))
	if err != nil {
		return modelapi.Response{}, modelapi.MarkInvalidRequest(fmt.Errorf("create chat completion request: %w", err))
	}
	modelhttp.SetRequestHeaders(httpRequest.Header, backend.header, request.SessionID)
	if err := backend.authorizer.Authorize(ctx, httpRequest); err != nil {
		return modelapi.Response{}, modelapi.MarkInvalidRequest(fmt.Errorf("authorize %s request: %w", backend.provider, err))
	}
	if err := ctx.Err(); err != nil {
		return modelapi.Response{}, err
	}

	requestStarted = true
	response, err := backend.client.Do(httpRequest)
	if err != nil {
		return modelapi.Response{}, fmt.Errorf("%s chat completion: %w", backend.provider, err)
	}
	defer response.Body.Close()
	usage.SetRequestID(response.Header)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return modelapi.Response{}, modelhttp.DecodeProviderError(backend.provider, backend.model, "API", response)
	}

	stream := modelhttp.OpenEventStream(ctx, response.Body, request.StreamIdleTimeout)
	defer stream.Close()
	var text, reasoning strings.Builder
	var calls toolCallAccumulator
	var stopReason string
	completionBytes := 0
	limited := false
	for {
		payload, err := stream.Next()
		if errors.Is(err, io.EOF) {
			if stopReason == "" {
				if !stream.SawDone() {
					return modelapi.Response{}, fmt.Errorf("%s stream ended before a finish reason", backend.provider)
				}
				// Some compatible gateways use the legacy sentinel as their only
				// terminal signal. Keep accepting it, but do not let an empty value
				// escape the adapter's closed completion-reason vocabulary.
				stopReason = "stop"
			}
			break
		}
		if errors.Is(err, modelhttp.ErrEventTooLarge) {
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
		if chunk.ID != "" {
			usage.Snapshot.ResponseID = chunk.ID
		}
		if chunk.Model != "" {
			usage.Snapshot.Model = chunk.Model
		}
		if chunk.Provider != "" {
			usage.Snapshot.Provider = chunk.Provider
		}
		finalUsage := len(chunk.Choices) == 0
		for _, choice := range chunk.Choices {
			if choice.Index == 0 && choice.FinishReason != "" && choice.FinishReason != "null" {
				finalUsage = true
			}
		}
		if err := usage.Observe(chunk.Usage, "chat_completions", backend.provider, finalUsage); err != nil {
			return modelapi.Response{}, err
		}
		if chunk.Error != nil {
			return modelapi.Response{}, modelhttp.NewProviderEnvelopeError(backend.provider, backend.model, chunk.Error)
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			if choice.Error != nil {
				return modelapi.Response{}, modelhttp.NewProviderEnvelopeError(backend.provider, backend.model, choice.Error)
			}
			// Reject a failed generation before accepting its output or tool
			// calls; a later usage chunk or size limit must not hide the error.
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
			if choice.Delta.ReasoningContent != "" {
				reasoning.WriteString(choice.Delta.ReasoningContent)
				emitModelEvent(emit, modelapi.EventReasoningSummaryDelta, choice.Delta.ReasoningContent)
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
