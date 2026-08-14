package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAutomaticToolResultPruningPreservesJournalAndAvoidsCompaction(t *testing.T) {
	journal := &memoryJournal{}
	largeResult := "BEGIN\n" + strings.Repeat("tool-output ", 16*1024) + "\nEND"
	tool := Tool{
		Spec: ToolSpec{Name: "large_output", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, string) (ToolOutput, error) {
			return ToolOutput{Content: largeResult}, nil
		},
	}
	seedModel := &scriptedModel{steps: []modelStep{
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "large_output", RawArguments: `{}`}}}}, nil
		},
		directModelResponse("old tool work complete"),
		directModelResponse("recent answer"),
	}}
	seedRuntime := newTestRuntime(t, Config{Model: seedModel, Journal: journal, Tools: []Tool{tool}})
	if _, err := seedRuntime.Run(context.Background(), "inspect a large result", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := seedRuntime.Run(context.Background(), "recent question", nil); err != nil {
		t.Fatal(err)
	}

	model := &scriptedModel{
		info: ModelInfo{Backend: "test", Provider: "test", Model: "test", ContextWindow: 32 * 1024},
		steps: []modelStep{func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			var result string
			for _, item := range request.Items {
				if item.Kind == ItemToolResult && item.ToolResult != nil {
					result = item.ToolResult.Content
					break
				}
			}
			if !strings.HasPrefix(result, "BEGIN\n") || !strings.HasSuffix(result, "\nEND") || !strings.Contains(result, "bytes omitted from old tool result") {
				t.Fatalf("provider tool result = %q", result)
			}
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "final answer"}}, StopReason: "stop"}, nil
		}},
	}
	runtime := newTestRuntime(t, Config{Model: model, Journal: journal, Tools: []Tool{tool}})
	var events []Event
	if _, err := runtime.Run(context.Background(), "new question", func(event Event) {
		assertEventCommittedAtEmission(t, journal, event)
		events = append(events, event)
	}); err != nil {
		t.Fatal(err)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if state.ToolPruning == nil || state.ToolPruningCount != 1 || state.Compaction != nil {
		t.Fatalf("maintenance state: pruning=%#v count=%d compaction=%#v", state.ToolPruning, state.ToolPruningCount, state.Compaction)
	}
	if hasStatusEvent(events, "pruned old tool results") || !hasEvent(events, EventToolResultsPruned) {
		t.Fatalf("events = %#v", events)
	}
	assertAuthoritativeEvents(t, events, journal.snapshot())
	var rawResult string
	for _, item := range state.Items {
		if item.Kind == ItemToolResult && item.ToolResult != nil {
			rawResult = item.ToolResult.Content
			break
		}
	}
	if rawResult != largeResult {
		t.Fatal("raw journal projection was pruned")
	}
	var projectedResult string
	for _, item := range state.VerbatimItems() {
		if item.Kind == ItemToolResult && item.ToolResult != nil {
			projectedResult = item.ToolResult.Content
			break
		}
	}
	if projectedResult == largeResult || !strings.Contains(projectedResult, "bytes omitted") {
		t.Fatalf("projected result = %q", projectedResult)
	}
}

func TestInsufficientToolPruningFallsThroughToCompaction(t *testing.T) {
	journal := &memoryJournal{}
	largeResult := strings.Repeat("large tool output ", 8*1024)
	tool := Tool{
		Spec: ToolSpec{Name: "large_output", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run:  func(context.Context, string) (ToolOutput, error) { return ToolOutput{Content: largeResult}, nil },
	}
	seedModel := &scriptedModel{steps: []modelStep{
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "large_output", RawArguments: `{}`}}}}, nil
		},
		directModelResponse("old answer"),
		directModelResponse("recent answer"),
	}}
	seedRuntime := newTestRuntime(t, Config{Model: seedModel, Journal: journal, Tools: []Tool{tool}})
	if _, err := seedRuntime.Run(context.Background(), strings.Repeat("large user context ", 4*1024), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := seedRuntime.Run(context.Background(), "recent question", nil); err != nil {
		t.Fatal(err)
	}

	model := &scriptedModel{
		info: ModelInfo{Backend: "test", Provider: "test", Model: "test", ContextWindow: 16 * 1024},
		steps: []modelStep{
			func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
				if request.Instructions != compactionSystemInstructions {
					t.Fatalf("first request was not compaction: %#v", request)
				}
				return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "compacted old work"}}, StopReason: "stop"}, nil
			},
			directModelResponse("final answer"),
		},
	}
	if _, err := newTestRuntime(t, Config{Model: model, Journal: journal, Tools: []Tool{tool}}).Run(context.Background(), "new question", nil); err != nil {
		t.Fatal(err)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if state.ToolPruning != nil || state.CompactionCount != 1 {
		t.Fatalf("maintenance state: pruning=%#v compactions=%d", state.ToolPruning, state.CompactionCount)
	}
}

