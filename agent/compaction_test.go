package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCompactionIsAdditiveAndRuntimeUsesSummaryPlusTail(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{
		directModelResponse("old answer"),
		directModelResponse("recent answer"),
	}}
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
	plan, err := planCompaction(state, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Input, "old question") || !strings.Contains(plan.Input, "old answer") || !strings.Contains(plan.Input, "[run_finished status=completed]") || strings.Contains(plan.Input, "recent question") {
		t.Fatalf("compaction input = %q", plan.Input)
	}
	if _, _, err := commitCompaction(context.Background(), journal, plan, "Objective: continue recent work. Old answer was recorded.", ModelUsage{}); err != nil {
		t.Fatal(err)
	}
	state, err = Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Items) != 4 {
		t.Fatalf("raw transcript changed: %#v", state.Items)
	}
	verbatim := state.VerbatimItems()
	if len(verbatim) != 2 || verbatim[0].Text != "recent question" || verbatim[1].Text != "recent answer" {
		t.Fatalf("verbatim tail = %#v", verbatim)
	}

	thirdModel := &scriptedModel{steps: []modelStep{func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
		if !strings.Contains(request.Summary, "continue recent work") {
			t.Fatalf("summary = %q", request.Summary)
		}
		if len(request.Items) != 3 || request.Items[0].Text != "recent question" || request.Items[2].Text != "new question" {
			t.Fatalf("projected request items = %#v", request.Items)
		}
		return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "new answer"}}}, nil
	}}}
	if _, err := newTestRuntime(t, Config{Backend: thirdModel, Journal: journal}).Run(context.Background(), "new question", nil); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCompactSummarizesWithoutToolsAndCommitsTailBoundary(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{
		directModelResponse("old answer"),
		directModelResponse("recent answer"),
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			if request.Instructions != compactionSystemInstructions {
				t.Fatalf("compaction instructions = %q", request.Instructions)
			}
			if request.Summary != "" || len(request.Tools) != 0 {
				t.Fatalf("compaction request carries runtime context: %#v", request)
			}
			if len(request.Items) != 1 || request.Items[0].Kind != ItemUserText {
				t.Fatalf("compaction items = %#v", request.Items)
			}
			input := request.Items[0].Text
			if !strings.Contains(input, "old question") || !strings.Contains(input, "old answer") || strings.Contains(input, "recent question") {
				t.Fatalf("compaction input = %q", input)
			}
			return ModelResponse{
				Items:      []Item{{Kind: ItemAssistantText, Text: "Objective: preserve the recent work."}},
				Usage:      ModelUsage{InputTokens: 30, OutputTokens: 8, TotalTokens: 38},
				StopReason: "stop",
			}, nil
		},
	}}
	runtime := newTestRuntime(t, Config{Backend: model, Journal: journal})
	if _, err := runtime.Run(context.Background(), "old question", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "recent question", nil); err != nil {
		t.Fatal(err)
	}

	compaction, err := runtime.Compact(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if compaction.Summary != "Objective: preserve the recent work." || compaction.Usage.TotalTokens != 38 {
		t.Fatalf("compaction = %#v", compaction)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if state.CompactionCount != 1 || state.Compaction.Summary != compaction.Summary {
		t.Fatalf("replayed compaction = %#v count=%d", state.Compaction, state.CompactionCount)
	}
	verbatim := state.VerbatimItems()
	if len(verbatim) != 2 || verbatim[0].Text != "recent question" || verbatim[1].Text != "recent answer" {
		t.Fatalf("verbatim tail = %#v", verbatim)
	}
}

func TestRuntimeCompactRejectsIncompleteSummaryWithoutJournalRecord(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{
		directModelResponse("old answer"),
		directModelResponse("recent answer"),
		func(_ context.Context, _ ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{
				Items:      []Item{{Kind: ItemAssistantText, Text: "truncated summary"}},
				StopReason: "length",
			}, nil
		},
	}}
	runtime := newTestRuntime(t, Config{Backend: model, Journal: journal})
	if _, err := runtime.Run(context.Background(), "old question", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "recent question", nil); err != nil {
		t.Fatal(err)
	}
	recordCount := len(journal.snapshot())
	if _, err := runtime.Compact(context.Background(), 1); err == nil || !strings.Contains(err.Error(), "incomplete summary") {
		t.Fatalf("compaction error = %v", err)
	}
	if records := journal.snapshot(); len(records) != recordCount || countRecordKind(records, RecordContextCompacted) != 0 {
		t.Fatalf("incomplete summary reached journal: %#v", records)
	}
}

