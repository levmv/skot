// Package anthropic adapts the Anthropic Messages protocol to Skot's
// product-native model items.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/levmv/skot/agent"
	productlimits "github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/internal/modelhttp"
)

const (
	defaultMaxTokens = 64 * 1024
	anthropicVersion = "2023-06-01"
	thinkingDataKind = "anthropic_messages.thinking_block.v1"
)

// ProviderStateContract identifies signature-bearing thinking blocks that must
// be replayed verbatim during a tool turn.
const ProviderStateContract agent.ProviderStateContract = "anthropic_messages.thinking_replay.v1"

// Authorizer applies provider-owned authorization to a request. Implementations
// may refresh credentials and must be safe for concurrent use.
type Authorizer interface {
	Authorize(context.Context, *http.Request) error
}

type AuthorizerFunc func(context.Context, *http.Request) error

func (fn AuthorizerFunc) Authorize(ctx context.Context, request *http.Request) error {
	return fn(ctx, request)
}

// APIKey returns the native Anthropic Messages authorizer.
func APIKey(token string) Authorizer {
	return AuthorizerFunc(func(_ context.Context, request *http.Request) error {
		request.Header.Set("x-api-key", token)
		return nil
	})
}

type Config struct {
	Provider string
	Model    string
	APIModel string
	// MaxTokens is required by the Messages protocol. Zero selects a
	// conservative compatibility default for explicitly configured routes.
	MaxTokens              int
	ContextWindow          int
	ContextWindowEstimated bool
	// PromptCache marks endpoints that honor cache_control breakpoints. It stays
	// off for compatible endpoints that place their own breakpoints, because the
	// protocol allows only a few per request.
	PromptCache bool
	BaseURL     string
	HTTPClient  *http.Client
	Authorizer  Authorizer
	Header      http.Header
}

type Backend struct {
	provider           string
	model              string
	apiModel           string
	maxTokens          int
	contextWindow      int
	contextEstimated   bool
	promptCache        bool
	baseURL            string
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
	if config.MaxTokens < 0 {
		return nil, agent.MarkInvalidRequest(errors.New("max tokens cannot be negative"))
	}
	if config.Authorizer == nil {
		return nil, agent.MarkInvalidRequest(errors.New("authorizer is required"))
	}
	maxTokens := config.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	client := config.HTTPClient
	if client == nil {
		client = modelhttp.DefaultClient()
	}
	return &Backend{
		provider: provider, model: model, apiModel: apiModel, maxTokens: maxTokens,
		contextWindow: config.ContextWindow, contextEstimated: config.ContextWindowEstimated,
		promptCache: config.PromptCache,
		baseURL:     baseURL, endpoint: baseURL + "/messages", client: client,
		authorizer: config.Authorizer, header: config.Header.Clone(),
		maxRequestBytes: productlimits.MaxModelRequestBytes, maxCompletionBytes: productlimits.MaxModelCompletionBytes,
	}, nil
}

// ProjectModelItems keeps all runtime-owned items because signed thinking stays
// replayable for the whole provider epoch.
func (backend *Backend) ProjectModelItems(items []agent.Item) []agent.Item {
	return items
}

func (backend *Backend) Info() agent.ModelInfo {
	return agent.ModelInfo{
		Backend: backend.backendID(), Provider: backend.provider, Model: backend.model,
		ProviderStateContract: ProviderStateContract,
		ContextWindow:         backend.contextWindow, ContextWindowEstimated: backend.contextEstimated,
		MaxRequestBytes: backend.maxRequestBytes, MaxCompletionBytes: backend.maxCompletionBytes,
		Endpoint: PublicEndpoint(backend.baseURL),
	}
}

// PublicEndpoint returns the canonical secret-free endpoint reported in model
// diagnostics.
func PublicEndpoint(value string) string {
	return modelhttp.PublicEndpoint(value)
}

func (backend *Backend) backendID() string {
	return "anthropic_messages." + backend.provider
}

