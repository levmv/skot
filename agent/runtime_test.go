package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRuntimeDirectResponse(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{
		func(_ context.Context, request ModelRequest, emit func(ModelStreamEvent)) (ModelResponse, error) {
			if request.Instructions != "be useful" {
				t.Fatalf("instructions = %q", request.Instructions)
			}
			if request.SessionID == "" || request.ProviderEpoch == "" {
				t.Fatalf("session context = %#v", request)
			}
			emit(ModelStreamEvent{Kind: EventTextDelta, Text: "hel"})
			return ModelResponse{
				Items:      []Item{{Kind: ItemAssistantText, Text: "hello"}},
				Usage:      ModelUsage{InputTokens: 9, OutputTokens: 2, ReasoningTokens: 1, TotalTokens: 11},
				StopReason: "stop",
			}, nil
		},
	}}
	runtime := newTestRuntime(t, Config{Model: model, Journal: journal, Instructions: "be useful"})
	var events []Event

	result, err := runtime.Run(context.Background(), "say hello", func(event Event) {
		assertEventCommittedAtEmission(t, journal, event)
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "hello" || result.Status != RunCompleted || result.RunID == "" {
		t.Fatalf("result = %#v", result)
	}
	assertRecordKinds(t, journal.snapshot(), RecordSessionStarted, RecordModelSelected, RecordSessionConfigured, RecordRunStarted, RecordRunInputAdded, RecordModelResponse, RecordRunFinished)
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if state.SchemaVersion != JournalSchemaVersion {
		t.Fatalf("schema version = %d", state.SchemaVersion)
	}
	if len(state.Items) != 2 || state.Items[0].Kind != ItemUserText || state.Items[0].Text != "say hello" ||
		state.Items[1].Kind != ItemAssistantText || state.Items[1].Text != "hello" {
		t.Fatalf("replayed items = %#v", state.Items)
	}
	if len(state.ActiveRuns) != 0 || len(state.PendingTools) != 0 {
		t.Fatalf("replayed unfinished state = %#v", state)
	}
	if state.Usage.TotalTokens != 11 || state.Usage.InputTokens != 9 || state.Usage.OutputTokens != 2 || state.Usage.ReasoningTokens != 1 {
		t.Fatalf("replayed usage = %#v", state.Usage)
	}
	status := runtime.SessionStatus()
	if status.Usage != state.Usage || status.ContextReport.InstructionTokens == 0 {
		t.Fatalf("session status = %#v, state usage = %#v", status, state.Usage)
	}
	if !hasEvent(events, EventTextDelta) || events[len(events)-1].Kind != EventRunFinished {
		t.Fatalf("events = %#v", events)
	}
	assertAuthoritativeEvents(t, events, journal.snapshot())

	resumed := newTestRuntime(t, Config{Model: model, Journal: journal, Instructions: "be useful"})
	resumedState, err := resumed.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resumed.SessionStatus().Usage != resumedState.Usage {
		t.Fatalf("status was not bootstrapped by State: %#v", resumed.SessionStatus())
	}
}

func TestSessionStatusPublishesDuringActiveRun(t *testing.T) {
	toolStarted := make(chan struct{})
	releaseTool := make(chan struct{}, 1)
	t.Cleanup(func() {
		select {
		case releaseTool <- struct{}{}:
		default:
		}
	})
	model := &scriptedModel{steps: []modelStep{
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{
				Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{
					ID: "call", Name: "wait", RawArguments: `{}`,
				}}},
				Usage: ModelUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12},
			}, nil
		},
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{
				Items:      []Item{{Kind: ItemAssistantText, Text: "done"}},
				Usage:      ModelUsage{InputTokens: 3, OutputTokens: 1, TotalTokens: 4},
				StopReason: "stop",
			}, nil
		},
	}}
	tool := Tool{
		Spec: ToolSpec{Name: "wait", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, string) (ToolOutput, error) {
			close(toolStarted)
			<-releaseTool
			return ToolOutput{Content: "ready"}, nil
		},
	}
	runtime := newTestRuntime(t, Config{
		Model: model, Journal: &memoryJournal{}, Tools: []Tool{tool},
	})
	done := make(chan error, 1)
	go func() {
		_, err := runtime.Run(context.Background(), "start", nil)
		done <- err
	}()
	waitForSignal(t, toolStarted, "tool start")
	during := runtime.SessionStatus()
	if during.Usage.TotalTokens != 12 || during.ContextReport.TotalInputTokens == 0 {
		t.Fatalf("status during run = %#v", during)
	}
	releaseTool <- struct{}{}
	if err := waitForError(t, done); err != nil {
		t.Fatal(err)
	}
	after := runtime.SessionStatus()
	if after.Usage.TotalTokens != 16 || after.ContextReport.TotalInputTokens <= during.ContextReport.TotalInputTokens {
		t.Fatalf("final status = %#v, during = %#v", after, during)
	}
}

