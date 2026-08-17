// Package chatcompletions adapts the OpenAI-compatible Chat Completions
// protocol to Skot's product-native model items.
package chatcompletions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/levmv/skot/agent"
	productlimits "github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/internal/modelhttp"
)

// Authorizer applies provider-owned authorization to a request. Implementations
// may refresh credentials and must be safe for concurrent use.
type Authorizer interface {
	Authorize(context.Context, *http.Request) error
}

type AuthorizerFunc func(context.Context, *http.Request) error

func (fn AuthorizerFunc) Authorize(ctx context.Context, request *http.Request) error {
	return fn(ctx, request)
}

func BearerToken(token string) Authorizer {
	return AuthorizerFunc(func(_ context.Context, request *http.Request) error {
		request.Header.Set("Authorization", "Bearer "+token)
		return nil
	})
}

type Config struct {
	Provider        string
	Model           string
	APIModel        string
	ReasoningEffort string
	Traits          RouteTraits
	ContextWindow   int
	// ContextWindowEstimated distinguishes a discovered/defaulted value from an
	// explicit or provider-declared limit in durable runtime diagnostics.
	ContextWindowEstimated bool
	BaseURL                string
	HTTPClient             *http.Client
	Authorizer             Authorizer
	Header                 http.Header
}

type Backend struct {
	provider           string
	model              string
	apiModel           string
	reasoningEffort    string
	traits             RouteTraits
	contextWindow      int
	contextEstimated   bool
	baseURL            string
	endpoint           string
	client             *http.Client
	authorizer         Authorizer
	header             http.Header
	maxRequestBytes    int
	maxCompletionBytes int
}

func (backend *Backend) Info() agent.ModelInfo {
	return agent.ModelInfo{
		Backend:                backend.backendID(),
		Provider:               backend.provider,
		Model:                  backend.model,
		ReasoningEffort:        backend.reasoningEffort,
		ProviderStateContract:  backend.traits.ProviderStateContract(),
		ContextWindow:          backend.contextWindow,
		ContextWindowEstimated: backend.contextEstimated,
		MaxRequestBytes:        backend.maxRequestBytes,
		MaxCompletionBytes:     backend.maxCompletionBytes,
		Endpoint:               PublicEndpoint(backend.baseURL),
	}
}

// PublicEndpoint returns the canonical secret-free endpoint reported in model
// diagnostics. Route activation uses the same value when deciding whether
// saved effective metadata still belongs to this adapter endpoint.
func PublicEndpoint(value string) string {
	return modelhttp.PublicEndpoint(value)
}

func (backend *Backend) backendID() string {
	return "chat_completions." + backend.provider
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
	if config.ContextWindow < 0 {
		return nil, agent.MarkInvalidRequest(errors.New("context window cannot be negative"))
	}
	if err := config.Traits.validate(reasoningEffort); err != nil {
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
		provider:           provider,
		model:              model,
		apiModel:           apiModel,
		reasoningEffort:    reasoningEffort,
		traits:             config.Traits,
		contextWindow:      config.ContextWindow,
		contextEstimated:   config.ContextWindowEstimated,
		baseURL:            baseURL,
		endpoint:           baseURL + "/chat/completions",
		client:             client,
		authorizer:         config.Authorizer,
		header:             config.Header.Clone(),
		maxRequestBytes:    productlimits.MaxModelRequestBytes,
		maxCompletionBytes: productlimits.MaxModelCompletionBytes,
	}, nil
}