func TestPruneToolResultKeepsUTF8HeadAndTail(t *testing.T) {
	content := "начало-" + strings.Repeat("界", 200) + "-конец"
	pruned := pruneToolResult(content, 11, 10)
	if !strings.HasPrefix(pruned, "начал") || !strings.HasSuffix(pruned, "конец") || !strings.Contains(pruned, "bytes omitted") || !utf8.ValidString(pruned) {
		t.Fatalf("pruned content = %q", pruned)
	}
	if expanded := pruneToolResult(strings.Repeat("x", 101), 50, 50); len(expanded) != 101 {
		t.Fatalf("small result expanded to %d bytes", len(expanded))
	}
	moderate := "HEAD" + strings.Repeat("x", 6*1024) + "TAIL"
	moderatePruned := pruneToolResult(moderate, defaultPrunedToolHeadBytes, defaultPrunedToolTailBytes)
	if len(moderatePruned) >= len(moderate) || !strings.HasPrefix(moderatePruned, "HEAD") || !strings.HasSuffix(moderatePruned, "TAIL") {
		t.Fatalf("moderate result was not usefully pruned: %d -> %d bytes", len(moderate), len(moderatePruned))
	}
}

func TestReplayRejectsToolPruningInsideConversationBlock(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{directModelResponse("old answer"), directModelResponse("recent answer")}}
	runtime := newTestRuntime(t, Config{Model: model, Journal: journal})
	if _, err := runtime.Run(context.Background(), "old question", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "recent question", nil); err != nil {
		t.Fatal(err)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, journal, RecordToolResultsPruned, ToolResultsPrunedRecord{
		ThroughSequence: state.Blocks[0].StartSequence,
		HeadBytes:       10,
		TailBytes:       10,
	})
	if _, err := Replay(journal.snapshot()); err == nil || !strings.Contains(err.Error(), "conversation-block boundary") {
		t.Fatalf("invalid pruning replay error = %v", err)
	}
}

func TestToolPruningCannotAdvanceIntoActiveRun(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{directModelResponse("old answer"), directModelResponse("recent answer")}}
	runtime := newTestRuntime(t, Config{Model: model, Journal: journal})
	if _, err := runtime.Run(context.Background(), "old question", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "recent question", nil); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, journal, RecordRunStarted, RunStartedRecord{RunID: "run-active"})
	mustAppend(t, journal, RecordRunInputAdded, RunInputAddedRecord{RunID: "run-active", Text: "active input"})
	mustAppend(t, journal, RecordRunInputAdded, RunInputAddedRecord{RunID: "run-active", Text: "active steering"})
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	safe := ToolResultsPrunedRecord{ThroughSequence: state.Blocks[1].EndSequence, HeadBytes: 10, TailBytes: 10}
	if err := validateToolPruningBoundary(state, safe, state.LastSequence+1, true); err != nil {
		t.Fatalf("completed-prefix pruning: %v", err)
	}
	unsafe := ToolResultsPrunedRecord{ThroughSequence: state.Blocks[2].EndSequence, HeadBytes: 10, TailBytes: 10}
	if err := validateToolPruningBoundary(state, unsafe, state.LastSequence+1, true); err == nil || !strings.Contains(err.Error(), "unfinished") {
		t.Fatalf("active block pruning error = %v", err)
	}
}

func hasStatusEvent(events []Event, text string) bool {
	for _, event := range events {
		if event.Kind == EventStatus && event.Text == text {
			return true
		}
	}
	return false
}