func TestRuntimeToolIterationFuseFinalizesWithoutTools(t *testing.T) {
	journal := &memoryJournal{}
	var executed int
	model := &scriptedModel{steps: []modelStep{
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			if len(request.Tools) != 1 {
				t.Fatalf("first request tools = %#v", request.Tools)
			}
			return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "inspect", RawArguments: `{"step":1}`}}}}, nil
		},
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			if got := request.Items[len(request.Items)-1]; got.Kind != ItemToolResult || got.ToolResult == nil || got.ToolResult.Error {
				t.Fatalf("first tool result = %#v", got)
			}
			return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "inspect", RawArguments: `{"step":2}`}}}}, nil
		},
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			if got := request.Items[len(request.Items)-1]; got.Kind != ItemToolResult || got.ToolResult == nil || got.ToolResult.Error {
				t.Fatalf("second tool result = %#v", got)
			}
			return ModelResponse{Items: []Item{
				{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "inspect", RawArguments: `{"step":3}`}},
				{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "inspect", RawArguments: `{"step":4}`}},
			}}, nil
		},
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			if len(request.Tools) != 0 {
				t.Fatalf("final request tools = %#v", request.Tools)
			}
			if len(request.Items) < 3 || request.Items[len(request.Items)-1].Kind != ItemUserText || request.Items[len(request.Items)-1].Text != toolLimitInstructions {
				t.Fatalf("final request items = %#v", request.Items)
			}
			for _, rejected := range request.Items[len(request.Items)-3 : len(request.Items)-1] {
				if rejected.Kind != ItemToolResult || rejected.ToolResult == nil || !rejected.ToolResult.Error || !strings.Contains(rejected.ToolResult.Content, "after 2 iterations") {
					t.Fatalf("rejected tool result = %#v", rejected)
				}
			}
			// A provider should not return a call when no tools were offered. If it
			// does, the finalization boundary still must not reopen the tool loop.
			return ModelResponse{Items: []Item{
				{Kind: ItemAssistantText, Text: "best effort answer"},
				{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "inspect", RawArguments: `{}`}},
			}}, nil
		},
	}}
	runtime := newTestRuntime(t, Config{
		Model: model, Journal: journal, MaxToolIterations: 2,
		Tools: []Tool{{
			Spec: ToolSpec{Name: "inspect", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Run: func(context.Context, string) (ToolOutput, error) {
				executed++
				return ToolOutput{Content: "ok"}, nil
			},
		}},
	})
	var events []Event
	result, err := runtime.Run(context.Background(), "keep inspecting", func(event Event) {
		assertEventCommittedAtEmission(t, journal, event)
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if executed != 2 {
		t.Fatalf("executed tools = %d, want 2", executed)
	}
	if result.Status != RunCompleted || result.Answer != "best effort answer" || !result.ToolLimitReached {
		t.Fatalf("result = %#v", result)
	}
	if model.next != 4 {
		t.Fatalf("model requests = %d, want 4", model.next)
	}
	records := journal.snapshot()
	state, err := Replay(records)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Blocks) != 1 || !state.Blocks[0].ToolLimitReached || state.Blocks[0].Status != RunCompleted {
		t.Fatalf("replayed block = %#v", state.Blocks)
	}
	if len(state.PendingTools) != 0 || len(state.Items) != 10 || state.Items[len(state.Items)-1].Kind != ItemAssistantText {
		t.Fatalf("replayed state = %#v", state)
	}
	finished, err := decodeRecord[RunFinishedRecord](records[len(records)-1])
	if err != nil {
		t.Fatal(err)
	}
	if !finished.ToolLimitReached || finished.Status != RunCompleted {
		t.Fatalf("finished record = %#v", finished)
	}
	rejectedEvents := 0
	for _, event := range events {
		if event.Kind == EventToolRejected {
			rejectedEvents++
		}
	}
	if rejectedEvents != 2 || !events[len(events)-1].ToolLimitReached {
		t.Fatalf("events = %#v", events)
	}
	assertAuthoritativeEvents(t, events, records)
}

func TestToolLimitFinalRequestRechecksContextCapacity(t *testing.T) {
	journal := &memoryJournal{}
	seedModel := &scriptedModel{steps: []modelStep{directModelResponse("old answer")}}
	seedRuntime := newTestRuntime(t, Config{Model: seedModel, Journal: journal})
	oldInput := strings.Repeat("old context ", 2_400)
	if _, err := seedRuntime.Run(context.Background(), oldInput, nil); err != nil {
		t.Fatal(err)
	}

	largeArguments := `{"padding":"` + strings.Repeat("x", 24*1024) + `"}`
	model := &scriptedModel{
		info: ModelInfo{Backend: "test", Provider: "test", Model: "test", ContextWindow: 20 * 1024},
		steps: []modelStep{
			func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
				return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "inspect", RawArguments: `{}`}}}}, nil
			},
			func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
				return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "inspect", RawArguments: largeArguments}}}}, nil
			},
			func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
				if request.Instructions != compactionSystemInstructions || !strings.Contains(request.Items[0].Text, "old context") {
					t.Fatalf("tool-limit compaction request = %#v", request)
				}
				return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "older work summarized"}}, StopReason: "stop"}, nil
			},
			func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
				if request.Summary != "older work summarized" || len(request.Tools) != 0 ||
					request.Items[len(request.Items)-1].Text != toolLimitInstructions {
					t.Fatalf("tool-limit final request = %#v", request)
				}
				return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "best effort"}}, StopReason: "stop"}, nil
			},
		},
	}
	runtime := newTestRuntime(t, Config{
		Model: model, Journal: journal, MaxToolIterations: 1,
		Tools: []Tool{{
			Spec: ToolSpec{Name: "inspect", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Run:  func(context.Context, string) (ToolOutput, error) { return ToolOutput{Content: "ok"}, nil },
		}},
	})
	result, err := runtime.Run(context.Background(), "current work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "best effort" || !result.ToolLimitReached || model.next != 4 {
		t.Fatalf("result/model requests = %#v/%d", result, model.next)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if state.CompactionCount != 1 || state.Compaction == nil {
		t.Fatalf("compaction state = %#v", state)
	}
}

func TestToolLimitFinalRequestDoesNotChargeOmittedToolSchemas(t *testing.T) {
	journal := &memoryJournal{}
	largeArguments := `{"padding":"` + strings.Repeat("x", 24*1024) + `"}`
	model := &scriptedModel{
		info: ModelInfo{Backend: "test", Provider: "test", Model: "test", ContextWindow: 20 * 1024},
		steps: []modelStep{
			func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
				return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "inspect", RawArguments: `{}`}}}}, nil
			},
			func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
				return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "inspect", RawArguments: largeArguments}}}}, nil
			},
			func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
				if len(request.Tools) != 0 || request.Items[len(request.Items)-1].Text != toolLimitInstructions {
					t.Fatalf("tool-limit final request = %#v", request)
				}
				return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "fits without schemas"}}, StopReason: "stop"}, nil
			},
		},
	}
	runtime := newTestRuntime(t, Config{
		Model: model, Journal: journal, MaxToolIterations: 1,
		Tools: []Tool{{
			Spec: ToolSpec{
				Name: "inspect", Description: strings.Repeat("detailed tool documentation ", 1_200),
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
			Run: func(context.Context, string) (ToolOutput, error) { return ToolOutput{Content: "ok"}, nil },
		}},
	})
	result, err := runtime.Run(context.Background(), "inspect the current state", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "fits without schemas" || !result.ToolLimitReached || model.next != 3 {
		t.Fatalf("result/model requests = %#v/%d", result, model.next)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if state.CompactionCount != 0 {
		t.Fatalf("tool-only overestimate triggered compaction: %#v", state.Compaction)
	}
}

func TestRuntimeToolIterationFuseMarksFinalizationFailure(t *testing.T) {
	journal := &memoryJournal{}
	finalErr := errors.New("provider unavailable")
	model := &scriptedModel{steps: []modelStep{
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "inspect", RawArguments: `{}`}}}}, nil
		},
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "inspect", RawArguments: `{}`}}}}, nil
		},
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{}, finalErr
		},
	}}
	runtime := newTestRuntime(t, Config{
		Model: model, Journal: journal, MaxToolIterations: 1,
		RequestPolicy: ModelRequestPolicy{MaxAttempts: 1},
		Tools: []Tool{{
			Spec: ToolSpec{Name: "inspect", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Run:  func(context.Context, string) (ToolOutput, error) { return ToolOutput{}, nil },
		}},
	})
	result, err := runtime.Run(context.Background(), "inspect", nil)
	if !errors.Is(err, finalErr) || result.Status != RunFailed || !result.ToolLimitReached {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	records := journal.snapshot()
	finished, decodeErr := decodeRecord[RunFinishedRecord](records[len(records)-1])
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if !finished.ToolLimitReached || finished.Status != RunFailed {
		t.Fatalf("finished record = %#v", finished)
	}
}

func TestRuntimeToolIterationLimits(t *testing.T) {
	newRuntime := func(limit int) (*Runtime, error) {
		return New(Config{
			Model: modelFunc(func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
				return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "done"}}}, nil
			}),
			Journal: &memoryJournal{}, MaxToolIterations: limit,
		})
	}
	defaultRuntime, err := newRuntime(0)
	if err != nil || defaultRuntime.maxToolIterations != DefaultMaxToolIterations {
		t.Fatalf("default runtime = %#v, %v", defaultRuntime, err)
	}
	unlimitedRuntime, err := newRuntime(-1)
	if err != nil || unlimitedRuntime.maxToolIterations != -1 {
		t.Fatalf("unlimited runtime = %#v, %v", unlimitedRuntime, err)
	}
	if _, err := unlimitedRuntime.Run(context.Background(), "finish", nil); err != nil {
		t.Fatal(err)
	}
	state, err := unlimitedRuntime.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Configured == nil || state.Configured.RuntimePolicy.MaxToolIterations != -1 || state.Configured.ModelContext.ToolLimitInstructions != toolLimitInstructions {
		t.Fatalf("unlimited effective configuration = %#v", state.Configured)
	}
	if _, err := newRuntime(-2); err == nil {
		t.Fatal("invalid negative tool iteration limit accepted")
	}
}

