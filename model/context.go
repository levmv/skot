package model

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ReplayContext identifies the model whose opaque state can be reused. Save it
// alongside accepted history; a fork uses the value saved at its branch point.
// A failed attempt need not replace the branch's last accepted context.
type ReplayContext struct {
	Backend               string                `json:"backend"`
	Provider              string                `json:"provider"`
	Model                 string                `json:"model"`
	ReasoningEffort       string                `json:"reasoning_effort,omitempty"`
	ProviderStateContract ProviderStateContract `json:"provider_state_contract,omitempty"`
	Epoch                 string                `json:"epoch"`
	Endpoint              string                `json:"endpoint,omitempty"`
}

// ForModel selects the model for the next call. It retains the current replay
// context when compatible and starts a fresh one after a model, protocol,
// reasoning setting, or endpoint change. The zero value starts a conversation.
func (current ReplayContext) ForModel(info Info) (ReplayContext, error) {
	info, err := NormalizeInfo(info)
	if err != nil {
		return ReplayContext{}, err
	}
	if current.Epoch != "" && selectionMatchesModel(current, info) {
		return current, nil
	}
	epoch, err := newID("epoch")
	if err != nil {
		return ReplayContext{}, err
	}
	return ReplayContext{
		Backend: info.BackendID, Provider: info.Provider, Model: info.Model,
		ReasoningEffort: info.ReasoningEffort, ProviderStateContract: info.ProviderStateContract,
		Epoch: epoch, Endpoint: info.Endpoint,
	}, nil
}

// PrepareRequest projects a copy of the supplied history for this context and
// backend. Keep the original Items for storage and future model selections.
func (current ReplayContext) PrepareRequest(backend Backend, request Request) Request {
	request.ProviderEpoch = current.Epoch
	request.Items = current.providerContext().ProjectItems(cloneItems(request.Items), backend)
	return request
}

// AcceptResponse prepares a successful backend response for storage and tool
// execution, assigning local IDs and retaining opaque provider references.
// Usage is preserved. Call it only for responses you accept into history;
// account for every Complete result's usage even when Complete returns an error.
func (current ReplayContext) AcceptResponse(response Response) (Response, error) {
	if current.Backend == "" || current.Epoch == "" {
		return response, errors.New("select a model before accepting its response")
	}
	if len(response.Items) == 0 {
		if IsIncompleteStopReason(response.StopReason) {
			return response, nil
		}
		return response, errors.New("model returned no items")
	}
	providerContext := current.providerContext()
	accepted := response
	accepted.Items = make([]Item, 0, len(response.Items))
	responseID, err := newID("response")
	if err != nil {
		return response, err
	}
	for _, item := range response.Items {
		item = item.Clone()
		item.ResponseID = responseID
		if len(item.Details) != 0 {
			return response, fmt.Errorf("%s item has product-owned details", item.Kind)
		}
		if item.Content != nil {
			return response, fmt.Errorf("%s response item has user content", item.Kind)
		}
		switch item.Kind {
		case ItemAssistantText:
			if len(item.ProviderData) != 0 || item.ToolCall != nil || item.ToolResult != nil {
				return response, fmt.Errorf("%s item has unrelated payload", item.Kind)
			}
			item.ProviderContext = nil
		case ItemReasoning:
			if item.ToolCall != nil || item.ToolResult != nil {
				return response, fmt.Errorf("%s item has unrelated payload", item.Kind)
			}
			for index := range item.ProviderData {
				item.ProviderData[index].Kind = strings.TrimSpace(item.ProviderData[index].Kind)
			}
			if err := ValidateProviderData(item.ProviderData); err != nil {
				return response, fmt.Errorf("invalid reasoning provider data: %w", err)
			}
			item.ProviderContext = &ProviderContext{Backend: providerContext.Backend, Epoch: providerContext.Epoch}
		case ItemToolCall:
			if len(item.ProviderData) != 0 || item.ToolCall == nil || strings.TrimSpace(item.ToolCall.Name) == "" {
				return response, errors.New("tool call name is required")
			}
			arguments, err := NormalizeToolArguments(item.ToolCall.RawArguments)
			if err != nil {
				return response, fmt.Errorf("tool call %q: %w", strings.TrimSpace(item.ToolCall.Name), err)
			}
			for _, reference := range item.ToolCall.ProviderReferences {
				if strings.TrimSpace(reference.Kind) == "" || !reference.Data.IsValid() {
					return response, errors.New("tool call provider reference is invalid")
				}
			}
			for index := range item.ToolCall.ProviderReferences {
				item.ToolCall.ProviderReferences[index].Backend = providerContext.Backend
				item.ToolCall.ProviderReferences[index].Epoch = providerContext.Epoch
			}
			item.ProviderContext = nil
			id, err := newID("call")
			if err != nil {
				return response, err
			}
			item.ToolCall.ID = id
			item.ToolCall.Name = strings.TrimSpace(item.ToolCall.Name)
			item.ToolCall.RawArguments = arguments
		default:
			return response, fmt.Errorf("model returned unsupported item kind %q", item.Kind)
		}
		accepted.Items = append(accepted.Items, item)
	}
	return accepted, nil
}

