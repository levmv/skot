package modelhttp

import (
	"bytes"
	"encoding/json/jsontext"
	"fmt"
	"math"
	"reflect"
	"testing"

	"github.com/levmv/skot/model"
)

func TestUsageCounterValidation(t *testing.T) {
	for _, test := range []struct {
		name, protocol, raw string
		valid               bool
	}{
		{"negative input component", "gemini", `{"promptTokenCount":10,"toolUsePromptTokenCount":-5}`, false},
		{"negative output component", "gemini", `{"candidatesTokenCount":10,"thoughtsTokenCount":-5}`, false},
		{"negative uncached input", "anthropic_messages", `{"input_tokens":-1,"cache_read_input_tokens":10}`, false},
		{"negative output", "chat_completions", `{"completion_tokens":-1}`, false},
		{"negative cache", "responses", `{"input_tokens_details":{"cached_tokens":-1}}`, false},
		{"negative total", "gemini", `{"promptTokenCount":10,"totalTokenCount":-1}`, false},
		{"cache exceeds input", "chat_completions", `{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":11}}`, false},
		{"cache write exceeds input", "responses", `{"input_tokens":10,"input_tokens_details":{"cache_write_tokens":11}}`, false},
		{"reasoning exceeds output", "responses", `{"output_tokens":10,"output_tokens_details":{"reasoning_tokens":11}}`, false},
		{"input overflow", "gemini", fmt.Sprintf(`{"promptTokenCount":%d,"toolUsePromptTokenCount":1}`, math.MaxInt), false},
		{"output overflow", "gemini", fmt.Sprintf(`{"candidatesTokenCount":%d,"thoughtsTokenCount":1}`, math.MaxInt), false},
		{"cache addition overflow", "anthropic_messages", fmt.Sprintf(`{"input_tokens":%d,"cache_creation_input_tokens":1}`, math.MaxInt), false},
		{"total overflow", "chat_completions", fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":1}`, math.MaxInt), false},
		{"largest valid total", "anthropic_messages", fmt.Sprintf(`{"input_tokens":%d,"cache_creation_input_tokens":1,"output_tokens":0}`, math.MaxInt-1), true},
		{"unknown input", "responses", `{"input_tokens_details":{"cached_tokens":4,"cache_write_tokens":2}}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var usage UsageAccumulator
			if err := usage.Observe(jsontext.Value(test.raw), test.protocol, "", true); (err == nil) != test.valid {
				t.Fatalf("usage %s: error = %v, want valid = %t", test.raw, err, test.valid)
			}
			if test.valid {
				return
			}
			var response model.Response
			usage.Attach(&response)
			if response.Usage.Status != model.UsageUnavailable || usage.RawUsage() != nil {
				t.Fatalf("invalid usage became available: %+v", response.Usage)
			}
		})
	}
}

func TestUsagePreservesLastValidSnapshot(t *testing.T) {
	var usage UsageAccumulator
	if err := usage.Observe(jsontext.Value(`{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4},"cost":0.5,"extra":"valid"}`), "chat_completions", "openrouter", false); err != nil {
		t.Fatal(err)
	}
	snapshot, raw := usage.Snapshot, usage.RawUsage()
	for _, invalid := range []string{
		`{"prompt_tokens":"invalid","cost":99,"extra":"invalid"}`,
		`{"prompt_tokens":3,"completion_tokens":9,"cost":99,"extra":"invalid"}`,
	} {
		if err := usage.Observe(jsontext.Value(invalid), "chat_completions", "openrouter", true); err == nil {
			t.Fatalf("accepted invalid usage: %s", invalid)
		}
		if !reflect.DeepEqual(usage.Snapshot, snapshot) || !bytes.Equal(usage.RawUsage(), raw) {
			t.Fatalf("invalid event replaced last valid usage: %+v, %s", usage.Snapshot, usage.RawUsage())
		}
	}
	if err := usage.Observe(jsontext.Value(`{"completion_tokens":5}`), "chat_completions", "openrouter", true); err != nil {
		t.Fatal(err)
	}
	want := model.TokenCounts{InputTokens: 10, OutputTokens: 5, CachedInputTokens: 4, TotalTokens: 15}
	if usage.Snapshot.Status != model.UsageFinal || usage.Snapshot.Tokens.Known() != want || !reflect.DeepEqual(usage.Snapshot.Costs, snapshot.Costs) || !bytes.Contains(usage.RawUsage(), []byte(`"extra":"valid"`)) {
		t.Fatalf("invalid event contaminated later usage: %+v, %s", usage.Snapshot, usage.RawUsage())
	}
}
