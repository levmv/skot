package agent

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRuntimeDeliversQueuedInputFIFOAtNextModelBoundary(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var requests []ModelRequest
	var requestsMu sync.Mutex
	model := modelFunc(func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
		requestsMu.Lock()
		requestIndex := len(requests)
		requests = append(requests, request)
		requestsMu.Unlock()
		if requestIndex == 0 {
			close(firstStarted)
			<-releaseFirst
			return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "read", RawArguments: `{}`}}}}, nil
		}
		return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "steered answer"}}, StopReason: "stop"}, nil
	})
	tool := Tool{
		Spec: ToolSpec{Name: "read", InputSchema: jsontext.Value(`{"type":"object"}`)},
		Run: func(context.Context, string) (ToolOutput, error) {
			return ToolOutput{Content: TextContent("contents")}, nil
		},
	}
	journal := &memoryJournal{}
	runtime := newTestRuntime(t, Config{Backend: model, Journal: journal, Tools: []Tool{tool}})
	var eventsMu sync.Mutex
	var events []Event
	done := make(chan error, 1)
	go func() {
		_, err := runtime.Run(context.Background(), "start", func(event Event) {
			eventsMu.Lock()
			events = append(events, event)
			eventsMu.Unlock()
		})
		done <- err
	}()
	waitForSignal(t, firstStarted, "first model request")
	if err := runtime.QueueInput("first steering"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.QueueInput("second steering"); err != nil {
		t.Fatal(err)
	}
	close(releaseFirst)
	if err := waitForError(t, done); err != nil {
		t.Fatal(err)
	}

	requestsMu.Lock()
	captured := append([]ModelRequest(nil), requests...)
	requestsMu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("model requests = %d", len(captured))
	}
	items := captured[1].Items
	if len(items) < 2 || items[len(items)-2].Kind != ItemUserText || items[len(items)-2].Text != "first steering" ||
		items[len(items)-1].Kind != ItemUserText || items[len(items)-1].Text != "second steering" {
		t.Fatalf("second request items = %#v", items)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Blocks) != 3 || state.Blocks[1].Entries[0].Item.Text != "first steering" || state.Blocks[2].Entries[0].Item.Text != "second steering" {
		t.Fatalf("replayed queued blocks = %#v", state.Blocks)
	}
	if queued := runtime.QueuedInputs(); len(queued) != 0 {
		t.Fatalf("delivered queue = %#v", queued)
	}
	eventsMu.Lock()
	capturedEvents := append([]Event(nil), events...)
	delivered := 0
	for _, event := range capturedEvents {
		if event.Kind == EventQueuedInputDelivered && event.Sequence != 0 {
			delivered++
		}
	}
	eventsMu.Unlock()
	if delivered != 2 {
		t.Fatalf("delivered events = %d", delivered)
	}
	assertAuthoritativeEvents(t, capturedEvents, journal.snapshot())
}