func TestRuntimePersistsPartialResponseAsIncomplete(t *testing.T) {
	journal := &memoryJournal{}
	toolRan := false
	model := &scriptedModel{steps: []modelStep{
		func(_ context.Context, _ ModelRequest, emit func(ModelStreamEvent)) (ModelResponse, error) {
			emit(ModelStreamEvent{Kind: EventTextDelta, Text: "partial answer"})
			return ModelResponse{
				Items: []Item{
					{Kind: ItemAssistantText, Text: "partial answer"},
					{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "write", RawArguments: `{}`}},
				},
				StopReason: "length",
			}, nil
		},
	}}
	runtime := newTestRuntime(t, Config{
		Model: model, Journal: journal,
		Tools: []Tool{{
			Spec: ToolSpec{Name: "write", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Run: func(context.Context, string) (ToolOutput, error) {
				toolRan = true
				return ToolOutput{}, nil
			},
		}},
	})

	result, err := runtime.Run(context.Background(), "start", nil)
	if !errors.Is(err, ErrRunIncomplete) {
		t.Fatalf("error = %v", err)
	}
	if result.Status != RunIncomplete || result.Answer != "partial answer" {
		t.Fatalf("result = %#v", result)
	}
	if toolRan {
		t.Fatal("tool from an incomplete response was executed")
	}
	records := journal.snapshot()
	assertRecordKinds(t, records, RecordSessionStarted, RecordModelSelected, RecordSessionConfigured, RecordRunStarted, RecordRunInputAdded, RecordModelResponse, RecordRunFinished)
	response, err := decodeRecord[ModelResponseRecord](records[5])
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != "length" || len(response.Items) != 1 || response.Items[0].Kind != ItemAssistantText {
		t.Fatalf("journaled partial response = %#v", response)
	}
	state, err := Replay(records)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.PendingTools) != 0 || len(state.Blocks) != 1 || state.Blocks[0].Status != RunIncomplete {
		t.Fatalf("replayed state = %#v", state)
	}
}

func TestIncompleteStopReasonsCoverTheNormalizedAdapterSet(t *testing.T) {
	for _, reason := range []string{
		"refusal", "pause_turn", "model_context_window_exceeded",
		// A Responses answer may report an incomplete status with no documented
		// cause, and DeepSeek reports its own inability to finish.
		"incomplete", "insufficient_system_resource",
	} {
		if !IsIncompleteStopReason(reason) {
			t.Errorf("stop reason %q is not incomplete", reason)
		}
	}
	for _, reason := range []string{"stop", "tool_calls"} {
		if IsIncompleteStopReason(reason) {
			t.Errorf("stop reason %q is incomplete", reason)
		}
	}
}

func TestRuntimePersistsEmptyRefusalAsIncomplete(t *testing.T) {
	journal := &memoryJournal{}
	runtime := newTestRuntime(t, Config{
		Model: &scriptedModel{steps: []modelStep{
			func(_ context.Context, _ ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
				return ModelResponse{StopReason: "refusal"}, nil
			},
		}},
		Journal: journal,
	})
	result, err := runtime.Run(context.Background(), "start", nil)
	if !errors.Is(err, ErrRunIncomplete) || result.Status != RunIncomplete || result.Answer != "" {
		t.Fatalf("result/error = %#v / %v", result, err)
	}
	records := journal.snapshot()
	response, err := decodeRecord[ModelResponseRecord](records[5])
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != "refusal" || len(response.Items) != 0 {
		t.Fatalf("journaled refusal = %#v", response)
	}
}

func TestRuntimeDoesNotOwnApplicationInstructions(t *testing.T) {
	model := &scriptedModel{steps: []modelStep{
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			if request.Instructions != "" {
				t.Fatalf("instructions = %q", request.Instructions)
			}
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "done"}}}, nil
		},
	}}
	runtime := newTestRuntime(t, Config{Model: model, Journal: &memoryJournal{}})
	if _, err := runtime.Run(context.Background(), "task", nil); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeDoesNotRetryInvalidModelRequest(t *testing.T) {
	attempts := 0
	model := modelFunc(func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
		attempts++
		return ModelResponse{}, MarkInvalidRequest(errors.New("request will remain invalid"))
	})
	runtime := newTestRuntime(t, Config{Model: model, Journal: &memoryJournal{}, RequestPolicy: ModelRequestPolicy{MaxAttempts: 3}})
	_, err := runtime.Run(context.Background(), "task", nil)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v", err)
	}
	if attempts != 1 {
		t.Fatalf("model attempts = %d, want 1", attempts)
	}
}

func TestRuntimeRetriesWithinFreshLogicalRequestBudget(t *testing.T) {
	attempts := 0
	var idleTimeout time.Duration
	model := modelFunc(func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
		attempts++
		idleTimeout = request.StreamIdleTimeout
		if attempts == 1 {
			return ModelResponse{}, MarkProviderFailure(errors.New("temporarily unavailable"))
		}
		return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "done"}}, StopReason: "stop"}, nil
	})
	runtime := newTestRuntime(t, Config{
		Model: model, Journal: &memoryJournal{},
		RequestPolicy: ModelRequestPolicy{
			MaxAttempts: -1, RetryBudget: time.Second, BaseDelay: time.Millisecond,
			MaxDelay: 2 * time.Millisecond, StreamIdleTimeout: 7 * time.Millisecond,
		},
	})
	var events []Event
	result, err := runtime.Run(context.Background(), "task", func(event Event) { events = append(events, event) })
	if err != nil || result.Answer != "done" {
		t.Fatalf("result/error = %#v / %v", result, err)
	}
	if attempts != 2 || idleTimeout != 7*time.Millisecond || !hasEvent(events, EventModelRetryScheduled) {
		t.Fatalf("attempts/idle/events = %d / %s / %#v", attempts, idleTimeout, events)
	}
}

