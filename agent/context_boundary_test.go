package agent

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/levmv/skot/model"
)

func TestActiveRunCompactionPreservesTaskAndCompleteExchanges(t *testing.T) {
	journal := &memoryJournal{}
	const task = "Inspect six pairs, preserve the public API, and report the results."
	const steering = "Use the revised procedure for the remaining pairs."
	const rounds = 6
	var runtime *Runtime
	mainCalls, summaries, tools, loads := 0, 0, 0, 0
	backend := modelFunc(func(_ context.Context, request model.Request, _ func(model.StreamEvent)) (model.Response, error) {
		if want := fmt.Sprintf("system instructions %d", summaries); request.Instructions != want || loads != summaries+1 {
			t.Fatalf("instructions=%q, loads=%d; want %q after %d compactions", request.Instructions, loads, want, summaries)
		}
		assertCompleteExchanges(t, request.Items)
		state, err := runtime.State(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		assertWholeResponses(t, state.Items, request.Items)
		if isCompactionRequest(request) {
			if len(state.ActiveRuns) != 1 || len(state.PendingTools) != 0 {
				t.Fatalf("summary outside an active model boundary: %v, %v", state.ActiveRuns, err)
			}
			summaries++
			return model.Response{Items: []model.Item{{Kind: model.ItemAssistantText, Text: fmt.Sprintf("Completed %d pairs; continue inspecting.", tools/2)}}, StopReason: "stop"}, nil
		}
		if !itemsContainText(request.Items, task) {
			t.Fatal("lost the authored task")
		}
		if summaries > 0 && (!itemsContainText(request.Items, steering) || !itemsContainText(request.Items, "current procedure") || itemsContainText(request.Items, "obsolete procedure")) {
			t.Fatal("lost steering, lost current instructions, or resurrected obsolete instructions")
		}
		mainCalls++
		if mainCalls > rounds {
			return model.Response{Items: []model.Item{{Kind: model.ItemAssistantText, Text: "All pairs inspected."}}, StopReason: "stop"}, nil
		}
		return model.Response{Items: []model.Item{
			{Kind: model.ItemReasoning, Text: strings.Repeat("r", 12*1024)},
			{Kind: model.ItemAssistantText, Text: "Inspecting a pair."},
			{Kind: model.ItemToolCall, ToolCall: &model.ToolCall{Name: "inspect", RawArguments: `{}`}},
			{Kind: model.ItemToolCall, ToolCall: &model.ToolCall{Name: "inspect", RawArguments: `{}`}},
		}, StopReason: "tool_calls"}, nil
	})
	runtime = newTestRuntime(t, Config{
		Model:   model.Info{BackendID: "test", Provider: "test", Model: "test", ContextWindow: 16 * 1024},
		Backend: backend, Journal: journal,
		LoadInstructions: func(context.Context, string) (string, error) {
			loads++
			return fmt.Sprintf("system instructions %d", summaries), nil
		},
		Tools: []Tool{{Spec: model.ToolSpec{Name: "inspect", InputSchema: jsontext.Value(`{"type":"object"}`)}, Run: func(context.Context, string) (ToolOutput, error) {
			tools++
			if tools == 2 {
				if err := runtime.QueueUserInput(Input{Text: steering, Instructions: []Instruction{{Key: "procedure", Text: "current procedure"}}}); err != nil {
					t.Fatal(err)
				}
			}
			return ToolOutput{Content: model.TextContent(strings.Repeat("x", 4*1024))}, nil
		}}},
	})
	var events []Event
	result, err := runtime.RunInput(t.Context(), Input{Text: task, Instructions: []Instruction{{Key: "procedure", Text: "obsolete procedure"}}}, func(event Event) { events = append(events, event) })
	if err != nil || result.Answer != "All pairs inspected." || summaries < 2 || tools != 2*rounds || mainCalls != rounds+1 {
		t.Fatalf("run = %#v, error = %v, summaries/tools/model = %d/%d/%d", result, err, summaries, tools, mainCalls)
	}
	records := journal.snapshot()
	assertAuthoritativeEvents(t, events, records)
	if countRecordKind(records, RecordRunStarted) != 1 || countRecordKind(records, RecordRunFinished) != 1 {
		t.Fatal("compaction changed the run lifecycle")
	}
	state, err := Replay(records)
	if err != nil || state.CompactionCount != summaries || len(state.ActiveRuns) != 0 {
		t.Fatalf("replay = compactions %d, active %v, error %v", state.CompactionCount, state.ActiveRuns, err)
	}
	if !itemsContainText(state.modelItems(), task) || !itemsContainText(state.modelItems(), steering) {
		t.Fatal("finishing/replaying the run lost the checkpoint's task inputs")
	}
	assertCompleteExchanges(t, state.modelItems())
	if len(state.Items) <= len(state.modelItems()) {
		t.Fatal("test did not reduce the projected history")
	}

	// Exercise rejection using real journal boundaries: neither a response's
	// calls nor a partially collected pair of results may straddle a checkpoint.
	for _, block := range state.Blocks {
		for i := 1; i < len(block.Entries); i++ {
			entry, previous := block.Entries[i], block.Entries[i-1]
			if entry.Item.Kind != model.ItemToolResult {
				continue
			}
			payload := ContextCompactedRecord{CoveredThroughSequence: previous.Sequence, FirstVerbatimSequence: entry.Sequence, Summary: "invalid cut", RetainedInputSequences: state.inputsBeforeBoundary(entry.Sequence)}
			if err := validateContextCheckpoint(state, payload, state.LastSequence+1); err == nil || !strings.Contains(err.Error(), "completed exchange") {
				t.Fatalf("split tool exchange accepted: %v", err)
			}
		}
	}
}

func TestSingleRunCompactionAndSmallerWindowUseTheSameHistory(t *testing.T) {
	for _, smaller := range []bool{false, true} {
		t.Run(fmt.Sprintf("smaller_window=%t", smaller), func(t *testing.T) {
			journal := &memoryJournal{}
			const task = "Inspect the eight sections, then report."
			calls, summaries, executed := 0, 0, 0
			info := model.Info{BackendID: "test", Provider: "test", Model: "test", ContextWindow: 64 * 1024}
			backend := modelFunc(func(_ context.Context, request model.Request, _ func(model.StreamEvent)) (model.Response, error) {
				if isCompactionRequest(request) {
					summaries++
					if summaries > 1 && request.Summary == "" {
						t.Fatal("rolling summary was dropped")
					}
					assertCompleteExchanges(t, request.Items)
					return model.Response{Items: []model.Item{{Kind: model.ItemAssistantText, Text: "Earlier sections inspected."}}, StopReason: "stop"}, nil
				}
				calls++
				if calls <= 8 {
					// Keep this history large even after tool-output pruning.
					return model.Response{Items: []model.Item{
						{Kind: model.ItemAssistantText, Text: strings.Repeat("inspection notes ", 768)},
						{Kind: model.ItemToolCall, ToolCall: &model.ToolCall{Name: "inspect", RawArguments: `{}`}},
					}}, nil
				}
				if summaries > 0 && (!itemsContainText(request.Items, task) || request.Summary == "") {
					t.Fatal("continuation lost its task or summary")
				}
				assertCompleteExchanges(t, request.Items)
				return model.Response{Items: []model.Item{{Kind: model.ItemAssistantText, Text: "done"}}, StopReason: "stop"}, nil
			})
			tool := Tool{Spec: model.ToolSpec{Name: "inspect", InputSchema: jsontext.Value(`{"type":"object"}`)}, Run: func(context.Context, string) (ToolOutput, error) {
				executed++
				return ToolOutput{Content: model.TextContent(strings.Repeat("x", 1024))}, nil
			}}
			runtime := newTestRuntime(t, Config{Model: info, Backend: backend, Journal: journal, Tools: []Tool{tool}})
			if _, err := runtime.Run(t.Context(), task, nil); err != nil {
				t.Fatal(err)
			}
			if smaller {
				info.Model, info.ContextWindow = "smaller", 16*1024
				if err := runtime.SwitchModel(t.Context(), info, backend); err != nil {
					t.Fatal(err)
				}
			} else if _, err := runtime.Compact(t.Context()); err != nil {
				t.Fatal(err)
			}
			// Reopen the journal and continue through the ordinary preflight.
			runtime = newTestRuntime(t, Config{Model: info, Backend: backend, Journal: journal, Tools: []Tool{tool}})
			if _, err := runtime.Run(t.Context(), "Continue with the report.", nil); err != nil {
				t.Fatal(err)
			}
			if summaries == 0 || (smaller && summaries < 2) || executed != 8 {
				t.Fatalf("summaries/executions = %d/%d", summaries, executed)
			}
			if report := runtime.SessionStatus().ContextReport; report.TotalInputTokens > report.InputLimit {
				t.Fatalf("context still over limit: %#v", report)
			}
		})
	}
}

func TestCheckpointInputRetentionIsOmittedOrComplete(t *testing.T) {
	journal := &memoryJournal{}
	inputs := []string{"Inspect the original task.", "Use the first correction.", "Also apply the second correction."}
	delivered := 0
	var runtime *Runtime
	runtime = newTestRuntime(t, Config{
		Journal: journal,
		Backend: modelFunc(func(context.Context, model.Request, func(model.StreamEvent)) (model.Response, error) {
			if delivered < 2 {
				return model.Response{Items: []model.Item{{Kind: model.ItemToolCall, ToolCall: &model.ToolCall{Name: "inspect", RawArguments: `{}`}}}}, nil
			}
			return model.Response{Items: []model.Item{{Kind: model.ItemAssistantText, Text: "done"}}, StopReason: "stop"}, nil
		}),
		Tools: []Tool{{Spec: model.ToolSpec{Name: "inspect", InputSchema: jsontext.Value(`{"type":"object"}`)}, Run: func(context.Context, string) (ToolOutput, error) {
			delivered++
			return ToolOutput{Content: model.TextContent("section inspected")}, runtime.QueueUserInput(Input{Text: inputs[delivered]})
		}}},
	})
	if _, err := runtime.Run(t.Context(), inputs[0], nil); err != nil {
		t.Fatal(err)
	}
	state, err := Replay(journal.snapshot())
	if err != nil || len(state.Blocks) != len(inputs) {
		t.Fatalf("seeded blocks = %d, error = %v", len(state.Blocks), err)
	}
	retained := []uint64{state.Blocks[0].StartSequence, state.Blocks[1].StartSequence}
	for _, test := range []struct {
		name     string
		retained []uint64
		want     []string
		invalid  bool
	}{
		{name: "omitted", want: inputs[2:]},
		{name: "complete", retained: retained, want: inputs},
		{name: "partial", retained: retained[:1], invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := ContextCompactedRecord{
				CoveredThroughSequence: state.Blocks[1].EndSequence,
				FirstVerbatimSequence:  state.Blocks[2].StartSequence,
				RetainedInputSequences: test.retained,
				Summary:                "Earlier work and the first correction summarized.",
			}
			records := append(journal.snapshot(), recordForTest(t, state.LastSequence+1, RecordContextCompacted, payload))
			projected, err := Replay(records)
			if test.invalid {
				if err == nil || !strings.Contains(err.Error(), "retained task's inputs") {
					t.Fatalf("incomplete input retention accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, item := range projected.modelItems() {
				if item.Kind == model.ItemUserText {
					got = append(got, item.Text)
				}
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("retained inputs = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCheckpointDoesNotRetainContextClearNotices(t *testing.T) {
	journal := &memoryJournal{}
	backend := &scriptedModel{steps: []modelStep{directModelResponse("first answer"), directModelResponse("second answer"), directModelResponse("third answer")}}
	runtime := newTestRuntime(t, Config{Backend: backend, Journal: journal})
	for _, text := range []string{"first", "second"} {
		if _, err := runtime.RunInput(t.Context(), Input{Text: text, Instructions: []Instruction{{Key: "procedure", Text: text + " procedure"}}}, nil); err != nil {
			t.Fatal(err)
		}
		if err := runtime.ClearInputContext(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runtime.Run(t.Context(), "third", nil); err != nil {
		t.Fatal(err)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var clear ConversationBlock
	for _, block := range state.Blocks {
		if block.RunID == "" {
			clear = block
		}
	}
	index := slices.IndexFunc(state.contextBoundaries(), func(boundary contextBoundary) bool { return boundary.first == clear.StartSequence })
	if index < 0 {
		t.Fatal("no boundary before the latest context clear")
	}
	boundary := state.contextBoundaries()[index]
	payload := ContextCompactedRecord{
		CoveredThroughSequence: boundary.through,
		FirstVerbatimSequence:  boundary.first,
		RetainedInputSequences: state.inputsBeforeBoundary(boundary.first),
		Summary:                "Earlier runs summarized.",
	}
	projected, err := Replay(append(journal.snapshot(), recordForTest(t, state.LastSequence+1, RecordContextCompacted, payload)))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, item := range projected.modelItems() {
		if item.Kind == model.ItemUserText {
			got = append(got, item.Text)
		}
	}
	if want := []string{inputContextClearedText, "third"}; !slices.Equal(got, want) {
		t.Fatalf("user items = %q, want %q", got, want)
	}
}

func assertWholeResponses(t *testing.T, original, projected []model.Item) {
	t.Helper()
	count := func(items []model.Item) map[string]int {
		groups := make(map[string]int)
		for _, item := range items {
			if item.ResponseID != "" {
				groups[item.ResponseID]++
			}
		}
		return groups
	}
	groups := count(original)
	for id, kept := range count(projected) {
		if kept != groups[id] {
			t.Fatalf("response %s split: retained %d of %d items", id, kept, groups[id])
		}
	}
}

func assertCompleteExchanges(t *testing.T, items []model.Item) {
	t.Helper()
	pending := make(map[string]bool)
	for _, item := range items {
		switch item.Kind {
		case model.ItemToolCall:
			pending[item.ToolCall.ID] = true
		case model.ItemToolResult:
			if !pending[item.ToolResult.CallID] {
				t.Fatal("orphan tool result in projected history")
			}
			delete(pending, item.ToolResult.CallID)
		case model.ItemUserText, model.ItemBoundaryText:
			if len(pending) != 0 {
				t.Fatal("input splits tool calls from their results")
			}
		}
	}
	if len(pending) != 0 {
		t.Fatal("tool calls lost their results")
	}
}
