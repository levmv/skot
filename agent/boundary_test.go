package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRuntimeJournalsCompletionBeforeDeliveryAndReplaysIt(t *testing.T) {
	journal := &memoryJournal{}
	finishedAt := time.Date(2026, 8, 10, 12, 30, 0, 0, time.UTC)
	completionAvailable := false
	delivered := false
	toolResultCommitted := false
	var sourceSession string
	var secondRequest ModelRequest
	model := &scriptedModel{steps: []modelStep{
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			for _, item := range request.Items {
				if item.Kind == ItemBoundaryText {
					t.Fatalf("completion reached the first request: %#v", request.Items)
				}
			}
			return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{Name: "start_work", RawArguments: `{}`}}}}, nil
		},
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			secondRequest = request
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "noticed completion"}}, StopReason: "stop"}, nil
		},
	}}
	tool := Tool{
		Spec: ToolSpec{Name: "start_work", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, string) (ToolOutput, error) {
			completionAvailable = true
			return ToolOutput{Content: "job started"}, nil
		},
	}
	runtime := newTestRuntime(t, Config{
		Model: model, Journal: journal, Tools: []Tool{tool},
		ExternalWork: externalWorkFuncs{pending: func(sessionID string) []BoundaryEvent {
			sourceSession = sessionID
			if !completionAvailable || delivered {
				return nil
			}
			return []BoundaryEvent{{
				JobID: "job-test", FinishedAt: finishedAt,
				Content: "Background job job-test completed: status=completed, exit_code=0.",
			}}
		}, committed: func(jobID string) {
			if jobID != "job-test" {
				t.Fatalf("delivered job = %q", jobID)
			}
			if countRecordKind(journal.snapshot(), RecordBoundaryEvent) != 1 {
				t.Fatal("completion was acknowledged before it reached the journal")
			}
			delivered = true
		}, toolCommitted: func(ToolResult) {
			if countRecordKind(journal.snapshot(), RecordToolResult) != 1 {
				t.Fatal("tool result commit hook ran before journal append")
			}
			toolResultCommitted = true
		}},
	})
	var events []Event
	if _, err := runtime.Run(context.Background(), "start it", func(event Event) {
		assertEventCommittedAtEmission(t, journal, event)
		events = append(events, event)
	}); err != nil {
		t.Fatal(err)
	}
	if sourceSession == "" || !delivered || !toolResultCommitted {
		t.Fatalf("source session/delivery/tool result = %q/%v/%v", sourceSession, delivered, toolResultCommitted)
	}
	boundaryIndex := -1
	for index, item := range secondRequest.Items {
		if item.Kind == ItemBoundaryText {
			boundaryIndex = index
			if !strings.Contains(item.Text, "job-test completed") {
				t.Fatalf("boundary content = %q", item.Text)
			}
		}
	}
	if boundaryIndex < 0 || boundaryIndex != len(secondRequest.Items)-1 {
		t.Fatalf("second request items = %#v", secondRequest.Items)
	}
	records := journal.snapshot()
	assertRecordKinds(t, records,
		RecordSessionStarted, RecordModelSelected, RecordSessionConfigured, RecordRunStarted, RecordRunInputAdded,
		RecordModelResponse, RecordToolResult, RecordBoundaryEvent, RecordModelResponse, RecordRunFinished,
	)
	payload, err := decodeRecord[BoundaryEventRecord](records[7])
	if err != nil {
		t.Fatal(err)
	}
	if payload.JobID != "job-test" || !payload.FinishedAt.Equal(finishedAt) || payload.RunID == "" {
		t.Fatalf("boundary record = %#v", payload)
	}
	state, err := Replay(records)
	if err != nil {
		t.Fatal(err)
	}
	for _, block := range state.Blocks {
		for _, entry := range block.Entries {
			if entry.Sequence == 0 || entry.Sequence > uint64(len(records)) {
				t.Fatalf("entry sequence = %d, records = %d", entry.Sequence, len(records))
			}
			if want := records[entry.Sequence-1].Time; !entry.Time.Equal(want) {
				t.Fatalf("entry %d time = %v, want %v", entry.Sequence, entry.Time, want)
			}
		}
	}
	if _, ok := state.DeliveredJobs["job-test"]; !ok {
		t.Fatalf("delivered jobs = %#v", state.DeliveredJobs)
	}
	if len(state.Items) < 1 || state.Items[len(state.Items)-2].Kind != ItemBoundaryText {
		t.Fatalf("replayed items = %#v", state.Items)
	}
	boundarySeen := false
	for _, event := range events {
		boundarySeen = boundarySeen || event.Kind == EventBoundaryDelivered && event.Sequence != 0 && strings.Contains(event.Text, "job-test completed")
	}
	if !boundarySeen {
		t.Fatalf("live events = %#v", events)
	}
	assertAuthoritativeEvents(t, events, records)

	replayModel := &scriptedModel{steps: []modelStep{func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
		found := false
		for _, item := range request.Items {
			found = found || item.Kind == ItemBoundaryText && strings.Contains(item.Text, "job-test completed")
		}
		if !found {
			t.Fatalf("replayed request lost boundary event: %#v", request.Items)
		}
		return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "still remembered"}}, StopReason: "stop"}, nil
	}}}
	if _, err := newTestRuntime(t, Config{Model: replayModel, Journal: journal}).Run(context.Background(), "continue", nil); err != nil {
		t.Fatal(err)
	}
	if countRecordKind(journal.snapshot(), RecordBoundaryEvent) != 1 {
		t.Fatalf("boundary event was duplicated: %#v", journal.snapshot())
	}
	state, err = Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planCompaction(state, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Input, "[boundary] Background job job-test completed") {
		t.Fatalf("compaction input lost boundary event: %q", plan.Input)
	}
}

