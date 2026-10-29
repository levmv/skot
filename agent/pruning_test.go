package agent

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	modelapi "github.com/levmv/skot/model"
)

func TestAutomaticToolResultPruningPreservesJournalAndAvoidsCompaction(t *testing.T) {
	journal := &memoryJournal{}
	largeResult := "BEGIN\n" + strings.Repeat("tool-output ", 16*1024) + "\nEND"
	tool := seedCompletedToolResultHistory(t, journal, largeResult, "inspect a large result")

	model := &scriptedModel{
		info: modelapi.Info{BackendID: "test", Provider: "test", Model: "test", ContextWindow: 32 * 1024},
		steps: []modelStep{func(_ context.Context, request modelapi.Request, _ func(modelapi.StreamEvent)) (modelapi.Response, error) {
			var result string
			for _, item := range request.Items {
				if item.Kind == modelapi.ItemToolResult && item.ToolResult != nil {
					result = item.ToolResult.Content.Text()
					break
				}
			}
			if !strings.HasPrefix(result, "BEGIN\n") || !strings.HasSuffix(result, "\nEND") || !strings.Contains(result, "bytes omitted from old tool result") {
				t.Fatalf("provider tool result = %q", result)
			}
			return modelapi.Response{Items: []modelapi.Item{{Kind: modelapi.ItemAssistantText, Text: "final answer"}}, StopReason: "stop"}, nil
		}},
	}
	runtime := newTestRuntime(t, Config{Backend: model, Journal: journal, Tools: []Tool{tool}})
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
		if item.Kind == modelapi.ItemToolResult && item.ToolResult != nil {
			rawResult = item.ToolResult.Content.Text()
			break
		}
	}
	if rawResult != largeResult {
		t.Fatal("raw journal projection was pruned")
	}
	var projectedResult string
	for _, item := range state.VerbatimItems() {
		if item.Kind == modelapi.ItemToolResult && item.ToolResult != nil {
			projectedResult = item.ToolResult.Content.Text()
			break
		}
	}
	if projectedResult == largeResult || !strings.Contains(projectedResult, "bytes omitted") {
		t.Fatalf("projected result = %q", projectedResult)
	}
}

func TestToolResultPruningRequiresACompactionSizedResult(t *testing.T) {
	before := ContextReport{Window: 1_000_000, InputLimit: 968_000, HistoryTokens: 900_000, TotalInputTokens: 970_000}
	stillLarge := ContextReport{Window: before.Window, InputLimit: before.InputLimit, HistoryTokens: 600_000, TotalInputTokens: 670_000}
	if toolResultPruningIsDecisive(before, stillLarge) {
		t.Fatal("large but still expensive projection qualified for pruning")
	}
	compactSized := ContextReport{Window: before.Window, InputLimit: before.InputLimit, HistoryTokens: 30_000, TotalInputTokens: 100_000}
	if !toolResultPruningIsDecisive(before, compactSized) {
		t.Fatal("tool-dominated projection did not qualify for pruning")
	}
}

