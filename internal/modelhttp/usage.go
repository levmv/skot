package modelhttp

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"math"
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
// profile identifies explicitly supported monetary extensions.
// Invalid snapshots leave both the report and the fields used by later merges intact.
func (usage *UsageAccumulator) Observe(raw jsontext.Value, protocol string, profile model.ResponseProfile, final bool) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("decode usage: %w", err)
	}
	mergedFields := maps.Clone(usage.fields)
	if mergedFields == nil {
		mergedFields = make(map[string]jsontext.Value)
	}
	maps.Copy(mergedFields, fields)
	merged, err := json.Marshal(mergedFields, json.Deterministic(true))
	if err != nil {
		return err
	}
	var values struct {
		GooglePrompt             int  `json:"promptTokenCount"`
		GoogleCandidates         int  `json:"candidatesTokenCount"`
		GoogleThoughts           int  `json:"thoughtsTokenCount"`
		GoogleCached             int  `json:"cachedContentTokenCount"`
		GoogleToolPrompt         int  `json:"toolUsePromptTokenCount"`
		GoogleTotal              *int `json:"totalTokenCount"`
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
	case "gemini":
		// GenerateContent's proto3 counters omit zero values. Once a usage
		// object is present, missing counters default to zero; in particular,
		// a thinking-only response can omit candidatesTokenCount entirely.
		// google/ai/generativelanguage/v1beta/generative_service.proto: UsageMetadata
		tokens.InputTokens, err = sumUsageTokens("input", &values.GooglePrompt, &values.GoogleToolPrompt)
		if err != nil {
			return err
		}
		tokens.OutputTokens, err = sumUsageTokens("output", &values.GoogleCandidates, &values.GoogleThoughts)
		if err != nil {
			return err
		}
		tokens.CachedInputTokens, tokens.ReasoningTokens = new(values.GoogleCached), new(values.GoogleThoughts)
		tokens.TotalTokens = values.GoogleTotal
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
			tokens.InputTokens, err = sumUsageTokens("input", tokens.InputTokens, tokens.CachedInputTokens, tokens.CacheWriteInputTokens)
			if err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unsupported usage protocol %q", protocol)
	}
	if err := validateUsageTokens(tokens); err != nil {
		return err
	}
	if tokens.InputTokens != nil && tokens.OutputTokens != nil {
		tokens.TotalTokens, err = sumUsageTokens("total", tokens.InputTokens, tokens.OutputTokens)
		if err != nil {
			return err
		}
	}
	usage.fields = mergedFields
	usage.Snapshot.Tokens = tokens
	usage.raw = merged
	usage.Snapshot.Status = model.UsagePartial
	if final {
		usage.Snapshot.Status = model.UsageFinal
	}
	usage.Snapshot.Costs = nil
	if profile == model.ResponseProfileOpenRouter {
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

func sumUsageTokens(name string, counts ...*int) (*int, error) {
	total := 0
	for _, count := range counts {
		if count == nil {
			continue
		}
		if *count < 0 {
			return nil, fmt.Errorf("invalid usage: negative %s token count", name)
		}
		if *count > math.MaxInt-total {
			return nil, fmt.Errorf("invalid usage: %s token count overflows int", name)
		}
		total += *count
	}
	return &total, nil
}

func validateUsageTokens(tokens model.ReportedTokens) error {
	for _, counter := range []struct {
		name, parent string
		count, limit *int
	}{
		{"input", "", tokens.InputTokens, nil},
		{"output", "", tokens.OutputTokens, nil},
		{"total", "", tokens.TotalTokens, nil},
		{"cached input", "input", tokens.CachedInputTokens, tokens.InputTokens},
		{"cache write input", "input", tokens.CacheWriteInputTokens, tokens.InputTokens},
		{"reasoning", "output", tokens.ReasoningTokens, tokens.OutputTokens},
	} {
		if counter.count == nil {
			continue
		}
		if *counter.count < 0 {
			return fmt.Errorf("invalid usage: negative %s token count", counter.name)
		}
		if counter.limit != nil && *counter.count > *counter.limit {
			return fmt.Errorf("invalid usage: %s tokens exceed %s tokens", counter.name, counter.parent)
		}
	}
	return nil
}

// RawUsage returns an owned encoding of the fields used to normalize usage.
func (usage *UsageAccumulator) RawUsage() jsontext.Value {
	return bytes.Clone(usage.raw)
}
