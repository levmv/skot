// Package anthropic adapts the Anthropic Messages protocol to Skot's
// product-native model items.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	productlimits "github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/internal/modelhttp"
	modelapi "github.com/levmv/skot/model"
)

const (
	defaultMaxTokens    = 64 * 1024
	anthropicVersion    = "2023-06-01"
	thinkingDataKind    = "anthropic_messages.thinking_block.v1"
	thinkingBindingBeta = "thinking-binding-controls-2026-08-01"
)

// ProviderStateContract identifies signature-bearing thinking blocks that must
// be replayed verbatim during a tool turn.
const ProviderStateContract modelapi.ProviderStateContract = "anthropic_messages.thinking_replay.v1"

type apiError = modelhttp.ProviderErrorEnvelope

type Authorizer = modelhttp.Authorizer
type AuthorizerFunc = modelhttp.AuthorizerFunc

// APIKey returns the native Anthropic Messages authorizer.
func APIKey(token string) Authorizer {
	return modelhttp.HeaderToken("x-api-key", token)
}

type Config struct {
	Provider string
	Model    string
	APIModel string
	// MaxTokens supplies the Messages output limit when Request.MaxOutputTokens
	// is zero. Zero here selects a conservative compatibility default.
	MaxTokens int
	// PromptCache marks endpoints that honor cache_control breakpoints. It stays
	// off for compatible endpoints that place their own breakpoints, because the
	// protocol allows only a few per request.
	PromptCache bool
	// DropMismatchedThinking asks the API to drop thinking invalidated by context
	// edits. It enables adaptive thinking and requires support for the binding beta.
	DropMismatchedThinking bool
	BaseURL                string
	HTTPClient             *http.Client
	Authorizer             Authorizer
	Header                 http.Header
}

type Backend struct {
	provider               string
	model                  string
	apiModel               string
	maxTokens              int
	promptCache            bool
	dropMismatchedThinking bool
	endpoint               string
	client                 *http.Client
	authorizer             Authorizer
	header                 http.Header
	maxRequestBytes        int
	maxCompletionBytes     int
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
		return nil, modelapi.MarkInvalidRequest(errors.New("provider is required"))
	}
	if model == "" {
		return nil, modelapi.MarkInvalidRequest(errors.New("model is required"))
	}
	if baseURL == "" {
		return nil, modelapi.MarkInvalidRequest(errors.New("base URL is required"))
	}
	if config.MaxTokens < 0 {
		return nil, modelapi.MarkInvalidRequest(errors.New("max tokens cannot be negative"))
	}
	if config.Authorizer == nil {
		return nil, modelapi.MarkInvalidRequest(errors.New("authorizer is required"))
	}
	maxTokens := config.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	client := modelhttp.ModelClient(config.HTTPClient)
	return &Backend{
		provider: provider, model: model, apiModel: apiModel, maxTokens: maxTokens,
		promptCache:            config.PromptCache,
		dropMismatchedThinking: config.DropMismatchedThinking,
		endpoint:               baseURL + "/messages", client: client,
		authorizer: config.Authorizer, header: config.Header.Clone(),
		maxRequestBytes: productlimits.MaxModelRequestBytes, maxCompletionBytes: productlimits.MaxModelCompletionBytes,
	}, nil
}

// ProjectModelItems retains thinking blocks for replay with their signatures.
func (backend *Backend) ProjectModelItems(items []modelapi.Item) []modelapi.Item {
	return items
}

func (backend *Backend) backendID() string {
	return BackendID(backend.provider)
}