func TestActiveRunToolPruningPreservesRecentExchanges(t *testing.T) {
	for _, trigger := range []string{"preflight", "provider_overflow"} {
		t.Run(trigger, func(t *testing.T) {
			journal := &memoryJournal{}
			largeResult := "BEGIN\n" + strings.Repeat("x", 40*1024) + "\nEND"
			recentResult := strings.Repeat("recent output ", 600)
			outputs := []string{largeResult, recentResult, recentResult, recentResult}
			executed := 0
			rejected := false
			var runtime *Runtime
			backend := modelFunc(func(_ context.Context, request modelapi.Request, _ func(modelapi.StreamEvent)) (modelapi.Response, error) {
				if isCompactionRequest(request) {
					t.Fatal("decisive pruning should avoid a summary call")
				}
				assertCompleteExchanges(t, request.Items)
				if !itemsContainText(request.Items, "Inspect four sections.") {
					t.Fatal("pruning lost the task")
				}
				var results []string
				for _, item := range request.Items {
					if item.Kind == modelapi.ItemToolResult {
						results = append(results, item.ToolResult.Content.Text())
					}
				}
				if len(results) != executed {
					t.Fatal("pruning dropped tool results")
				}
				for i, result := range results {
					if i > 0 && result != outputs[i] {
						t.Fatal("pruning changed an exchange in the recent tail")
					}
				}
				if executed < len(outputs) {
					return modelapi.Response{Items: []modelapi.Item{{Kind: modelapi.ItemToolCall, ToolCall: &modelapi.ToolCall{Name: "inspect", RawArguments: `{}`}}}}, nil
				}
				if trigger == "provider_overflow" && !rejected {
					rejected = true
					return modelapi.Response{}, &modelapi.ProviderError{
						Cause: modelapi.MarkProviderFailure(errors.New("payload too large")), Kind: modelapi.ProviderErrorRequestTooLarge,
					}
				}
				if !strings.HasPrefix(results[0], "BEGIN\n") || !strings.HasSuffix(results[0], "\nEND") || !strings.Contains(results[0], "bytes omitted") {
					t.Fatal("the older large tool result was not pruned")
				}
				return modelapi.Response{Items: []modelapi.Item{{Kind: modelapi.ItemAssistantText, Text: "done"}}, StopReason: "stop"}, nil
			})
			info := modelapi.Info{BackendID: "test", Provider: "test", Model: "test", ContextWindow: 64 * 1024}
			if trigger == "preflight" {
				info.ContextWindow = 24 * 1024
			}
			runtime = newTestRuntime(t, Config{Model: info, Backend: backend, Journal: journal, Tools: []Tool{{
				Spec: modelapi.ToolSpec{Name: "inspect", InputSchema: jsontext.Value(`{"type":"object"}`)},
				Run: func(context.Context, string) (ToolOutput, error) {
					executed++
					return ToolOutput{Content: modelapi.TextContent(outputs[executed-1])}, nil
				},
			}}})
			result, err := runtime.Run(t.Context(), "Inspect four sections.", func(event Event) {
				if event.Kind == EventToolResultsPruned {
					state, err := runtime.State(t.Context())
					if err != nil || len(state.ActiveRuns) != 1 || len(state.Blocks) != 1 {
						t.Fatalf("pruning did not happen inside the only run: %v", err)
					}
				}
			})
			if err != nil || result.Answer != "done" || executed != len(outputs) {
				t.Fatalf("run = %#v, error = %v, tool executions = %d", result, err, executed)
			}
			state, err := Replay(journal.snapshot())
			if err != nil || state.ToolPruningCount != 1 || state.CompactionCount != 0 || len(state.ActiveRuns) != 0 {
				t.Fatalf("replay: pruning %d, compactions %d, active %v, error %v", state.ToolPruningCount, state.CompactionCount, state.ActiveRuns, err)
			}
			var rawResults []string
			for _, item := range state.Items {
				if item.Kind == modelapi.ItemToolResult {
					rawResults = append(rawResults, item.ToolResult.Content.Text())
				}
			}
			if !slices.Equal(rawResults, outputs) {
				t.Fatal("pruning changed the journaled output")
			}
		})
	}
}

