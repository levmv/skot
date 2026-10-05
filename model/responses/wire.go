package responses

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"github.com/levmv/skot/internal/modelhttp"
	"github.com/levmv/skot/model"
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

// RouteTraits records demonstrated Responses route differences.
// The zero value sends no optional summary request.
type RouteTraits struct {
	ReasoningSummary       ReasoningSummary
	EncryptedReasoning     bool
	PromptCacheKey         bool
	RequireInstructions    bool
	OutputLimitUnsupported bool
}

func (traits RouteTraits) validate() error {
	switch traits.ReasoningSummary {
	case "", ReasoningSummaryAuto:
		return nil
	default:
		return fmt.Errorf("unsupported reasoning summary mode %q", traits.ReasoningSummary)
	}
}

func (RouteTraits) ProviderStateContract() model.ProviderStateContract {
	return "responses.manual_history.v1"
}

type responseRequest struct {
	Model           string           `json:"model"`
	Instructions    *string          `json:"instructions,omitzero"`
	Input           []jsontext.Value `json:"input"`
	Tools           []responseTool   `json:"tools,omitempty"`
	Reasoning       *reasoningConfig `json:"reasoning,omitzero"`
	Store           bool             `json:"store"`
	Stream          bool             `json:"stream"`
	Include         []string         `json:"include,omitempty"`
	PromptCacheKey  string           `json:"prompt_cache_key,omitempty"`
	MaxOutputTokens int              `json:"max_output_tokens,omitzero"`
}

type reasoningConfig struct {
	Effort  string           `json:"effort,omitempty"`
	Summary ReasoningSummary `json:"summary,omitempty"`
}

type responseTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  jsontext.Value `json:"parameters"`
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
	Output any    `json:"output"`
}

type functionCallOutputPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
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
// stateless turn. The visible summary deliberately lives in model.Item.Text so
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
	Model             string            `json:"model"`
	Provider          string            `json:"provider"`
	ID                string            `json:"id"`
	Status            string            `json:"status"`
	Output            []jsontext.Value  `json:"output"`
	Usage             jsontext.Value    `json:"usage,omitzero"`
	Error             *apiError         `json:"error,omitzero"`
	IncompleteDetails *incompleteDetail `json:"incomplete_details,omitzero"`
}

type incompleteDetail struct {
	Reason string `json:"reason"`
}

type streamEvent struct {
	Type        string         `json:"type"`
	Delta       string         `json:"delta,omitempty"`
	OutputIndex *int           `json:"output_index,omitzero"`
	Item        jsontext.Value `json:"item,omitzero"`
	Response    *wireResponse  `json:"response,omitzero"`
	Error       *apiError      `json:"error,omitzero"`
	Code        string         `json:"code,omitempty"`
	Message     string         `json:"message,omitempty"`
}

type apiError = modelhttp.ProviderErrorEnvelope