func TestRuntimeStartsNewRetryBudgetAfterSuccessfulToolCallResponse(t *testing.T) {
	model := &scriptedModel{steps: []modelStep{
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{}, MarkProviderFailure(errors.New("first transient failure"))
		},
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "read", RawArguments: `{}`}}}, StopReason: "tool_calls"}, nil
		},
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{}, MarkProviderFailure(errors.New("second transient failure"))
		},
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "done"}}, StopReason: "stop"}, nil
		},
	}}
	runtime := newTestRuntime(t, Config{
		Model: model, Journal: &memoryJournal{},
		Tools: []Tool{{
			Spec: ToolSpec{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Run:  func(context.Context, string) (ToolOutput, error) { return ToolOutput{Content: "read"}, nil },
		}},
		RequestPolicy: ModelRequestPolicy{
			MaxAttempts: -1, RetryBudget: 80 * time.Millisecond,
			BaseDelay: 50 * time.Millisecond, MaxDelay: 50 * time.Millisecond,
		},
	})
	started := time.Now()
	result, err := runtime.Run(context.Background(), "task", nil)
	if err != nil || result.Answer != "done" || model.next != 4 {
		t.Fatalf("result/error/attempts = %#v / %v / %d", result, err, model.next)
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond {
		t.Fatalf("two independent retry delays completed in %s", elapsed)
	}
}

func TestRuntimeRequestBudgetBoundsOneHungAttempt(t *testing.T) {
	model := modelFunc(func(ctx context.Context, _ ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
		<-ctx.Done()
		return ModelResponse{}, ctx.Err()
	})
	runtime := newTestRuntime(t, Config{
		Model: model, Journal: &memoryJournal{},
		RequestPolicy: ModelRequestPolicy{
			MaxAttempts: -1, RetryBudget: 20 * time.Millisecond, BaseDelay: time.Millisecond,
		},
	})
	started := time.Now()
	_, err := runtime.Run(context.Background(), "task", nil)
	if !errors.Is(err, ErrModelRequestBudget) || !errors.Is(err, ErrProviderFailure) {
		t.Fatalf("budget error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("budget took %s", elapsed)
	}
}

func TestRuntimeDoesNotRetryNonRetryableProviderFailure(t *testing.T) {
	attempts := 0
	model := modelFunc(func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
		attempts++
		return ModelResponse{}, &ProviderError{Cause: MarkProviderFailure(errors.New("payment required")), StatusCode: 402}
	})
	runtime := newTestRuntime(t, Config{
		Model: model, Journal: &memoryJournal{},
		RequestPolicy: ModelRequestPolicy{MaxAttempts: -1, RetryBudget: time.Second, BaseDelay: time.Millisecond},
	})
	_, err := runtime.Run(context.Background(), "task", nil)
	if !errors.Is(err, ErrProviderFailure) || attempts != 1 {
		t.Fatalf("error/attempts = %v / %d", err, attempts)
	}
}

func TestRuntimeHonorsProviderRetryAfter(t *testing.T) {
	attempts := 0
	model := modelFunc(func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
		attempts++
		if attempts == 1 {
			return ModelResponse{}, &ProviderError{
				Cause: MarkProviderFailure(errors.New("rate limited")), StatusCode: 429,
				Retryable: true, RetryAfter: 20 * time.Millisecond,
			}
		}
		return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "done"}}, StopReason: "stop"}, nil
	})
	runtime := newTestRuntime(t, Config{
		Model: model, Journal: &memoryJournal{},
		RequestPolicy: ModelRequestPolicy{
			MaxAttempts: -1, RetryBudget: time.Second,
			BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond,
		},
	})
	started := time.Now()
	if _, err := runtime.Run(context.Background(), "task", nil); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond {
		t.Fatalf("Retry-After was not honored: %s", elapsed)
	}
}

func TestRuntimeRedactsKnownSecretsBeforeJournalToolsAndModel(t *testing.T) {
	const secret = "top-secret-token"
	journal := &memoryJournal{}
	var toolArguments string
	model := &scriptedModel{steps: []modelStep{
		func(_ context.Context, request ModelRequest, emit func(ModelStreamEvent)) (ModelResponse, error) {
			if strings.Contains(request.Instructions, secret) || len(request.Items) != 1 || strings.Contains(request.Items[0].Text, secret) {
				t.Fatalf("secret reached first request: %#v", request)
			}
			emit(ModelStreamEvent{Kind: EventTextDelta, Text: "stream " + secret})
			return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{
				ID: "provider-call", Name: "read", RawArguments: `{"token":"` + secret + `"}`,
			}}}}, nil
		},
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			for _, item := range request.Items {
				if item.ToolCall != nil && strings.Contains(item.ToolCall.RawArguments, secret) {
					t.Fatalf("secret reached replayed tool call: %#v", item)
				}
				if item.ToolResult != nil && strings.Contains(item.ToolResult.Content, secret) {
					t.Fatalf("secret reached tool result: %#v", item)
				}
			}
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "answer " + secret}}}, nil
		},
	}}
	runtime := newTestRuntime(t, Config{
		Model: model, Journal: journal, Instructions: "instructions " + secret,
		Sanitize: func(text string) string { return strings.ReplaceAll(text, secret, "[REDACTED]") },
		Tools: []Tool{{
			Spec: ToolSpec{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Run: func(_ context.Context, arguments string) (ToolOutput, error) {
				toolArguments = arguments
				return ToolOutput{Content: "output " + secret, Details: []Detail{{
					Kind: "test", Data: json.RawMessage(`{"failure_tail":"` + secret + `"}`),
				}}}, nil
			},
		}},
	})
	var events []Event
	result, err := runtime.Run(context.Background(), "input "+secret, func(event Event) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "answer [REDACTED]" || strings.Contains(toolArguments, secret) {
		t.Fatalf("result/tool arguments = %#v / %q", result, toolArguments)
	}
	for _, event := range events {
		if strings.Contains(event.Text, secret) || event.Result != nil && strings.Contains(event.Result.Content, secret) {
			t.Fatalf("secret reached event: %#v", event)
		}
	}
	raw, err := json.Marshal(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("secret reached journal: %s", raw)
	}
}

func TestRuntimeRejectsToolDetailsExpandedPastLimitByRedaction(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "inspect", RawArguments: `{}`}}}}, nil
		},
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			last := request.Items[len(request.Items)-1]
			if last.ToolResult == nil || !last.ToolResult.Error || len(last.ToolResult.Details) != 0 || !strings.Contains(last.ToolResult.Content, "tool details exceed size limit") {
				t.Fatalf("tool result after oversized redaction = %#v", last)
			}
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "handled"}}}, nil
		},
	}}
	detailData, err := json.Marshal(map[string]string{"value": strings.Repeat("~", 30_000)})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newTestRuntime(t, Config{
		Model: model, Journal: journal,
		Sanitize: func(text string) string { return strings.ReplaceAll(text, "~", "[REDACTED]") },
		Tools: []Tool{{
			Spec: ToolSpec{Name: "inspect", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Run: func(context.Context, string) (ToolOutput, error) {
				return ToolOutput{Content: "done", Details: []Detail{{Kind: "inspection", Data: detailData}}}, nil
			},
		}},
	})
	result, err := runtime.Run(context.Background(), "inspect", nil)
	if err != nil || result.Answer != "handled" {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if _, err := Replay(journal.snapshot()); err != nil {
		t.Fatalf("journal is not replayable: %v", err)
	}
}

func TestNormalizeToolsReturnsCanonicalOwnedCatalog(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	input := []Tool{{
		Spec: ToolSpec{Name: " read ", InputSchema: schema},
		Run:  func(context.Context, string) (ToolOutput, error) { return ToolOutput{}, nil },
	}}

	normalized, err := NormalizeTools(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != 1 || normalized[0].Spec.Name != "read" {
		t.Fatalf("normalized tools = %#v", normalized)
	}

	input[0].Spec.Name = "changed"
	input[0].Spec.InputSchema[0] = '['
	if normalized[0].Spec.Name != "read" || string(normalized[0].Spec.InputSchema) != `{"type":"object"}` {
		t.Fatalf("normalized catalog aliases input: %#v", normalized)
	}
}

func TestRuntimePersistsConfiguredSessionIdentityAndWorkspace(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			if request.SessionID != "session_fixed" {
				t.Fatalf("request session ID = %q", request.SessionID)
			}
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "done"}}}, nil
		},
	}}
	runtime := newTestRuntime(t, Config{
		Model:     model,
		Journal:   journal,
		SessionID: "session_fixed",
		Workspace: "/workspace",
	})
	if _, err := runtime.Run(context.Background(), "task", nil); err != nil {
		t.Fatal(err)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if state.SessionID != "session_fixed" || state.Workspace != "/workspace" {
		t.Fatalf("session metadata = %#v", state)
	}
}