func TestRequestTooLargePrunesOldToolResultsBeforeCompaction(t *testing.T) {
	journal := &memoryJournal{}
	largeResult := "BEGIN\n" + strings.Repeat("tool-output ", 16*1024) + "\nEND"
	tool := seedCompletedToolResultHistory(t, journal, largeResult, "inspect a large result")

	attempts := 0
	model := &scriptedModel{
		info: modelapi.Info{BackendID: "test", Provider: "test", Model: "test"},
		steps: []modelStep{
			func(_ context.Context, request modelapi.Request, _ func(modelapi.StreamEvent)) (modelapi.Response, error) {
				attempts++
				for _, item := range request.Items {
					if item.Kind == modelapi.ItemToolResult && item.ToolResult != nil && item.ToolResult.Content.Text() == largeResult {
						return modelapi.Response{}, &modelapi.ProviderError{
							Cause: modelapi.MarkProviderFailure(errors.New("payload too large")), Kind: modelapi.ProviderErrorRequestTooLarge,
						}
					}
				}
				t.Fatalf("first request did not contain the full tool result: %#v", request.Items)
				return modelapi.Response{}, nil
			},
			func(_ context.Context, request modelapi.Request, _ func(modelapi.StreamEvent)) (modelapi.Response, error) {
				attempts++
				for _, item := range request.Items {
					if item.Kind == modelapi.ItemToolResult && item.ToolResult != nil && strings.Contains(item.ToolResult.Content.Text(), "bytes omitted from old tool result") {
						return modelapi.Response{Items: []modelapi.Item{{Kind: modelapi.ItemAssistantText, Text: "recovered"}}, StopReason: "stop"}, nil
					}
				}
				t.Fatalf("retry did not contain the pruned tool result: %#v", request.Items)
				return modelapi.Response{}, nil
			},
		},
	}
	var events []Event
	result, err := newTestRuntime(t, Config{Backend: model, Journal: journal, Tools: []Tool{tool}}).Run(context.Background(), "new question", func(event Event) {
		events = append(events, event)
	})
	if err != nil || result.Answer != "recovered" || attempts != 2 {
		t.Fatalf("result/error/attempts = %#v / %v / %d", result, err, attempts)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if state.ToolPruningCount != 1 || state.CompactionCount != 0 || !hasEvent(events, EventToolResultsPruned) || hasEvent(events, EventContextCompacted) {
		t.Fatalf("state/events = pruning %d, compactions %d / %#v", state.ToolPruningCount, state.CompactionCount, events)
	}
}

func TestFailedShrinkKeepsLiveStatusAlignedWithCommittedMaintenance(t *testing.T) {
	journal := &memoryJournal{}
	largeResult := strings.Repeat("tool-output ", 16*1024)
	tool := seedCompletedToolResultHistory(t, journal, largeResult, "inspect a large result")

	mainAttempts, summaryAttempts := 0, 0
	runtime := newTestRuntime(t, Config{
		Backend: modelFunc(func(_ context.Context, request modelapi.Request, _ func(modelapi.StreamEvent)) (modelapi.Response, error) {
			if isCompactionRequest(request) {
				summaryAttempts++
				t.Fatal("unexpected compaction request after decisive pruning")
			}
			mainAttempts++
			return modelapi.Response{}, &modelapi.ProviderError{
				Cause: modelapi.MarkProviderFailure(errors.New("payload too large")), Kind: modelapi.ProviderErrorRequestTooLarge,
			}
		}),
		Journal: journal,
		Tools:   []Tool{tool},
	})
	result, err := runtime.Run(context.Background(), "new question", nil)
	if result.Status != RunFailed || !errors.Is(err, modelapi.ErrModelRequestTooLarge) || !errors.Is(err, errCompactionNotNeeded) ||
		mainAttempts != 2 || summaryAttempts != 0 {
		t.Fatalf("result/error/attempts = %#v / %v / %d/%d", result, err, mainAttempts, summaryAttempts)
	}
	state, replayErr := Replay(journal.snapshot())
	if replayErr != nil {
		t.Fatal(replayErr)
	}
	if state.ToolPruningCount != 1 || state.CompactionCount != 0 {
		t.Fatalf("replayed maintenance = pruning %d, compactions %d", state.ToolPruningCount, state.CompactionCount)
	}
	if got, want := runtime.SessionStatus(), runtime.calculateSessionStatus(state); got != want {
		t.Fatalf("live status diverged from replay\ngot:  %#v\nwant: %#v", got, want)
	}
}

func seedCompletedToolResultHistory(t *testing.T, journal *memoryJournal, content, firstInput string) Tool {
	return seedCompletedToolContentHistory(t, journal, modelapi.TextContent(content), firstInput)
}

func seedCompletedToolContentHistory(t *testing.T, journal *memoryJournal, content modelapi.Content, firstInput string) Tool {
	t.Helper()
	tool := Tool{
		Spec: modelapi.ToolSpec{Name: "large_output", InputSchema: jsontext.Value(`{"type":"object"}`)},
		Run: func(context.Context, string) (ToolOutput, error) {
			return ToolOutput{Content: content}, nil
		},
	}
	seedModel := &scriptedModel{steps: []modelStep{
		func(context.Context, modelapi.Request, func(modelapi.StreamEvent)) (modelapi.Response, error) {
			return modelapi.Response{Items: []modelapi.Item{{Kind: modelapi.ItemToolCall, ToolCall: &modelapi.ToolCall{Name: "large_output", RawArguments: `{}`}}}}, nil
		},
		directModelResponse("old tool work complete"),
		directModelResponse("recent answer"),
	}}
	seedRuntime := newTestRuntime(t, Config{Backend: seedModel, Journal: journal, Tools: []Tool{tool}})
	if _, err := seedRuntime.Run(context.Background(), firstInput, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := seedRuntime.Run(context.Background(), "recent question", nil); err != nil {
		t.Fatal(err)
	}
	return tool
}

func TestImagePruningChangesOnlyTheModelProjection(t *testing.T) {
	journal := &memoryJournal{}
	beforeText := "BEGIN" + strings.Repeat("a", 100)
	afterText := strings.Repeat("b", 100) + "END"
	seedCompletedToolContentHistory(t, journal, modelapi.Content{
		{Kind: modelapi.ContentPartText, Text: beforeText},
		{Kind: modelapi.ContentPartImage, Image: &modelapi.ImageContent{MediaType: "image/png", Data: []byte{1, 2, 3}, Width: 10, Height: 5}},
		{Kind: modelapi.ContentPartText, Text: afterText},
	}, "inspect an image")
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, journal, RecordToolResultsPruned, ToolResultsPrunedRecord{
		ThroughSequence: state.Blocks[0].EndSequence,
		HeadBytes:       10,
		TailBytes:       10,
	})
	state, err = Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}

	var stored, projected modelapi.Content
	for _, item := range state.Items {
		if item.Kind == modelapi.ItemToolResult && item.ToolResult != nil {
			stored = item.ToolResult.Content
			break
		}
	}
	for _, item := range state.VerbatimItems() {
		if item.Kind == modelapi.ItemToolResult && item.ToolResult != nil {
			projected = item.ToolResult.Content
			break
		}
	}
	if !stored.HasImage() {
		t.Fatalf("journal image was pruned: %#v", stored)
	}
	projectedText := projected.Text()
	before := strings.Index(projectedText, "BEGIN")
	omitted := strings.Index(projectedText, "bytes omitted")
	imageMarker := strings.Index(projectedText, "image pruned")
	after := strings.Index(projectedText, "END")
	if projected.HasImage() || before < 0 || omitted <= before || imageMarker <= omitted || after <= imageMarker {
		t.Fatalf("image pruning projection = %#v", projected)
	}
}