// BackendID returns the stable replay identity used by Anthropic Messages for
// a provider. Route resolution and the adapter must use this same function.
func BackendID(provider string) string { return "anthropic_messages." + strings.TrimSpace(provider) }

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
		return modelapi.Response{}, modelapi.MarkInvalidRequest(fmt.Errorf("encode Anthropic Messages request: %w", err))
	}
	if len(body) > backend.maxRequestBytes {
		return modelapi.Response{}, modelapi.MarkInvalidRequest(fmt.Errorf("%w: messages request is %d bytes, limit is %d", modelapi.ErrModelRequestTooLarge, len(body), backend.maxRequestBytes))
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, backend.endpoint, bytes.NewReader(body))
	if err != nil {
		return modelapi.Response{}, modelapi.MarkInvalidRequest(fmt.Errorf("create Anthropic Messages request: %w", err))
	}
	httpRequest.Header.Set("anthropic-version", anthropicVersion)
	modelhttp.SetRequestHeaders(httpRequest.Header, backend.header, request.SessionID)
	if backend.dropMismatchedThinking {
		betas := append(httpRequest.Header.Values("anthropic-beta"), thinkingBindingBeta)
		httpRequest.Header.Set("anthropic-beta", strings.Join(betas, ","))
	}
	if err := backend.authorizer.Authorize(ctx, httpRequest); err != nil {
		return modelapi.Response{}, modelapi.MarkInvalidRequest(fmt.Errorf("authorize %s request: %w", backend.provider, err))
	}
	if err := ctx.Err(); err != nil {
		return modelapi.Response{}, err
	}

	requestStarted = true
	response, err := backend.client.Do(httpRequest)
	if err != nil {
		return modelapi.Response{}, fmt.Errorf("%s Anthropic Messages request: %w", backend.provider, err)
	}
	defer response.Body.Close()
	usage.SetRequestID(response.Header)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return modelapi.Response{}, modelhttp.DecodeProviderError(backend.provider, backend.model, "Anthropic Messages API", response)
	}

	stream := modelhttp.OpenEventStream(ctx, response.Body, request.StreamIdleTimeout)
	defer stream.Close()
	blocks := make(map[int]*streamBlock)
	var stopReason string
	completionBytes := 0
	limited := false
	terminal := false
	for !terminal {
		payload, readErr := stream.Next()
		if errors.Is(readErr, io.EOF) {
			return modelapi.Response{}, fmt.Errorf("%s Anthropic Messages stream ended before message_stop", backend.provider)
		}
		if errors.Is(readErr, modelhttp.ErrEventTooLarge) {
			limited = true
			break
		}
		if readErr != nil {
			return modelapi.Response{}, fmt.Errorf("read %s Anthropic Messages stream: %w", backend.provider, readErr)
		}
		if len(payload) > backend.maxCompletionBytes-completionBytes {
			limited = true
			break
		}
		completionBytes += len(payload)
		var event streamEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			return modelapi.Response{}, fmt.Errorf("decode %s Anthropic Messages stream event: %w", backend.provider, err)
		}
		finalUsage := event.Type == "message_delta" && event.Delta.StopReason != ""
		if err := usage.Observe(event.Usage, "anthropic_messages", backend.provider, finalUsage); err != nil {
			return modelapi.Response{}, err
		}
		switch event.Type {
		case "message_start":
			if event.Message != nil {
				if event.Message.ID != "" {
					usage.Snapshot.ResponseID = event.Message.ID
				}
				if event.Message.Model != "" {
					usage.Snapshot.Model = event.Message.Model
				}
				if err := usage.Observe(event.Message.Usage, "anthropic_messages", backend.provider, false); err != nil {
					return modelapi.Response{}, err
				}
				if event.Message.StopReason != nil {
					stopReason = *event.Message.StopReason
				}
			}
		case "content_block_start":
			if event.Index < 0 {
				return modelapi.Response{}, fmt.Errorf("%s content block index %d is negative", backend.provider, event.Index)
			}
			if _, exists := blocks[event.Index]; exists {
				return modelapi.Response{}, fmt.Errorf("%s repeated content block index %d", backend.provider, event.Index)
			}
			block := &streamBlock{kind: event.ContentBlock.Type, id: event.ContentBlock.ID, name: event.ContentBlock.Name}
			block.text.WriteString(event.ContentBlock.Text)
			block.reasoning.WriteString(event.ContentBlock.Thinking)
			block.signature.WriteString(event.ContentBlock.Signature)
			block.data = event.ContentBlock.Data.Clone()
			if len(event.ContentBlock.Input) != 0 && string(event.ContentBlock.Input) != "{}" {
				block.arguments.Write(event.ContentBlock.Input)
			}
			blocks[event.Index] = block
		case "content_block_delta":
			block := blocks[event.Index]
			if block == nil {
				return modelapi.Response{}, fmt.Errorf("%s content delta references unopened block %d", backend.provider, event.Index)
			}
			switch event.Delta.Type {
			case "text_delta":
				if block.kind == "text" {
					block.text.WriteString(event.Delta.Text)
					emitModelEvent(emit, modelapi.EventTextDelta, event.Delta.Text)
				}
			case "thinking_delta":
				if block.kind == "thinking" {
					block.reasoning.WriteString(event.Delta.Thinking)
					emitModelEvent(emit, modelapi.EventReasoningSummaryDelta, event.Delta.Thinking)
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
				return modelapi.Response{}, fmt.Errorf("%s content stop references unopened block %d", backend.provider, event.Index)
			}
			blocks[event.Index].closed = true
		case "message_delta":
			if event.Delta.StopReason != "" {
				stopReason = event.Delta.StopReason
			}
		case "message_stop":
			terminal = true
		case "error":
			return modelapi.Response{}, modelhttp.NewProviderEnvelopeError(backend.provider, backend.model, event.Error)
		case "ping":
		default:
			// Anthropic's versioning contract permits adding new stream events.
			// Unknown events are ignored until their content affects Skot items.
		}
	}

	if limited {
		stopReason = modelapi.StopReasonOutputLimit
	} else if strings.TrimSpace(stopReason) == "" {
		return modelapi.Response{}, fmt.Errorf("%s Anthropic Messages stream ended without a stop reason", backend.provider)
	} else {
		normalized, err := backend.normalizeStopReason(stopReason)
		if err != nil {
			return modelapi.Response{}, err
		}
		stopReason = normalized
	}
	items, err := backend.responseItems(blocks, stopReason)
	if err != nil {
		return modelapi.Response{}, err
	}
	if len(items) == 0 && !modelapi.IsIncompleteStopReason(stopReason) {
		return modelapi.Response{}, errors.New("messages response returned no output items")
	}
	return modelapi.Response{Items: items, StopReason: stopReason}, nil
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