func TestRuntimeCommitsToolCallBeforeExecutionAndUsesSkotID(t *testing.T) {
	journal := &memoryJournal{}
	providerID := "provider-call-id"
	model := &scriptedModel{steps: []modelStep{
		func(_ context.Context, _ ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{{
				Kind: ItemToolCall,
				ToolCall: &ToolCall{
					ID:           providerID,
					Name:         "echo",
					RawArguments: `{"text":"hi"}`,
					ProviderReferences: []ProviderReference{{
						Kind: "call_id",
						Data: json.RawMessage(`"provider-call-id"`),
					}},
				},
			}}}, nil
		},
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			if len(request.Items) != 3 || request.Items[1].Kind != ItemToolCall || request.Items[2].Kind != ItemToolResult {
				t.Fatalf("second request items = %#v", request.Items)
			}
			if request.Items[2].ToolResult.CallID != request.Items[1].ToolCall.ID {
				t.Fatalf("call/result identity mismatch: %#v", request.Items)
			}
			if len(request.Items[2].ToolResult.Details) != 0 {
				t.Fatalf("product details leaked into model request: %#v", request.Items[2].ToolResult.Details)
			}
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "done"}}}, nil
		},
	}}
	executed := 0
	tool := Tool{
		Spec: ToolSpec{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(_ context.Context, arguments string) (ToolOutput, error) {
			executed++
			records := journal.snapshot()
			if records[len(records)-1].Kind != RecordModelResponse {
				t.Fatalf("last record before tool execution = %q", records[len(records)-1].Kind)
			}
			return ToolOutput{Content: arguments, Details: []Detail{{
				Kind: "test_detail",
				Data: json.RawMessage(`{"value":1}`),
			}}}, nil
		},
	}
	runtime := newTestRuntime(t, Config{Model: model, Journal: journal, Tools: []Tool{tool}})

	result, err := runtime.Run(context.Background(), "use echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "done" || executed != 1 {
		t.Fatalf("result=%#v executed=%d", result, executed)
	}
	records := journal.snapshot()
	assertRecordKinds(t, records, RecordSessionStarted, RecordModelSelected, RecordSessionConfigured, RecordRunStarted, RecordRunInputAdded, RecordModelResponse, RecordToolResult, RecordModelResponse, RecordRunFinished)
	response, err := decodeRecord[ModelResponseRecord](records[5])
	if err != nil {
		t.Fatal(err)
	}
	call := response.Items[0].ToolCall
	if call.ID == "" || call.ID == providerID {
		t.Fatalf("Skot call ID = %q", call.ID)
	}
	if len(call.ProviderReferences) != 1 || call.ProviderReferences[0].Backend != response.Backend || call.ProviderReferences[0].Epoch != response.Epoch {
		t.Fatalf("provider references = %#v, response = %#v", call.ProviderReferences, response)
	}
	toolResult, err := decodeRecord[ToolResultRecord](records[6])
	if err != nil {
		t.Fatal(err)
	}
	if toolResult.Result.CallID != call.ID {
		t.Fatalf("result call ID = %q, want %q", toolResult.Result.CallID, call.ID)
	}
	if len(toolResult.Result.Details) != 1 || toolResult.Result.Details[0].Kind != "test_detail" || string(toolResult.Result.Details[0].Data) != `{"value":1}` {
		t.Fatalf("journaled details = %#v", toolResult.Result.Details)
	}
}

func TestRuntimeDiscardsFailedPartialAttempt(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{
		func(_ context.Context, _ ModelRequest, emit func(ModelStreamEvent)) (ModelResponse, error) {
			emit(ModelStreamEvent{Kind: EventTextDelta, Text: "partial"})
			return ModelResponse{}, errors.New("stream broke")
		},
		func(_ context.Context, _ ModelRequest, emit func(ModelStreamEvent)) (ModelResponse, error) {
			emit(ModelStreamEvent{Kind: EventTextDelta, Text: "accepted"})
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "accepted"}}}, nil
		},
	}}
	runtime := newTestRuntime(t, Config{Model: model, Journal: journal})
	var events []Event

	result, err := runtime.Run(context.Background(), "retry", func(event Event) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "accepted" {
		t.Fatalf("result = %#v", result)
	}
	if countRecordKind(journal.snapshot(), RecordModelResponse) != 1 {
		t.Fatalf("records = %#v", journal.snapshot())
	}
	if !hasEvent(events, EventModelAttemptDiscarded) || !hasEvent(events, EventModelRetryScheduled) {
		t.Fatalf("events = %#v", events)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range state.Items {
		if item.Text == "partial" {
			t.Fatalf("partial output was persisted: %#v", state.Items)
		}
	}
}

func TestRuntimeChangesProviderEpochAndFiltersOwnedItemsOnModelSwitch(t *testing.T) {
	journal := &memoryJournal{}
	var firstRequest ModelRequest
	firstModel := &scriptedModel{
		info: ModelInfo{Backend: "backend.a", Provider: "provider-a", Model: "alpha"},
		steps: []modelStep{func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			firstRequest = request
			return ModelResponse{Items: []Item{
				{Kind: ItemReasoning, Text: "private continuation"},
				{Kind: ItemAssistantText, Text: "first answer"},
			}}, nil
		}},
	}
	if _, err := newTestRuntime(t, Config{Model: firstModel, Journal: journal}).Run(context.Background(), "first", nil); err != nil {
		t.Fatal(err)
	}

	var secondRequest ModelRequest
	secondModel := &scriptedModel{
		info: ModelInfo{Backend: "backend.b", Provider: "provider-b", Model: "beta"},
		steps: []modelStep{func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			secondRequest = request
			for _, item := range request.Items {
				if item.Kind == ItemReasoning {
					t.Fatalf("old provider reasoning leaked after model switch: %#v", request.Items)
				}
			}
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "second answer"}}}, nil
		}},
	}
	if _, err := newTestRuntime(t, Config{Model: secondModel, Journal: journal}).Run(context.Background(), "second", nil); err != nil {
		t.Fatal(err)
	}
	if firstRequest.SessionID == "" || firstRequest.SessionID != secondRequest.SessionID {
		t.Fatalf("session IDs = %q, %q", firstRequest.SessionID, secondRequest.SessionID)
	}
	if firstRequest.ProviderEpoch == "" || firstRequest.ProviderEpoch == secondRequest.ProviderEpoch {
		t.Fatalf("provider epochs = %q, %q", firstRequest.ProviderEpoch, secondRequest.ProviderEpoch)
	}
	if countRecordKind(journal.snapshot(), RecordSessionStarted) != 1 || countRecordKind(journal.snapshot(), RecordModelSelected) != 2 {
		t.Fatalf("selection records = %#v", journal.snapshot())
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if state.Selection.Backend != "backend.b" || state.Selection.Model != "beta" || state.Selection.Epoch != secondRequest.ProviderEpoch {
		t.Fatalf("replayed selection = %#v", state.Selection)
	}
}

