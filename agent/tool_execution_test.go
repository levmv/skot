package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeExecutesParallelSafeCallsConcurrentlyAndCommitsInOrder(t *testing.T) {
	journal := &memoryJournal{}
	started := make(chan string, 2)
	finished := make(chan string, 2)
	release := map[string]chan struct{}{"first": make(chan struct{}), "second": make(chan struct{})}
	model := &scriptedModel{steps: []modelStep{
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{
				{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "read", RawArguments: `"first"`}},
				{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "read", RawArguments: `"second"`}},
			}}, nil
		},
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			var results []*ToolResult
			for _, item := range request.Items {
				if item.ToolResult != nil {
					results = append(results, item.ToolResult)
				}
			}
			if len(results) != 2 || results[0].Content != "first" || results[0].Error ||
				results[1].Content != "second failed" || !results[1].Error {
				t.Fatalf("model tool results = %#v", results)
			}
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "done"}}}, nil
		},
	}}
	tool := Tool{
		Spec: ToolSpec{Name: "read", InputSchema: json.RawMessage(`{"type":"string"}`), ParallelSafe: true},
		Run: func(_ context.Context, raw string) (ToolOutput, error) {
			name := strings.Trim(raw, `"`)
			started <- name
			<-release[name]
			finished <- name
			if name == "second" {
				return ToolOutput{}, errors.New("second failed")
			}
			return ToolOutput{Content: name}, nil
		},
	}
	runtime := newTestRuntime(t, Config{Model: model, Journal: journal, Tools: []Tool{tool}})
	done := make(chan runTestOutcome, 1)
	go func() {
		result, err := runtime.Run(context.Background(), "inspect", nil)
		done <- runTestOutcome{result: result, err: err}
	}()

	seen := map[string]bool{
		waitToolTestString(t, started): true,
		waitToolTestString(t, started): true,
	}
	if !seen["first"] || !seen["second"] {
		t.Fatalf("started calls = %#v", seen)
	}
	close(release["second"])
	if got := waitToolTestString(t, finished); got != "second" {
		t.Fatalf("first completed call = %q", got)
	}
	if got := countRecordKind(journal.snapshot(), RecordToolResult); got != 0 {
		t.Fatalf("out-of-order result was committed early: %d records", got)
	}
	close(release["first"])
	if got := waitToolTestString(t, finished); got != "first" {
		t.Fatalf("second completed call = %q", got)
	}
	outcome := waitToolTestRun(t, done)
	if outcome.err != nil || outcome.result.Status != RunCompleted || outcome.result.Answer != "done" {
		t.Fatalf("run outcome = %#v, %v", outcome.result, outcome.err)
	}

	var committed []ToolResult
	for _, record := range journal.snapshot() {
		if record.Kind != RecordToolResult {
			continue
		}
		payload, err := decodeRecord[ToolResultRecord](record)
		if err != nil {
			t.Fatal(err)
		}
		committed = append(committed, payload.Result)
	}
	if len(committed) != 2 || committed[0].Content != "first" || committed[1].Content != "second failed" {
		t.Fatalf("committed results = %#v", committed)
	}
}

func TestFatalToolFailureIsJournaledBeforeRunFails(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "configured", RawArguments: `{}`}}}}, nil
		},
	}}
	tool := Tool{
		Spec: ToolSpec{Name: "configured", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, string) (ToolOutput, error) {
			return ToolOutput{}, fmt.Errorf("%w: executable disappeared", ErrToolFatal)
		},
	}
	runtime := newTestRuntime(t, Config{Model: model, Journal: journal, Tools: []Tool{tool}})
	result, err := runtime.Run(context.Background(), "run it", nil)
	if !errors.Is(err, ErrToolFatal) || result.Status != RunFailed {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	records := journal.snapshot()
	var resultSequence, finishSequence uint64
	for _, record := range records {
		switch record.Kind {
		case RecordToolResult:
			payload, decodeErr := decodeRecord[ToolResultRecord](record)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if !payload.Result.Error || !strings.Contains(payload.Result.Content, "executable disappeared") {
				t.Fatalf("tool result = %#v", payload.Result)
			}
			resultSequence = record.Sequence
		case RecordRunFinished:
			payload, decodeErr := decodeRecord[RunFinishedRecord](record)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if payload.Status != RunFailed {
				t.Fatalf("run finish = %#v", payload)
			}
			finishSequence = record.Sequence
		}
	}
	if resultSequence == 0 || finishSequence <= resultSequence {
		t.Fatalf("fatal result was not committed before finish: result=%d finish=%d", resultSequence, finishSequence)
	}
}

