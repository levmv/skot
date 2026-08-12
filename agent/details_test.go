package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeDetailsClonesValidatedJSON(t *testing.T) {
	data := json.RawMessage(`{"path":"file.go"}`)
	details, err := normalizeDetails([]Detail{{Kind: " file_change ", Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	data[2] = 'X'
	if len(details) != 1 || details[0].Kind != "file_change" || string(details[0].Data) != `{"path":"file.go"}` {
		t.Fatalf("details = %#v", details)
	}
}

func TestNormalizeDetailsRejectsInvalidAndOversizedData(t *testing.T) {
	if _, err := normalizeDetails([]Detail{{Kind: "broken", Data: json.RawMessage(`{`)}}); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
	oversized := json.RawMessage(`"` + strings.Repeat("x", maxToolDetailsBytes) + `"`)
	if _, err := normalizeDetails([]Detail{{Kind: "large", Data: oversized}}); err == nil {
		t.Fatal("oversized detail was accepted")
	}
}

func TestReplayRejectsInvalidJournaledDetail(t *testing.T) {
	records := []Record{
		recordForTest(t, 1, RecordSessionStarted, SessionStartedRecord{SchemaVersion: JournalSchemaVersion, SessionID: "session"}),
		recordForTest(t, 2, RecordModelSelected, ModelSelectedRecord{Backend: "test", Model: "model", Epoch: "epoch"}),
		recordForTest(t, 3, RecordRunStarted, RunStartedRecord{RunID: "run"}),
		recordForTest(t, 4, RecordRunInputAdded, RunInputAddedRecord{RunID: "run", Text: "hello"}),
		recordForTest(t, 5, RecordModelResponse, ModelResponseRecord{
			RunID: "run", Backend: "test", Model: "model", Epoch: "epoch",
			Items: []Item{{Kind: ItemToolCall, ResponseID: "response", ToolCall: &ToolCall{ID: "call", Name: "tool", RawArguments: `{}`}}},
		}),
		recordForTest(t, 6, RecordToolResult, ToolResultRecord{RunID: "run", Result: ToolResult{
			CallID: "call", Details: []Detail{{Kind: " ", Data: json.RawMessage(`{}`)}},
		}}),
	}
	if _, err := Replay(records); err == nil || !strings.Contains(err.Error(), "invalid tool result") {
		t.Fatalf("Replay() error = %v", err)
	}
}

func TestVerbatimModelItemsSkipDetailsWithoutAliasingState(t *testing.T) {
	state := State{Blocks: []ConversationBlock{
		{Entries: []ConversationEntry{
			{Item: Item{
				Kind: ItemToolResult,
				ToolResult: &ToolResult{
					CallID: "call", Content: "result",
					Details: []Detail{{Kind: "inspection", Data: json.RawMessage(`{"value":1}`)}},
				},
			}},
		}},
	}}
	items := state.verbatimModelItems()
	if len(items) != 1 || items[0].ToolResult == nil || len(items[0].ToolResult.Details) != 0 {
		t.Fatalf("model items = %#v", items)
	}
	items[0].ToolResult.Content = "mutated"
	if state.Blocks[0].Entries[0].Item.ToolResult.Content != "result" {
		t.Fatal("model projection aliases replay state")
	}
	productItems := state.VerbatimItems()
	if len(productItems[0].ToolResult.Details) != 1 {
		t.Fatalf("product details were discarded: %#v", productItems)
	}
}

func recordForTest(t *testing.T, sequence uint64, kind RecordKind, payload any) Record {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return Record{Sequence: sequence, Kind: kind, Data: data}
}
