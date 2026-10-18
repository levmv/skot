package model

// ReportedTokens preserves the difference between an absent counter and zero.
// Cache reads/writes and text/image modalities are reported subsets of input;
// reasoning and text/image modalities are reported subsets of output. Absent
// modalities are unknown and are not derived from the totals.
// TotalTokens is derived only when both input and output are known.
type ReportedTokens struct {
	InputTokens           *int `json:"input_tokens,omitzero"`
	CachedInputTokens     *int `json:"cached_input_tokens,omitzero"`
	CacheWriteInputTokens *int `json:"cache_write_input_tokens,omitzero"`
	InputTextTokens       *int `json:"input_text_tokens,omitzero"`
	InputImageTokens      *int `json:"input_image_tokens,omitzero"`
	OutputTokens          *int `json:"output_tokens,omitzero"`
	ReasoningTokens       *int `json:"reasoning_tokens,omitzero"`
	OutputTextTokens      *int `json:"output_text_tokens,omitzero"`
	OutputImageTokens     *int `json:"output_image_tokens,omitzero"`
	TotalTokens           *int `json:"total_tokens,omitzero"`
}

// Known returns the observed counters, using zero for absent fields.
// Keep ReportedTokens when the distinction between missing and zero matters.
func (tokens ReportedTokens) Known() TokenCounts {
	value := func(v *int) int {
		if v == nil {
			return 0
		}
		return *v
	}
	return TokenCounts{
		InputTokens:           value(tokens.InputTokens),
		CachedInputTokens:     value(tokens.CachedInputTokens),
		CacheWriteInputTokens: value(tokens.CacheWriteInputTokens),
		InputTextTokens:       value(tokens.InputTextTokens),
		InputImageTokens:      value(tokens.InputImageTokens),
		OutputTokens:          value(tokens.OutputTokens),
		ReasoningTokens:       value(tokens.ReasoningTokens),
		OutputTextTokens:      value(tokens.OutputTextTokens),
		OutputImageTokens:     value(tokens.OutputImageTokens),
		TotalTokens:           value(tokens.TotalTokens),
	}
}

// TokenCounts holds aggregate counts without presence information.
type TokenCounts struct {
	InputTokens int `json:"input_tokens,omitzero"`
	// CachedInputTokens is the cached subset of InputTokens, not additional
	// input.
	CachedInputTokens int `json:"cached_input_tokens,omitzero"`
	// CacheWriteInputTokens is another subset of InputTokens.
	CacheWriteInputTokens int `json:"cache_write_input_tokens,omitzero"`
	// InputTextTokens and InputImageTokens are reported subsets of InputTokens.
	InputTextTokens  int `json:"input_text_tokens,omitzero"`
	InputImageTokens int `json:"input_image_tokens,omitzero"`
	OutputTokens     int `json:"output_tokens,omitzero"`
	// ReasoningTokens is the reported reasoning subset of OutputTokens, not an
	// additional token count.
	ReasoningTokens int `json:"reasoning_tokens,omitzero"`
	// OutputTextTokens and OutputImageTokens are reported subsets of
	// OutputTokens.
	OutputTextTokens  int `json:"output_text_tokens,omitzero"`
	OutputImageTokens int `json:"output_image_tokens,omitzero"`
	// TotalTokens counts InputTokens plus OutputTokens when both are known.
	// Partial usage can lack a total; inspect Usage.Tokens for presence.
	TotalTokens int `json:"total_tokens,omitzero"`
}

func (usage TokenCounts) Add(other TokenCounts) TokenCounts {
	return TokenCounts{
		InputTokens:           usage.InputTokens + other.InputTokens,
		CachedInputTokens:     usage.CachedInputTokens + other.CachedInputTokens,
		CacheWriteInputTokens: usage.CacheWriteInputTokens + other.CacheWriteInputTokens,
		InputTextTokens:       usage.InputTextTokens + other.InputTextTokens,
		InputImageTokens:      usage.InputImageTokens + other.InputImageTokens,
		OutputTokens:          usage.OutputTokens + other.OutputTokens,
		ReasoningTokens:       usage.ReasoningTokens + other.ReasoningTokens,
		OutputTextTokens:      usage.OutputTextTokens + other.OutputTextTokens,
		OutputImageTokens:     usage.OutputImageTokens + other.OutputImageTokens,
		TotalTokens:           usage.TotalTokens + other.TotalTokens,
	}
}

type UsageStatus string

const (
	UsageUnavailable UsageStatus = "unavailable"
	UsagePartial     UsageStatus = "partial"
	UsageFinal       UsageStatus = "final"
)

// ReportedCost preserves a decimal amount supplied by the provider.
// Amount preserves the JSON number's decimal representation (including zero).
// OpenRouter's account_charge is denominated in USD credits; BYOK requests can
// also report upstream_inference in USD. These are not automatically added together.
type ReportedCost struct {
	Kind     string `json:"kind"`
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// Usage describes the latest observed usage for one provider call.
// Final means a terminal usage snapshot was observed, not that every optional
// counter or monetary charge is available.
type Usage struct {
	Status     UsageStatus    `json:"status,omitempty"`
	Tokens     ReportedTokens `json:"tokens"`
	Costs      []ReportedCost `json:"costs,omitempty"`
	RequestID  string         `json:"request_id,omitempty"`
	ResponseID string         `json:"response_id,omitempty"`
	Model      string         `json:"model,omitempty"`
	Provider   string         `json:"provider,omitempty"`
}
