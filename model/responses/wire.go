package responses

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/modelhttp"
)

// incompleteReasons is the closed set of documented Responses incomplete
// reasons. An unknown value must not fall through as a complete answer.
var incompleteReasons = map[string]string{
	"max_tokens":        "max_tokens",
	"max_output_tokens": "max_output_tokens",
	"content_filter":    "content_filter",
}

func (backend *Backend) normalizeIncompleteReason(details *incompleteDetail) (string, error) {
	if details == nil || strings.TrimSpace(details.Reason) == "" {
		// The status alone still proves the response is incomplete.
		return "incomplete", nil
	}
	reason := strings.TrimSpace(details.Reason)
	normalized, known := incompleteReasons[strings.ToLower(reason)]
	if !known {
		return "", modelhttp.UnsupportedCompletionReasonError(backend.provider, reason)
	}
	return normalized, nil
}

const reasoningItemDataKind = "responses.reasoning_item"

type ReasoningSummary string

const ReasoningSummaryAuto ReasoningSummary = "auto"

// RouteTraits contains only optional Responses fields demonstrated for a
// concrete route. The zero value sends no optional summary request.
type RouteTraits struct {
	ReasoningSummary ReasoningSummary
}

func (traits RouteTraits) validate() error {
	switch traits.ReasoningSummary {
	case "", ReasoningSummaryAuto:
		return nil
	default:
		return fmt.Errorf("unsupported reasoning summary mode %q", traits.ReasoningSummary)
	}
}

func (RouteTraits) ProviderStateContract() agent.ProviderStateContract {
	return "responses.manual_history.v1"
}

type responseRequest struct {
	Model        string            `json:"model"`
	Instructions string            `json:"instructions,omitempty"`
	Input        []json.RawMessage `json:"input"`
	Tools        []responseTool    `json:"tools,omitempty"`
	Reasoning    *reasoningConfig  `json:"reasoning,omitempty"`
	Store        bool              `json:"store"`
	Stream       bool              `json:"stream"`
}

type reasoningConfig struct {
	Effort  string           `json:"effort,omitempty"`
	Summary ReasoningSummary `json:"summary,omitempty"`
}

type responseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	// Responses otherwise attempts strict mode and may silently resolve an
	// incompatible schema differently. Skot's existing tools are explicitly
	// non-strict until a route baseline says otherwise.
	Strict bool `json:"strict"`
}

type inputMessage struct {
	Type    string `json:"type,omitempty"`
	Role    string `json:"role"`
	Status  string `json:"status,omitempty"`
	Content any    `json:"content"`
}

type functionCallItem struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status,omitempty"`
}

type functionCallOutputItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type functionCallIdentity struct {
	ID     string `json:"id,omitempty"`
	CallID string `json:"call_id"`
	Status string `json:"status,omitempty"`
}

type responseOutputItem struct {
	Type             string                  `json:"type"`
	ID               string                  `json:"id,omitempty"`
	CallID           string                  `json:"call_id,omitempty"`
	Name             string                  `json:"name,omitempty"`
	Arguments        string                  `json:"arguments,omitempty"`
	Status           string                  `json:"status,omitempty"`
	Role             string                  `json:"role,omitempty"`
	EncryptedContent string                  `json:"encrypted_content,omitempty"`
	Content          []responseOutputContent `json:"content,omitempty"`
	Summary          []responseSummaryPart   `json:"summary,omitempty"`
}

type responseOutputContent struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Refusal string `json:"refusal,omitempty"`
}

type responseSummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// responseReasoningState is the provider-owned subset which must survive a
// stateless turn. The visible summary deliberately lives in agent.Item.Text so
// it follows the ordinary sanitization path instead of being duplicated inside
// opaque journal data.
type responseReasoningState struct {
	ID               string `json:"id"`
	EncryptedContent string `json:"encrypted_content"`
}

type responseReasoningInput struct {
	Type             string                `json:"type"`
	ID               string                `json:"id"`
	Summary          []responseSummaryPart `json:"summary"`
	EncryptedContent string                `json:"encrypted_content"`
}