func TestRuntimeSwitchModelJournalsSelectionBeforeNextRun(t *testing.T) {
	journal := &memoryJournal{}
	firstModel := &scriptedModel{
		info: ModelInfo{Backend: "backend.a", Provider: "provider-a", Model: "alpha"},
		steps: []modelStep{func(_ context.Context, _ ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "first"}}}, nil
		}},
	}
	runtime := newTestRuntime(t, Config{Model: firstModel, Journal: journal})
	if _, err := runtime.Run(context.Background(), "first", nil); err != nil {
		t.Fatal(err)
	}

	var secondRequest ModelRequest
	secondModel := &scriptedModel{
		info: ModelInfo{Backend: "backend.b", Provider: "provider-b", Model: "beta", ReasoningEffort: "high"},
		steps: []modelStep{func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			secondRequest = request
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "second"}}}, nil
		}},
	}
	if err := runtime.SwitchModel(context.Background(), secondModel); err != nil {
		t.Fatal(err)
	}
	if runtime.CurrentModel() != "provider-b/beta" || runtime.CurrentReasoningEffort() != "high" {
		t.Fatalf("current model = %q, effort = %q", runtime.CurrentModel(), runtime.CurrentReasoningEffort())
	}
	if countRecordKind(journal.snapshot(), RecordModelSelected) != 2 {
		t.Fatalf("model switch was not journaled immediately: %#v", journal.snapshot())
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	switchedEpoch := state.Selection.Epoch
	if state.Selection.Backend != "backend.b" || state.Selection.ReasoningEffort != "high" || switchedEpoch == "" {
		t.Fatalf("selection = %#v", state.Selection)
	}
	if _, err := runtime.Run(context.Background(), "second", nil); err != nil {
		t.Fatal(err)
	}
	if secondRequest.ProviderEpoch != switchedEpoch || countRecordKind(journal.snapshot(), RecordModelSelected) != 2 {
		t.Fatalf("request epoch = %q, switch epoch = %q, records = %#v", secondRequest.ProviderEpoch, switchedEpoch, journal.snapshot())
	}
}

func TestRuntimeRotatesEpochWhenProviderStateContractChanges(t *testing.T) {
	journal := &memoryJournal{}
	first := &scriptedModel{
		info: ModelInfo{
			Backend: "chat_completions.test", Provider: "test", Model: "same",
			ProviderStateContract: "chat_completions.reasoning_replay.current_turn.v1",
		},
		steps: []modelStep{func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "first"}}}, nil
		}},
	}
	runtime := newTestRuntime(t, Config{Model: first, Journal: journal})
	if _, err := runtime.Run(context.Background(), "first", nil); err != nil {
		t.Fatal(err)
	}
	before, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}

	second := &scriptedModel{info: ModelInfo{
		Backend: "chat_completions.test", Provider: "test", Model: "same",
		ProviderStateContract: "chat_completions.reasoning_replay.tool_turns.v1",
	}}
	if err := runtime.SwitchModel(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	after, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if after.Selection.Epoch == before.Selection.Epoch || after.Selection.ProviderStateContract != second.info.ProviderStateContract {
		t.Fatalf("selection before/after = %#v / %#v", before.Selection, after.Selection)
	}
	if countRecordKind(journal.snapshot(), RecordModelSelected) != 2 {
		t.Fatalf("selection records = %#v", journal.snapshot())
	}
}

func TestRuntimeRequiresModelProvider(t *testing.T) {
	_, err := New(Config{
		Model:   &scriptedModel{info: ModelInfo{Backend: "chat_completions.deepseek", Model: "model"}},
		Journal: &memoryJournal{},
	})
	if err == nil || !strings.Contains(err.Error(), "provider") {
		t.Fatalf("New() error = %v", err)
	}
}

func TestRuntimeCanReplaceToolsBetweenRuns(t *testing.T) {
	model := &scriptedModel{steps: []modelStep{
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			if got := toolNames(request.Tools); got != "read,edit" {
				t.Fatalf("initial tools = %q", got)
			}
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "one"}}}, nil
		},
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			if got := toolNames(request.Tools); got != "read" {
				t.Fatalf("replacement tools = %q", got)
			}
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "two"}}}, nil
		},
	}}
	read := testRuntimeTool("read")
	edit := testRuntimeTool("edit")
	runtime := newTestRuntime(t, Config{Model: model, Journal: &memoryJournal{}, Tools: []Tool{read, edit}})
	if _, err := runtime.Run(context.Background(), "first", nil); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetTools(context.Background(), []Tool{read}, "read-only"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "second", nil); err != nil {
		t.Fatal(err)
	}
}

func testRuntimeTool(name string) Tool {
	return Tool{
		Spec: ToolSpec{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run:  func(context.Context, string) (ToolOutput, error) { return ToolOutput{}, nil },
	}
}

func toolNames(specs []ToolSpec) string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return strings.Join(names, ",")
}