func (backend *Backend) responseItems(blocks map[int]*streamBlock, stopReason string) ([]modelapi.Item, error) {
	locallyLimited := stopReason == modelapi.StopReasonOutputLimit
	items := make([]modelapi.Item, 0, len(blocks))
	indices := make([]int, 0, len(blocks))
	for index := range blocks {
		indices = append(indices, index)
	}
	slices.Sort(indices)
	for _, index := range indices {
		block := blocks[index]
		// Tool arguments may be truncated even when the thinking blocks are complete.
		if block.kind == "tool_use" && modelapi.IsIncompleteStopReason(stopReason) {
			continue
		}
		if !locallyLimited && (block.kind == "text" || block.kind == "thinking" || block.kind == "redacted_thinking" || block.kind == "tool_use") && !block.closed {
			return nil, fmt.Errorf("%s returned an incomplete %s block", backend.provider, block.kind)
		}
		switch block.kind {
		case "text":
			if block.text.Len() != 0 {
				items = append(items, modelapi.Item{Kind: modelapi.ItemAssistantText, Text: block.text.String()})
			}
		case "thinking":
			if block.reasoning.Len() != 0 || block.signature.Len() != 0 {
				item := modelapi.Item{Kind: modelapi.ItemReasoning, Text: block.reasoning.String()}
				if !locallyLimited && block.signature.Len() != 0 {
					state, err := json.Marshal(thinkingBlockState{
						Type: "thinking", Thinking: block.reasoning.String(), Signature: block.signature.String(),
					}, json.Deterministic(true))
					if err != nil {
						return nil, fmt.Errorf("encode %s thinking state: %w", backend.provider, err)
					}
					item.ProviderData = []modelapi.ProviderData{{Kind: thinkingDataKind, Data: state}}
				}
				items = append(items, item)
			}
		case "redacted_thinking":
			// Ignore malformed incoming state for compatibility; saved state is validated before replay.
			if !locallyLimited && validRedactedThinkingData(block.data) {
				state, err := json.Marshal(thinkingBlockState{Type: "redacted_thinking", Data: block.data}, json.Deterministic(true))
				if err != nil {
					return nil, fmt.Errorf("encode %s redacted thinking state: %w", backend.provider, err)
				}
				items = append(items, modelapi.Item{
					Kind:         modelapi.ItemReasoning,
					ProviderData: []modelapi.ProviderData{{Kind: thinkingDataKind, Data: state}},
				})
			}
		case "tool_use":
			if strings.TrimSpace(block.id) == "" || strings.TrimSpace(block.name) == "" {
				return nil, fmt.Errorf("%s returned an incomplete tool_use block", backend.provider)
			}
			arguments, err := modelapi.NormalizeToolArguments(block.arguments.String())
			if err != nil {
				return nil, fmt.Errorf("%s returned invalid arguments for tool %q: %w", backend.provider, block.name, err)
			}
			providerID, _ := json.Marshal(block.id, json.Deterministic(true))
			items = append(items, modelapi.Item{Kind: modelapi.ItemToolCall, ToolCall: &modelapi.ToolCall{
				Name: block.name, RawArguments: arguments,
				ProviderReferences: []modelapi.ProviderReference{{Kind: backend.callIDReferenceKind(), Data: providerID}},
			}})
		}
	}
	return items, nil
}

func emitModelEvent(emit func(modelapi.StreamEvent), kind modelapi.StreamEventKind, value string) {
	if emit != nil && value != "" {
		emit(modelapi.StreamEvent{Kind: kind, Text: value})
	}
}

func (backend *Backend) callIDReferenceKind() string {
	return backend.backendID() + ".tool_use_id"
}
