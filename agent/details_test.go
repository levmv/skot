package agent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/levmv/skot/model"
)

func TestNormalizeDetailsClonesValidatedJSON(t *testing.T) {
	data := jsontext.Value(`{"path":"file.go"}`)
	details, err := normalizeDetails([]model.Detail{{Kind: " file_change ", Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	data[2] = 'X'
	if len(details) != 1 || details[0].Kind != "file_change" || string(details[0].Data) != `{"path":"file.go"}` {
		t.Fatalf("details = %#v", details)
	}
}

func TestNormalizeDetailsRejectsInvalidAndOversizedData(t *testing.T) {
	if _, err := normalizeDetails([]model.Detail{{Kind: "kind-\xff", Data: jsontext.Value(`{}`)}}); err == nil {
		t.Fatal("invalid UTF-8 kind was accepted")
	}
	if _, err := normalizeDetails([]model.Detail{{Kind: "broken", Data: jsontext.Value(`{`)}}); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
	oversized := jsontext.Value(`"` + strings.Repeat("x", maxDetailsBytes) + `"`)
	if _, err := normalizeDetails([]model.Detail{{Kind: "large", Data: oversized}}); err == nil {
		t.Fatal("oversized detail was accepted")
	}
}

func TestReplayRejectsInvalidJournaledDetail(t *testing.T) {
	records := []Record{
		recordForTest(t, 1, RecordSessionStarted, SessionStartedRecord{SchemaVersion: JournalSchemaVersion, SessionID: "session"}),
		recordForTest(t, 2, RecordModelSelected, model.ReplayContext{Backend: "test", Provider: "test", Model: "model", Epoch: "epoch"}),
		recordForTest(t, 3, RecordRunStarted, RunStartedRecord{RunID: "run"}),
		recordForTest(t, 4, RecordRunInputAdded, RunInputAddedRecord{RunID: "run", Text: "hello"}),
		recordForTest(t, 5, RecordModelResponse, ModelResponseRecord{
			RunID: "run", Backend: "test", Model: "model", Epoch: "epoch",
			Items: []model.Item{{Kind: model.ItemToolCall, ResponseID: "response", ToolCall: &model.ToolCall{ID: "call", Name: "tool", RawArguments: `{}`}}},
		}),
		recordForTest(t, 6, RecordToolResult, ToolResultRecord{RunID: "run", Result: model.ToolResult{
			CallID: "call", Details: []model.Detail{{Kind: " ", Data: jsontext.Value(`{}`)}},
		}}),
	}
	if _, err := Replay(records); err == nil || !strings.Contains(err.Error(), "invalid tool result") {
		t.Fatalf("Replay() error = %v", err)
	}
}

func TestVerbatimModelItemsSkipDetailsWithoutAliasingState(t *testing.T) {
	state := State{Blocks: []ConversationBlock{
		{Entries: []ConversationEntry{
			{Item: model.Item{
				Kind: model.ItemToolResult,
				ToolResult: &model.ToolResult{
					CallID: "call", Content: model.TextContent("result"),
					Details: []model.Detail{{Kind: "inspection", Data: jsontext.Value(`{"value":1}`)}},
				},
			}},
			{Item: model.Item{Kind: model.ItemBoundaryText, Text: "job completed", Details: []model.Detail{{Kind: "inspection", Data: jsontext.Value(`{"value":2}`)}}}},
		}},
	}}
	items := state.verbatimModelItems()
	if len(items) != 2 || items[0].ToolResult == nil || len(items[0].ToolResult.Details) != 0 || len(items[1].Details) != 0 {
		t.Fatalf("model items = %#v", items)
	}
	items[0].ToolResult.Content = model.TextContent("mutated")
	if state.Blocks[0].Entries[0].Item.ToolResult.Content.Text() != "result" {
		t.Fatal("model projection aliases replay state")
	}
	productItems := state.VerbatimItems()
	if len(productItems[0].ToolResult.Details) != 1 || len(productItems[1].Details) != 1 {
		t.Fatalf("product details were discarded: %#v", productItems)
	}
	productItems[1].Details[0].Data[9] = '3'
	if string(state.Blocks[0].Entries[1].Item.Details[0].Data) != `{"value":2}` {
		t.Fatal("product projection aliases boundary details")
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
