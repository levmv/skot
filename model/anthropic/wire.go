package anthropic

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
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
	Type         string         `json:"type"`
	Text         string         `json:"text,omitempty"`
	Thinking     *string        `json:"thinking,omitzero"`
	Signature    string         `json:"signature,omitempty"`
	Data         jsontext.Value `json:"data,omitzero"`
	ID           string         `json:"id,omitempty"`
	Name         string         `json:"name,omitempty"`
	Input        jsontext.Value `json:"input,omitzero"`
	ToolUseID    string         `json:"tool_use_id,omitempty"`
	Content      any            `json:"content,omitzero"`
	IsError      bool           `json:"is_error,omitzero"`
	CacheControl *cacheControl  `json:"cache_control,omitzero"`
}

type toolResultContentBlock struct {
	Type   string       `json:"type"`
	Text   string       `json:"text,omitempty"`
	Source *imageSource `json:"source,omitzero"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      []byte `json:"data"`
}

type cacheControl struct {
	Type string `json:"type"`
}

type toolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema jsontext.Value `json:"input_schema"`
}

type streamEvent struct {
	Type         string             `json:"type"`
	Index        int                `json:"index,omitzero"`
	Message      *streamMessage     `json:"message,omitzero"`
	ContentBlock streamContentBlock `json:"content_block"`
	Delta        streamDelta        `json:"delta"`
	Usage        wireUsage          `json:"usage"`
	Error        *apiError          `json:"error,omitzero"`
}

type streamMessage struct {
	StopReason *string   `json:"stop_reason,omitzero"`
	Usage      wireUsage `json:"usage"`
}

type streamContentBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	Thinking  string         `json:"thinking,omitempty"`
	Signature string         `json:"signature,omitempty"`
	Data      jsontext.Value `json:"data,omitzero"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     jsontext.Value `json:"input,omitzero"`
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
	InputTokens              *int `json:"input_tokens,omitzero"`
	OutputTokens             *int `json:"output_tokens,omitzero"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens,omitzero"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitzero"`
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

type streamBlock struct {
	kind      string
	id        string
	name      string
	text      strings.Builder
	reasoning strings.Builder
	signature strings.Builder
	arguments strings.Builder
	data      jsontext.Value
	closed    bool
}

type thinkingBlockState struct {
	Type      string         `json:"type"`
	Thinking  string         `json:"thinking,omitempty"`
	Signature string         `json:"signature,omitempty"`
	Data      jsontext.Value `json:"data,omitzero"`
}

func (backend *Backend) buildRequest(request agent.ModelRequest) (messagesRequest, error) {
	messages, err := backend.buildMessages(request)
	if err != nil {
		return messagesRequest{}, err
	}
	toolSpecs, err := agent.NormalizeToolSpecs(request.Tools)
	if err != nil {
		return messagesRequest{}, err
	}
	tools := make([]toolDefinition, 0, len(toolSpecs))
	for _, tool := range toolSpecs {
		tools = append(tools, toolDefinition{
			Name: tool.Name, Description: tool.Description,
			InputSchema: tool.InputSchema,
		})
	}
	system := request.Instructions
	if request.Summary != "" {
		if system != "" {
			system += "\n\n"
		}
		system += agent.ConversationSummaryPrefix + request.Summary
	}
	if backend.promptCache {
		markPromptCacheBreakpoint(messages)
	}
	return messagesRequest{
		Model: backend.apiModel, MaxTokens: backend.maxTokens, System: system,
		Messages: messages, Tools: tools, Stream: true,
	}, nil
}

// markPromptCacheBreakpoint caches everything the request sends before its final
// block: tools, instructions, and the whole replayed history. A tool turn then
// reads the previous turn's prefix and writes only what it appended, which is
// what keeps a long agent loop affordable on metered Anthropic routes.
func markPromptCacheBreakpoint(messages []message) {
	if len(messages) == 0 {
		return
	}
	blocks := messages[len(messages)-1].Content
	if len(blocks) == 0 {
		return
	}
	blocks[len(blocks)-1].CacheControl = &cacheControl{Type: "ephemeral"}
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
					arguments, err := agent.NormalizeToolArguments(part.ToolCall.RawArguments)
					if err != nil {
						return nil, fmt.Errorf("assistant item %d has invalid tool arguments: %w", index, err)
					}
					providerID := backend.providerCallID(*part.ToolCall, request.ProviderEpoch)
					callIDs[part.ToolCall.ID] = providerID
					blocks = append(blocks, contentBlock{
						Type: "tool_use", ID: providerID, Name: strings.TrimSpace(part.ToolCall.Name),
						Input: jsontext.Value(arguments),
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
				Type: "tool_result", ToolUseID: providerID, Content: anthropicToolResultContent(item.ToolResult.Content), IsError: item.ToolResult.Error,
			})
			index++
		default:
			return nil, fmt.Errorf("unsupported model item kind %q", item.Kind)
		}
	}
	return messages, nil
}

func anthropicToolResultContent(content agent.Content) any {
	if !content.HasImage() {
		return content.Text()
	}
	blocks := make([]toolResultContentBlock, 0, len(content))
	for _, part := range content {
		switch part.Kind {
		case agent.ContentPartText:
			if part.Text != "" {
				blocks = append(blocks, toolResultContentBlock{Type: "text", Text: part.Text})
			}
		case agent.ContentPartImage:
			if part.Image != nil {
				blocks = append(blocks, toolResultContentBlock{Type: "image", Source: &imageSource{
					Type: "base64", MediaType: part.Image.MediaType, Data: part.Image.Data,
				}})
			}
		}
	}
	return blocks
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
			return contentBlock{Type: "thinking", Thinking: new(state.Thinking), Signature: state.Signature}, true, nil
		case "redacted_thinking":
			if !validRedactedThinkingData(state.Data) {
				return contentBlock{}, false, errors.New("saved redacted thinking block has no opaque data")
			}
			return contentBlock{Type: "redacted_thinking", Data: state.Data.Clone()}, true, nil
		default:
			return contentBlock{}, false, fmt.Errorf("saved thinking block has unsupported type %q", state.Type)
		}
	}
	return contentBlock{}, false, nil
}

func validRedactedThinkingData(data jsontext.Value) bool {
	var opaque string
	return json.Unmarshal(data, &opaque) == nil && opaque != ""
}