func (backend *Backend) Complete(ctx context.Context, request agent.ModelRequest, emit func(agent.ModelStreamEvent)) (result agent.ModelResponse, returnErr error) {
	wireRequest, err := backend.buildRequest(request)
	if err != nil {
		return agent.ModelResponse{}, agent.MarkInvalidRequest(err)
	}
	body, err := json.Marshal(wireRequest)
	if err != nil {
		return agent.ModelResponse{}, agent.MarkInvalidRequest(fmt.Errorf("encode Anthropic Messages request: %w", err))
	}
	if len(body) > backend.maxRequestBytes {
		return agent.ModelResponse{}, agent.MarkInvalidRequest(fmt.Errorf("messages request is %d bytes, limit is %d", len(body), backend.maxRequestBytes))
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, backend.endpoint, bytes.NewReader(body))
	if err != nil {
		return agent.ModelResponse{}, agent.MarkInvalidRequest(fmt.Errorf("create Anthropic Messages request: %w", err))
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	httpRequest.Header.Set("anthropic-version", anthropicVersion)
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
		return agent.ModelResponse{}, fmt.Errorf("%s Anthropic Messages request: %w", backend.provider, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return agent.ModelResponse{}, decodeHTTPError(backend.provider, backend.model, response)
	}

	stream := modelhttp.OpenEventStream(ctx, response.Body, request.StreamIdleTimeout)
	defer stream.Close()
	blocks := make(map[int]*streamBlock)
	var usage usageAccumulator
	var stopReason string
	completionBytes := 0
	limited := false
	terminal := false
	for !terminal {
		payload, readErr := stream.Next()
		if errors.Is(readErr, io.EOF) {
			return agent.ModelResponse{}, fmt.Errorf("%s Anthropic Messages stream ended before message_stop", backend.provider)
		}
		if errors.Is(readErr, modelhttp.ErrEventTooLarge) {
			limited = true
			break
		}
		if readErr != nil {
			return agent.ModelResponse{}, fmt.Errorf("read %s Anthropic Messages stream: %w", backend.provider, readErr)
		}
		if len(payload) > backend.maxCompletionBytes-completionBytes {
			limited = true
			break
		}
		completionBytes += len(payload)
		var event streamEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			return agent.ModelResponse{}, fmt.Errorf("decode %s Anthropic Messages stream event: %w", backend.provider, err)
		}
		switch event.Type {
		case "message_start":
			if event.Message != nil {
				usage.merge(event.Message.Usage)
				if event.Message.StopReason != nil {
					stopReason = *event.Message.StopReason
				}
			}
		case "content_block_start":
			if event.Index < 0 {
				return agent.ModelResponse{}, fmt.Errorf("%s content block index %d is negative", backend.provider, event.Index)
			}
			if _, exists := blocks[event.Index]; exists {
				return agent.ModelResponse{}, fmt.Errorf("%s repeated content block index %d", backend.provider, event.Index)
			}
			block := &streamBlock{kind: event.ContentBlock.Type, id: event.ContentBlock.ID, name: event.ContentBlock.Name}
			block.text.WriteString(event.ContentBlock.Text)
			block.reasoning.WriteString(event.ContentBlock.Thinking)
			block.signature.WriteString(event.ContentBlock.Signature)
			block.data = append(json.RawMessage(nil), event.ContentBlock.Data...)
			if len(event.ContentBlock.Input) != 0 && string(event.ContentBlock.Input) != "{}" {
				block.arguments.Write(event.ContentBlock.Input)
			}
			blocks[event.Index] = block
		case "content_block_delta":
			block := blocks[event.Index]
			if block == nil {
				return agent.ModelResponse{}, fmt.Errorf("%s content delta references unopened block %d", backend.provider, event.Index)
			}
			switch event.Delta.Type {
			case "text_delta":
				if block.kind == "text" {
					block.text.WriteString(event.Delta.Text)
					emitModelEvent(emit, agent.EventTextDelta, event.Delta.Text)
				}
			case "thinking_delta":
				if block.kind == "thinking" {
					block.reasoning.WriteString(event.Delta.Thinking)
					emitModelEvent(emit, agent.EventReasoningSummaryDelta, event.Delta.Thinking)
				}
			case "signature_delta":
				if block.kind == "thinking" {
					block.signature.WriteString(event.Delta.Signature)
				}
			case "input_json_delta":
				if block.kind == "tool_use" {
					block.arguments.WriteString(event.Delta.PartialJSON)
				}
			}
		case "content_block_stop":
			if blocks[event.Index] == nil {
				return agent.ModelResponse{}, fmt.Errorf("%s content stop references unopened block %d", backend.provider, event.Index)
			}
			blocks[event.Index].closed = true
		case "message_delta":
			if event.Delta.StopReason != "" {
				stopReason = event.Delta.StopReason
			}
			usage.merge(event.Usage)
		case "message_stop":
			terminal = true
		case "error":
			message := "unknown error"
			if event.Error != nil && strings.TrimSpace(event.Error.Message) != "" {
				message = strings.TrimSpace(event.Error.Message)
			}
			return agent.ModelResponse{}, fmt.Errorf("%s Anthropic Messages stream error: %s", backend.provider, message)
		case "ping":
		default:
			// Anthropic's versioning contract permits adding new stream events.
			// Unknown events are ignored until their content affects Skot items.
		}
	}

	if limited {
		stopReason = agent.StopReasonOutputLimit
	} else if strings.TrimSpace(stopReason) == "" {
		return agent.ModelResponse{}, fmt.Errorf("%s Anthropic Messages stream ended without a stop reason", backend.provider)
	} else {
		normalized, err := backend.normalizeStopReason(stopReason)
		if err != nil {
			return agent.ModelResponse{}, err
		}
		stopReason = normalized
	}
	items, err := backend.responseItems(blocks, limited)
	if err != nil {
		return agent.ModelResponse{}, err
	}
	if len(items) == 0 && !limited && !agent.IsIncompleteStopReason(stopReason) {
		return agent.ModelResponse{}, errors.New("messages response returned no output items")
	}
	return agent.ModelResponse{Items: items, Usage: usage.modelUsage(), StopReason: stopReason}, nil
}

