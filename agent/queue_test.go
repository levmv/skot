package agent

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	modelapi "github.com/levmv/skot/model"
)

func TestRuntimeDeliversQueuedInputFIFOAtNextModelBoundary(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var requests []modelapi.Request
	var requestsMu sync.Mutex
	model := modelFunc(func(_ context.Context, request modelapi.Request, _ func(modelapi.StreamEvent)) (modelapi.Response, error) {
		requestsMu.Lock()
		requestIndex := len(requests)
		requests = append(requests, request)
		requestsMu.Unlock()
		if requestIndex == 0 {
			close(firstStarted)
			<-releaseFirst
			return modelapi.Response{Items: []modelapi.Item{{Kind: modelapi.ItemToolCall, ToolCall: &modelapi.ToolCall{Name: "read", RawArguments: `{}`}}}}, nil
		}
		return modelapi.Response{Items: []modelapi.Item{{Kind: modelapi.ItemAssistantText, Text: "steered answer"}}, StopReason: "stop"}, nil
	})
	tool := Tool{
		Spec: modelapi.ToolSpec{Name: "read", InputSchema: jsontext.Value(`{"type":"object"}`)},
		Run: func(context.Context, string) (ToolOutput, error) {
			return ToolOutput{Content: modelapi.TextContent("contents")}, nil
		},
	}
	journal := &memoryJournal{}
	runtime := newTestRuntime(t, Config{Backend: model, Journal: journal, Tools: []Tool{tool}})
	initial := Input{Text: "start", Instructions: []Instruction{{Key: "review", Text: "Review the changes carefully."}}}
	first := Input{Text: "first steering", Instructions: []Instruction{initial.Instructions[0], {Key: "explain", Text: "Explain the tradeoffs."}}}
	second := Input{Text: "second steering", Instructions: first.Instructions}
	var eventsMu sync.Mutex
	var events []Event
	done := make(chan error, 1)
	go func() {
		_, err := runtime.RunInput(context.Background(), initial, func(event Event) {
			eventsMu.Lock()
			events = append(events, event)
			eventsMu.Unlock()
		})
		done <- err
	}()
	waitForSignal(t, firstStarted, "first model request")
	if err := runtime.QueueUserInput(first); err != nil {
		t.Fatal(err)
	}
	if err := runtime.QueueUserInput(second); err != nil {
		t.Fatal(err)
	}
	close(releaseFirst)
	if err := waitForError(t, done); err != nil {
		t.Fatal(err)
	}

	requestsMu.Lock()
	captured := append([]modelapi.Request(nil), requests...)
	requestsMu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("model requests = %d", len(captured))
	}
	items := captured[1].Items
	if len(items) < 3 || !strings.Contains(items[0].Text, initial.Text) ||
		items[len(items)-2].Kind != modelapi.ItemUserText || !strings.Contains(items[len(items)-2].Text, first.Text) ||
		items[len(items)-1].Kind != modelapi.ItemUserText || !strings.Contains(items[len(items)-1].Text, second.Text) {
		t.Fatalf("second request items = %#v", items)
	}
	review, explain := initial.Instructions[0].Text, first.Instructions[1].Text
	if !strings.Contains(items[0].Text, review) || !strings.Contains(items[len(items)-2].Text, explain) ||
		itemsContainText(items[len(items)-2:], review) || itemsContainText(items[len(items)-1:], explain) {
		t.Fatal("queued instructions were lost or repeated")
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Blocks) != 3 || InputText(state.Blocks[1].Entries[0].Item) != first.Text || InputText(state.Blocks[2].Entries[0].Item) != second.Text {
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
			liveReferences := InputReferences(modelapi.Item{Kind: modelapi.ItemUserText, Details: event.Details})
			replayedReferences := InputReferences(state.Blocks[delivered].Entries[0].Item)
			if len(liveReferences) != 2 || !slices.Equal(liveReferences, replayedReferences) {
				t.Fatal("queued selections differ between delivery and replay")
			}
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
		info: modelapi.Info{BackendID: "test", Provider: "test", Model: "test", ContextWindow: 20 * 1024},
		steps: []modelStep{
			func(_ context.Context, _ modelapi.Request, _ func(modelapi.StreamEvent)) (modelapi.Response, error) {
				close(firstStarted)
				<-releaseFirst
				return modelapi.Response{Items: []modelapi.Item{{Kind: modelapi.ItemToolCall, ToolCall: &modelapi.ToolCall{Name: "inspect", RawArguments: `{}`}}}}, nil
			},
			func(_ context.Context, request modelapi.Request, _ func(modelapi.StreamEvent)) (modelapi.Response, error) {
				if !isCompactionRequest(request) || !itemsContainText(request.Items, "old context") ||
					!itemsContainText(request.Items, "recent context") || itemsContainText(request.Items, "current work") ||
					itemsContainText(request.Items, "queued context") {
					t.Fatalf("model-boundary compaction request = %#v", request)
				}
				return modelapi.Response{Items: []modelapi.Item{{Kind: modelapi.ItemAssistantText, Text: "older work summarized"}}, StopReason: "stop"}, nil
			},
			func(_ context.Context, request modelapi.Request, _ func(modelapi.StreamEvent)) (modelapi.Response, error) {
				if request.Summary != "older work summarized" {
					t.Fatalf("summary = %q", request.Summary)
				}
				if len(request.Items) < 2 || request.Items[0].Text != "current work" || request.Items[len(request.Items)-1].Text != normalizedQueuedInput {
					t.Fatalf("post-compaction request = %#v", request.Items)
				}
				return modelapi.Response{Items: []modelapi.Item{{Kind: modelapi.ItemAssistantText, Text: "steered answer"}}, StopReason: "stop"}, nil
			},
		},
	}
	runtime := newTestRuntime(t, Config{
		Backend: model, Journal: journal,
		Tools: []Tool{{
			Spec: modelapi.ToolSpec{Name: "inspect", InputSchema: jsontext.Value(`{"type":"object"}`)},
			Run: func(context.Context, string) (ToolOutput, error) {
				return ToolOutput{Content: modelapi.TextContent("ok")}, nil
			},
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
	model := modelFunc(func(context.Context, modelapi.Request, func(modelapi.StreamEvent)) (modelapi.Response, error) {
		close(started)
		<-release
		return modelapi.Response{Items: []modelapi.Item{{Kind: modelapi.ItemAssistantText, Text: "done"}}, StopReason: "stop"}, nil
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
	if input, ok := runtime.ClaimQueued(); !ok || input != "second" {
		t.Fatalf("final ClaimQueued = %q, %v", input, ok)
	}
	if _, ok := runtime.PopQueued(); ok {
		t.Fatal("cleared queue still had input")
	}
}

func TestCancellationKeepsUndeliveredQueuedInput(t *testing.T) {
	started := make(chan struct{})
	model := modelFunc(func(ctx context.Context, _ modelapi.Request, _ func(modelapi.StreamEvent)) (modelapi.Response, error) {
		close(started)
		<-ctx.Done()
		return modelapi.Response{}, ctx.Err()
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
	if input, ok := runtime.ClaimQueued(); !ok || input != "restore me" {
		t.Fatalf("queued input after cancellation = %q, %v", input, ok)
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
