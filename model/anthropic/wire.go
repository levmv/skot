package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/levmv/skot/agent"
)

type messagesRequest struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	System    string           `json:"system,omitempty"`
	Messages  []message        `json:"messages"`
	Tools     []toolDefinition `json:"tools,omitempty"`
	Stream    bool             `json:"stream"`
}

type message struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  *string         `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   *string         `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type toolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type streamEvent struct {
	Type         string             `json:"type"`
	Index        int                `json:"index,omitempty"`
	Message      *streamMessage     `json:"message,omitempty"`
	ContentBlock streamContentBlock `json:"content_block,omitempty"`
	Delta        streamDelta        `json:"delta,omitempty"`
	Usage        wireUsage          `json:"usage,omitempty"`
	Error        *apiError          `json:"error,omitempty"`
}

type streamMessage struct {
	StopReason *string   `json:"stop_reason,omitempty"`
	Usage      wireUsage `json:"usage,omitempty"`
}

type streamContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

type streamDelta struct {
	Type        string `json:"type,omitempty"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}

type wireUsage struct {
	InputTokens              *int `json:"input_tokens,omitempty"`
	OutputTokens             *int `json:"output_tokens,omitempty"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
}

type usageAccumulator struct {
	input, output, cacheRead, cacheCreation int
}

func (usage *usageAccumulator) merge(value wireUsage) {
	if value.InputTokens != nil {
		usage.input = *value.InputTokens
	}
	if value.OutputTokens != nil {
		usage.output = *value.OutputTokens
	}
	if value.CacheReadInputTokens != nil {
		usage.cacheRead = *value.CacheReadInputTokens
	}
	if value.CacheCreationInputTokens != nil {
		usage.cacheCreation = *value.CacheCreationInputTokens
	}
}

func (usage usageAccumulator) modelUsage() agent.ModelUsage {
	input := usage.input + usage.cacheRead + usage.cacheCreation
	return agent.ModelUsage{
		InputTokens: input, CachedInputTokens: usage.cacheRead,
		OutputTokens: usage.output, TotalTokens: input + usage.output,
	}
}

type apiError struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
}

type streamBlock struct {
	kind      string
	id        string
	name      string
	text      strings.Builder
	reasoning strings.Builder
	signature strings.Builder
	arguments strings.Builder
	data      json.RawMessage
	closed    bool
}

