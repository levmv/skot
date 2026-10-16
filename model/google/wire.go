package google

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"github.com/levmv/skot/internal/modelinput"
	"github.com/levmv/skot/model"
)

type content struct {
	Role  string `json:"role"`
	Parts []part `json:"parts"`
}

type part struct {
	Text             *string                   `json:"text,omitzero"`
	Thought          bool                      `json:"thought,omitzero"`
	ThoughtSignature string                    `json:"thoughtSignature,omitempty"`
	FunctionCall     *functionCall             `json:"functionCall,omitzero"`
	FunctionResponse *functionResponse         `json:"functionResponse,omitzero"`
	InlineData       *blob                     `json:"inlineData,omitzero"`
	Other            map[string]jsontext.Value `json:"-"`
}

func (value *part) UnmarshalJSON(data []byte) error {
	type wirePart part
	var decoded wirePart
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"text", "thought", "thoughtSignature", "functionCall", "functionResponse", "inlineData"} {
		delete(fields, name)
	}
	decoded.Other = fields
	*value = part(decoded)
	return nil
}

func (value part) MarshalJSON() ([]byte, error) {
	type wirePart part
	data, err := json.Marshal(wirePart(value))
	if err != nil {
		return nil, err
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	for name, data := range value.Other {
		fields[name] = data
	}
	return json.Marshal(fields, json.Deterministic(true))
}

type blob struct {
	MIMEType string `json:"mimeType"`
	Data     []byte `json:"data"`
}

type functionCall struct {
	ID           string         `json:"id,omitempty"`
	Name         string         `json:"name"`
	Args         jsontext.Value `json:"args,omitzero"`
	PartialArgs  jsontext.Value `json:"partialArgs,omitzero"`
	WillContinue *bool          `json:"willContinue,omitzero"`
}

type functionResponse struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
	Parts    []part         `json:"parts,omitempty"`
}

type functionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  jsontext.Value `json:"parametersJsonSchema"`
}

type generateRequest struct {
	Contents          []content `json:"contents"`
	SystemInstruction *content  `json:"systemInstruction,omitzero"`
	Tools             []struct {
		Functions []functionDeclaration `json:"functionDeclarations"`
	} `json:"tools,omitempty"`
	GenerationConfig generationConfig `json:"generationConfig"`
	ToolConfig       struct {
		FunctionCallingConfig struct {
			Mode string `json:"mode"`
		} `json:"functionCallingConfig"`
	} `json:"toolConfig,omitzero"`
}

type generationConfig struct {
	CandidateCount  int `json:"candidateCount"`
	MaxOutputTokens int `json:"maxOutputTokens,omitzero"`
	ThinkingConfig  struct {
		ThinkingLevel   string `json:"thinkingLevel,omitempty"`
		IncludeThoughts bool   `json:"includeThoughts"`
	} `json:"thinkingConfig"`
}