func TestRuntimeAutomaticallyCompactsBeforeOversizedRequest(t *testing.T) {
	journal := &memoryJournal{}
	seedModel := &scriptedModel{steps: []modelStep{
		directModelResponse("old answer"),
		directModelResponse("recent answer"),
	}}
	seedRuntime := newTestRuntime(t, Config{Backend: seedModel, Journal: journal})
	oldQuestion := strings.Repeat("old context ", 4_000)
	if _, err := seedRuntime.Run(context.Background(), oldQuestion, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := seedRuntime.Run(context.Background(), "recent question", nil); err != nil {
		t.Fatal(err)
	}

	model := &scriptedModel{
		info: ModelInfo{BackendID: "test", Provider: "test", Model: "test", ContextWindow: 16 * 1024},
		steps: []modelStep{
			func(_ context.Context, request ModelRequest, emit func(ModelStreamEvent)) (ModelResponse, error) {
				if request.Instructions != compactionSystemInstructions || len(request.Tools) != 0 || len(request.Items) != 1 {
					t.Fatalf("automatic compaction request = %#v", request)
				}
				if !strings.Contains(request.Items[0].Text, "old context") || strings.Contains(request.Items[0].Text, "recent question") {
					t.Fatalf("automatic compaction input = %q", request.Items[0].Text)
				}
				emit(ModelStreamEvent{Kind: EventReasoningSummaryDelta, Text: "internal reasoning"})
				emit(ModelStreamEvent{Kind: EventTextDelta, Text: "Old work was summarized."})
				return ModelResponse{
					Items:      []Item{{Kind: ItemAssistantText, Text: "Old work was summarized."}},
					Usage:      ModelUsage{InputTokens: 8_000, OutputTokens: 20, TotalTokens: 8_020},
					StopReason: "stop",
				}, nil
			},
			func(_ context.Context, request ModelRequest, emit func(ModelStreamEvent)) (ModelResponse, error) {
				if request.Summary != "Old work was summarized." {
					t.Fatalf("summary = %q", request.Summary)
				}
				if len(request.Items) != 3 || request.Items[0].Text != "recent question" || request.Items[2].Text != "new question" {
					t.Fatalf("post-compaction items = %#v", request.Items)
				}
				emit(ModelStreamEvent{Kind: EventTextDelta, Text: "final answer"})
				return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "final answer"}}, StopReason: "stop"}, nil
			},
		},
	}
	runtime := newTestRuntime(t, Config{Backend: model, Journal: journal})
	var events []Event
	result, err := runtime.Run(context.Background(), "new question", func(event Event) {
		assertEventCommittedAtEmission(t, journal, event)
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "final answer" || !hasEvent(events, EventStatus) || !hasEvent(events, EventContextCompacted) {
		t.Fatalf("result=%#v events=%#v", result, events)
	}
	modelAttempts := 0
	for _, event := range events {
		if event.Kind == EventModelAttemptStarted {
			modelAttempts++
		}
	}
	if modelAttempts != 2 {
		t.Fatalf("model attempts = %d, want compaction plus final request; events=%#v", modelAttempts, events)
	}
	textDeltas := make([]string, 0, 1)
	for _, event := range events {
		if event.Kind == EventTextDelta {
			textDeltas = append(textDeltas, event.Text)
		}
		if event.Kind == EventReasoningSummaryDelta {
			t.Fatalf("compaction reasoning leaked into run events: %#v", events)
		}
	}
	if len(textDeltas) != 1 || textDeltas[0] != "final answer" {
		t.Fatalf("streamed answer deltas = %q", textDeltas)
	}
	assertAuthoritativeEvents(t, events, journal.snapshot())
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if state.CompactionCount != 1 || state.Usage.TotalTokens != 8_020 {
		t.Fatalf("state compactions=%d usage=%#v", state.CompactionCount, state.Usage)
	}
	report := runtime.SessionStatus().ContextReport
	if report.Window != 16*1024 || report.TotalInputTokens > report.InputLimit || report.CompactionCount != 1 {
		t.Fatalf("context report = %#v", report)
	}
}

func TestRollingCompactionAdvancesFromPreviousBoundary(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{
		directModelResponse("answer one"),
		directModelResponse("answer two"),
		directModelResponse("answer three"),
	}}
	runtime := newTestRuntime(t, Config{Backend: model, Journal: journal})
	for _, input := range []string{"question one", "question two", "question three"} {
		if _, err := runtime.Run(context.Background(), input, nil); err != nil {
			t.Fatal(err)
		}
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	first, err := planCompaction(state, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := commitCompaction(context.Background(), journal, first, "summary one", ModelUsage{}); err != nil {
		t.Fatal(err)
	}

	fourthModel := &scriptedModel{steps: []modelStep{directModelResponse("answer four")}}
	if _, err := newTestRuntime(t, Config{Backend: fourthModel, Journal: journal}).Run(context.Background(), "question four", nil); err != nil {
		t.Fatal(err)
	}
	state, err = Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	second, err := planCompaction(state, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Input, "Previous rolling summary:\nsummary one") ||
		!strings.Contains(second.Input, "question two") || !strings.Contains(second.Input, "question three") ||
		strings.Contains(second.Input, "question one") || strings.Contains(second.Input, "question four") {
		t.Fatalf("rolling input = %q", second.Input)
	}
	if second.CoveredThroughSequence <= first.CoveredThroughSequence || second.FirstVerbatimSequence <= first.FirstVerbatimSequence {
		t.Fatalf("boundaries did not advance: first=%#v second=%#v", first, second)
	}
	if _, _, err := commitCompaction(context.Background(), journal, second, "summary two", ModelUsage{}); err != nil {
		t.Fatal(err)
	}
	state, err = Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if state.CompactionCount != 2 || state.Compaction.Summary != "summary two" {
		t.Fatalf("compaction state = %#v count=%d", state.Compaction, state.CompactionCount)
	}
	verbatim := state.VerbatimItems()
	if len(verbatim) != 2 || verbatim[0].Text != "question four" {
		t.Fatalf("rolling verbatim tail = %#v", verbatim)
	}
}

func TestCompactionBlockKeepsToolCallAndResultTogether(t *testing.T) {
	journal := &memoryJournal{}
	providerID := json.RawMessage(`"provider_call"`)
	model := &scriptedModel{steps: []modelStep{
		func(_ context.Context, _ ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{
				{Kind: ItemReasoning, Text: "provider-private reasoning", ProviderData: []ProviderData{{
					Kind: "responses.reasoning_item", Data: json.RawMessage(`{"encrypted_content":"opaque-secret"}`),
				}}},
				{Kind: ItemToolCall, ToolCall: &ToolCall{
					Name: "echo", RawArguments: `{"text":"hello"}`,
					ProviderReferences: []ProviderReference{{Kind: "call_id", Data: providerID}},
				}},
			}}, nil
		},
		directModelResponse("tool complete"),
		directModelResponse("recent answer"),
	}}
	tool := Tool{
		Spec: ToolSpec{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(_ context.Context, arguments string) (ToolOutput, error) {
			return ToolOutput{Content: "echo result: " + arguments}, nil
		},
	}
	runtime := newTestRuntime(t, Config{Backend: model, Journal: journal, Tools: []Tool{tool}})
	if _, err := runtime.Run(context.Background(), "use echo", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "recent", nil); err != nil {
		t.Fatal(err)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planCompaction(state, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Input, `[tool_call] echo args={"text":"hello"}`) || !strings.Contains(plan.Input, "echo result") {
		t.Fatalf("tool block missing from input: %q", plan.Input)
	}
	if strings.Contains(plan.Input, "provider-private reasoning") || strings.Contains(plan.Input, "provider_call") || strings.Contains(plan.Input, "opaque-secret") {
		t.Fatalf("provider-owned data leaked into summary input: %q", plan.Input)
	}
	invalid := ContextCompactedRecord{
		CoveredThroughSequence: state.Blocks[0].StartSequence,
		FirstVerbatimSequence:  state.Blocks[0].EndSequence,
		Summary:                "bad cut",
	}
	if err := validateCompactionBoundary(state, invalid, state.LastSequence+1, false); err == nil {
		t.Fatal("cut inside a tool block was accepted")
	}
}

func TestCompactionRejectsUnfinishedAndStalePlans(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{directModelResponse("one"), directModelResponse("two"), directModelResponse("three")}}
	runtime := newTestRuntime(t, Config{Backend: model, Journal: journal})
	if _, err := runtime.Run(context.Background(), "one", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "two", nil); err != nil {
		t.Fatal(err)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planCompaction(state, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "three", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := commitCompaction(context.Background(), journal, plan, "stale summary", ModelUsage{}); err == nil || !strings.Contains(err.Error(), "session changed") {
		t.Fatalf("stale commit error = %v", err)
	}

	unfinished := &memoryJournal{}
	mustAppend(t, unfinished, RecordSessionStarted, SessionStartedRecord{SchemaVersion: JournalSchemaVersion, SessionID: "session"})
	mustAppend(t, unfinished, RecordModelSelected, ModelSelectedRecord{Backend: "test", Provider: "test", Model: "test", Epoch: "epoch"})
	mustAppend(t, unfinished, RecordRunStarted, RunStartedRecord{RunID: "run"})
	mustAppend(t, unfinished, RecordRunInputAdded, RunInputAddedRecord{RunID: "run", Text: "unfinished"})
	unfinishedState, err := Replay(unfinished.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planCompaction(unfinishedState, 1); err == nil || !strings.Contains(err.Error(), "unfinished") {
		t.Fatalf("unfinished plan error = %v", err)
	}
}

func TestCompactionRetryDiagnosticsDoNotInvalidatePlan(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{
		directModelResponse("old answer"),
		directModelResponse("recent answer"),
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{}, &ProviderError{
				Cause: MarkProviderFailure(errors.New("summarizer temporarily unavailable")),
				Kind:  ProviderErrorUnavailable, Retryable: true,
			}
		},
		directModelResponse("summary"),
	}}
	runtime := newTestRuntime(t, Config{
		Backend: model, Journal: journal,
		RequestPolicy: ModelRequestPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond},
	})
	if _, err := runtime.Run(context.Background(), "old question", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "recent question", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Compact(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	records := journal.snapshot()
	if countRecordKind(records, RecordContextCompacted) != 1 || countRecordKind(records, RecordModelAttemptFailed) != 1 {
		t.Fatalf("compaction records = %#v", records)
	}
	for _, record := range records {
		if record.Kind != RecordModelAttemptFailed {
			continue
		}
		diagnostic, err := decodeRecord[ModelAttemptFailedRecord](record)
		if err != nil {
			t.Fatal(err)
		}
		if diagnostic.Purpose != ModelRequestCompaction || diagnostic.RunID != "" || diagnostic.Attempt != 1 {
			t.Fatalf("compaction diagnostic = %#v", diagnostic)
		}
	}
}

func TestModelBoundaryCompactionCannotCoverActiveRunBlocks(t *testing.T) {
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
	plan, err := planCompactionForModelBoundary(state, 1)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CoveredThroughSequence != state.Blocks[1].EndSequence || plan.FirstVerbatimSequence != state.Blocks[2].StartSequence {
		t.Fatalf("active-boundary plan = %#v", plan)
	}
	unsafe := ContextCompactedRecord{
		CoveredThroughSequence: state.Blocks[2].EndSequence,
		FirstVerbatimSequence:  state.Blocks[3].StartSequence,
		Summary:                "would swallow active input",
	}
	if err := validateCompactionBoundary(state, unsafe, state.LastSequence+1, true); err == nil || !strings.Contains(err.Error(), "unfinished") {
		t.Fatalf("active block compaction error = %v", err)
	}
}

func directModelResponse(text string) modelStep {
	return func(_ context.Context, _ ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
		return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: text}}}, nil
	}
}
