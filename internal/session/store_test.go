package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
)

func TestStorePersistsAndReopensRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions", "test.jsonl")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Append(context.Background(), agent.PendingRecord{
		Kind: agent.RecordRunStarted,
		Data: json.RawMessage(`{"run_id":"run_1"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Append(context.Background(), agent.PendingRecord{
		Kind: agent.RecordRunFinished,
		Data: json.RawMessage(`{"run_id":"run_1","status":"completed"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Sequence != 1 || second.Sequence != 2 {
		t.Fatalf("sequences = %d, %d", first.Sequence, second.Sequence)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode = %o", info.Mode().Perm())
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	records, err := reopened.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Kind != agent.RecordRunStarted || records[1].Kind != agent.RecordRunFinished {
		t.Fatalf("records = %#v", records)
	}
}

func TestStoreKeepsExplicitJournalPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("journal mode = %v, %v", info, err)
	}
}

func TestStoreReadsRecordLargerThanInitialScannerBuffer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("x", initialJournalBufferBytes*2)
	if _, err := store.Append(context.Background(), agent.PendingRecord{
		Kind: agent.RecordRunInputAdded,
		Data: json.RawMessage(`{"run_id":"run","text":"` + text + `"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	records, err := reopened.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var input agent.RunInputAddedRecord
	if len(records) != 1 || json.Unmarshal(records[0].Data, &input) != nil || input.Text != text {
		t.Fatalf("large record did not round-trip: records=%d text=%d", len(records), len(input.Text))
	}
}

func TestStoreRepairsOnlyIncompleteFinalRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(context.Background(), agent.PendingRecord{
		Kind: agent.RecordRunStarted,
		Data: json.RawMessage(`{"run_id":"run_1"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"sequence":2,"kind":"run_finished"`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	repaired, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !repaired.TailRepaired() {
		t.Fatal("incomplete tail was not reported as repaired")
	}
	records, err := repaired.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Sequence != 1 {
		t.Fatalf("records after repair = %#v", records)
	}
	next, err := repaired.Append(context.Background(), agent.PendingRecord{
		Kind: agent.RecordRunFinished,
		Data: json.RawMessage(`{"run_id":"run_1","status":"interrupted"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.Sequence != 2 {
		t.Fatalf("sequence after repair = %d", next.Sequence)
	}
	if err := repaired.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Fatalf("journal after repair = %q", raw)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 2 || !json.Valid([]byte(lines[0])) || !json.Valid([]byte(lines[1])) {
		t.Fatalf("journal records after repair = %#v", lines)
	}
}

func TestStoreDoesNotRepairCompleteMalformedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "decode journal record") {
		t.Fatalf("malformed complete record error = %v", err)
	}
}

func TestStoreRejectsConcurrentWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := Open(path)
	if !errors.Is(err, ErrLocked) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second Open() error = %v", err)
	}
}

func TestStoreAppliesSameRecordLimitOnAppendAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.maxRecordBytes = 96
	if _, err := store.Append(context.Background(), agent.PendingRecord{
		Kind: agent.RecordRunInputAdded,
		Data: json.RawMessage(`{"run_id":"run","text":"` + strings.Repeat("x", 128) + `"}`),
	}); err == nil || !strings.Contains(err.Error(), "journal record") {
		t.Fatalf("oversized append error = %v", err)
	}
	store.maxRecordBytes = 1024
	record, err := store.Append(context.Background(), agent.PendingRecord{
		Kind: agent.RecordRunStarted,
		Data: json.RawMessage(`{"run_id":"run"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Sequence != 1 {
		t.Fatalf("sequence after rejected append = %d", record.Sequence)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := readRecords(file, 64); err == nil {
		t.Fatalf("oversized read error = %v", err)
	}
}

func TestClosePruningEmptyRemovesOnlyEmptyManagedSession(t *testing.T) {
	home := t.TempDir()
	empty, emptyID, err := Create(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.ClosePruningEmpty(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "sessions", emptyID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty session still exists: %v", err)
	}

	nonempty, nonemptyID, err := Create(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nonempty.Append(context.Background(), agent.PendingRecord{Kind: agent.RecordSessionStarted, Data: json.RawMessage(`{"schema_version":1,"session_id":"kept"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := nonempty.ClosePruningEmpty(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "sessions", nonemptyID, journalName)); err != nil {
		t.Fatalf("nonempty session was removed: %v", err)
	}
}

func TestHasUserTurnTracksLiveAndReopenedJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if store.HasUserTurn() {
		t.Fatal("empty journal has a user turn")
	}
	for _, pending := range []agent.PendingRecord{
		{Kind: agent.RecordSessionStarted, Data: json.RawMessage(`{"schema_version":1,"session_id":"session"}`)},
		{Kind: agent.RecordRunStarted, Data: json.RawMessage(`{"run_id":"run"}`)},
	} {
		if _, err := store.Append(context.Background(), pending); err != nil {
			t.Fatal(err)
		}
	}
	if store.HasUserTurn() {
		t.Fatal("session metadata has a user turn")
	}
	if _, err := store.Append(context.Background(), agent.PendingRecord{
		Kind: agent.RecordRunInputAdded,
		Data: json.RawMessage(`{"run_id":"run","text":"hello"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if !store.HasUserTurn() {
		t.Fatal("live journal missed its user turn")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reopened.HasUserTurn() {
		t.Fatal("reopened journal missed its user turn")
	}
}

func TestCloseDiscardingRemovesNonemptyProvisionalSession(t *testing.T) {
	home := t.TempDir()
	store, id, err := Create(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(context.Background(), agent.PendingRecord{
		Kind: agent.RecordSessionStarted,
		Data: json.RawMessage(`{"schema_version":1,"session_id":"provisional"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseDiscarding(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "sessions", id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("provisional session still exists: %v", err)
	}
}
