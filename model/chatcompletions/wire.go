package chatcompletions

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/modelhttp"
)

// ReasoningEffortEncoding selects the explicitly supported wire field for a
// route. The zero value sends no reasoning control.
type ReasoningEffortEncoding string

const (
	ReasoningEffortTopLevel ReasoningEffortEncoding = "reasoning_effort"
	ReasoningEffortNested   ReasoningEffortEncoding = "reasoning"
	// ReasoningEffortThinking uses DeepSeek's thinking switch. Enabled requests
	// also carry reasoning_effort; disabled requests do not.
	ReasoningEffortThinking ReasoningEffortEncoding = "thinking"
)

const reasoningEffortOff = "off"

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
	case "", ReasoningEffortTopLevel, ReasoningEffortNested, ReasoningEffortThinking:
	default:
		return fmt.Errorf("unsupported reasoning effort encoding %q", traits.ReasoningEffort)
	}
	if reasoningEffort == reasoningEffortOff && traits.ReasoningEffort != ReasoningEffortThinking {
		return fmt.Errorf("reasoning effort %q needs the %q encoding", reasoningEffortOff, ReasoningEffortThinking)
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
	switch traits.ReasoningReplay {
	case ReasoningReplayToolTurns:
		// v2 narrowed source selection from "every owned reasoning item" to the
		// assistant turns that actually made tool calls, so a session saved
		// under v1 must start a new epoch instead of replaying the old set.
		return "chat_completions.reasoning_replay.tool_turns.v2"
	case ReasoningReplayCurrentTurn:
		return "chat_completions.reasoning_replay.current_turn.v1"
	default:
		return ""
	}
}

type chatRequest struct {
	Model           string           `json:"model"`
	Messages        []chatMessage    `json:"messages"`
	Tools           []chatTool       `json:"tools,omitempty"`
	Stream          bool             `json:"stream"`
	StreamOptions   *streamOptions   `json:"stream_options,omitzero"`
	PromptCacheKey  string           `json:"prompt_cache_key,omitempty"`
	ReasoningEffort string           `json:"reasoning_effort,omitempty"`
	Reasoning       *reasoningConfig `json:"reasoning,omitzero"`
	Thinking        *thinkingControl `json:"thinking,omitzero"`
}

type reasoningConfig struct {
	Effort string `json:"effort"`
}

type thinkingControl struct {
	Type string `json:"type"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role             string         `json:"role"`
	Content          chatContent    `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

type chatContent struct {
	Text  string
	Parts []chatContentPart
}

type chatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitzero"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

func textChatContent(text string) chatContent { return chatContent{Text: text} }

func (content chatContent) MarshalJSON() ([]byte, error) {
	if content.Parts == nil {
		return json.Marshal(content.Text, json.Deterministic(true))
	}
	return json.Marshal(content.Parts, json.Deterministic(true))
}

func (content *chatContent) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) != 0 && data[0] == '"' {
		content.Parts = nil
		return json.Unmarshal(data, &content.Text)
	}
	content.Text = ""
	return json.Unmarshal(data, &content.Parts)
}

type chatTool struct {
	Type     string             `json:"type"`
	Function chatToolDefinition `json:"function"`
}

type chatToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  jsontext.Value `json:"parameters"`
}

type wireToolCall struct {
	Index    *int             `json:"index,omitzero"`
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
	Usage   *wireUsage     `json:"usage,omitzero"`
	Error   *apiError      `json:"error,omitzero"`
}

type wireUsage struct {
	PromptTokens            int                     `json:"prompt_tokens"`
	CompletionTokens        int                     `json:"completion_tokens"`
	TotalTokens             int                     `json:"total_tokens"`
	PromptCacheHitTokens    *int                    `json:"prompt_cache_hit_tokens,omitzero"`
	PromptTokensDetails     *promptTokenDetails     `json:"prompt_tokens_details,omitzero"`
	CompletionTokensDetails *completionTokenDetails `json:"completion_tokens_details,omitzero"`
}

type promptTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type completionTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

func (usage wireUsage) modelUsage() agent.ModelUsage {
	cached := 0
	if usage.PromptCacheHitTokens != nil {
		cached = *usage.PromptCacheHitTokens
	} else if usage.PromptTokensDetails != nil {
		cached = usage.PromptTokensDetails.CachedTokens
	}
	reasoning := 0
	if usage.CompletionTokensDetails != nil {
		reasoning = usage.CompletionTokensDetails.ReasoningTokens
	}
	return agent.ModelUsage{
		InputTokens:       usage.PromptTokens,
		CachedInputTokens: cached,
		OutputTokens:      usage.CompletionTokens,
		ReasoningTokens:   reasoning,
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

type apiError = modelhttp.ProviderErrorEnvelope

func (backend *Backend) buildRequest(request agent.ModelRequest) (chatRequest, error) {
	// Direct callers may supply unprojected, caller-owned items. Project a copy.
	request.Items = backend.ProjectModelItems(append([]agent.Item(nil), request.Items...))
	messages, err := backend.buildMessages(request)
	if err != nil {
		return chatRequest{}, err
	}
	toolSpecs, err := agent.NormalizeToolSpecs(request.Tools)
	if err != nil {
		return chatRequest{}, err
	}
	tools := make([]chatTool, 0, len(toolSpecs))
	for _, tool := range toolSpecs {
		tools = append(tools, chatTool{
			Type: "function",
			Function: chatToolDefinition{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.InputSchema,
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
		case ReasoningEffortThinking:
			if backend.reasoningEffort == reasoningEffortOff {
				wireRequest.Thinking = &thinkingControl{Type: "disabled"}
				break
			}
			wireRequest.Thinking = &thinkingControl{Type: "enabled"}
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
		messages = append(messages, chatMessage{Role: "system", Content: textChatContent(request.Instructions)})
	}
	if request.Summary != "" {
		messages = append(messages, chatMessage{Role: "system", Content: textChatContent(agent.ConversationSummaryPrefix + request.Summary)})
	}
	callIDs := make(map[string]string)
	for index := 0; index < len(request.Items); {
		item := request.Items[index]
		switch item.Kind {
		case agent.ItemUserText:
			messages = append(messages, chatMessage{Role: "user", Content: textChatContent(item.Text)})
			index++

		case agent.ItemBoundaryText:
			messages = append(messages, chatMessage{Role: "system", Content: textChatContent(item.Text)})
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
					message.Content.Text += part.Text
				case agent.ItemReasoning:
					if backend.matchesProviderContext(part.ProviderContext, request.ProviderEpoch) {
						message.ReasoningContent += part.Text
					}
				case agent.ItemToolCall:
					if part.ToolCall == nil || part.ToolCall.ID == "" || part.ToolCall.Name == "" {
						return nil, fmt.Errorf("assistant item %d has an invalid tool call", index)
					}
					arguments, err := agent.NormalizeToolArguments(part.ToolCall.RawArguments)
					if err != nil {
						return nil, fmt.Errorf("assistant item %d has invalid tool arguments: %w", index, err)
					}
					providerID := backend.providerCallID(*part.ToolCall, request.ProviderEpoch)
					callIDs[part.ToolCall.ID] = providerID
					message.ToolCalls = append(message.ToolCalls, wireToolCall{
						ID:   providerID,
						Type: "function",
						Function: wireFunctionCall{
							Name:      part.ToolCall.Name,
							Arguments: arguments,
						},
					})
				default:
					return nil, fmt.Errorf("response %q contains item kind %q", responseID, part.Kind)
				}
				index++
			}
			messages = append(messages, message)

		case agent.ItemToolResult:
			var imageParts []chatContentPart
			for index < len(request.Items) && request.Items[index].Kind == agent.ItemToolResult {
				result := request.Items[index].ToolResult
				if result == nil || result.CallID == "" {
					return nil, fmt.Errorf("tool result item %d is invalid", index)
				}
				providerID := callIDs[result.CallID]
				if providerID == "" {
					providerID = result.CallID
				}
				text := result.Content.Text()
				if text == "" && result.Content.HasImage() {
					text = "Tool returned image content."
				}
				messages = append(messages, chatMessage{
					Role: "tool", Content: textChatContent(text), ToolCallID: providerID,
				})
				for _, part := range result.Content {
					if part.Kind != agent.ContentPartImage || part.Image == nil {
						continue
					}
					imageParts = append(imageParts,
						chatContentPart{Type: "text", Text: "Image returned by tool call " + result.CallID + ":"},
						chatContentPart{Type: "image_url", ImageURL: &chatImageURL{URL: imageDataURL(*part.Image)}},
					)
				}
				index++
			}
			if len(imageParts) != 0 {
				messages = append(messages, chatMessage{Role: "user", Content: chatContent{Parts: imageParts}})
			}

		default:
			return nil, fmt.Errorf("unsupported model item kind %q", item.Kind)
		}
	}
	return messages, nil
}

func imageDataURL(image agent.ImageContent) string {
	return "data:" + image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(image.Data)
}

// finishReasons is closed: unknown values are rejected. Gateway aliases
// normalize to agent reasons, while incomplete provider values remain visible.
// Anthropic-shaped aliases support gateways proxying those models through Chat
// Completions.
var finishReasons = map[string]string{
	"stop":                          "stop",
	"end_turn":                      "stop",
	"stop_sequence":                 "stop",
	"tool_calls":                    "tool_calls",
	"function_call":                 "tool_calls",
	"tool_use":                      "tool_calls",
	"length":                        "length",
	"max_tokens":                    "max_tokens",
	"max_output_tokens":             "max_output_tokens",
	"model_context_window_exceeded": "model_context_window_exceeded",
	"content_filter":                "content_filter",
	"refusal":                       "refusal",
	"pause_turn":                    "pause_turn",
	"insufficient_system_resource":  "insufficient_system_resource",
}

func (backend *Backend) normalizeFinishReason(reason string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(reason))
	normalized, known := finishReasons[trimmed]
	if !known {
		return "", modelhttp.UnsupportedCompletionReasonError(backend.provider, reason)
	}
	return normalized, nil
}

// ProjectModelItems applies the route's reasoning replay policy. Tool-turn
// replay keeps reasoning only for assistant turns with tool calls; current-turn
// replay keeps reasoning after the last user message; the zero policy drops it.
func (backend *Backend) ProjectModelItems(items []agent.Item) []agent.Item {
	toolTurns := map[string]bool(nil)
	lastUser := -1
	switch backend.traits.ReasoningReplay {
	case ReasoningReplayToolTurns:
		toolTurns = make(map[string]bool)
		for _, item := range items {
			if item.Kind == agent.ItemToolCall && item.ResponseID != "" {
				toolTurns[item.ResponseID] = true
			}
		}
	case ReasoningReplayCurrentTurn:
		for index, item := range items {
			if item.Kind == agent.ItemUserText {
				lastUser = index
			}
		}
	}

	projected := items[:0]
	for index, item := range items {
		if item.Kind == agent.ItemReasoning {
			keep := false
			switch backend.traits.ReasoningReplay {
			case ReasoningReplayToolTurns:
				keep = toolTurns[item.ResponseID]
			case ReasoningReplayCurrentTurn:
				keep = index > lastUser
			}
			if !keep {
				continue
			}
		}
		projected = append(projected, item)
	}
	return projected
}

func (backend *Backend) providerCallID(call agent.ToolCall, epoch string) string {
	for _, reference := range call.ProviderReferences {
		if !reference.MatchesReplayContext(backend.callIDReferenceKind(), backend.backendID(), epoch) {
			continue
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
