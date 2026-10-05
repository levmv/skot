package model

import (
	"context"
	"encoding/json/jsontext"
	"time"
)

type ItemKind string

const (
	ItemUserText      ItemKind = "user_text"
	ItemBoundaryText  ItemKind = "boundary_text"
	ItemAssistantText ItemKind = "assistant_text"
	ItemReasoning     ItemKind = "reasoning_summary"
	ItemToolCall      ItemKind = "tool_call"
	ItemToolResult    ItemKind = "tool_result"
)

type Item struct {
	Kind ItemKind `json:"kind"`
	// ResponseID is a local ID grouping items from one accepted response.
	ResponseID      string           `json:"response_id,omitempty"`
	ProviderContext *ProviderContext `json:"provider_context,omitzero"`
	// ProviderData carries opaque replay state. Preserve it unchanged alongside
	// ProviderContext. Visible reasoning remains in Text.
	ProviderData []ProviderData `json:"provider_data,omitempty"`
	Text         string         `json:"text,omitempty"`
	ToolCall     *ToolCall      `json:"tool_call,omitzero"`
	ToolResult   *ToolResult    `json:"tool_result,omitzero"`
	// Details hold application metadata and stay out of model input.
	Details []Detail `json:"details,omitempty"`
}

// ProviderContext binds an item's opaque state to a backend and replay epoch.
// ReplayContext.AcceptResponse assigns it when accepting provider output.
type ProviderContext struct {
	Backend string `json:"backend"`
	Epoch   string `json:"epoch"`
}

type ProviderData struct {
	Kind string         `json:"kind"`
	Data jsontext.Value `json:"data"`
}

type ToolCall struct {
	// ID is assigned by ReplayContext.AcceptResponse. Use it as ToolResult.CallID.
	ID   string `json:"id"`
	Name string `json:"name"`
	// RawArguments is a normalized JSON object; empty provider input is {}.
	RawArguments       string              `json:"raw_arguments"`
	ProviderReferences []ProviderReference `json:"provider_references,omitempty"`
}

// ProviderReference carries adapter-owned replay identity in saved tool
// calls. Kind identifies the adapter payload; Backend and Epoch bind it to a
// provider selection. Empty Backend and Epoch identify a legacy reference.
type ProviderReference struct {
	Kind    string         `json:"kind"`
	Backend string         `json:"backend"`
	Epoch   string         `json:"epoch"`
	Data    jsontext.Value `json:"data"`
}

// MatchesReplayContext reports whether a reference of kind belongs to the
// selected backend epoch. A legacy unattributed reference matches only when
// the selected epoch is also empty.
func (reference ProviderReference) MatchesReplayContext(kind, backend, epoch string) bool {
	if reference.Kind != kind {
		return false
	}
	if reference.Backend == "" && reference.Epoch == "" && epoch == "" {
		return true
	}
	return reference.Backend == backend && reference.Epoch == epoch
}

type ToolResult struct {
	CallID  string   `json:"call_id"`
	Content Content  `json:"content"`
	Details []Detail `json:"details,omitempty"`
	Error   bool     `json:"error,omitzero"`
	Unknown bool     `json:"unknown,omitzero"`
}

type Detail struct {
	Kind string         `json:"kind"`
	Data jsontext.Value `json:"data"`
}

type ToolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// InputSchema must be a JSON Schema with top-level type object.
	InputSchema  jsontext.Value `json:"input_schema"`
	ParallelSafe bool           `json:"parallel_safe,omitzero"`
}

// ConversationSummaryPrefix introduces compacted history to a model.
const ConversationSummaryPrefix = "Conversation summary:\n"

type Request struct {
	SessionID     string
	ProviderEpoch string
	Instructions  string
	Summary       string
	Items         []Item
	Tools         []ToolSpec
	// MaxOutputTokens requests a provider-side generation limit. Zero keeps the
	// route default; negative values are invalid. OpenAI, Anthropic, and DeepSeek
	// count reasoning within this limit; compatible services may count differently.
	// Routes known not to support it return ErrOutputLimitUnsupported before sending.
	MaxOutputTokens int
	// StreamIdleTimeout bounds silence between provider stream payloads. Zero
	// leaves this concern to the backend implementation or caller context.
	StreamIdleTimeout time.Duration
}

type Info struct {
	// BackendID identifies the provider and protocol.
	BackendID       string `json:"backend"`
	Provider        string `json:"provider,omitempty"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// ProviderStateContract identifies the rules for opaque replay state.
	// Changing it starts a fresh ReplayContext even for the same model.
	ProviderStateContract ProviderStateContract `json:"provider_state_contract,omitempty"`
	// ImageInputUnsupported reports routes known not to accept images.
	// False does not guarantee image support.
	ImageInputUnsupported  bool `json:"image_input_unsupported,omitzero"`
	ContextWindow          int  `json:"context_window,omitzero"`
	ContextWindowEstimated bool `json:"context_window_estimated,omitzero"`
	MaxRequestBytes        int  `json:"max_request_bytes,omitzero"`
	MaxCompletionBytes     int  `json:"max_completion_bytes,omitzero"`
	// Endpoint identifies the effective provider endpoint without credentials.
	// It does not configure requests or authorization.
	Endpoint string `json:"endpoint,omitempty"`
}

// Response includes observed usage even when Complete returns an error.
type Response struct {
	Items      []Item
	Usage      Usage
	StopReason string
}

const StopReasonOutputLimit = "output_limit"

type StreamEventKind string

const (
	EventTextDelta             StreamEventKind = "text_delta"
	EventReasoningSummaryDelta StreamEventKind = "reasoning_summary_delta"
)

type StreamEvent struct {
	Kind StreamEventKind
	Text string
}

// Backend executes one model attempt and owns protocol-specific replay policy.
type Backend interface {
	Complete(context.Context, Request, func(StreamEvent)) (Response, error)
	// ProjectModelItems applies adapter replay policy after ReplayContext ownership
	// filtering. It may filter reasoning items in place, but must preserve order
	// and every other item, and must not retain the slice.
	ProjectModelItems([]Item) []Item
}

// ProviderStateContract identifies how an adapter stores and replays opaque data.
type ProviderStateContract string