func TestRuntimeWaitsForRequiredJobsBeforeAcceptingFinalResponse(t *testing.T) {
	journal := &memoryJournal{}
	completionAvailable := false
	delivered := false
	waits := 0
	model := &scriptedModel{steps: []modelStep{
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			for _, item := range request.Items {
				if item.Kind == ItemBoundaryText {
					t.Fatalf("completion reached first request: %#v", request.Items)
				}
			}
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "premature answer"}}, StopReason: "stop"}, nil
		},
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			found := false
			for _, item := range request.Items {
				found = found || item.Kind == ItemBoundaryText && strings.Contains(item.Text, "required job completed")
			}
			if !found {
				t.Fatalf("completion absent after join: %#v", request.Items)
			}
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "final answer"}}, StopReason: "stop"}, nil
		},
	}}
	runtime := newTestRuntime(t, Config{
		Model: model, Journal: journal,
		ExternalWork: externalWorkFuncs{await: func(context.Context, string) (bool, error) {
			waits++
			if waits == 1 {
				completionAvailable = true
				return true, nil
			}
			return false, nil
		}, pending: func(string) []BoundaryEvent {
			if !completionAvailable || delivered {
				return nil
			}
			return []BoundaryEvent{{JobID: "job-required", Content: "required job completed"}}
		}, committed: func(string) { delivered = true }},
	})
	result, err := runtime.Run(context.Background(), "do the work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "final answer" || waits != 2 || !delivered {
		t.Fatalf("result/waits/delivered = %#v/%d/%v", result, waits, delivered)
	}
	assertRecordKinds(t, journal.snapshot(),
		RecordSessionStarted, RecordModelSelected, RecordSessionConfigured, RecordRunStarted, RecordRunInputAdded,
		RecordModelResponse, RecordBoundaryEvent, RecordModelResponse, RecordRunFinished,
	)
}

func TestRuntimeUsesJournalAsAuthorityWhenDurableCompletionIsOfferedAgain(t *testing.T) {
	journal := &memoryJournal{}
	commits := 0
	work := externalWorkFuncs{
		pending: func(string) []BoundaryEvent {
			return []BoundaryEvent{{JobID: "job-redelivered", Content: "durable completion"}}
		},
		committed: func(jobID string) {
			if jobID != "job-redelivered" {
				t.Fatalf("committed job = %q", jobID)
			}
			commits++
		},
	}
	firstModel := &scriptedModel{steps: []modelStep{func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
		return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "first"}}, StopReason: "stop"}, nil
	}}}
	if _, err := newTestRuntime(t, Config{Model: firstModel, Journal: journal, ExternalWork: work}).Run(context.Background(), "one", nil); err != nil {
		t.Fatal(err)
	}
	if count := countRecordKind(journal.snapshot(), RecordBoundaryEvent); count != 1 {
		t.Fatalf("initial boundary count = %d", count)
	}

	secondModel := &scriptedModel{steps: []modelStep{func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
		return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "second"}}, StopReason: "stop"}, nil
	}}}
	if _, err := newTestRuntime(t, Config{Model: secondModel, Journal: journal, ExternalWork: work}).Run(context.Background(), "two", nil); err != nil {
		t.Fatal(err)
	}
	if count := countRecordKind(journal.snapshot(), RecordBoundaryEvent); count != 1 {
		t.Fatalf("redelivered boundary was duplicated: count=%d", count)
	}
	if commits < 3 {
		// First append, restart reconciliation, and rejection of the stale
		// mailbox entry each acknowledge the same idempotency key.
		t.Fatalf("completion commit callbacks = %d", commits)
	}
}