func TestProjectModelItemsKeepsOnlyCurrentProviderReferences(t *testing.T) {
	items := []Item{{
		Kind:       ItemToolCall,
		ResponseID: "response_1",
		ToolCall: &ToolCall{ID: "call_1", Name: "read", ProviderReferences: []ProviderReference{
			{Kind: "call_id", Backend: "backend.a", Epoch: "epoch_a", Data: json.RawMessage(`"a"`)},
			{Kind: "call_id", Backend: "backend.b", Epoch: "epoch_b", Data: json.RawMessage(`"b"`)},
		}},
	}}
	projected := projectOwnedModelItems(cloneItems(items), ProviderContext{Backend: "backend.b", Epoch: "epoch_b"})
	if len(projected) != 1 || len(projected[0].ToolCall.ProviderReferences) != 1 || string(projected[0].ToolCall.ProviderReferences[0].Data) != `"b"` {
		t.Fatalf("projected items = %#v", projected)
	}
	if len(items[0].ToolCall.ProviderReferences) != 2 {
		t.Fatalf("source items were mutated: %#v", items)
	}
}

func TestRuntimeCancellationIsDurable(t *testing.T) {
	journal := &memoryJournal{}
	started := make(chan struct{})
	model := modelFunc(func(ctx context.Context, _ ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
		close(started)
		<-ctx.Done()
		return ModelResponse{}, ctx.Err()
	})
	runtime := newTestRuntime(t, Config{Model: model, Journal: journal})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var result RunResult
	var runErr error
	go func() {
		defer close(done)
		result, runErr = runtime.Run(ctx, "wait", nil)
	}()
	<-started
	cancel()
	<-done

	if !errors.Is(runErr, context.Canceled) || result.Status != RunCancelled {
		t.Fatalf("result=%#v err=%v", result, runErr)
	}
	records := journal.snapshot()
	assertRecordKinds(t, records, RecordSessionStarted, RecordModelSelected, RecordSessionConfigured, RecordRunStarted, RecordRunInputAdded, RecordRunFinished)
	finished, err := decodeRecord[RunFinishedRecord](records[len(records)-1])
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != RunCancelled {
		t.Fatalf("finished = %#v", finished)
	}
}

func TestRuntimeCancellationDuringToolSettlesCallBeforeRunFinishes(t *testing.T) {
	journal := &memoryJournal{}
	toolStarted := make(chan struct{})
	model := &scriptedModel{steps: []modelStep{func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
		return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{ID: "call-cancelled", Name: "wait", RawArguments: `{}`}}}}, nil
	}}}
	tool := Tool{
		Spec: ToolSpec{Name: "wait", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(ctx context.Context, _ string) (ToolOutput, error) {
			close(toolStarted)
			<-ctx.Done()
			return ToolOutput{}, ctx.Err()
		},
	}
	runtime := newTestRuntime(t, Config{Model: model, Journal: journal, Tools: []Tool{tool}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		_, runErr = runtime.Run(ctx, "wait", nil)
	}()
	<-toolStarted
	cancel()
	<-done

	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("run error = %v", runErr)
	}
	records := journal.snapshot()
	assertRecordKinds(t, records,
		RecordSessionStarted, RecordModelSelected, RecordSessionConfigured,
		RecordRunStarted, RecordRunInputAdded, RecordModelResponse,
		RecordToolResult, RecordRunFinished,
	)
	result, err := decodeRecord[ToolResultRecord](records[len(records)-2])
	if err != nil {
		t.Fatal(err)
	}
	if result.Result.CallID == "" || !result.Result.Error || !result.Result.Unknown {
		t.Fatalf("cancelled tool result = %#v", result.Result)
	}
	state, err := Replay(records)
	if err != nil || len(state.ActiveRuns) != 0 || len(state.PendingTools) != 0 {
		t.Fatalf("replayed cancelled run = %#v, %v", state, err)
	}
}

func TestReconcileRejectsFinishedRunWithPendingTool(t *testing.T) {
	journal := &memoryJournal{}
	runID := "run-cancelled"
	call := ToolCall{ID: "call-cancelled", Name: "wait", RawArguments: `{}`}
	mustAppend(t, journal, RecordSessionStarted, SessionStartedRecord{SchemaVersion: JournalSchemaVersion, SessionID: "session-cancelled"})
	mustAppend(t, journal, RecordModelSelected, ModelSelectedRecord{Backend: "test", Provider: "test", Model: "test", Epoch: "epoch-cancelled"})
	mustAppend(t, journal, RecordRunStarted, RunStartedRecord{RunID: runID})
	mustAppend(t, journal, RecordRunInputAdded, RunInputAddedRecord{RunID: runID, Text: "wait"})
	mustAppend(t, journal, RecordModelResponse, ModelResponseRecord{
		RunID: runID, Backend: "test", Model: "test", Epoch: "epoch-cancelled",
		Items: []Item{{Kind: ItemToolCall, ResponseID: "response-cancelled", ToolCall: &call}},
	})
	mustAppend(t, journal, RecordRunFinished, RunFinishedRecord{RunID: runID, Status: RunCancelled, Error: context.Canceled.Error()})

	if _, _, err := Reconcile(context.Background(), journal); err == nil || !strings.Contains(err.Error(), "with pending tool call") {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if records := journal.snapshot(); len(records) != 6 {
		t.Fatalf("reconciliation changed invalid journal: %#v", records)
	}
}

func TestReconcileRecordsUnknownWithoutExecutingTool(t *testing.T) {
	journal := &memoryJournal{}
	runID := "run_interrupted"
	call := ToolCall{ID: "call_local", Name: "write", RawArguments: `{}`}
	mustAppend(t, journal, RecordSessionStarted, SessionStartedRecord{SchemaVersion: JournalSchemaVersion, SessionID: "session_interrupted"})
	mustAppend(t, journal, RecordModelSelected, ModelSelectedRecord{Backend: "test", Provider: "test", Model: "test", Epoch: "epoch_interrupted"})
	mustAppend(t, journal, RecordRunStarted, RunStartedRecord{RunID: runID})
	mustAppend(t, journal, RecordRunInputAdded, RunInputAddedRecord{RunID: runID, Text: "change it"})
	mustAppend(t, journal, RecordModelResponse, ModelResponseRecord{
		RunID:   runID,
		Backend: "test",
		Model:   "test",
		Epoch:   "epoch_interrupted",
		Items:   []Item{{Kind: ItemToolCall, ResponseID: "response_interrupted", ToolCall: &call}},
	})

	state, records, err := Reconcile(context.Background(), journal)
	if err != nil {
		t.Fatal(err)
	}
	assertRecordKinds(t, records, RecordSessionStarted, RecordModelSelected, RecordRunStarted, RecordRunInputAdded, RecordModelResponse, RecordToolResult, RecordRunFinished)
	result, err := decodeRecord[ToolResultRecord](records[5])
	if err != nil {
		t.Fatal(err)
	}
	if !result.Result.Unknown || result.Result.CallID != call.ID {
		t.Fatalf("reconciled result = %#v", result.Result)
	}
	finished, err := decodeRecord[RunFinishedRecord](records[6])
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != RunInterrupted {
		t.Fatalf("finished = %#v", finished)
	}
	if len(state.ActiveRuns) != 0 || len(state.PendingTools) != 0 {
		t.Fatalf("unfinished state after reconciliation = %#v", state)
	}
}

type modelStep func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error)