func TestPruneToolResultKeepsUTF8HeadAndTail(t *testing.T) {
	content := "начало-" + strings.Repeat("界", 200) + "-конец"
	pruned := pruneToolResult(modelapi.TextContent(content), 11, 10).Text()
	if !strings.HasPrefix(pruned, "начал") || !strings.HasSuffix(pruned, "конец") || !strings.Contains(pruned, "bytes omitted") || !utf8.ValidString(pruned) {
		t.Fatalf("pruned content = %q", pruned)
	}
	if expanded := pruneToolResult(modelapi.TextContent(strings.Repeat("x", 101)), 50, 50).Text(); len(expanded) != 101 {
		t.Fatalf("small result expanded to %d bytes", len(expanded))
	}
	moderate := "HEAD" + strings.Repeat("x", 6*1024) + "TAIL"
	moderatePruned := pruneToolResult(modelapi.TextContent(moderate), defaultPrunedToolHeadBytes, defaultPrunedToolTailBytes).Text()
	if len(moderatePruned) >= len(moderate) || !strings.HasPrefix(moderatePruned, "HEAD") || !strings.HasSuffix(moderatePruned, "TAIL") {
		t.Fatalf("moderate result was not usefully pruned: %d -> %d bytes", len(moderate), len(moderatePruned))
	}
}

func TestReplayRejectsToolPruningInsideUnansweredExchange(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{directModelResponse("old answer"), directModelResponse("recent answer")}}
	runtime := newTestRuntime(t, Config{Backend: model, Journal: journal})
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
	if _, err := Replay(journal.snapshot()); err == nil || !strings.Contains(err.Error(), "completed older exchange") {
		t.Fatalf("invalid pruning replay error = %v", err)
	}
}

func TestToolPruningCannotAdvanceIntoUnansweredInput(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{directModelResponse("old answer"), directModelResponse("recent answer")}}
	runtime := newTestRuntime(t, Config{Backend: model, Journal: journal})
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
	if err := validateToolPruningBoundary(state, safe, state.LastSequence+1); err != nil {
		t.Fatalf("completed-prefix pruning: %v", err)
	}
	unsafe := ToolResultsPrunedRecord{ThroughSequence: state.Blocks[2].EndSequence, HeadBytes: 10, TailBytes: 10}
	if err := validateToolPruningBoundary(state, unsafe, state.LastSequence+1); err == nil || !strings.Contains(err.Error(), "completed older exchange") {
		t.Fatalf("unanswered input pruning error = %v", err)
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
