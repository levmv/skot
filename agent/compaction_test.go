package agent

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
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
	oldQuestion := compactionTestText("old question", 132*1024)
	if _, err := runtime.Run(context.Background(), oldQuestion, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "recent question", nil); err != nil {
		t.Fatal(err)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := runtime.planCompactionForModelBoundary(state, runRequestSpec{})
	if err != nil {
		t.Fatal(err)
	}
	request := mustCompactionRequest(t, runtime, state, runRequestSpec{}, plan)
	if !itemsContainText(request.Items, "old question") || !itemsContainText(request.Items, "old answer") || itemsContainText(request.Items, "recent question") {
		t.Fatalf("compaction items = %#v", request.Items)
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

func TestCompactionDoesNotProbeImagesAfterModelSwitch(t *testing.T) {
	journal := &memoryJournal{}
	tool := seedCompletedToolContentHistory(t, journal, ImageToolContent("old image metadata", ImageContent{
		MediaType: "image/png", Data: []byte{1, 2, 3}, Width: 10, Height: 5,
	}), "inspect an image")
	runtime := newTestRuntime(t, Config{
		Backend: modelFunc(func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{}, nil
		}),
		Journal: journal,
		Tools:   []Tool{tool},
	})
	nextBackend := modelFunc(func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
		return ModelResponse{}, nil
	})
	if err := runtime.SwitchModel(context.Background(), ModelInfo{
		BackendID: "next", Provider: "next", Model: "next", ContextWindow: 64 * 1024,
	}, nextBackend); err != nil {
		t.Fatal(err)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if state.ImageDelivery.Status != ImageDeliveryUnknown || len(state.Blocks) < 2 {
		t.Fatalf("switched state = %#v", state.ImageDelivery)
	}
	plan := compactionPlan{FirstVerbatimSequence: state.Blocks[1].StartSequence}
	request := mustCompactionRequest(t, runtime, state, runRequestSpec{}, plan)
	if modelRequestHasImages(request) || !requestContainsImageOmission(request) {
		t.Fatalf("unknown-route compaction request = %#v", request.Items)
	}
	ordinary, err := runtime.modelRequestForRun(state, runRequestSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if !modelRequestHasImages(ordinary) {
		t.Fatalf("ordinary request did not retain image probe: %#v", ordinary.Items)
	}
}

func TestCompactionRetainsRecentTailByProjectedTokenBudget(t *testing.T) {
	journal := &memoryJournal{}
	const blocks = 14
	steps := make([]modelStep, blocks)
	for index := range steps {
		steps[index] = func(_ context.Context, _ ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{
				{Kind: ItemReasoning, Text: strings.Repeat("r", 28*1024)},
				{Kind: ItemAssistantText, Text: "answer"},
			}, StopReason: "stop"}, nil
		}
	}
	model := &scriptedModel{
		steps: steps,
	}
	seedRuntime := newTestRuntime(t, Config{Backend: model, Journal: journal})
	for index := range blocks {
		if _, err := seedRuntime.Run(context.Background(), fmt.Sprintf("block %02d ", index)+strings.Repeat("x", 4*1024), nil); err != nil {
			t.Fatal(err)
		}
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	runtime := newTestRuntime(t, Config{
		Model: ModelInfo{BackendID: "test", Provider: "test", Model: "test", ContextWindow: 128 * 1024},
		Backend: modelFunc(func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{}, nil
		}),
		Journal: journal,
	})
	plan, err := runtime.planCompactionForModelBoundary(state, runRequestSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.FirstVerbatimSequence != state.Blocks[12].StartSequence || plan.CoveredThroughSequence != state.Blocks[11].EndSequence {
		t.Fatalf("token-budget boundary = %#v", plan)
	}
	request := mustCompactionRequest(t, runtime, state, runRequestSpec{}, plan)
	if !itemsContainText(request.Items, "block 11 ") || itemsContainText(request.Items, "block 12 ") || !itemsContainText(request.Items, strings.Repeat("r", 128)) {
		t.Fatalf("token-budget items = %#v", request.Items)
	}
}

func TestCompactionVerbatimTokenBudgetHasAbsoluteBounds(t *testing.T) {
	tests := []struct {
		name   string
		report ContextReport
		want   int
	}{
		{name: "floor", report: ContextReport{Window: 32 * 1024, InputLimit: 24 * 1024}, want: 8 * 1024},
		{name: "proportional", report: ContextReport{Window: 128 * 1024, InputLimit: 102 * 1024}, want: 128 * 1024 * 15 / 100},
		{name: "ceiling", report: ContextReport{Window: 1_000_000, InputLimit: 968_000}, want: 32 * 1024},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := compactionVerbatimTokenBudget(test.report); got != test.want {
				t.Fatalf("verbatim budget = %d, want %d", got, test.want)
			}
		})
	}
}