type wireResponse struct {
	ID                string            `json:"id"`
	Status            string            `json:"status"`
	Output            []json.RawMessage `json:"output"`
	Usage             *responseUsage    `json:"usage,omitempty"`
	Error             *apiError         `json:"error,omitempty"`
	IncompleteDetails *incompleteDetail `json:"incomplete_details,omitempty"`
}

type incompleteDetail struct {
	Reason string `json:"reason"`
}

type responseUsage struct {
	InputTokens        int                 `json:"input_tokens"`
	InputTokenDetails  *inputTokenDetails  `json:"input_tokens_details,omitempty"`
	OutputTokens       int                 `json:"output_tokens"`
	OutputTokenDetails *outputTokenDetails `json:"output_tokens_details,omitempty"`
	TotalTokens        int                 `json:"total_tokens"`
}

type inputTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type outputTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

func (usage responseUsage) modelUsage() agent.ModelUsage {
	cached := 0
	if usage.InputTokenDetails != nil {
		cached = usage.InputTokenDetails.CachedTokens
	}
	reasoning := 0
	if usage.OutputTokenDetails != nil {
		reasoning = usage.OutputTokenDetails.ReasoningTokens
	}
	return agent.ModelUsage{
		InputTokens: usage.InputTokens, CachedInputTokens: cached,
		OutputTokens: usage.OutputTokens, ReasoningTokens: reasoning,
		TotalTokens: usage.TotalTokens,
	}
}

