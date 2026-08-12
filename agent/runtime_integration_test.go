package agent_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
)

// The in-memory adapters exercise a complete run and replay without involving
// the filesystem or a network model backend.
func TestRuntimeRunsWithJournalAndModelAdapters(t *testing.T) {
	journal := &memoryJournal{}
	runtime, err := agent.New(agent.Config{
		Model:        echoModel{},
		Journal:      journal,
		Instructions: "Answer briefly.",
		SessionID:    "session-public-test",
		Workspace:    "/workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	var events []agent.Event
	result, err := runtime.Run(context.Background(), "hello", func(event agent.Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "echo: hello" || result.Status != agent.RunCompleted {
		t.Fatalf("result = %#v", result)
	}
	state, err := agent.Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if state.SessionID != "session-public-test" || len(state.Items) != 2 {
		t.Fatalf("state = %#v", state)
	}
	if len(events) < 2 || events[0].Kind != agent.EventRunStarted || events[len(events)-1].Kind != agent.EventRunFinished {
		t.Fatalf("events = %#v", events)
	}
}

type echoModel struct{}

func (echoModel) Info() agent.ModelInfo {
	return agent.ModelInfo{Backend: "example", Model: "echo", ContextWindow: 128_000}
}

func (echoModel) Complete(_ context.Context, request agent.ModelRequest, emit func(agent.ModelStreamEvent)) (agent.ModelResponse, error) {
	answer := "echo: " + request.Items[len(request.Items)-1].Text
	if emit != nil {
		emit(agent.ModelStreamEvent{Kind: agent.EventTextDelta, Text: answer})
	}
	return agent.ModelResponse{
		Items:      []agent.Item{{Kind: agent.ItemAssistantText, Text: answer}},
		StopReason: "stop",
	}, nil
}

type memoryJournal struct {
	mu      sync.Mutex
	records []agent.Record
}

func (journal *memoryJournal) Append(ctx context.Context, pending agent.PendingRecord) (agent.Record, error) {
	if err := ctx.Err(); err != nil {
		return agent.Record{}, err
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	record := agent.Record{
		Sequence: uint64(len(journal.records) + 1),
		Time:     time.Now().UTC(),
		Kind:     pending.Kind,
		Data:     append(json.RawMessage(nil), pending.Data...),
	}
	journal.records = append(journal.records, record)
	return record, nil
}

func (journal *memoryJournal) Records(ctx context.Context) ([]agent.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return journal.snapshot(), nil
}

func (journal *memoryJournal) snapshot() []agent.Record {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	records := make([]agent.Record, len(journal.records))
	for index, record := range journal.records {
		record.Data = append(json.RawMessage(nil), record.Data...)
		records[index] = record
	}
	return records
}