type scriptedModel struct {
	info  ModelInfo
	steps []modelStep
	next  int
}

func (model *scriptedModel) Info() ModelInfo {
	if model.info.Backend == "" {
		return ModelInfo{Backend: "test", Provider: "test", Model: "test"}
	}
	return model.info
}

func (model *scriptedModel) Complete(ctx context.Context, request ModelRequest, emit func(ModelStreamEvent)) (ModelResponse, error) {
	if model.next >= len(model.steps) {
		return ModelResponse{}, errors.New("unexpected model request")
	}
	step := model.steps[model.next]
	model.next++
	return step(ctx, request, emit)
}

type modelFunc func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error)

func (modelFunc) Info() ModelInfo { return ModelInfo{Backend: "test", Provider: "test", Model: "test"} }

func (function modelFunc) Complete(ctx context.Context, request ModelRequest, emit func(ModelStreamEvent)) (ModelResponse, error) {
	return function(ctx, request, emit)
}

type memoryJournal struct {
	mu      sync.Mutex
	records []Record
}

type failingJournal struct {
	*memoryJournal
	failMu    sync.Mutex
	failKind  RecordKind
	remaining int
}

func (journal *failingJournal) Append(ctx context.Context, pending PendingRecord) (Record, error) {
	journal.failMu.Lock()
	if pending.Kind == journal.failKind && journal.remaining > 0 {
		journal.remaining--
		journal.failMu.Unlock()
		return Record{}, errors.New("injected journal failure")
	}
	journal.failMu.Unlock()
	return journal.memoryJournal.Append(ctx, pending)
}

func TestRuntimeJournalFailureLeavesRecoverableUnfinishedRun(t *testing.T) {
	journal := &failingJournal{
		memoryJournal: &memoryJournal{},
		failKind:      RecordModelResponse,
		remaining:     1,
	}
	runtime := newTestRuntime(t, Config{
		Journal: journal,
		Model: &scriptedModel{steps: []modelStep{func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "not committed"}}}, nil
		}}},
	})
	result, err := runtime.Run(context.Background(), "first", nil)
	if err == nil || result.RunID == "" || !strings.Contains(err.Error(), "injected journal failure") {
		t.Fatalf("failed run = %#v, %v", result, err)
	}
	if _, err := runtime.Run(context.Background(), "second", nil); err == nil ||
		!strings.Contains(err.Error(), "restart Skot") || !strings.Contains(err.Error(), "/clear") {
		t.Fatalf("unfinished-run guidance = %v", err)
	}
	if _, _, err := Reconcile(context.Background(), journal); err != nil {
		t.Fatal(err)
	}
	state, err := Replay(journal.snapshot())
	if err != nil || len(state.ActiveRuns) != 0 || len(state.PendingTools) != 0 {
		t.Fatalf("reconciled state = %#v, %v", state, err)
	}
}

func (journal *memoryJournal) Append(ctx context.Context, pending PendingRecord) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	record := Record{
		Sequence: uint64(len(journal.records) + 1),
		Time:     time.Now().UTC(),
		Kind:     pending.Kind,
		Data:     append(json.RawMessage(nil), pending.Data...),
	}
	journal.records = append(journal.records, record)
	return record, nil
}

func (journal *memoryJournal) Records(ctx context.Context) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return journal.snapshot(), nil
}

func (journal *memoryJournal) snapshot() []Record {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	records := make([]Record, len(journal.records))
	for index, record := range journal.records {
		record.Data = append(json.RawMessage(nil), record.Data...)
		records[index] = record
	}
	return records
}

func newTestRuntime(t *testing.T, config Config) *Runtime {
	t.Helper()
	runtime, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func mustAppend(t *testing.T, journal Journal, kind RecordKind, payload any) {
	t.Helper()
	if _, err := appendRecord(context.Background(), journal, kind, payload); err != nil {
		t.Fatal(err)
	}
}

func assertRecordKinds(t *testing.T, records []Record, kinds ...RecordKind) {
	t.Helper()
	if len(records) != len(kinds) {
		t.Fatalf("record count = %d, want %d: %#v", len(records), len(kinds), records)
	}
	for index, kind := range kinds {
		if records[index].Kind != kind {
			t.Fatalf("record %d kind = %q, want %q", index, records[index].Kind, kind)
		}
	}
}

func countRecordKind(records []Record, kind RecordKind) int {
	count := 0
	for _, record := range records {
		if record.Kind == kind {
			count++
		}
	}
	return count
}

func hasEvent(events []Event, kind EventKind) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

func assertEventCommittedAtEmission(t *testing.T, journal *memoryJournal, event Event) {
	t.Helper()
	if event.Sequence == 0 {
		return
	}
	for _, record := range journal.snapshot() {
		if record.Sequence == event.Sequence {
			return
		}
	}
	t.Fatalf("event %#v was emitted before sequence %d was visible", event, event.Sequence)
}

func assertAuthoritativeEvents(t *testing.T, events []Event, records []Record) {
	t.Helper()
	recordBySequence := make(map[uint64]Record, len(records))
	for _, record := range records {
		recordBySequence[record.Sequence] = record
	}
	previous := uint64(0)
	for _, event := range events {
		if event.Sequence == 0 {
			continue
		}
		if event.Sequence <= previous {
			t.Fatalf("authoritative event sequences are not strictly increasing: %d after %d", event.Sequence, previous)
		}
		expectedKind, ok := authoritativeEventRecordKind(event.Kind)
		if !ok {
			t.Fatalf("transient event %q has authoritative sequence %d", event.Kind, event.Sequence)
		}
		record, ok := recordBySequence[event.Sequence]
		if !ok {
			t.Fatalf("event %q refers to missing journal sequence %d", event.Kind, event.Sequence)
		}
		if record.Kind != expectedKind {
			t.Fatalf("event %q sequence %d refers to %q, want %q", event.Kind, event.Sequence, record.Kind, expectedKind)
		}
		previous = event.Sequence
	}
	if previous == 0 {
		t.Fatal("run emitted no authoritative events")
	}
	state, err := Replay(records)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastSequence != previous {
		t.Fatalf("state last sequence = %d, last authoritative event = %d", state.LastSequence, previous)
	}
}

func authoritativeEventRecordKind(kind EventKind) (RecordKind, bool) {
	switch kind {
	case EventRunStarted, EventQueuedInputDelivered:
		return RecordRunInputAdded, true
	case EventBoundaryDelivered:
		return RecordBoundaryEvent, true
	case EventToolFinished, EventToolRejected:
		return RecordToolResult, true
	case EventContextCompacted:
		return RecordContextCompacted, true
	case EventToolResultsPruned:
		return RecordToolResultsPruned, true
	case EventRunFinished:
		return RecordRunFinished, true
	default:
		return "", false
	}
}

func (model *scriptedModel) ProjectModelItems(items []Item) []Item { return items }

func (function modelFunc) ProjectModelItems(items []Item) []Item { return items }