func TestRuntimeCompactUsesTokenTailBeforeWindowPressure(t *testing.T) {
	journal := &memoryJournal{}
	var compactionRequest ModelRequest
	steps := make([]modelStep, 0, 5)
	for range 4 {
		steps = append(steps, directModelResponse("answer"))
	}
	steps = append(steps, func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
		compactionRequest = request
		return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "summary"}}, StopReason: "stop"}, nil
	})
	runtime := newTestRuntime(t, Config{
		Model: ModelInfo{BackendID: "test", Provider: "test", Model: "test", ContextWindow: 1_000_000},
		Backend: &scriptedModel{
			steps: steps,
		},
		Journal: journal,
	})
	for index := range 4 {
		input := fmt.Sprintf("block %d ", index) + strings.Repeat("x", 40*1024)
		if _, err := runtime.Run(context.Background(), input, nil); err != nil {
			t.Fatal(err)
		}
	}
	before, err := runtime.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	report := runtime.contextReport(before)
	if report.TotalInputTokens > report.InputLimit || report.HistoryTokens <= maxCompactionVerbatimTokens {
		t.Fatalf("pre-compaction report = %#v", report)
	}

	compaction, err := runtime.Compact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if compaction.FirstVerbatimSequence != before.Blocks[1].StartSequence {
		t.Fatalf("manual compaction boundary = %#v", compaction)
	}
	if !isCompactionRequest(compactionRequest) || !itemsContainText(compactionRequest.Items, "block 0 ") || itemsContainText(compactionRequest.Items, "block 1 ") {
		t.Fatalf("manual compaction request = %#v", compactionRequest)
	}
}

func TestRuntimeCompactSkipsHistoryWithinTokenTail(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{directModelResponse("old answer"), directModelResponse("recent answer")}}
	runtime := newTestRuntime(t, Config{Backend: model, Journal: journal})
	if _, err := runtime.Run(context.Background(), "old question", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "recent question", nil); err != nil {
		t.Fatal(err)
	}
	recordCount := len(journal.snapshot())
	if _, err := runtime.Compact(context.Background()); !errors.Is(err, errCompactionNotNeeded) {
		t.Fatalf("compact small history error = %v", err)
	}
	if len(journal.snapshot()) != recordCount || model.next != 2 {
		t.Fatalf("small-history compaction changed journal or called model: records=%d, calls=%d", len(journal.snapshot()), model.next)
	}
}

func TestCompactionPromptLeavesInstructionsLast(t *testing.T) {
	prompt := compactionPrompt("summarize now", "run failed")
	if !strings.Contains(prompt, "run failed") || !strings.HasSuffix(prompt, "summarize now") {
		t.Fatalf("compaction prompt = %q", prompt)
	}
}