func TestQueuedInputTriggersCompactionBeforeNextModelRequest(t *testing.T) {
	journal := &memoryJournal{}
	seedModel := &scriptedModel{steps: []modelStep{
		directModelResponse("old answer"),
		directModelResponse("recent answer"),
	}}
	seedRuntime := newTestRuntime(t, Config{Backend: seedModel, Journal: journal})
	oldInput := strings.Repeat("old context ", 1_800)
	recentInput := strings.Repeat("recent context ", 900)
	if _, err := seedRuntime.Run(context.Background(), oldInput, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := seedRuntime.Run(context.Background(), recentInput, nil); err != nil {
		t.Fatal(err)
	}

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	queuedInput := strings.Repeat("queued context ", 1_200)
	normalizedQueuedInput := strings.TrimSpace(queuedInput)
	model := &scriptedModel{
		info: ModelInfo{BackendID: "test", Provider: "test", Model: "test", ContextWindow: 20 * 1024},
		steps: []modelStep{
			func(_ context.Context, _ ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
				close(firstStarted)
				<-releaseFirst
				return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "inspect", RawArguments: `{}`}}}}, nil
			},
			func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
				if !isCompactionRequest(request) || !itemsContainText(request.Items, "old context") ||
					!itemsContainText(request.Items, "recent context") || itemsContainText(request.Items, "current work") ||
					itemsContainText(request.Items, "queued context") {
					t.Fatalf("model-boundary compaction request = %#v", request)
				}
				return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "older work summarized"}}, StopReason: "stop"}, nil
			},
			func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
				if request.Summary != "older work summarized" {
					t.Fatalf("summary = %q", request.Summary)
				}
				if len(request.Items) < 2 || request.Items[0].Text != "current work" || request.Items[len(request.Items)-1].Text != normalizedQueuedInput {
					t.Fatalf("post-compaction request = %#v", request.Items)
				}
				return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "steered answer"}}, StopReason: "stop"}, nil
			},
		},
	}
	runtime := newTestRuntime(t, Config{
		Backend: model, Journal: journal,
		Tools: []Tool{{
			Spec: ToolSpec{Name: "inspect", InputSchema: jsontext.Value(`{"type":"object"}`)},
			Run:  func(context.Context, string) (ToolOutput, error) { return ToolOutput{Content: TextContent("ok")}, nil },
		}},
	})
	done := make(chan error, 1)
	go func() {
		_, err := runtime.Run(context.Background(), "current work", nil)
		done <- err
	}()
	waitForSignal(t, firstStarted, "first model request")
	if err := runtime.QueueInput(queuedInput); err != nil {
		t.Fatal(err)
	}
	close(releaseFirst)
	if err := waitForError(t, done); err != nil {
		t.Fatal(err)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if state.CompactionCount != 1 || state.Compaction == nil || len(state.ActiveRuns) != 0 {
		t.Fatalf("replayed state = %#v", state)
	}
	if model.next != 3 {
		t.Fatalf("model requests = %d, want initial request, compaction, and steered request", model.next)
	}
}

func TestQueuedInputAfterFinalBoundaryRemainsClaimable(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	model := modelFunc(func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error) {
		close(started)
		<-release
		return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "done"}}, StopReason: "stop"}, nil
	})
	runtime := newTestRuntime(t, Config{Backend: model, Journal: &memoryJournal{}})
	done := make(chan error, 1)
	go func() {
		_, err := runtime.Run(context.Background(), "start", nil)
		done <- err
	}()
	waitForSignal(t, started, "final model request")
	for _, input := range []string{"first", "second", "third"} {
		if err := runtime.QueueInput(input); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	if err := waitForError(t, done); err != nil {
		t.Fatal(err)
	}
	report := runtime.SessionStatus().ContextReport
	if report.PendingTokens == 0 {
		t.Fatalf("queued input missing from context report: %#v", report)
	}
	if input, ok := runtime.PopQueued(); !ok || input != "third" {
		t.Fatalf("PopQueued = %q, %v", input, ok)
	}
	if input, ok := runtime.ClaimQueued(); !ok || input != "first" {
		t.Fatalf("ClaimQueued = %q, %v", input, ok)
	}
	if restored := runtime.RestoreQueued(); len(restored) != 1 || restored[0] != "second" {
		t.Fatalf("RestoreQueued = %#v", restored)
	}
	if _, ok := runtime.PopQueued(); ok {
		t.Fatal("cleared queue still had input")
	}
}

func TestCancellationKeepsUndeliveredQueuedInput(t *testing.T) {
	started := make(chan struct{})
	model := modelFunc(func(ctx context.Context, _ ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
		close(started)
		<-ctx.Done()
		return ModelResponse{}, ctx.Err()
	})
	journal := &memoryJournal{}
	runtime := newTestRuntime(t, Config{Backend: model, Journal: journal})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runtime.Run(ctx, "start", nil)
		done <- err
	}()
	waitForSignal(t, started, "cancellable model request")
	if err := runtime.QueueInput("restore me"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := waitForError(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v", err)
	}
	if restored := runtime.RestoreQueued(); len(restored) != 1 || restored[0] != "restore me" {
		t.Fatalf("restored queue = %#v", restored)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Blocks) != 1 || state.Blocks[0].Status != RunCancelled {
		t.Fatalf("cancelled state = %#v", state.Blocks)
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitForError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for run")
		return nil
	}
}