type streamEvent struct {
	Type     string        `json:"type"`
	Delta    string        `json:"delta,omitempty"`
	Response *wireResponse `json:"response,omitempty"`
	Error    *apiError     `json:"error,omitempty"`
	Code     string        `json:"code,omitempty"`
	Message  string        `json:"message,omitempty"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    any    `json:"code,omitempty"`
}

func (apiError *apiError) message() string {
	if apiError == nil || strings.TrimSpace(apiError.Message) == "" {
		return "unknown error"
	}
	return strings.TrimSpace(apiError.Message)
}

func (backend *Backend) buildRequest(request agent.ModelRequest) (responseRequest, error) {
	input := make([]json.RawMessage, 0, len(request.Items)+1)
	if request.Summary != "" {
		message, err := marshalInputItem(inputMessage{Role: "developer", Content: "Conversation summary:\n" + request.Summary})
		if err != nil {
			return responseRequest{}, err
		}
		input = append(input, message)
	}
	callIDs := make(map[string]string)
	for index, item := range request.Items {
		switch item.Kind {
		case agent.ItemUserText:
			raw, err := marshalInputItem(inputMessage{Role: "user", Content: item.Text})
			if err != nil {
				return responseRequest{}, err
			}
			input = append(input, raw)
		case agent.ItemBoundaryText:
			raw, err := marshalInputItem(inputMessage{Role: "developer", Content: item.Text})
			if err != nil {
				return responseRequest{}, err
			}
			input = append(input, raw)
		case agent.ItemAssistantText:
			if item.ResponseID == "" {
				return responseRequest{}, fmt.Errorf("assistant item %d has no response ID", index)
			}
			// Assistant text is semantic history and intentionally carries no
			// provider-owned message ID in agent.Item. Responses accepts prior
			// assistant output in the portable easy-input message form.
			raw, err := marshalInputItem(inputMessage{Role: "assistant", Content: item.Text})
			if err != nil {
				return responseRequest{}, err
			}
			input = append(input, raw)
		case agent.ItemReasoning:
			if item.ResponseID == "" {
				return responseRequest{}, fmt.Errorf("reasoning item %d has no response ID", index)
			}
			raw, ok, err := backend.reasoningInput(item, request.ProviderEpoch)
			if err != nil {
				return responseRequest{}, fmt.Errorf("reasoning item %d: %w", index, err)
			}
			if ok {
				input = append(input, raw)
			}
		case agent.ItemToolCall:
			if item.ResponseID == "" || item.ToolCall == nil || item.ToolCall.ID == "" || strings.TrimSpace(item.ToolCall.Name) == "" {
				return responseRequest{}, fmt.Errorf("assistant item %d has an invalid tool call", index)
			}
			identity, err := backend.functionCallIdentity(*item.ToolCall, request.ProviderEpoch)
			if err != nil {
				return responseRequest{}, fmt.Errorf("tool call item %d: %w", index, err)
			}
			if identity.CallID == "" {
				identity.CallID = item.ToolCall.ID
			}
			callIDs[item.ToolCall.ID] = identity.CallID
			raw, err := marshalInputItem(functionCallItem{
				Type: "function_call", ID: identity.ID, CallID: identity.CallID,
				Name: item.ToolCall.Name, Arguments: item.ToolCall.RawArguments, Status: identity.Status,
			})
			if err != nil {
				return responseRequest{}, err
			}
			input = append(input, raw)
		case agent.ItemToolResult:
			if item.ToolResult == nil || item.ToolResult.CallID == "" {
				return responseRequest{}, fmt.Errorf("tool result item %d is invalid", index)
			}
			callID := callIDs[item.ToolResult.CallID]
			if callID == "" {
				callID = item.ToolResult.CallID
			}
			raw, err := marshalInputItem(functionCallOutputItem{
				Type: "function_call_output", CallID: callID, Output: item.ToolResult.Content,
			})
			if err != nil {
				return responseRequest{}, err
			}
			input = append(input, raw)
		default:
			return responseRequest{}, fmt.Errorf("unsupported model item kind %q", item.Kind)
		}
	}

	tools := make([]responseTool, 0, len(request.Tools))
	for _, tool := range request.Tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			return responseRequest{}, errors.New("tool name is required")
		}
		if !json.Valid(tool.InputSchema) {
			return responseRequest{}, fmt.Errorf("tool %q input schema is invalid", name)
		}
		tools = append(tools, responseTool{
			Type: "function", Name: name, Description: tool.Description,
			Parameters: append(json.RawMessage(nil), tool.InputSchema...), Strict: false,
		})
	}
	wireRequest := responseRequest{
		Model: backend.apiModel, Instructions: request.Instructions, Input: input,
		Tools: tools, Store: false, Stream: true,
	}
	if backend.reasoningEffort != "" || backend.traits.ReasoningSummary != "" {
		wireRequest.Reasoning = &reasoningConfig{
			Effort: backend.reasoningEffort, Summary: backend.traits.ReasoningSummary,
		}
	}
	return wireRequest, nil
}

func marshalInputItem(value any) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode Responses input item: %w", err)
	}
	return data, nil
}

func (backend *Backend) reasoningInput(item agent.Item, epoch string) (json.RawMessage, bool, error) {
	if !backend.matchesProviderContext(item.ProviderContext, epoch) {
		return nil, false, nil
	}
	for _, data := range item.ProviderData {
		if data.Kind != reasoningItemDataKind {
			continue
		}
		var state responseReasoningState
		if err := json.Unmarshal(data.Data, &state); err != nil {
			return nil, false, fmt.Errorf("decode saved reasoning item: %w", err)
		}
		if strings.TrimSpace(state.ID) == "" || state.EncryptedContent == "" {
			return nil, false, errors.New("saved reasoning item is incomplete")
		}
		summary := make([]responseSummaryPart, 0, 1)
		if item.Text != "" {
			summary = append(summary, responseSummaryPart{Type: "summary_text", Text: item.Text})
		}
		raw, err := marshalInputItem(responseReasoningInput{
			Type: "reasoning", ID: state.ID, Summary: summary,
			EncryptedContent: state.EncryptedContent,
		})
		if err != nil {
			return nil, false, err
		}
		return raw, true, nil
	}
	return nil, false, nil
}

func (backend *Backend) functionCallIdentity(call agent.ToolCall, epoch string) (functionCallIdentity, error) {
	for _, reference := range call.ProviderReferences {
		if reference.Kind != backend.callReferenceKind() {
			continue
		}
		if reference.Backend != "" || reference.Epoch != "" || epoch != "" {
			if reference.Backend != backend.backendID() || reference.Epoch != epoch {
				continue
			}
		}
		var identity functionCallIdentity
		if err := json.Unmarshal(reference.Data, &identity); err != nil {
			return functionCallIdentity{}, fmt.Errorf("decode saved function call identity: %w", err)
		}
		if strings.TrimSpace(identity.CallID) == "" {
			return functionCallIdentity{}, errors.New("saved function call identity has no call ID")
		}
		return identity, nil
	}
	return functionCallIdentity{CallID: call.ID}, nil
}

func (backend *Backend) matchesProviderContext(context *agent.ProviderContext, epoch string) bool {
	if context == nil {
		return epoch == ""
	}
	return context.Backend == backend.backendID() && context.Epoch == epoch
}

func (backend *Backend) parseResponse(response wireResponse) (agent.ModelResponse, error) {
	if response.Error != nil {
		return agent.ModelResponse{}, fmt.Errorf("%s Responses API error: %s", backend.provider, response.Error.message())
	}
	items := make([]agent.Item, 0, len(response.Output))
	hasToolCall := false
	for index, raw := range response.Output {
		var output responseOutputItem
		if err := json.Unmarshal(raw, &output); err != nil {
			return agent.ModelResponse{}, fmt.Errorf("decode %s response output item %d: %w", backend.provider, index, err)
		}
		switch output.Type {
		case "reasoning":
			if strings.TrimSpace(output.ID) == "" || output.EncryptedContent == "" {
				return agent.ModelResponse{}, fmt.Errorf("%s response reasoning item %d is missing encrypted state", backend.provider, index)
			}
			var summary strings.Builder
			for _, part := range output.Summary {
				if part.Type != "summary_text" {
					return agent.ModelResponse{}, fmt.Errorf("%s response reasoning item %d has unsupported summary part %q", backend.provider, index, part.Type)
				}
				summary.WriteString(part.Text)
			}
			state, err := json.Marshal(responseReasoningState{
				ID: output.ID, EncryptedContent: output.EncryptedContent,
			})
			if err != nil {
				return agent.ModelResponse{}, fmt.Errorf("encode %s reasoning state: %w", backend.provider, err)
			}
			items = append(items, agent.Item{
				Kind: agent.ItemReasoning, Text: summary.String(),
				ProviderData: []agent.ProviderData{{Kind: reasoningItemDataKind, Data: state}},
			})
		case "message":
			if output.Role != "assistant" {
				return agent.ModelResponse{}, fmt.Errorf("%s response message item %d has role %q", backend.provider, index, output.Role)
			}
			var text strings.Builder
			for _, content := range output.Content {
				switch content.Type {
				case "output_text":
					text.WriteString(content.Text)
				case "refusal":
					text.WriteString(content.Refusal)
				default:
					return agent.ModelResponse{}, fmt.Errorf("%s response message item %d has unsupported content %q", backend.provider, index, content.Type)
				}
			}
			if text.Len() != 0 {
				items = append(items, agent.Item{Kind: agent.ItemAssistantText, Text: text.String()})
			}
		case "function_call":
			if strings.TrimSpace(output.CallID) == "" || strings.TrimSpace(output.Name) == "" {
				return agent.ModelResponse{}, fmt.Errorf("%s response function call item %d is incomplete", backend.provider, index)
			}
			identity, err := json.Marshal(functionCallIdentity{ID: output.ID, CallID: output.CallID, Status: output.Status})
			if err != nil {
				return agent.ModelResponse{}, fmt.Errorf("encode %s function call identity: %w", backend.provider, err)
			}
			items = append(items, agent.Item{Kind: agent.ItemToolCall, ToolCall: &agent.ToolCall{
				Name: output.Name, RawArguments: output.Arguments,
				ProviderReferences: []agent.ProviderReference{{Kind: backend.callReferenceKind(), Data: identity}},
			}})
			hasToolCall = true
		default:
			return agent.ModelResponse{}, fmt.Errorf("%s response contains unsupported output item %q", backend.provider, output.Type)
		}
	}
	usage := agent.ModelUsage{}
	if response.Usage != nil {
		usage = response.Usage.modelUsage()
	}
	stopReason := "stop"
	if hasToolCall {
		stopReason = "tool_calls"
	}
	if response.Status == "incomplete" {
		reason, err := backend.normalizeIncompleteReason(response.IncompleteDetails)
		if err != nil {
			return agent.ModelResponse{}, err
		}
		stopReason = reason
	}
	if len(items) == 0 && response.Status != "incomplete" {
		return agent.ModelResponse{}, fmt.Errorf("%s response returned no output items", backend.provider)
	}
	return agent.ModelResponse{Items: items, Usage: usage, StopReason: stopReason}, nil
}
