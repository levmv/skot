package session

import (
	"context"
	"encoding/json/jsontext"
	"testing"

	"github.com/levmv/skot/agent"
)

func TestMemoryStoreImplementsJournalLifecycleWithoutFiles(t *testing.T) {
	store, id, err := CreateMemory()
	if err != nil {
		t.Fatal(err)
	}
	if !validSessionID(id) {
		t.Fatalf("memory session ID = %q", id)
	}
	record, err := store.Append(context.Background(), agent.PendingRecord{
		Kind: agent.RecordRunInputAdded,
		Data: jsontext.Value(`{"run_id":"run","text":"secret"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Sequence != 1 || !store.HasUserTurn() {
		t.Fatalf("memory journal state = %#v, user=%v", record, store.HasUserTurn())
	}
	records, err := store.Records(context.Background())
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %#v, %v", records, err)
	}
	records[0].Data[0] = 'x'
	again, err := store.Records(context.Background())
	if err != nil || string(again[0].Data) != `{"run_id":"run","text":"secret"}` {
		t.Fatalf("records alias memory store = %#v, %v", again, err)
	}
	if err := store.CloseDiscarding(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Records(context.Background()); err == nil {
		t.Fatal("closed memory journal remained readable")
	}
}
