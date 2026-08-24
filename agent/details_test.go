package agent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
)

func TestNormalizeDetailsClonesValidatedJSON(t *testing.T) {
	data := jsontext.Value(`{"path":"file.go"}`)
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
	if _, err := normalizeDetails([]Detail{{Kind: "broken", Data: jsontext.Value(`{`)}}); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
	oversized := jsontext.Value(`"` + strings.Repeat("x", maxToolDetailsBytes) + `"`)
	if _, err := normalizeDetails([]Detail{{Kind: "large", Data: oversized}}); err == nil {
		t.Fatal("oversized detail was accepted")
	}
}

func TestStructuredDetailVocabularyRoundTrips(t *testing.T) {
	changeDetail, err := NewDetail(FileChangeDetailKind, FileChange{
		Type: FileChangeDetailKind, Path: "note.txt", Operation: "edited", Additions: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	change, ok := FileChangeFromDetail(changeDetail)
	if !ok || change.Path != "note.txt" || change.Operation != "edited" || change.Additions != 1 {
		t.Fatalf("file change = %#v, ok=%v", change, ok)
	}

	processDetail, err := NewDetail(ProcessResultDetailKind, ProcessResult{
		JobID: "job-1", Status: ProcessCompleted, OutputBytes: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	process, ok := ProcessResultFromDetail(processDetail)
	if !ok || process.JobID != "job-1" || process.Status != ProcessCompleted || process.OutputBytes != 12 {
		t.Fatalf("process result = %#v, ok=%v", process, ok)
	}

	if _, err := NewDetail("unsupported", make(chan struct{})); err == nil {
		t.Fatal("unsupported detail payload was accepted")
	}
}

func TestReplayRejectsInvalidJournaledDetail(t *testing.T) {
	records := []Record{
		recordForTest(t, 1, RecordSessionStarted, SessionStartedRecord{SchemaVersion: JournalSchemaVersion, SessionID: "session"}),
		recordForTest(t, 2, RecordModelSelected, ModelSelectedRecord{Backend: "test", Provider: "test", Model: "model", Epoch: "epoch"}),
		recordForTest(t, 3, RecordRunStarted, RunStartedRecord{RunID: "run"}),
		recordForTest(t, 4, RecordRunInputAdded, RunInputAddedRecord{RunID: "run", Text: "hello"}),
		recordForTest(t, 5, RecordModelResponse, ModelResponseRecord{
			RunID: "run", Backend: "test", Model: "model", Epoch: "epoch",
			Items: []Item{{Kind: ItemToolCall, ResponseID: "response", ToolCall: &ToolCall{ID: "call", Name: "tool", RawArguments: `{}`}}},
		}),
		recordForTest(t, 6, RecordToolResult, ToolResultRecord{RunID: "run", Result: ToolResult{
			CallID: "call", Details: []Detail{{Kind: " ", Data: jsontext.Value(`{}`)}},
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
					CallID: "call", Content: TextContent("result"),
					Details: []Detail{{Kind: "inspection", Data: jsontext.Value(`{"value":1}`)}},
				},
			}},
		}},
	}}
	items := state.verbatimModelItems()
	if len(items) != 1 || items[0].ToolResult == nil || len(items[0].ToolResult.Details) != 0 {
		t.Fatalf("model items = %#v", items)
	}
	items[0].ToolResult.Content = TextContent("mutated")
	if state.Blocks[0].Entries[0].Item.ToolResult.Content.Text() != "result" {
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