func TestRuntimeCompactUsesCacheAlignedPrefixAndCommitsTailBoundary(t *testing.T) {
	journal := &memoryJournal{}
	tool := Tool{
		Spec: ToolSpec{Name: "inspect", InputSchema: jsontext.Value(`{"type":"object"}`)},
		Run: func(context.Context, string) (ToolOutput, error) {
			return ToolOutput{}, nil
		},
	}
	oldQuestion := compactionTestText("old question", 132*1024)
	model := &scriptedModel{steps: []modelStep{
		directModelResponse("old answer"),
		directModelResponse("recent answer"),
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			if request.Instructions != "main instructions" {
				t.Fatalf("compaction instructions = %q", request.Instructions)
			}
			if request.Summary != "" || len(request.Tools) != 1 || request.Tools[0].Name != "inspect" {
				t.Fatalf("compaction request carries runtime context: %#v", request)
			}
			if len(request.Items) != 3 || request.Items[0].Text != oldQuestion || request.Items[1].Text != "old answer" || !isCompactionRequest(request) {
				t.Fatalf("compaction items = %#v", request.Items)
			}
			if itemsContainText(request.Items, "recent question") {
				t.Fatalf("compaction request includes verbatim tail: %#v", request.Items)
			}
			return ModelResponse{
				Items: []Item{
					{Kind: ItemAssistantText, Text: "Objective: preserve the recent work."},
					{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "inspect", RawArguments: `{}`}},
				},
				Usage:      ModelUsage{InputTokens: 30, OutputTokens: 8, TotalTokens: 38},
				StopReason: "tool_calls",
			}, nil
		},
	}}
	runtime := newTestRuntime(t, Config{Backend: model, Journal: journal, Instructions: "main instructions", Tools: []Tool{tool}})
	if _, err := runtime.Run(context.Background(), oldQuestion, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "recent question", nil); err != nil {
		t.Fatal(err)
	}

	compaction, err := runtime.Compact(context.Background())
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
	if _, err := runtime.Run(context.Background(), compactionTestText("old question", 132*1024), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "recent question", nil); err != nil {
		t.Fatal(err)
	}
	recordCount := len(journal.snapshot())
	if _, err := runtime.Compact(context.Background()); err == nil || !strings.Contains(err.Error(), "incomplete summary") {
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
	oldQuestion := strings.Repeat("old context ", 2_100)
	recentQuestion := "recent question " + strings.Repeat("r", 8*1024)
	if _, err := seedRuntime.Run(context.Background(), oldQuestion, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := seedRuntime.Run(context.Background(), recentQuestion, nil); err != nil {
		t.Fatal(err)
	}

	model := &scriptedModel{
		info: ModelInfo{BackendID: "test", Provider: "test", Model: "test", ContextWindow: 16 * 1024},
		steps: []modelStep{
			func(_ context.Context, request ModelRequest, emit func(ModelStreamEvent)) (ModelResponse, error) {
				if !isCompactionRequest(request) || len(request.Tools) != 0 {
					t.Fatalf("automatic compaction request = %#v", request)
				}
				if !itemsContainText(request.Items, "old context") || itemsContainText(request.Items, "recent question") {
					t.Fatalf("automatic compaction items = %#v", request.Items)
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
				if len(request.Items) != 3 || request.Items[0].Text != recentQuestion || request.Items[2].Text != "new question" {
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

func TestRuntimePreflightUsesBoundedCacheAlignedCompactionRequests(t *testing.T) {
	journal := &memoryJournal{}
	seedRuntime := newTestRuntime(t, Config{
		Backend: modelFunc(func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "old answer"}}, StopReason: "stop"}, nil
		}),
		Journal: journal,
	})
	oldInput := strings.Repeat("old context ", 3_000)
	for range 18 {
		if _, err := seedRuntime.Run(context.Background(), oldInput, nil); err != nil {
			t.Fatal(err)
		}
	}

	compactions := 0
	mainRequests := 0
	model := modelFunc(func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
		if isCompactionRequest(request) {
			compactions++
			return ModelResponse{
				Items:      []Item{{Kind: ItemAssistantText, Text: fmt.Sprintf("summary %d", compactions)}},
				StopReason: "stop",
			}, nil
		}
		mainRequests++
		return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "fits"}}, StopReason: "stop"}, nil
	})
	runtime := newTestRuntime(t, Config{
		Model: ModelInfo{
			BackendID: "test", Provider: "test", Model: "test", ContextWindow: 96 * 1024,
		},
		Backend: model,
		Journal: journal,
	})
	result, err := runtime.Run(context.Background(), "new question", nil)
	if err != nil || result.Answer != "fits" {
		t.Fatalf("result/error = %#v / %v", result, err)
	}
	if compactions < 2 || mainRequests != 1 {
		t.Fatalf("compactions/main requests = %d/%d, want bounded compaction requests followed by one main request", compactions, mainRequests)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if state.CompactionCount != compactions || runtime.SessionStatus().ContextReport.TotalInputTokens > runtime.SessionStatus().ContextReport.InputLimit {
		t.Fatalf("state/status = compactions %d, report %#v", state.CompactionCount, runtime.SessionStatus().ContextReport)
	}
}

