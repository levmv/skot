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
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/levmv/skot/agent"
	productlimits "github.com/levmv/skot/internal/limits"
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
		ContextWindow:          backend.contextWindow,
		ContextWindowEstimated: backend.contextEstimated,
		MaxRequestBytes:        backend.maxRequestBytes,
		MaxCompletionBytes:     backend.maxCompletionBytes,
		Endpoint:               publicEndpoint(backend.baseURL),
	}
}

func publicEndpoint(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
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
	if config.Authorizer == nil {
		return nil, agent.MarkInvalidRequest(errors.New("authorizer is required"))
	}
	client := config.HTTPClient
	if client == nil {
		client = defaultHTTPClient()
	}
	return &Backend{
		provider:           provider,
		model:              model,
		apiModel:           apiModel,
		reasoningEffort:    reasoningEffort,
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

func defaultHTTPClient() *http.Client {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		cloned := transport.Clone()
		cloned.ResponseHeaderTimeout = 5 * time.Minute
		return &http.Client{Transport: cloned}
	}
	return &http.Client{}
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
		return agent.ModelResponse{}, decodeHTTPError(backend.provider, response)
	}

	readCtx, stopReading := context.WithCancel(ctx)
	defer stopReading()
	reader := newSSEReader(response.Body)
	readResults := readSSE(readCtx, reader)
	var idleTimer *time.Timer
	var idle <-chan time.Time
	if request.StreamIdleTimeout > 0 {
		idleTimer = time.NewTimer(request.StreamIdleTimeout)
		idle = idleTimer.C
		defer idleTimer.Stop()
	}
	var text, reasoning strings.Builder
	var calls toolCallAccumulator
	var usage agent.ModelUsage
	var stopReason string
	completionBytes := 0
	limited := false
	for {
		var read sseReadResult
		select {
		case value, ok := <-readResults:
			if !ok {
				if ctx.Err() != nil {
					return agent.ModelResponse{}, ctx.Err()
				}
				return agent.ModelResponse{}, errors.New("provider stream reader stopped without a result")
			}
			read = value
			if idleTimer != nil {
				if !idleTimer.Stop() {
					select {
					case <-idleTimer.C:
					default:
					}
				}
				idleTimer.Reset(request.StreamIdleTimeout)
			}
		case <-idle:
			return agent.ModelResponse{}, fmt.Errorf("%w after %s", agent.ErrModelStreamIdle, request.StreamIdleTimeout)
		case <-ctx.Done():
			return agent.ModelResponse{}, ctx.Err()
		}
		payload, err := read.payload, read.err
		if errors.Is(err, io.EOF) {
			if stopReason == "" && !reader.done {
				return agent.ModelResponse{}, fmt.Errorf("%s stream ended before a finish reason", backend.provider)
			}
			break
		}
		if errors.Is(err, errSSETokenTooLarge) {
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
	if len(items) == 0 && !limited {
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

func decodeHTTPError(provider string, response *http.Response) error {
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
	message := ""
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != nil {
		message = envelope.Error.message()
	}
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	return classifyHTTPError(response.StatusCode, parseRetryAfter(response.Header.Get("Retry-After"), time.Now()), fmt.Errorf("%s API returned HTTP %s: %s", provider, response.Status, message))
}

func classifyHTTPError(status int, retryAfter time.Duration, err error) error {
	retryable := status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
	// Once a syntactically valid request reaches the provider, its HTTP failure
	// belongs to provider/external state (exit 3), matching the fork's unattended
	// contract. Retryable independently controls immediate retries. Locally
	// detected request-construction errors remain ErrInvalidRequest (exit 2).
	err = agent.MarkProviderFailure(err)
	return &agent.ProviderError{Cause: err, StatusCode: status, Retryable: retryable, RetryAfter: retryAfter}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}
