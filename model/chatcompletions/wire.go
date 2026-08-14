package chatcompletions

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/levmv/skot/agent"
)

// ReasoningEffortEncoding selects the explicitly supported wire field for a
// route. The zero value sends no reasoning control.
type ReasoningEffortEncoding string

const (
	ReasoningEffortTopLevel ReasoningEffortEncoding = "reasoning_effort"
	ReasoningEffortNested   ReasoningEffortEncoding = "reasoning"
)

// ReasoningReplayPolicy controls which provider-owned reasoning summaries may
// be returned on later requests. The zero value replays none.
type ReasoningReplayPolicy string

const (
	ReasoningReplayCurrentTurn ReasoningReplayPolicy = "current_turn"
	ReasoningReplayToolTurns   ReasoningReplayPolicy = "tool_turns"
)

// RouteTraits contains only demonstrated Chat Completions route differences.
// Optional request extensions stay disabled in the zero value.
type RouteTraits struct {
	ReasoningEffort ReasoningEffortEncoding
	ReasoningReplay ReasoningReplayPolicy
	PromptCacheKey  bool
}

func (traits RouteTraits) validate(reasoningEffort string) error {
	switch traits.ReasoningEffort {
	case "", ReasoningEffortTopLevel, ReasoningEffortNested:
	default:
		return fmt.Errorf("unsupported reasoning effort encoding %q", traits.ReasoningEffort)
	}
	switch traits.ReasoningReplay {
	case "", ReasoningReplayCurrentTurn, ReasoningReplayToolTurns:
	default:
		return fmt.Errorf("unsupported reasoning replay policy %q", traits.ReasoningReplay)
	}
	if reasoningEffort != "" && traits.ReasoningEffort == "" {
		return errors.New("reasoning effort is not supported by this route")
	}
	return nil
}

func (traits RouteTraits) ProviderStateContract() agent.ProviderStateContract {
	if traits.ReasoningReplay == "" {
		return ""
	}
	return agent.ProviderStateContract("chat_completions.reasoning_replay." + traits.ReasoningReplay + ".v1")
}

type chatRequest struct {
	Model           string           `json:"model"`
	Messages        []chatMessage    `json:"messages"`
	Tools           []chatTool       `json:"tools,omitempty"`
	Stream          bool             `json:"stream"`
	StreamOptions   *streamOptions   `json:"stream_options,omitempty"`
	PromptCacheKey  string           `json:"prompt_cache_key,omitempty"`
	ReasoningEffort string           `json:"reasoning_effort,omitempty"`
	Reasoning       *reasoningConfig `json:"reasoning,omitempty"`
}

type reasoningConfig struct {
	Effort string `json:"effort"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

type chatTool struct {
	Type     string             `json:"type"`
	Function chatToolDefinition `json:"function"`
}

type chatToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function wireFunctionCall `json:"function"`
}

type wireFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type streamChunk struct {
	Choices []streamChoice `json:"choices"`
	Usage   *wireUsage     `json:"usage,omitempty"`
	Error   *apiError      `json:"error,omitempty"`
}

type wireUsage struct {
	PromptTokens         int                 `json:"prompt_tokens"`
	CompletionTokens     int                 `json:"completion_tokens"`
	TotalTokens          int                 `json:"total_tokens"`
	PromptCacheHitTokens *int                `json:"prompt_cache_hit_tokens,omitempty"`
	PromptTokensDetails  *promptTokenDetails `json:"prompt_tokens_details,omitempty"`
}

type promptTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

func (usage wireUsage) modelUsage() agent.ModelUsage {
	cached := 0
	if usage.PromptCacheHitTokens != nil {
		cached = *usage.PromptCacheHitTokens
	} else if usage.PromptTokensDetails != nil {
		cached = usage.PromptTokensDetails.CachedTokens
	}
	return agent.ModelUsage{
		InputTokens:       usage.PromptTokens,
		CachedInputTokens: cached,
		OutputTokens:      usage.CompletionTokens,
		TotalTokens:       usage.TotalTokens,
	}
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason string      `json:"finish_reason"`
}

type streamDelta struct {
	Content          string         `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
}