func TestRuntimeCompactsUntilRequestTooLargeRecovers(t *testing.T) {
	journal := &memoryJournal{}
	seedRuntime := newTestRuntime(t, Config{
		Backend: modelFunc(func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "old answer"}}, StopReason: "stop"}, nil
		}),
		Journal: journal,
	})
	oldInput := strings.Repeat("old context ", 3_000)
	for range 18 {
		if _, err := seedRuntime.Run(context.Background(), oldInput, nil); err != nil {
			t.Fatal(err)
		}
	}

	mainAttempts := 0
	compactions := 0
	const requestLimit = 250 * 1024
	var mainRequestSizes []int
	model := modelFunc(func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
		if isCompactionRequest(request) {
			compactions++
			if len(request.Tools) != 0 || request.Items[len(request.Items)-1].Kind != ItemUserText {
				t.Fatalf("compaction request = %#v", request)
			}
			return ModelResponse{
				Items:      []Item{{Kind: ItemAssistantText, Text: fmt.Sprintf("summary %d", compactions)}},
				StopReason: "stop",
			}, nil
		}
		mainAttempts++
		requestSize := encodedTestModelRequestBytes(t, request)
		mainRequestSizes = append(mainRequestSizes, requestSize)
		if requestSize > requestLimit {
			return ModelResponse{}, &ProviderError{
				Cause: MarkProviderFailure(fmt.Errorf("payload is %d bytes, limit is %d", requestSize, requestLimit)),
				Kind:  ProviderErrorRequestTooLarge,
			}
		}
		if request.Summary != fmt.Sprintf("summary %d", compactions) {
			t.Fatalf("recovered request summary = %q after %d compactions", request.Summary, compactions)
		}
		return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "recovered"}}, StopReason: "stop"}, nil
	})
	runtime := newTestRuntime(t, Config{Backend: model, Journal: journal})
	var events []Event
	result, err := runtime.Run(context.Background(), "new question", func(event Event) {
		assertEventCommittedAtEmission(t, journal, event)
		events = append(events, event)
	})
	if err != nil || result.Answer != "recovered" {
		t.Fatalf("result/error = %#v / %v", result, err)
	}
	if compactions != 1 || mainAttempts != 2 {
		t.Fatalf("main attempts/compactions = %d/%d", mainAttempts, compactions)
	}
	for index, size := range mainRequestSizes {
		if index > 0 && mainRequestSizes[index-1] <= size {
			t.Fatalf("main request sizes did not decrease: %v", mainRequestSizes)
		}
		if (index < len(mainRequestSizes)-1) != (size > requestLimit) {
			t.Fatalf("main request sizes = %v, limit %d", mainRequestSizes, requestLimit)
		}
	}
	records := journal.snapshot()
	if countRecordKind(records, RecordModelAttemptFailed) != compactions || countRecordKind(records, RecordContextCompacted) != compactions {
		t.Fatalf("recovery records = %#v", records)
	}
	compactionEvents := 0
	for _, event := range events {
		if event.Kind == EventContextCompacted {
			compactionEvents++
		}
	}
	if compactionEvents != compactions {
		t.Fatalf("recovery events = %#v", events)
	}
	assertAuthoritativeEvents(t, events, records)
}