func (backend *Backend) buildRequest(request model.Request) (generateRequest, error) {
	var result generateRequest
	if err := request.ToolChoice.Validate(); err != nil {
		return result, err
	}
	if err := modelinput.ValidateContent(request, backend.imageInputUnsupported); err != nil {
		return result, err
	}
	if request.MaxOutputTokens < 0 {
		return result, errors.New("max output tokens cannot be negative")
	}
	result.GenerationConfig = generationConfig{CandidateCount: 1, MaxOutputTokens: request.MaxOutputTokens}
	result.GenerationConfig.ThinkingConfig.ThinkingLevel = backend.reasoningEffort
	result.GenerationConfig.ThinkingConfig.IncludeThoughts = true
	switch request.ToolChoice {
	case model.ToolChoiceAuto:
		result.ToolConfig.FunctionCallingConfig.Mode = "AUTO"
	case model.ToolChoiceNone:
		result.ToolConfig.FunctionCallingConfig.Mode = "NONE"
	}
	instructions := request.Instructions
	if request.Summary != "" {
		if instructions != "" {
			instructions += "\n\n"
		}
		instructions += model.ConversationSummaryPrefix + request.Summary
	}
	if instructions != "" {
		result.SystemInstruction = &content{Parts: []part{{Text: new(instructions)}}}
	}
	tools, err := model.NormalizeToolSpecs(request.Tools)
	if err != nil {
		return result, err
	}
	if len(tools) != 0 {
		var declarations []functionDeclaration
		for _, tool := range tools {
			declarations = append(declarations, functionDeclaration{Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema})
		}
		result.Tools = append(result.Tools, struct {
			Functions []functionDeclaration `json:"functionDeclarations"`
		}{declarations})
	}
	calls := make(map[string]functionCall)
	for index := 0; index < len(request.Items); {
		item := request.Items[index]
		switch item.Kind {
		case model.ItemUserText:
			result.Contents = appendContent(result.Contents, "user", userParts(item.UserContent()))
			index++
		case model.ItemBoundaryText:
			result.Contents = appendContent(result.Contents, "user", []part{{Text: new(item.Text)}})
			index++
		case model.ItemAssistantText, model.ItemReasoning, model.ItemToolCall:
			if item.ResponseID == "" {
				return result, fmt.Errorf("assistant item %d has no response ID", index)
			}
			start := index
			for index < len(request.Items) && request.Items[index].ResponseID == item.ResponseID {
				index++
			}
			parts, err := backend.replayParts(request.Items[start:index], request.ProviderEpoch, calls)
			if err != nil {
				return result, err
			}
			result.Contents = appendContent(result.Contents, "model", parts)
		case model.ItemToolResult:
			if item.ToolResult == nil {
				return result, fmt.Errorf("tool result item %d is invalid", index)
			}
			call, ok := calls[item.ToolResult.CallID]
			if !ok {
				return result, fmt.Errorf("tool result item %d has no preceding function call", index)
			}
			key := "output"
			if item.ToolResult.Error {
				key = "error"
			}
			response := &functionResponse{ID: call.ID, Name: call.Name, Response: map[string]any{key: item.ToolResult.Content.Text()}}
			for _, entry := range item.ToolResult.Content {
				if entry.Kind == model.ContentPartImage {
					response.Parts = append(response.Parts, imagePart(*entry.Image))
				}
			}
			result.Contents = appendContent(result.Contents, "user", []part{{FunctionResponse: response}})
			index++
		default:
			return result, fmt.Errorf("unsupported model item kind %q", item.Kind)
		}
	}
	if len(result.Contents) == 0 {
		return result, errors.New("missing conversation contents for Gemini")
	}
	return result, nil
}

func appendContent(contents []content, role string, parts []part) []content {
	if len(parts) == 0 {
		return contents
	}
	if len(contents) != 0 && contents[len(contents)-1].Role == role {
		contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, parts...)
		return contents
	}
	return append(contents, content{Role: role, Parts: parts})
}

func userParts(value model.Content) []part {
	var parts []part
	for _, entry := range value {
		switch entry.Kind {
		case model.ContentPartText:
			parts = append(parts, part{Text: new(entry.Text)})
		case model.ContentPartImage:
			parts = append(parts, imagePart(*entry.Image))
		}
	}
	return parts
}

func imagePart(image model.ImageContent) part {
	return part{InlineData: &blob{MIMEType: strings.ToLower(strings.TrimSpace(image.MediaType)), Data: image.Data}}
}

// ReconcileResponseState runs after semantic sanitization, before journaling.
// Signed parts contain plaintext copies of the response. If redaction changed
// any of it, discard the entire signed sequence: storing the original leaks the
// redacted text, while editing it invalidates the signature. replayParts then
// imports the sanitized tool history using Google's supported marker.
func (backend *Backend) ReconcileResponseState(items []model.Item) []model.Item {
	changed := false
	for _, item := range items {
		for _, data := range item.ProviderData {
			if data.Kind != partsDataKind {
				continue
			}
			var parts []part
			if err := json.Unmarshal(data.Data, &parts); err != nil {
				changed = true
				continue
			}
			if !partsMatchItems(parts, items) {
				changed = true
			}
		}
	}
	if !changed {
		return items
	}
	kept := items[:0]
	for _, item := range items {
		data := item.ProviderData[:0]
		for _, entry := range item.ProviderData {
			if entry.Kind != partsDataKind {
				data = append(data, entry)
			}
		}
		item.ProviderData = data
		if item.Kind != model.ItemReasoning || item.Text != "" || len(data) != 0 {
			kept = append(kept, item)
		}
	}
	return kept
}