func (delta *streamDelta) UnmarshalJSON(data []byte) error {
	type plain streamDelta
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*delta = streamDelta(decoded)
	var alias struct {
		ReasoningContent *string `json:"reasoning_content"`
		Reasoning        string  `json:"reasoning"`
	}
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	if alias.ReasoningContent != nil {
		delta.ReasoningContent = *alias.ReasoningContent
		return nil
	}
	delta.ReasoningContent = alias.Reasoning
	return nil
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    any    `json:"code,omitempty"`
}

func (e *apiError) message() string {
	if e == nil || e.Message == "" {
		return "unknown error"
	}
	return e.Message
}

func (backend *Backend) buildRequest(request agent.ModelRequest) (chatRequest, error) {
	messages, err := backend.buildMessages(request)
	if err != nil {
		return chatRequest{}, err
	}
	tools := make([]chatTool, 0, len(request.Tools))
	for _, tool := range request.Tools {
		if tool.Name == "" {
			return chatRequest{}, errors.New("tool name is required")
		}
		if !json.Valid(tool.InputSchema) {
			return chatRequest{}, fmt.Errorf("tool %q input schema is invalid", tool.Name)
		}
		tools = append(tools, chatTool{
			Type: "function",
			Function: chatToolDefinition{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  append(json.RawMessage(nil), tool.InputSchema...),
			},
		})
	}
	wireRequest := chatRequest{
		Model:         backend.apiModel,
		Messages:      messages,
		Tools:         tools,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}
	if backend.reasoningEffort != "" {
		switch backend.traits.ReasoningEffort {
		case ReasoningEffortNested:
			wireRequest.Reasoning = &reasoningConfig{Effort: backend.reasoningEffort}
		case ReasoningEffortTopLevel:
			wireRequest.ReasoningEffort = backend.reasoningEffort
		}
	}
	if backend.traits.PromptCacheKey {
		wireRequest.PromptCacheKey = request.SessionID
	}
	return wireRequest, nil
}

func (backend *Backend) buildMessages(request agent.ModelRequest) ([]chatMessage, error) {
	messages := make([]chatMessage, 0, len(request.Items)+1)
	if request.Instructions != "" {
		messages = append(messages, chatMessage{Role: "system", Content: request.Instructions})
	}
	if request.Summary != "" {
		messages = append(messages, chatMessage{Role: "system", Content: "Conversation summary:\n" + request.Summary})
	}
	lastUser := -1
	for index, item := range request.Items {
		if item.Kind == agent.ItemUserText {
			lastUser = index
		}
	}
	callIDs := make(map[string]string)
	for index := 0; index < len(request.Items); {
		item := request.Items[index]
		switch item.Kind {
		case agent.ItemUserText:
			messages = append(messages, chatMessage{Role: "user", Content: item.Text})
			index++

		case agent.ItemBoundaryText:
			messages = append(messages, chatMessage{Role: "system", Content: item.Text})
			index++

		case agent.ItemAssistantText, agent.ItemReasoning, agent.ItemToolCall:
			if item.ResponseID == "" {
				return nil, fmt.Errorf("assistant item %d has no response ID", index)
			}
			responseID := item.ResponseID
			message := chatMessage{Role: "assistant"}
			for index < len(request.Items) && request.Items[index].ResponseID == responseID {
				part := request.Items[index]
				switch part.Kind {
				case agent.ItemAssistantText:
					message.Content += part.Text
				case agent.ItemReasoning:
					if backend.matchesProviderContext(part.ProviderContext, request.ProviderEpoch) && backend.replaysReasoning(index, lastUser) {
						message.ReasoningContent += part.Text
					}
				case agent.ItemToolCall:
					if part.ToolCall == nil || part.ToolCall.ID == "" || part.ToolCall.Name == "" {
						return nil, fmt.Errorf("assistant item %d has an invalid tool call", index)
					}
					providerID := backend.providerCallID(*part.ToolCall, request.ProviderEpoch)
					callIDs[part.ToolCall.ID] = providerID
					message.ToolCalls = append(message.ToolCalls, wireToolCall{
						ID:   providerID,
						Type: "function",
						Function: wireFunctionCall{
							Name:      part.ToolCall.Name,
							Arguments: part.ToolCall.RawArguments,
						},
					})
				default:
					return nil, fmt.Errorf("response %q contains item kind %q", responseID, part.Kind)
				}
				index++
			}
			messages = append(messages, message)

		case agent.ItemToolResult:
			if item.ToolResult == nil || item.ToolResult.CallID == "" {
				return nil, fmt.Errorf("tool result item %d is invalid", index)
			}
			providerID := callIDs[item.ToolResult.CallID]
			if providerID == "" {
				providerID = item.ToolResult.CallID
			}
			messages = append(messages, chatMessage{
				Role:       "tool",
				Content:    item.ToolResult.Content,
				ToolCallID: providerID,
			})
			index++

		default:
			return nil, fmt.Errorf("unsupported model item kind %q", item.Kind)
		}
	}
	return messages, nil
}