func TestRuntimeStopsRequestTooLargeRecoveryWithoutCompactionBoundary(t *testing.T) {
	journal := &memoryJournal{}
	attempts := 0
	runtime := newTestRuntime(t, Config{
		Backend: modelFunc(func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			attempts++
			return ModelResponse{}, &ProviderError{
				Cause: MarkProviderFailure(errors.New("payload too large")), Kind: ProviderErrorRequestTooLarge, Retryable: true,
			}
		}),
		Journal: journal, RequestPolicy: ModelRequestPolicy{MaxAttempts: 3},
	})
	result, err := runtime.Run(context.Background(), "only active block", nil)
	if err == nil || result.Status != RunFailed || !errors.Is(err, ErrModelRequestTooLarge) || !strings.Contains(err.Error(), "payload too large") ||
		!strings.Contains(err.Error(), "automatic context reduction") {
		t.Fatalf("result/error = %#v / %v", result, err)
	}
	if attempts != 1 {
		t.Fatalf("model attempts = %d, want 1", attempts)
	}
	records := journal.snapshot()
	if countRecordKind(records, RecordModelAttemptFailed) != 1 || countRecordKind(records, RecordContextCompacted) != 0 ||
		countRecordKind(records, RecordRunFinished) != 1 {
		t.Fatalf("terminal records = %#v", records)
	}
}

func encodedTestModelRequestBytes(t *testing.T, request ModelRequest) int {
	t.Helper()
	type encodableRequest struct {
		SessionID         string
		ProviderEpoch     string
		Instructions      string
		Summary           string
		Items             []Item
		Tools             []ToolSpec
		StreamIdleTimeout int64
	}
	body, err := json.Marshal(encodableRequest{
		SessionID:         request.SessionID,
		ProviderEpoch:     request.ProviderEpoch,
		Instructions:      request.Instructions,
		Summary:           request.Summary,
		Items:             request.Items,
		Tools:             request.Tools,
		StreamIdleTimeout: int64(request.StreamIdleTimeout),
	})
	if err != nil {
		t.Fatal(err)
	}
	return len(body)
}

func isCompactionRequest(request ModelRequest) bool {
	if len(request.Items) == 0 {
		return false
	}
	last := request.Items[len(request.Items)-1]
	return last.Kind == ItemUserText && strings.HasSuffix(last.Text, compactionInstructions)
}