// Google signs the part sequence, including text and function calls. Store that
// sequence once as opaque reasoning state, alongside the ordinary semantic items.
// It is scoped by ReplayContext and must still match those items before replay.
func (backend *Backend) replayParts(items []model.Item, epoch string, calls map[string]functionCall) ([]part, error) {
	var saved []part
	for _, item := range items {
		if item.Kind == model.ItemReasoning && item.ProviderContext != nil &&
			item.ProviderContext.Backend == BackendID(backend.provider) && item.ProviderContext.Epoch == epoch {
			for _, data := range item.ProviderData {
				if data.Kind != partsDataKind {
					continue
				}
				if saved != nil {
					return nil, errors.New("duplicate Gemini replay state")
				}
				if err := json.Unmarshal(data.Data, &saved); err != nil || saved == nil {
					return nil, errors.New("invalid Gemini replay state")
				}
			}
		}
	}
	if saved != nil && !partsMatchItems(saved, items) {
		// The visible history is authoritative even when a host edits it
		// without reconciling provider state. Import it without stale signatures.
		saved = nil
	}
	var parts []part
	for _, item := range items {
		switch item.Kind {
		case model.ItemAssistantText:
			if item.Text != "" {
				parts = append(parts, part{Text: new(item.Text)})
			}
		case model.ItemReasoning:
			// Unattributed thoughts cannot be reused by a different model.
		case model.ItemToolCall:
			if item.ToolCall == nil || item.ToolCall.ID == "" || strings.TrimSpace(item.ToolCall.Name) == "" {
				return nil, errors.New("invalid Gemini function call history")
			}
			arguments, err := model.NormalizeToolArguments(item.ToolCall.RawArguments)
			if err != nil {
				return nil, err
			}
			call := functionCall{Name: item.ToolCall.Name, Args: jsontext.Value(arguments)}
			for _, reference := range item.ToolCall.ProviderReferences {
				if reference.MatchesReplayContext(callIDKind, BackendID(backend.provider), epoch) {
					if err := json.Unmarshal(reference.Data, &call.ID); err != nil {
						return nil, errors.New("invalid Gemini function call ID")
					}
				}
			}
			calls[item.ToolCall.ID] = call
			// Reconstructed calls have no reusable signature after history edits
			// or a model/endpoint change. Google requires this marker for imported
			// tool history. Original signed parts below are replayed unchanged.
			// https://ai.google.dev/gemini-api/docs/generate-content/thought-signatures#faqs
			parts = append(parts, part{FunctionCall: &call, ThoughtSignature: "skip_thought_signature_validator"})
		default:
			return nil, fmt.Errorf("invalid item %q inside Gemini model response", item.Kind)
		}
	}
	if saved != nil {
		return saved, nil
	}
	return parts, nil
}

func partsMatchItems(parts []part, items []model.Item) bool {
	left, err := semanticItems(parts, false)
	if err != nil {
		return false
	}
	var right []model.Item
	for _, item := range items {
		if item.Kind != model.ItemReasoning || item.Text != "" {
			right = append(right, item)
		}
	}
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Kind != right[i].Kind || left[i].Text != right[i].Text {
			return false
		}
		if left[i].ToolCall != nil {
			if right[i].ToolCall == nil || left[i].ToolCall.Name != right[i].ToolCall.Name {
				return false
			}
			arguments, err := model.NormalizeToolArguments(right[i].ToolCall.RawArguments)
			if err != nil || left[i].ToolCall.RawArguments != arguments {
				return false
			}
		}
	}
	return true
}

func semanticItems(parts []part, incomplete bool) ([]model.Item, error) {
	var items []model.Item
	var text strings.Builder
	var textKind model.ItemKind
	flush := func() {
		if text.Len() != 0 {
			items = append(items, model.Item{Kind: textKind, Text: text.String()})
			text.Reset()
		}
	}
	for _, value := range parts {
		if value.InlineData != nil || value.FunctionResponse != nil {
			return nil, errors.New("unsupported Gemini output part")
		}
		// Keep unknown metadata alongside recognized content for signed replay.
		// A part with no supported content is rejected below.
		if value.FunctionCall != nil {
			if value.Text != nil || value.Thought {
				return nil, errors.New("invalid mixed Gemini function call part")
			}
			if incomplete {
				continue
			}
			call := value.FunctionCall
			if len(call.PartialArgs) != 0 || (call.WillContinue != nil && *call.WillContinue) {
				return nil, errors.New("partial Gemini function arguments without enabling argument streaming")
			}
			if strings.TrimSpace(call.Name) == "" {
				return nil, errors.New("missing Gemini function call name")
			}
			arguments, err := model.NormalizeToolArguments(string(call.Args))
			if err != nil {
				return nil, err
			}
			item := model.Item{Kind: model.ItemToolCall, ToolCall: &model.ToolCall{Name: call.Name, RawArguments: arguments}}
			if call.ID != "" {
				id, _ := json.Marshal(call.ID)
				item.ToolCall.ProviderReferences = []model.ProviderReference{{Kind: callIDKind, Data: id}}
			}
			flush()
			items = append(items, item)
		} else if value.Text != nil && *value.Text != "" {
			kind := model.ItemAssistantText
			if value.Thought {
				kind = model.ItemReasoning
			}
			if kind != textKind {
				flush()
				textKind = kind
			}
			text.WriteString(*value.Text)
		} else if value.ThoughtSignature == "" && value.Text == nil {
			return nil, errors.New("empty Gemini output part")
		}
	}
	flush()
	return items, nil
}