func TestFatalParallelToolFailureSettlesSiblingCalls(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{
				{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "configured", RawArguments: `{}`}},
				{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "slow", RawArguments: `{}`}},
			}}, nil
		},
	}}
	configured := Tool{
		Spec: ToolSpec{Name: "configured", InputSchema: json.RawMessage(`{"type":"object"}`), ParallelSafe: true},
		Run: func(context.Context, string) (ToolOutput, error) {
			return ToolOutput{}, fmt.Errorf("%w: executable disappeared", ErrToolFatal)
		},
	}
	slow := Tool{
		Spec: ToolSpec{Name: "slow", InputSchema: json.RawMessage(`{"type":"object"}`), ParallelSafe: true},
		Run: func(ctx context.Context, _ string) (ToolOutput, error) {
			<-ctx.Done()
			return ToolOutput{}, ctx.Err()
		},
	}
	runtime := newTestRuntime(t, Config{Model: model, Journal: journal, Tools: []Tool{configured, slow}})
	result, err := runtime.Run(context.Background(), "run both", nil)
	if !errors.Is(err, ErrToolFatal) || result.Status != RunFailed {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	state, replayErr := Replay(journal.snapshot())
	if replayErr != nil {
		t.Fatal(replayErr)
	}
	if len(state.PendingTools) != 0 || len(state.ActiveRuns) != 0 || countRecordKind(journal.snapshot(), RecordToolResult) != 2 {
		t.Fatalf("unsettled fatal group: pending=%#v active=%#v", state.PendingTools, state.ActiveRuns)
	}
}

func TestRuntimeKeepsUnsafeToolCallsAsSerialBarriers(t *testing.T) {
	started := make(chan string, 3)
	releaseWrite := make(chan struct{})
	model := &scriptedModel{steps: []modelStep{
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{
				{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "read", RawArguments: `"before"`}},
				{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "write", RawArguments: `{}`}},
				{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "read", RawArguments: `"after"`}},
			}}, nil
		},
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "done"}}}, nil
		},
	}}
	read := Tool{
		Spec: ToolSpec{Name: "read", InputSchema: json.RawMessage(`{"type":"string"}`), ParallelSafe: true},
		Run: func(_ context.Context, raw string) (ToolOutput, error) {
			started <- "read:" + strings.Trim(raw, `"`)
			return ToolOutput{Content: raw}, nil
		},
	}
	write := Tool{
		Spec: ToolSpec{Name: "write", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, string) (ToolOutput, error) {
			started <- "write"
			<-releaseWrite
			return ToolOutput{Content: "written"}, nil
		},
	}
	runtime := newTestRuntime(t, Config{Model: model, Journal: &memoryJournal{}, Tools: []Tool{read, write}})
	done := make(chan runTestOutcome, 1)
	go func() {
		result, err := runtime.Run(context.Background(), "change", nil)
		done <- runTestOutcome{result: result, err: err}
	}()
	if got := waitToolTestString(t, started); got != "read:before" {
		t.Fatalf("first call = %q", got)
	}
	if got := waitToolTestString(t, started); got != "write" {
		t.Fatalf("barrier call = %q", got)
	}
	select {
	case got := <-started:
		t.Fatalf("call %q crossed the unsafe barrier", got)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseWrite)
	if got := waitToolTestString(t, started); got != "read:after" {
		t.Fatalf("call after barrier = %q", got)
	}
	outcome := waitToolTestRun(t, done)
	if outcome.err != nil || outcome.result.Status != RunCompleted {
		t.Fatalf("run outcome = %#v, %v", outcome.result, outcome.err)
	}
}