// stopReasons is the closed set of Anthropic Messages stop reasons Skot
// interprets. A finished turn collapses to one value, while a truncated or
// blocked turn keeps the provider word the user is shown.
var stopReasons = map[string]string{
	"end_turn":                      "stop",
	"stop_sequence":                 "stop",
	"tool_use":                      "tool_calls",
	"max_tokens":                    "max_tokens",
	"model_context_window_exceeded": "model_context_window_exceeded",
	"pause_turn":                    "pause_turn",
	"refusal":                       "refusal",
}

func (backend *Backend) normalizeStopReason(reason string) (string, error) {
	normalized, known := stopReasons[strings.ToLower(strings.TrimSpace(reason))]
	if !known {
		return "", modelhttp.UnsupportedCompletionReasonError(backend.provider, reason)
	}
	return normalized, nil
}

func (backend *Backend) responseItems(blocks map[int]*streamBlock, limited bool) ([]agent.Item, error) {
	items := make([]agent.Item, 0, len(blocks))
	indices := make([]int, 0, len(blocks))
	for index := range blocks {
		indices = append(indices, index)
	}
	slices.Sort(indices)
	for _, index := range indices {
		block := blocks[index]
		if !limited && (block.kind == "text" || block.kind == "thinking" || block.kind == "redacted_thinking" || block.kind == "tool_use") && !block.closed {
			return nil, fmt.Errorf("%s returned an incomplete %s block", backend.provider, block.kind)
		}
		switch block.kind {
		case "text":
			if block.text.Len() != 0 {
				items = append(items, agent.Item{Kind: agent.ItemAssistantText, Text: block.text.String()})
			}
		case "thinking":
			if block.reasoning.Len() != 0 || block.signature.Len() != 0 {
				item := agent.Item{Kind: agent.ItemReasoning, Text: block.reasoning.String()}
				if !limited && block.signature.Len() != 0 {
					state, err := json.Marshal(thinkingBlockState{
						Type: "thinking", Thinking: block.reasoning.String(), Signature: block.signature.String(),
					})
					if err != nil {
						return nil, fmt.Errorf("encode %s thinking state: %w", backend.provider, err)
					}
					item.ProviderData = []agent.ProviderData{{Kind: thinkingDataKind, Data: state}}
				}
				items = append(items, item)
			}
		case "redacted_thinking":
			// Ignore malformed incoming state for compatibility; saved state is validated before replay.
			if !limited && validRedactedThinkingData(block.data) {
				state, err := json.Marshal(thinkingBlockState{Type: "redacted_thinking", Data: block.data})
				if err != nil {
					return nil, fmt.Errorf("encode %s redacted thinking state: %w", backend.provider, err)
				}
				items = append(items, agent.Item{
					Kind:         agent.ItemReasoning,
					ProviderData: []agent.ProviderData{{Kind: thinkingDataKind, Data: state}},
				})
			}
		case "tool_use":
			if limited {
				continue
			}
			if strings.TrimSpace(block.id) == "" || strings.TrimSpace(block.name) == "" {
				return nil, fmt.Errorf("%s returned an incomplete tool_use block", backend.provider)
			}
			arguments := block.arguments.String()
			if arguments == "" {
				arguments = "{}"
			}
			if err := validateObjectJSON(arguments); err != nil {
				return nil, fmt.Errorf("%s returned invalid arguments for tool %q: %w", backend.provider, block.name, err)
			}
			providerID, _ := json.Marshal(block.id)
			items = append(items, agent.Item{Kind: agent.ItemToolCall, ToolCall: &agent.ToolCall{
				Name: block.name, RawArguments: arguments,
				ProviderReferences: []agent.ProviderReference{{Kind: backend.callIDReferenceKind(), Data: providerID}},
			}})
		}
	}
	return items, nil
}

func emitModelEvent(emit func(agent.ModelStreamEvent), kind agent.EventKind, value string) {
	if emit != nil && value != "" {
		emit(agent.ModelStreamEvent{Kind: kind, Text: value})
	}
}

func (backend *Backend) callIDReferenceKind() string {
	return backend.backendID() + ".tool_use_id"
}

func decodeHTTPError(provider, model string, response *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(response.Body, productlimits.MaxModelCompletionBytes+1))
	if err != nil {
		return fmt.Errorf("%s Anthropic Messages API returned HTTP %s (read body: %w)", provider, response.Status, err)
	}
	if len(body) > productlimits.MaxModelCompletionBytes {
		body = body[:productlimits.MaxModelCompletionBytes]
	}
	var envelope struct {
		Error *apiError `json:"error"`
	}
	message, errorType := "", ""
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != nil {
		message = strings.TrimSpace(envelope.Error.Message)
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
		Message: message, Type: errorType, RetryAfter: parseRetryAfter(response.Header.Get("Retry-After"), time.Now()),
	})
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
