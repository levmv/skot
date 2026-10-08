package modelhttp

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/http"

	"github.com/levmv/skot/model"
)

// UsageAccumulator merges cumulative usage snapshots from a stream.
// Counters in successive events replace earlier values; they are not summed.
type UsageAccumulator struct {
	Snapshot model.Usage
	fields   map[string]jsontext.Value
	raw      jsontext.Value
}

func (usage *UsageAccumulator) Attach(response *model.Response) {
	response.Usage = usage.Snapshot
	if response.Usage.Status == "" {
		response.Usage.Status = model.UsageUnavailable
	}
}

func (usage *UsageAccumulator) SetRequestID(header http.Header) {
	usage.Snapshot.RequestID = header.Get("x-request-id")
	if usage.Snapshot.RequestID == "" {
		usage.Snapshot.RequestID = header.Get("request-id")
	}
}

// Observe updates the fields present in each usage snapshot; nested values are
// replaced whole. protocol identifies token semantics;
// provider identifies explicitly supported monetary extensions.
func (usage *UsageAccumulator) Observe(raw jsontext.Value, protocol, provider string, final bool) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("decode usage: %w", err)
	}
	if usage.fields == nil {
		usage.fields = make(map[string]jsontext.Value)
	}
	maps.Copy(usage.fields, fields)
	merged, err := json.Marshal(usage.fields, json.Deterministic(true))
	if err != nil {
		return err
	}
	var values struct {
		PromptTokens             *int `json:"prompt_tokens"`
		CompletionTokens         *int `json:"completion_tokens"`
		InputTokens              *int `json:"input_tokens"`
		OutputTokens             *int `json:"output_tokens"`
		TotalTokens              *int `json:"total_tokens"`
		PromptCacheHitTokens     *int `json:"prompt_cache_hit_tokens"`
		CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
		PromptDetails            struct {
			Cached *int `json:"cached_tokens"`
			Write  *int `json:"cache_write_tokens"`
		} `json:"prompt_tokens_details"`
		InputDetails struct {
			Cached *int `json:"cached_tokens"`
			Write  *int `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
		CompletionDetails struct {
			Reasoning *int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
		OutputDetails struct {
			Reasoning *int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	}
	if err := json.Unmarshal(merged, &values); err != nil {
		return fmt.Errorf("decode usage counters: %w", err)
	}
	tokens := model.ReportedTokens{TotalTokens: values.TotalTokens}
	switch protocol {
	case "chat_completions":
		tokens.InputTokens, tokens.OutputTokens = values.PromptTokens, values.CompletionTokens
		tokens.CachedInputTokens = values.PromptDetails.Cached
		if values.PromptCacheHitTokens != nil {
			tokens.CachedInputTokens = values.PromptCacheHitTokens
		}
		tokens.CacheWriteInputTokens, tokens.ReasoningTokens = values.PromptDetails.Write, values.CompletionDetails.Reasoning
	case "responses":
		tokens.InputTokens, tokens.OutputTokens = values.InputTokens, values.OutputTokens
		tokens.CachedInputTokens, tokens.ReasoningTokens = values.InputDetails.Cached, values.OutputDetails.Reasoning
		tokens.CacheWriteInputTokens = values.InputDetails.Write
	case "anthropic_messages":
		tokens.InputTokens, tokens.OutputTokens = values.InputTokens, values.OutputTokens
		tokens.CachedInputTokens, tokens.CacheWriteInputTokens = values.CacheReadInputTokens, values.CacheCreationInputTokens
		if tokens.InputTokens != nil {
			input := *tokens.InputTokens
			for _, extra := range []*int{tokens.CachedInputTokens, tokens.CacheWriteInputTokens} {
				if extra != nil {
					input += *extra
				}
			}
			tokens.InputTokens = &input
		}
	default:
		return fmt.Errorf("unsupported usage protocol %q", protocol)
	}
	if tokens.InputTokens != nil && tokens.OutputTokens != nil {
		total := *tokens.InputTokens + *tokens.OutputTokens
		tokens.TotalTokens = &total
	}
	usage.Snapshot.Tokens = tokens
	usage.raw = merged
	usage.Snapshot.Status = model.UsagePartial
	if final {
		usage.Snapshot.Status = model.UsageFinal
	}
	usage.Snapshot.Costs = nil
	if provider == "openrouter" {
		// Unsupported monetary values do not invalidate token counts or output.
		var costs map[string]jsontext.Value
		// With OpenRouter credits, upstream inference overlaps account_charge.
		// Keep it as a separate amount only when the provider bills a BYOK key.
		if bytes.Equal(bytes.TrimSpace(usage.fields["is_byok"]), []byte("true")) {
			_ = json.Unmarshal(usage.fields["cost_details"], &costs)
		}
		for _, cost := range []struct {
			kind string
			raw  jsontext.Value
		}{
			{"account_charge", usage.fields["cost"]}, {"upstream_inference", costs["upstream_inference_cost"]},
		} {
			if cost.raw.Kind() == '0' {
				usage.Snapshot.Costs = append(usage.Snapshot.Costs, model.ReportedCost{Kind: cost.kind, Amount: string(bytes.TrimSpace(cost.raw)), Currency: "USD"})
			}
		}
	}
	return nil
}

// RawUsage returns an owned encoding of the fields used to normalize usage.
func (usage *UsageAccumulator) RawUsage() jsontext.Value {
	return bytes.Clone(usage.raw)
}