func mustCompactionRequest(t *testing.T, runtime *Runtime, state State, spec runRequestSpec, plan compactionPlan) ModelRequest {
	t.Helper()
	request, err := runtime.compactionRequest(state, spec, plan)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func itemsContainText(items []Item, text string) bool {
	for _, item := range items {
		if strings.Contains(item.Text, text) {
			return true
		}
	}
	return false
}

func TestRollingCompactionAdvancesFromPreviousBoundary(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{
		directModelResponse("answer one"),
		directModelResponse("answer two"),
		directModelResponse("answer three"),
	}}
	runtime := newTestRuntime(t, Config{Backend: model, Journal: journal})
	questions := []string{
		compactionTestText("question one", 48*1024),
		compactionTestText("question two", 48*1024),
		compactionTestText("question three", 48*1024),
		compactionTestText("question four", 48*1024),
	}
	for _, input := range questions[:3] {
		if _, err := runtime.Run(context.Background(), input, nil); err != nil {
			t.Fatal(err)
		}
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	first, err := runtime.planCompactionForModelBoundary(state, runRequestSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := commitCompaction(context.Background(), journal, first, "summary one", ModelUsage{}); err != nil {
		t.Fatal(err)
	}

	fourthModel := &scriptedModel{steps: []modelStep{directModelResponse("answer four")}}
	if _, err := newTestRuntime(t, Config{Backend: fourthModel, Journal: journal}).Run(context.Background(), questions[3], nil); err != nil {
		t.Fatal(err)
	}
	state, err = Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	second, err := runtime.planCompactionForModelBoundary(state, runRequestSpec{})
	if err != nil {
		t.Fatal(err)
	}
	request := mustCompactionRequest(t, runtime, state, runRequestSpec{}, second)
	if request.Summary != "summary one" || !itemsContainText(request.Items, "question two") ||
		itemsContainText(request.Items, "question three") || itemsContainText(request.Items, "question one") ||
		itemsContainText(request.Items, "question four") {
		t.Fatalf("rolling request = %#v", request)
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
	if len(verbatim) != 4 || verbatim[0].Text != questions[2] {
		t.Fatalf("rolling verbatim tail = %#v", verbatim)
	}
}

func TestCompactionBlockKeepsToolCallAndResultTogether(t *testing.T) {
	journal := &memoryJournal{}
	providerID := jsontext.Value(`"provider_call"`)
	model := &scriptedModel{steps: []modelStep{
		func(_ context.Context, _ ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{
				{Kind: ItemReasoning, Text: "provider-private reasoning", ProviderData: []ProviderData{{
					Kind: "responses.reasoning_item", Data: jsontext.Value(`{"encrypted_content":"opaque-secret"}`),
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
		Spec: ToolSpec{Name: "echo", InputSchema: jsontext.Value(`{"type":"object"}`)},
		Run: func(_ context.Context, arguments string) (ToolOutput, error) {
			return ToolOutput{Content: TextContent("echo result: " + arguments + strings.Repeat("x", 132*1024))}, nil
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
	plan, err := runtime.planCompactionForModelBoundary(state, runRequestSpec{})
	if err != nil {
		t.Fatal(err)
	}
	request := mustCompactionRequest(t, runtime, state, runRequestSpec{}, plan)
	var reasoning, call, result *Item
	for index := range request.Items {
		item := &request.Items[index]
		switch item.Kind {
		case ItemReasoning:
			reasoning = item
		case ItemToolCall:
			call = item
		case ItemToolResult:
			result = item
		}
	}
	if reasoning == nil || reasoning.Text != "provider-private reasoning" || len(reasoning.ProviderData) != 1 ||
		call == nil || call.ToolCall == nil || call.ToolCall.RawArguments != `{"text":"hello"}` || len(call.ToolCall.ProviderReferences) != 1 ||
		result == nil || result.ToolResult == nil || !strings.Contains(result.ToolResult.Content.Text(), "echo result") {
		t.Fatalf("cache-aligned tool block = %#v", request.Items)
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
	if _, err := runtime.Run(context.Background(), compactionTestText("one", 132*1024), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "two", nil); err != nil {
		t.Fatal(err)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := runtime.planCompactionForModelBoundary(state, runRequestSpec{})
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
	unfinishedRuntime := newTestRuntime(t, Config{
		Backend: modelFunc(func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{}, nil
		}),
		Journal: unfinished,
	})
	if _, err := unfinishedRuntime.Compact(context.Background()); err == nil || !strings.Contains(err.Error(), "unfinished") {
		t.Fatalf("unfinished compaction error = %v", err)
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
	if _, err := runtime.Run(context.Background(), compactionTestText("old question", 132*1024), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "recent question", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Compact(context.Background()); err != nil {
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
		diagnostic, err := record.decode[ModelAttemptFailedRecord]()
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
	if _, err := runtime.Run(context.Background(), compactionTestText("old question", 132*1024), nil); err != nil {
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
	plan, err := runtime.planCompactionForModelBoundary(state, runRequestSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.CoveredThroughSequence >= state.Blocks[2].StartSequence || plan.FirstVerbatimSequence > state.Blocks[2].StartSequence {
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

func compactionTestText(text string, paddingBytes int) string {
	return text + " " + strings.Repeat("x", paddingBytes)
}