type thinkingBlockState struct {
	Type      string          `json:"type"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

func (backend *Backend) buildRequest(request agent.ModelRequest) (messagesRequest, error) {
	messages, err := backend.buildMessages(request)
	if err != nil {
		return messagesRequest{}, err
	}
	tools := make([]toolDefinition, 0, len(request.Tools))
	for _, tool := range request.Tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			return messagesRequest{}, errors.New("tool name is required")
		}
		if err := validateObjectJSON(string(tool.InputSchema)); err != nil {
			return messagesRequest{}, fmt.Errorf("tool %q input schema is invalid: %w", name, err)
		}
		tools = append(tools, toolDefinition{
			Name: name, Description: tool.Description,
			InputSchema: append(json.RawMessage(nil), tool.InputSchema...),
		})
	}
	system := request.Instructions
	if request.Summary != "" {
		if system != "" {
			system += "\n\n"
		}
		system += "Conversation summary:\n" + request.Summary
	}
	return messagesRequest{
		Model: backend.apiModel, MaxTokens: backend.maxTokens, System: system,
		Messages: messages, Tools: tools, Stream: true,
	}, nil
}

func (backend *Backend) buildMessages(request agent.ModelRequest) ([]message, error) {
	messages := make([]message, 0, len(request.Items))
	callIDs := make(map[string]string)
	for index := 0; index < len(request.Items); {
		item := request.Items[index]
		switch item.Kind {
		case agent.ItemUserText, agent.ItemBoundaryText:
			messages = appendMessage(messages, "user", contentBlock{Type: "text", Text: item.Text})
			index++
		case agent.ItemAssistantText, agent.ItemReasoning, agent.ItemToolCall:
			if item.ResponseID == "" {
				return nil, fmt.Errorf("assistant item %d has no response ID", index)
			}
			responseID := item.ResponseID
			blocks := make([]contentBlock, 0, 2)
			for index < len(request.Items) && request.Items[index].ResponseID == responseID {
				part := request.Items[index]
				switch part.Kind {
				case agent.ItemAssistantText:
					if part.Text != "" {
						blocks = append(blocks, contentBlock{Type: "text", Text: part.Text})
					}
				case agent.ItemReasoning:
					block, ok, err := backend.replayThinkingBlock(part, request.ProviderEpoch)
					if err != nil {
						return nil, fmt.Errorf("reasoning item %d: %w", index, err)
					}
					if ok {
						blocks = append(blocks, block)
					}
				case agent.ItemToolCall:
					if part.ToolCall == nil || part.ToolCall.ID == "" || strings.TrimSpace(part.ToolCall.Name) == "" {
						return nil, fmt.Errorf("assistant item %d has an invalid tool call", index)
					}
					arguments := strings.TrimSpace(part.ToolCall.RawArguments)
					if arguments == "" {
						arguments = "{}"
					}
					if err := validateObjectJSON(arguments); err != nil {
						return nil, fmt.Errorf("assistant item %d has invalid tool arguments: %w", index, err)
					}
					providerID := backend.providerCallID(*part.ToolCall, request.ProviderEpoch)
					callIDs[part.ToolCall.ID] = providerID
					blocks = append(blocks, contentBlock{
						Type: "tool_use", ID: providerID, Name: strings.TrimSpace(part.ToolCall.Name),
						Input: json.RawMessage(arguments),
					})
				default:
					return nil, fmt.Errorf("response %q contains item kind %q", responseID, part.Kind)
				}
				index++
			}
			messages = appendBlocks(messages, "assistant", blocks)
		case agent.ItemToolResult:
			if item.ToolResult == nil || item.ToolResult.CallID == "" {
				return nil, fmt.Errorf("tool result item %d is invalid", index)
			}
			providerID := callIDs[item.ToolResult.CallID]
			if providerID == "" {
				providerID = item.ToolResult.CallID
			}
			messages = appendMessage(messages, "user", contentBlock{
				Type: "tool_result", ToolUseID: providerID, Content: stringPointer(item.ToolResult.Content), IsError: item.ToolResult.Error,
			})
			index++
		default:
			return nil, fmt.Errorf("unsupported model item kind %q", item.Kind)
		}
	}
	return messages, nil
}

func appendMessage(messages []message, role string, block contentBlock) []message {
	return appendBlocks(messages, role, []contentBlock{block})
}

func appendBlocks(messages []message, role string, blocks []contentBlock) []message {
	if len(blocks) == 0 {
		return messages
	}
	if len(messages) != 0 && messages[len(messages)-1].Role == role {
		messages[len(messages)-1].Content = append(messages[len(messages)-1].Content, blocks...)
		return messages
	}
	return append(messages, message{Role: role, Content: append([]contentBlock(nil), blocks...)})
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

func (backend *Backend) replayThinkingBlock(item agent.Item, epoch string) (contentBlock, bool, error) {
	if item.ProviderContext == nil || item.ProviderContext.Backend != backend.backendID() || item.ProviderContext.Epoch != epoch {
		return contentBlock{}, false, nil
	}
	for _, data := range item.ProviderData {
		if data.Kind != thinkingDataKind {
			continue
		}
		var state thinkingBlockState
		if err := json.Unmarshal(data.Data, &state); err != nil {
			return contentBlock{}, false, fmt.Errorf("decode saved thinking block: %w", err)
		}
		switch state.Type {
		case "thinking":
			if strings.TrimSpace(state.Signature) == "" {
				return contentBlock{}, false, errors.New("saved thinking block has no signature")
			}
			return contentBlock{Type: "thinking", Thinking: stringPointer(state.Thinking), Signature: state.Signature}, true, nil
		case "redacted_thinking":
			if !validRedactedThinkingData(state.Data) {
				return contentBlock{}, false, errors.New("saved redacted thinking block has no opaque data")
			}
			return contentBlock{Type: "redacted_thinking", Data: append(json.RawMessage(nil), state.Data...)}, true, nil
		default:
			return contentBlock{}, false, fmt.Errorf("saved thinking block has unsupported type %q", state.Type)
		}
	}
	return contentBlock{}, false, nil
}

func validRedactedThinkingData(data json.RawMessage) bool {
	var opaque string
	return json.Unmarshal(data, &opaque) == nil && opaque != ""
}

func stringPointer(value string) *string {
	return &value
}

func validateObjectJSON(value string) error {
	if trimmed := strings.TrimSpace(value); trimmed == "" || trimmed[0] != '{' {
		return errors.New("expected a JSON object")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &object); err != nil {
		return err
	}
	if object == nil {
		return errors.New("expected a JSON object")
	}
	return nil
}
