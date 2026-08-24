package agent

import (
	"context"
	"encoding/json/jsontext"
	"strings"
	"testing"
)

func TestNormalizeProviderDataClonesValidatedJSON(t *testing.T) {
	data := jsontext.Value(`{"id":"rs_1","encrypted_content":"opaque-secret"}`)
	entries, err := normalizeProviderData([]ProviderData{{Kind: " responses.reasoning_item ", Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	data[2] = 'X'
	if len(entries) != 1 || entries[0].Kind != "responses.reasoning_item" || string(entries[0].Data) != `{"id":"rs_1","encrypted_content":"opaque-secret"}` {
		t.Fatalf("provider data = %#v", entries)
	}
}

func TestNormalizeProviderDataRejectsInvalidDuplicateAndOversizedData(t *testing.T) {
	for _, entries := range [][]ProviderData{
		{{Kind: "", Data: jsontext.Value(`{}`)}},
		{{Kind: "broken", Data: jsontext.Value(`{`)}},
		{{Kind: "same", Data: jsontext.Value(`1`)}, {Kind: "same", Data: jsontext.Value(`2`)}},
		{{Kind: "large", Data: jsontext.Value(`"` + strings.Repeat("x", maxProviderDataBytes) + `"`)}},
	} {
		if _, err := normalizeProviderData(entries); err == nil {
			t.Fatalf("invalid provider data was accepted: %#v", entries)
		}
	}
}

func TestProviderDataIsClonedButNotSanitized(t *testing.T) {
	runtime := &Runtime{sanitize: func(value string) string { return strings.ReplaceAll(value, "secret", "[redacted]") }}
	original := []Item{{
		Kind: ItemReasoning, Text: "visible secret",
		ProviderData: []ProviderData{{Kind: "responses.reasoning_item", Data: jsontext.Value(`{"encrypted_content":"secret"}`)}},
	}}
	sanitized := runtime.sanitizeItems(original)
	if sanitized[0].Text != "visible [redacted]" || string(sanitized[0].ProviderData[0].Data) != `{"encrypted_content":"secret"}` {
		t.Fatalf("sanitized item = %#v", sanitized[0])
	}
	sanitized[0].ProviderData[0].Data[2] = 'X'
	if string(original[0].ProviderData[0].Data) != `{"encrypted_content":"secret"}` {
		t.Fatal("sanitized provider data aliases its source")
	}
}

func TestAcceptedReasoningOwnsNormalizedProviderData(t *testing.T) {
	data := jsontext.Value(`{"encrypted_content":"ciphertext"}`)
	accepted, err := acceptResponse(ModelResponse{Items: []Item{{
		Kind: ItemReasoning, Text: "summary",
		ProviderData: []ProviderData{{Kind: " responses.reasoning_item ", Data: data}},
	}}}, ProviderContext{Backend: "responses.openai", Epoch: "epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	data[2] = 'X'
	item := accepted.Items[0]
	if item.ProviderContext == nil || item.ProviderContext.Backend != "responses.openai" || item.ProviderContext.Epoch != "epoch_1" ||
		len(item.ProviderData) != 1 || item.ProviderData[0].Kind != "responses.reasoning_item" || string(item.ProviderData[0].Data) != `{"encrypted_content":"ciphertext"}` {
		t.Fatalf("accepted reasoning = %#v", item)
	}
}

func TestReplayRejectsInvalidReasoningProviderData(t *testing.T) {
	records := []Record{
		recordForTest(t, 1, RecordSessionStarted, SessionStartedRecord{SchemaVersion: JournalSchemaVersion, SessionID: "session"}),
		recordForTest(t, 2, RecordModelSelected, ModelSelectedRecord{Backend: "responses.openai", Provider: "openai", Model: "model", Epoch: "epoch"}),
		recordForTest(t, 3, RecordRunStarted, RunStartedRecord{RunID: "run"}),
		recordForTest(t, 4, RecordRunInputAdded, RunInputAddedRecord{RunID: "run", Text: "hello"}),
		recordForTest(t, 5, RecordModelResponse, ModelResponseRecord{
			RunID: "run", Backend: "responses.openai", Model: "model", Epoch: "epoch",
			Items: []Item{{
				Kind: ItemReasoning, ResponseID: "response", ProviderContext: &ProviderContext{Backend: "responses.openai", Epoch: "epoch"},
				ProviderData: []ProviderData{{Kind: " responses.reasoning_item ", Data: jsontext.Value(`{}`)}},
			}},
		}),
	}
	if _, err := Replay(records); err == nil || !strings.Contains(err.Error(), "provider data") {
		t.Fatalf("Replay() error = %v", err)
	}
}

func TestOpaqueProviderDataDoesNotAffectTokenEstimate(t *testing.T) {
	without := estimateItemsTokens([]Item{{Kind: ItemReasoning, Text: "summary"}})
	with := estimateItemsTokens([]Item{{
		Kind: ItemReasoning, Text: "summary",
		ProviderData: []ProviderData{{Kind: "responses.reasoning_item", Data: jsontext.Value(`{"encrypted_content":"` + strings.Repeat("x", 4096) + `"}`)}},
	}})
	if with != without {
		t.Fatalf("opaque bytes changed token estimate: %d != %d", with, without)
	}
}

func TestRuntimeJournalsAndReplaysProviderDataAcrossToolTurn(t *testing.T) {
	journal := &memoryJournal{}
	state := jsontext.Value(`{"id":"rs_1","encrypted_content":"ciphertext"}`)
	model := &scriptedModel{
		info: ModelInfo{
			BackendID: "responses.test", Provider: "test", Model: "model",
			ProviderStateContract: "responses.manual_history.v1",
		},
		steps: []modelStep{
			func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
				return ModelResponse{Items: []Item{
					{Kind: ItemReasoning, Text: "safe summary", ProviderData: []ProviderData{{Kind: "responses.reasoning_item", Data: state}}},
					{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "inspect", RawArguments: `{}`}},
				}}, nil
			},
			func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
				var reasoning *Item
				for index := range request.Items {
					if request.Items[index].Kind == ItemReasoning {
						reasoning = &request.Items[index]
					}
				}
				if reasoning == nil || reasoning.ProviderContext == nil ||
					reasoning.ProviderContext.Backend != "responses.test" || reasoning.ProviderContext.Epoch != request.ProviderEpoch ||
					len(reasoning.ProviderData) != 1 || string(reasoning.ProviderData[0].Data) != string(state) {
					t.Fatalf("replayed reasoning = %#v, request epoch = %q", reasoning, request.ProviderEpoch)
				}
				return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "done"}}}, nil
			},
		},
	}
	runtime := newTestRuntime(t, Config{
		Backend: model, Journal: journal,
		Tools: []Tool{{
			Spec: ToolSpec{Name: "inspect", InputSchema: jsontext.Value(`{"type":"object"}`)},
			Run:  func(context.Context, string) (ToolOutput, error) { return ToolOutput{Content: TextContent("ok")}, nil },
		}},
	})
	if _, err := runtime.Run(context.Background(), "inspect", nil); err != nil {
		t.Fatal(err)
	}
	replayed, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var persisted *Item
	for index := range replayed.Items {
		if replayed.Items[index].Kind == ItemReasoning {
			persisted = &replayed.Items[index]
		}
	}
	if persisted == nil || len(persisted.ProviderData) != 1 || string(persisted.ProviderData[0].Data) != string(state) {
		t.Fatalf("persisted reasoning = %#v", persisted)
	}
}