func (backend *Backend) replaysReasoning(index, lastUser int) bool {
	switch backend.traits.ReasoningReplay {
	case ReasoningReplayToolTurns:
		return true
	case ReasoningReplayCurrentTurn:
		return index > lastUser
	default:
		return false
	}
}

func (backend *Backend) providerCallID(call agent.ToolCall, epoch string) string {
	for _, reference := range call.ProviderReferences {
		if reference.Kind != backend.callIDReferenceKind() {
			continue
		}
		if reference.Backend != "" || reference.Epoch != "" || epoch != "" {
			if reference.Backend != backend.backendID() || reference.Epoch != epoch {
				continue
			}
		}
		var value string
		if json.Unmarshal(reference.Data, &value) == nil && value != "" {
			return value
		}
	}
	return call.ID
}

func (backend *Backend) matchesProviderContext(providerContext *agent.ProviderContext, epoch string) bool {
	if providerContext == nil {
		return epoch == ""
	}
	return providerContext.Backend == backend.backendID() && providerContext.Epoch == epoch
}

type toolCallAccumulator struct {
	calls []wireToolCall
}

const maxToolCallsPerCompletion = 1024

func (accumulator *toolCallAccumulator) merge(deltas []wireToolCall) error {
	for _, delta := range deltas {
		index := len(accumulator.calls)
		if delta.Index != nil {
			index = *delta.Index
		} else if len(accumulator.calls) > 0 {
			index = len(accumulator.calls) - 1
			if delta.ID != "" && accumulator.calls[index].ID != "" && accumulator.calls[index].ID != delta.ID {
				index = len(accumulator.calls)
			}
		}
		if index < 0 {
			return fmt.Errorf("tool call index %d is negative", index)
		}
		if index >= maxToolCallsPerCompletion {
			return fmt.Errorf("tool call index %d exceeds limit %d", index, maxToolCallsPerCompletion)
		}
		for len(accumulator.calls) <= index {
			accumulator.calls = append(accumulator.calls, wireToolCall{Type: "function"})
		}
		call := &accumulator.calls[index]
		if delta.ID != "" {
			call.ID = delta.ID
		}
		if delta.Type != "" {
			call.Type = delta.Type
		}
		call.Function.Name += delta.Function.Name
		call.Function.Arguments += delta.Function.Arguments
	}
	return nil
}

func (accumulator *toolCallAccumulator) snapshot() []wireToolCall {
	result := make([]wireToolCall, 0, len(accumulator.calls))
	for _, call := range accumulator.calls {
		if call.ID == "" && call.Function.Name == "" && call.Function.Arguments == "" {
			continue
		}
		call.Index = nil
		if call.Type == "" {
			call.Type = "function"
		}
		result = append(result, call)
	}
	return result
}