func (current ReplayContext) providerContext() ProviderContext {
	return ProviderContext{Backend: current.Backend, Epoch: current.Epoch}
}

// ProjectItems filters provider state in place and applies the backend's replay
// policy. Use ReplayContext.PrepareRequest to keep the original history intact.
func (providerContext ProviderContext) ProjectItems(items []Item, backend Backend) []Item {
	projected := items[:0]
	for _, item := range items {
		if item.Kind == ItemReasoning {
			if item.ProviderContext == nil || item.ProviderContext.Backend != providerContext.Backend || item.ProviderContext.Epoch != providerContext.Epoch {
				continue
			}
		}
		if item.ToolCall != nil {
			references := item.ToolCall.ProviderReferences[:0]
			for _, reference := range item.ToolCall.ProviderReferences {
				if reference.Backend == providerContext.Backend && reference.Epoch == providerContext.Epoch {
					references = append(references, reference)
				}
			}
			item.ToolCall.ProviderReferences = references
		}
		// Model backends receive semantic content without presentation metadata.
		item.Details = nil
		if item.ToolResult != nil {
			item.ToolResult.Details = nil
		}
		projected = append(projected, item)
	}
	if backend != nil {
		projected = backend.ProjectModelItems(projected)
	}
	return projected
}

// NormalizeInfo validates a model descriptor and normalizes names and settings.
func NormalizeInfo(modelInfo Info) (Info, error) {
	modelInfo.BackendID = strings.TrimSpace(modelInfo.BackendID)
	modelInfo.Provider = strings.TrimSpace(modelInfo.Provider)
	modelInfo.Model = strings.TrimSpace(modelInfo.Model)
	modelInfo.ReasoningEffort = strings.ToLower(strings.TrimSpace(modelInfo.ReasoningEffort))
	modelInfo.ProviderStateContract = ProviderStateContract(strings.TrimSpace(string(modelInfo.ProviderStateContract)))
	modelInfo.Endpoint = strings.TrimSpace(modelInfo.Endpoint)
	if modelInfo.BackendID == "" || modelInfo.Provider == "" || modelInfo.Model == "" {
		return Info{}, errors.New("model backend ID, provider, and model name are required")
	}
	if modelInfo.ContextWindow < 0 {
		return Info{}, errors.New("model context window cannot be negative")
	}
	if modelInfo.MaxRequestBytes < 0 || modelInfo.MaxCompletionBytes < 0 {
		return Info{}, errors.New("model byte limits cannot be negative")
	}
	if modelInfo.ContextWindowEstimated && modelInfo.ContextWindow == 0 {
		return Info{}, errors.New("estimated model context window must be positive")
	}
	return modelInfo, nil
}

func selectionMatchesModel(selection ReplayContext, modelInfo Info) bool {
	return selection.Backend == modelInfo.BackendID &&
		selection.Provider == modelInfo.Provider &&
		selection.Model == modelInfo.Model &&
		selection.ReasoningEffort == modelInfo.ReasoningEffort &&
		selection.ProviderStateContract == modelInfo.ProviderStateContract &&
		selection.Endpoint == modelInfo.Endpoint
}

func newID(prefix string) (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("generate %s ID: %w", prefix, err)
	}
	return prefix + "_" + hex.EncodeToString(data[:]), nil
}