func (backend *Backend) buildRequest(request model.Request) (responseRequest, error) {
	if request.MaxOutputTokens < 0 {
		return responseRequest{}, errors.New("max output tokens cannot be negative")
	}
	if request.MaxOutputTokens > 0 && backend.traits.OutputLimitUnsupported {
		return responseRequest{}, model.ErrOutputLimitUnsupported
	}
	input := make([]jsontext.Value, 0, len(request.Items)+1)
	if request.Summary != "" {
		message, err := marshalInputItem(inputMessage{Role: "developer", Content: model.ConversationSummaryPrefix + request.Summary})
		if err != nil {
			return responseRequest{}, err
		}
		input = append(input, message)
	}
	callIDs := make(map[string]string)
	for index, item := range request.Items {
		switch item.Kind {
		case model.ItemUserText:
			raw, err := marshalInputItem(inputMessage{Role: "user", Content: item.Text})
			if err != nil {
				return responseRequest{}, err
			}
			input = append(input, raw)
		case model.ItemBoundaryText:
			raw, err := marshalInputItem(inputMessage{Role: "developer", Content: item.Text})
			if err != nil {
				return responseRequest{}, err
			}
			input = append(input, raw)
		case model.ItemAssistantText:
			if item.ResponseID == "" {
				return responseRequest{}, fmt.Errorf("assistant item %d has no response ID", index)
			}
			// Assistant text is semantic history and intentionally carries no
			// provider-owned message ID in model.Item. Responses accepts prior
			// assistant output in the portable easy-input message form.
			raw, err := marshalInputItem(inputMessage{Role: "assistant", Content: item.Text})
			if err != nil {
				return responseRequest{}, err
			}
			input = append(input, raw)
		case model.ItemReasoning:
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
		case model.ItemToolCall:
			if item.ResponseID == "" || item.ToolCall == nil || item.ToolCall.ID == "" || strings.TrimSpace(item.ToolCall.Name) == "" {
				return responseRequest{}, fmt.Errorf("assistant item %d has an invalid tool call", index)
			}
			arguments, err := model.NormalizeToolArguments(item.ToolCall.RawArguments)
			if err != nil {
				return responseRequest{}, fmt.Errorf("assistant item %d has invalid tool arguments: %w", index, err)
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
				Name: item.ToolCall.Name, Arguments: arguments, Status: identity.Status,
			})
			if err != nil {
				return responseRequest{}, err
			}
			input = append(input, raw)
		case model.ItemToolResult:
			if item.ToolResult == nil || item.ToolResult.CallID == "" {
				return responseRequest{}, fmt.Errorf("tool result item %d is invalid", index)
			}
			callID := callIDs[item.ToolResult.CallID]
			if callID == "" {
				callID = item.ToolResult.CallID
			}
			raw, err := marshalInputItem(functionCallOutputItem{
				Type: "function_call_output", CallID: callID, Output: responsesToolResultContent(item.ToolResult.Content),
			})
			if err != nil {
				return responseRequest{}, err
			}
			input = append(input, raw)
		default:
			return responseRequest{}, fmt.Errorf("unsupported model item kind %q", item.Kind)
		}
	}

	toolSpecs, err := model.NormalizeToolSpecs(request.Tools)
	if err != nil {
		return responseRequest{}, err
	}
	tools := make([]responseTool, 0, len(toolSpecs))
	for _, tool := range toolSpecs {
		tools = append(tools, responseTool{
			Type: "function", Name: tool.Name, Description: tool.Description,
			Parameters: tool.InputSchema, Strict: false,
		})
	}
	wireRequest := responseRequest{
		Model: backend.apiModel, Input: input,
		Tools: tools, Store: false, Stream: true,
		MaxOutputTokens: request.MaxOutputTokens,
	}
	if backend.traits.EncryptedReasoning {
		wireRequest.Include = []string{"reasoning.encrypted_content"}
	}
	if backend.traits.PromptCacheKey {
		wireRequest.PromptCacheKey = request.SessionID
	}
	if backend.traits.RequireInstructions || request.Instructions != "" {
		wireRequest.Instructions = &request.Instructions
	}
	if backend.reasoningEffort != "" || backend.traits.ReasoningSummary != "" {
		wireRequest.Reasoning = &reasoningConfig{
			Effort: backend.reasoningEffort, Summary: backend.traits.ReasoningSummary,
		}
	}
	return wireRequest, nil
}

func responsesToolResultContent(content model.Content) any {
	if !content.HasImage() {
		return content.Text()
	}
	parts := make([]functionCallOutputPart, 0, len(content))
	for _, part := range content {
		switch part.Kind {
		case model.ContentPartText:
			if part.Text != "" {
				parts = append(parts, functionCallOutputPart{Type: "input_text", Text: part.Text})
			}
		case model.ContentPartImage:
			if part.Image != nil {
				parts = append(parts, functionCallOutputPart{
					Type:     "input_image",
					ImageURL: "data:" + part.Image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(part.Image.Data),
				})
			}
		}
	}
	return parts
}

func marshalInputItem(value any) (jsontext.Value, error) {
	data, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("encode Responses input item: %w", err)
	}
	return data, nil
}