func TestRuntimeReacknowledgesJournaledToolResultsAfterRestart(t *testing.T) {
	journal := &memoryJournal{}
	toolCommits := 0
	committedCallID := ""
	work := externalWorkFuncs{toolCommitted: func(result ToolResult) {
		if committedCallID == "" {
			committedCallID = result.CallID
		} else if result.CallID != committedCallID {
			t.Fatalf("committed tool result changed identity: %#v", result)
		}
		toolCommits++
	}}
	tool := Tool{
		Spec: ToolSpec{Name: "durable_tool", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run:  func(context.Context, string) (ToolOutput, error) { return ToolOutput{Content: "complete"}, nil },
	}
	firstModel := &scriptedModel{steps: []modelStep{
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{ID: "call-durable", Name: "durable_tool", RawArguments: `{}`}}}}, nil
		},
		func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "first"}}, StopReason: "stop"}, nil
		},
	}}
	if _, err := newTestRuntime(t, Config{Model: firstModel, Journal: journal, Tools: []Tool{tool}, ExternalWork: work}).Run(context.Background(), "one", nil); err != nil {
		t.Fatal(err)
	}
	if toolCommits == 0 {
		t.Fatal("tool result was never acknowledged")
	}
	beforeRestart := toolCommits

	secondModel := &scriptedModel{steps: []modelStep{func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
		return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "second"}}, StopReason: "stop"}, nil
	}}}
	if _, err := newTestRuntime(t, Config{Model: secondModel, Journal: journal, ExternalWork: work}).Run(context.Background(), "two", nil); err != nil {
		t.Fatal(err)
	}
	if toolCommits <= beforeRestart {
		t.Fatalf("journaled tool result was not reacknowledged: before=%d after=%d", beforeRestart, toolCommits)
	}
}

func TestRunFinishedRecordsDetachedJobs(t *testing.T) {
	journal := &memoryJournal{}
	model := &scriptedModel{steps: []modelStep{func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
		return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "done"}}, StopReason: "stop"}, nil
	}}}
	work := externalWorkFuncs{detached: func(sessionID string) []string {
		if sessionID == "" {
			t.Fatal("detached jobs queried without session")
		}
		return []string{"job-detached"}
	}}
	var events []Event
	result, err := newTestRuntime(t, Config{Model: model, Journal: journal, ExternalWork: work}).Run(context.Background(), "work", func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	records := journal.snapshot()
	finished, err := decodeRecord[RunFinishedRecord](records[len(records)-1])
	if err != nil {
		t.Fatal(err)
	}
	if len(finished.DetachedJobs) != 1 || finished.DetachedJobs[0] != "job-detached" {
		t.Fatalf("run finished = %#v", finished)
	}
	state, err := Replay(records)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.DetachedJobs) != 1 || state.DetachedJobs[0] != "job-detached" ||
		len(state.Blocks) != 1 || len(state.Blocks[0].DetachedJobs) != 1 {
		t.Fatalf("replayed detached state = %#v / %#v", state.DetachedJobs, state.Blocks)
	}
	if len(result.DetachedJobs) != 1 || result.DetachedJobs[0] != "job-detached" {
		t.Fatalf("run result = %#v", result)
	}
	last := events[len(events)-1]
	if last.Kind != EventRunFinished || len(last.DetachedJobs) != 1 || last.DetachedJobs[0] != "job-detached" {
		t.Fatalf("run finished event = %#v", last)
	}
}

type externalWorkFuncs struct {
	status        func(string) ([]Detail, bool)
	pending       func(string) []BoundaryEvent
	committed     func(string)
	toolCommitted func(ToolResult)
	await         func(context.Context, string) (bool, error)
	detached      func(string) []string
}

func (work externalWorkFuncs) Status(id string) ([]Detail, bool) {
	if work.status == nil {
		return nil, false
	}
	return work.status(id)
}

func (work externalWorkFuncs) PendingEvents(sessionID string) []BoundaryEvent {
	if work.pending == nil {
		return nil
	}
	return work.pending(sessionID)
}

func (work externalWorkFuncs) EventCommitted(jobID string) {
	if work.committed != nil {
		work.committed(jobID)
	}
}

func (work externalWorkFuncs) ToolResultCommitted(result ToolResult) {
	if work.toolCommitted != nil {
		work.toolCommitted(result)
	}
}

func (work externalWorkFuncs) Await(ctx context.Context, sessionID string) (bool, error) {
	if work.await == nil {
		return false, nil
	}
	return work.await(ctx, sessionID)
}

func (work externalWorkFuncs) DetachedJobs(sessionID string) []string {
	if work.detached == nil {
		return nil
	}
	return work.detached(sessionID)
}