func TestRuntimeBoundsParallelSafeFanout(t *testing.T) {
	const callCount = maxParallelToolCalls + 5
	started := make(chan struct{}, callCount)
	release := make(chan struct{})
	var active atomic.Int32
	var peak atomic.Int32
	calls := make([]Item, callCount)
	for index := range calls {
		calls[index] = Item{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "read", RawArguments: fmt.Sprintf("%d", index)}}
	}
	model := &scriptedModel{steps: []modelStep{
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: calls}, nil
		},
		func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "done"}}}, nil
		},
	}}
	tool := Tool{
		Spec: ToolSpec{Name: "read", InputSchema: json.RawMessage(`{"type":"integer"}`), ParallelSafe: true},
		Run: func(context.Context, string) (ToolOutput, error) {
			current := active.Add(1)
			for previous := peak.Load(); current > previous && !peak.CompareAndSwap(previous, current); previous = peak.Load() {
			}
			started <- struct{}{}
			<-release
			active.Add(-1)
			return ToolOutput{Content: "read"}, nil
		},
	}
	runtime := newTestRuntime(t, Config{Model: model, Journal: &memoryJournal{}, Tools: []Tool{tool}})
	done := make(chan runTestOutcome, 1)
	go func() {
		result, err := runtime.Run(context.Background(), "many reads", nil)
		done <- runTestOutcome{result: result, err: err}
	}()
	for range maxParallelToolCalls {
		waitToolTestSignal(t, started)
	}
	select {
	case <-started:
		t.Fatalf("more than %d tool calls became active", maxParallelToolCalls)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	outcome := waitToolTestRun(t, done)
	if outcome.err != nil || outcome.result.Status != RunCompleted {
		t.Fatalf("run outcome = %#v, %v", outcome.result, outcome.err)
	}
	if got := peak.Load(); got != maxParallelToolCalls {
		t.Fatalf("peak parallel calls = %d, want %d", got, maxParallelToolCalls)
	}
}

func TestRuntimeCancelsAllActiveParallelSafeCalls(t *testing.T) {
	started := make(chan struct{}, 2)
	stopped := make(chan struct{}, 2)
	model := &scriptedModel{steps: []modelStep{func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
		return ModelResponse{Items: []Item{
			{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "read", RawArguments: `"first"`}},
			{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "read", RawArguments: `"second"`}},
		}}, nil
	}}}
	tool := Tool{
		Spec: ToolSpec{Name: "read", InputSchema: json.RawMessage(`{"type":"string"}`), ParallelSafe: true},
		Run: func(ctx context.Context, _ string) (ToolOutput, error) {
			started <- struct{}{}
			<-ctx.Done()
			stopped <- struct{}{}
			return ToolOutput{}, ctx.Err()
		},
	}
	journal := &memoryJournal{}
	runtime := newTestRuntime(t, Config{Model: model, Journal: journal, Tools: []Tool{tool}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan runTestOutcome, 1)
	go func() {
		result, err := runtime.Run(ctx, "cancel reads", nil)
		done <- runTestOutcome{result: result, err: err}
	}()
	waitToolTestSignal(t, started)
	waitToolTestSignal(t, started)
	cancel()
	waitToolTestSignal(t, stopped)
	waitToolTestSignal(t, stopped)
	outcome := waitToolTestRun(t, done)
	if !errors.Is(outcome.err, context.Canceled) || outcome.result.Status != RunCancelled {
		t.Fatalf("run outcome = %#v, %v", outcome.result, outcome.err)
	}
	records := journal.snapshot()
	if got := countRecordKind(records, RecordToolResult); got != 2 {
		t.Fatalf("cancelled tool results = %d", got)
	}
	if len(records) < 3 || records[len(records)-3].Kind != RecordToolResult ||
		records[len(records)-2].Kind != RecordToolResult || records[len(records)-1].Kind != RecordRunFinished {
		t.Fatalf("cancelled record tail = %#v", records[max(0, len(records)-3):])
	}
	seen := make(map[string]bool)
	for _, record := range records[len(records)-3 : len(records)-1] {
		payload, err := decodeRecord[ToolResultRecord](record)
		if err != nil {
			t.Fatal(err)
		}
		if !payload.Result.Error || !payload.Result.Unknown {
			t.Fatalf("cancelled tool result = %#v", payload.Result)
		}
		if payload.Result.CallID == "" {
			t.Fatal("cancelled tool result has no call ID")
		}
		seen[payload.Result.CallID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("cancelled call IDs = %#v", seen)
	}
	state, err := Replay(records)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.ActiveRuns) != 0 || len(state.PendingTools) != 0 {
		t.Fatalf("cancelled state = %#v", state)
	}
}

type runTestOutcome struct {
	result RunResult
	err    error
}

func waitToolTestString(t *testing.T, values <-chan string) string {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for tool")
		return ""
	}
}

func waitToolTestSignal(t *testing.T, values <-chan struct{}) {
	t.Helper()
	select {
	case <-values:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for tool")
	}
}

func waitToolTestRun(t *testing.T, done <-chan runTestOutcome) runTestOutcome {
	t.Helper()
	select {
	case outcome := <-done:
		return outcome
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for run")
		return runTestOutcome{}
	}
}
