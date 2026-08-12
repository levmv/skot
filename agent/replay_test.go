package agent

import (
	"fmt"
	"strings"
	"testing"
)

func TestReplayRequiresSupportedJournalSchemaVersion(t *testing.T) {
	for _, version := range []int{0, JournalSchemaVersion + 1} {
		t.Run(fmt.Sprintf("version_%d", version), func(t *testing.T) {
			records := []Record{recordForTest(t, 1, RecordSessionStarted, SessionStartedRecord{
				SchemaVersion: version,
				SessionID:     "session",
			})}
			_, err := Replay(records)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("unsupported journal schema version %d", version)) {
				t.Fatalf("Replay() error = %v", err)
			}
		})
	}
}

func TestReplayRemovesPendingToolCallsByIdentity(t *testing.T) {
	toolCall := func(id string) Item {
		return Item{
			Kind: ItemToolCall, ResponseID: "response",
			ToolCall: &ToolCall{ID: id, Name: "tool", RawArguments: `{}`},
		}
	}
	records := []Record{
		recordForTest(t, 1, RecordSessionStarted, SessionStartedRecord{SchemaVersion: JournalSchemaVersion, SessionID: "session"}),
		recordForTest(t, 2, RecordModelSelected, ModelSelectedRecord{Backend: "test", Model: "model", Epoch: "epoch"}),
		recordForTest(t, 3, RecordRunStarted, RunStartedRecord{RunID: "run"}),
		recordForTest(t, 4, RecordRunInputAdded, RunInputAddedRecord{RunID: "run", Text: "hello"}),
		recordForTest(t, 5, RecordModelResponse, ModelResponseRecord{
			RunID: "run", Backend: "test", Model: "model", Epoch: "epoch",
			Items: []Item{toolCall("first"), toolCall("middle"), toolCall("last")},
		}),
		recordForTest(t, 6, RecordToolResult, ToolResultRecord{RunID: "run", Result: ToolResult{CallID: "middle"}}),
		recordForTest(t, 7, RecordToolResult, ToolResultRecord{RunID: "run", Result: ToolResult{CallID: "first"}}),
	}
	state, err := Replay(records)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.PendingTools) != 1 || state.PendingTools[0].Call.ID != "last" {
		t.Fatalf("pending tools = %#v", state.PendingTools)
	}
}