func (backend *Backend) Complete(ctx context.Context, request agent.ModelRequest, emit func(agent.ModelStreamEvent)) (result agent.ModelResponse, returnErr error) {
	wireRequest, err := backend.buildRequest(request)
	if err != nil {
		return agent.ModelResponse{}, agent.MarkInvalidRequest(err)
	}
	body, err := json.Marshal(wireRequest)
	if err != nil {
		return agent.ModelResponse{}, agent.MarkInvalidRequest(fmt.Errorf("encode chat completion request: %w", err))
	}
	if len(body) > backend.maxRequestBytes {
		return agent.ModelResponse{}, agent.MarkInvalidRequest(fmt.Errorf("chat completion request is %d bytes, limit is %d", len(body), backend.maxRequestBytes))
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, backend.endpoint, bytes.NewReader(body))
	if err != nil {
		return agent.ModelResponse{}, agent.MarkInvalidRequest(fmt.Errorf("create chat completion request: %w", err))
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
		return agent.ModelResponse{}, fmt.Errorf("%s chat completion: %w", backend.provider, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return agent.ModelResponse{}, decodeHTTPError(backend.provider, backend.model, response)
	}

	stream := modelhttp.OpenEventStream(ctx, response.Body, request.StreamIdleTimeout)
	defer stream.Close()
	var text, reasoning strings.Builder
	var calls toolCallAccumulator
	var usage agent.ModelUsage
	var stopReason string
	completionBytes := 0
	limited := false
	for {
		payload, err := stream.Next()
		if errors.Is(err, io.EOF) {
			if stopReason == "" {
				if !stream.SawDone() {
					return agent.ModelResponse{}, fmt.Errorf("%s stream ended before a finish reason", backend.provider)
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
			return agent.ModelResponse{}, fmt.Errorf("read %s stream: %w", backend.provider, err)
		}
		if len(payload) > backend.maxCompletionBytes-completionBytes {
			limited = true
			break
		}
		completionBytes += len(payload)
		var chunk streamChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			return agent.ModelResponse{}, fmt.Errorf("decode %s stream chunk: %w", backend.provider, err)
		}
		if chunk.Error != nil {
			return agent.ModelResponse{}, fmt.Errorf("%s API error: %s", backend.provider, chunk.Error.message())
		}
		if chunk.Usage != nil {
			usage = chunk.Usage.modelUsage()
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			if choice.Delta.ReasoningContent != "" {
				reasoning.WriteString(choice.Delta.ReasoningContent)
				emitModelEvent(emit, agent.EventReasoningSummaryDelta, choice.Delta.ReasoningContent)
			}
			if choice.Delta.Content != "" {
				text.WriteString(choice.Delta.Content)
				emitModelEvent(emit, agent.EventTextDelta, choice.Delta.Content)
			}
			if err := calls.merge(choice.Delta.ToolCalls); err != nil {
				return agent.ModelResponse{}, fmt.Errorf("decode %s tool calls: %w", backend.provider, err)
			}
			if choice.FinishReason != "" && choice.FinishReason != "null" {
				stopReason = choice.FinishReason
			}
		}
	}

	if limited {
		stopReason = agent.StopReasonOutputLimit
	} else {
		normalized, err := backend.normalizeFinishReason(stopReason)
		if err != nil {
			return agent.ModelResponse{}, err
		}
		stopReason = normalized
	}
	items := make([]agent.Item, 0, 2+len(calls.calls))
	if reasoning.Len() != 0 {
		items = append(items, agent.Item{Kind: agent.ItemReasoning, Text: reasoning.String()})
	}
	if text.Len() != 0 {
		items = append(items, agent.Item{Kind: agent.ItemAssistantText, Text: text.String()})
	}
	// Tool calls are not durable unless they are complete enough to execute.
	// If the response was cut locally, keep only renderable partial content.
	if !limited {
		for _, call := range calls.snapshot() {
			if strings.TrimSpace(call.Function.Name) == "" {
				return agent.ModelResponse{}, errors.New("chat completion returned a tool call without a name")
			}
			toolCall := agent.ToolCall{Name: call.Function.Name, RawArguments: call.Function.Arguments}
			if call.ID != "" {
				data, _ := json.Marshal(call.ID)
				toolCall.ProviderReferences = []agent.ProviderReference{{
					Kind: backend.callIDReferenceKind(),
					Data: data,
				}}
			}
			items = append(items, agent.Item{Kind: agent.ItemToolCall, ToolCall: &toolCall})
		}
	}
	// Empty output is valid for an incomplete provider stop, which the runtime
	// reports as an incomplete run rather than a transport failure.
	if len(items) == 0 && !limited && !agent.IsIncompleteStopReason(stopReason) {
		return agent.ModelResponse{}, errors.New("chat completion returned no output items")
	}
	return agent.ModelResponse{Items: items, Usage: usage, StopReason: stopReason}, nil
}

func emitModelEvent(emit func(agent.ModelStreamEvent), kind agent.EventKind, text string) {
	if emit != nil {
		emit(agent.ModelStreamEvent{Kind: kind, Text: text})
	}
}

func (backend *Backend) callIDReferenceKind() string {
	return "chat_completions." + backend.provider + ".call_id"
}

func decodeHTTPError(provider, model string, response *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(response.Body, productlimits.MaxModelCompletionBytes+1))
	if err != nil {
		return fmt.Errorf("%s API returned HTTP %s (read body: %w)", provider, response.Status, err)
	}
	if len(body) > productlimits.MaxModelCompletionBytes {
		body = body[:productlimits.MaxModelCompletionBytes]
	}
	var envelope struct {
		Error *apiError `json:"error"`
	}
	message, code, errorType := "", "", ""
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != nil {
		message = envelope.Error.message()
		code = modelhttp.ErrorCode(envelope.Error.Code)
		errorType = strings.TrimSpace(envelope.Error.Type)
	}
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	return modelhttp.NewProviderError(modelhttp.ProviderErrorDetails{
		Provider: provider, Model: model, Status: response.Status, StatusCode: response.StatusCode,
		Message: message, Code: code, Type: errorType,
		RetryAfter: modelhttp.ParseRetryAfter(response.Header.Get("Retry-After"), time.Now()),
	})
}