func (backend *Backend) reasoningInput(item model.Item, epoch string) (jsontext.Value, bool, error) {
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

func (backend *Backend) functionCallIdentity(call model.ToolCall, epoch string) (functionCallIdentity, error) {
	for _, reference := range call.ProviderReferences {
		if !reference.MatchesReplayContext(backend.callReferenceKind(), backend.backendID(), epoch) {
			continue
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

func (backend *Backend) matchesProviderContext(context *model.ProviderContext, epoch string) bool {
	if context == nil {
		return epoch == ""
	}
	return context.Backend == backend.backendID() && context.Epoch == epoch
}

func (backend *Backend) parseResponse(response wireResponse) (model.Response, error) {
	if response.Error != nil {
		return model.Response{}, modelhttp.NewProviderEnvelopeError(backend.provider, backend.model, response.Error)
	}
	stopReason := "stop"
	if response.Status == "incomplete" {
		reason, err := backend.normalizeIncompleteReason(response.IncompleteDetails)
		if err != nil {
			return model.Response{}, err
		}
		stopReason = reason
	}
	items := make([]model.Item, 0, len(response.Output))
	for index, raw := range response.Output {
		var output responseOutputItem
		if err := json.Unmarshal(raw, &output); err != nil {
			return model.Response{}, fmt.Errorf("decode %s response output item %d: %w", backend.provider, index, err)
		}
		switch output.Type {
		case "reasoning":
			if strings.TrimSpace(output.ID) == "" || output.EncryptedContent == "" {
				return model.Response{}, fmt.Errorf("%s response reasoning item %d is missing encrypted state", backend.provider, index)
			}
			var summary strings.Builder
			for _, part := range output.Summary {
				if part.Type != "summary_text" {
					return model.Response{}, fmt.Errorf("%s response reasoning item %d has unsupported summary part %q", backend.provider, index, part.Type)
				}
				summary.WriteString(part.Text)
			}
			state, err := json.Marshal(responseReasoningState{
				ID: output.ID, EncryptedContent: output.EncryptedContent,
			}, json.Deterministic(true))
			if err != nil {
				return model.Response{}, fmt.Errorf("encode %s reasoning state: %w", backend.provider, err)
			}
			items = append(items, model.Item{
				Kind: model.ItemReasoning, Text: summary.String(),
				ProviderData: []model.ProviderData{{Kind: reasoningItemDataKind, Data: state}},
			})
		case "message":
			if output.Role != "assistant" {
				return model.Response{}, fmt.Errorf("%s response message item %d has role %q", backend.provider, index, output.Role)
			}
			var text strings.Builder
			for _, content := range output.Content {
				switch content.Type {
				case "output_text":
					text.WriteString(content.Text)
				case "refusal":
					text.WriteString(content.Refusal)
				default:
					return model.Response{}, fmt.Errorf("%s response message item %d has unsupported content %q", backend.provider, index, content.Type)
				}
			}
			if text.Len() != 0 {
				items = append(items, model.Item{Kind: model.ItemAssistantText, Text: text.String()})
			}
		case "function_call":
			// Skip all calls in an incomplete response; their arguments may be truncated.
			if response.Status == "incomplete" {
				continue
			}
			if strings.TrimSpace(output.CallID) == "" || strings.TrimSpace(output.Name) == "" {
				return model.Response{}, fmt.Errorf("%s response function call item %d is incomplete", backend.provider, index)
			}
			identity, err := json.Marshal(functionCallIdentity{ID: output.ID, CallID: output.CallID, Status: output.Status}, json.Deterministic(true))
			if err != nil {
				return model.Response{}, fmt.Errorf("encode %s function call identity: %w", backend.provider, err)
			}
			arguments, err := model.NormalizeToolArguments(output.Arguments)
			if err != nil {
				return model.Response{}, fmt.Errorf("%s response function call item %d has invalid arguments: %w", backend.provider, index, err)
			}
			items = append(items, model.Item{Kind: model.ItemToolCall, ToolCall: &model.ToolCall{
				Name: output.Name, RawArguments: arguments,
				ProviderReferences: []model.ProviderReference{{Kind: backend.callReferenceKind(), Data: identity}},
			}})
			stopReason = "tool_calls"
		default:
			return model.Response{}, fmt.Errorf("%s response contains unsupported output item %q", backend.provider, output.Type)
		}
	}
	if len(items) == 0 && response.Status != "incomplete" {
		return model.Response{}, fmt.Errorf("%s response returned no output items", backend.provider)
	}
	return model.Response{Items: items, StopReason: stopReason}, nil
}
