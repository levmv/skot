package session

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/levmv/skot/agent"
	productlimits "github.com/levmv/skot/internal/limits"
)

// MemoryStore is a process-local journal for one-shot sessions which cannot
// create durable external work. It deliberately implements the same lifecycle
// surface as Store so the application can discard either representation
// without special cleanup paths.
type MemoryStore struct {
	mu      sync.Mutex
	records []agent.Record
	closed  bool
}

func CreateMemory() (*MemoryStore, string, error) {
	id, err := newSessionID()
	if err != nil {
		return nil, "", err
	}
	return &MemoryStore{}, id, nil
}

func (store *MemoryStore) Append(ctx context.Context, pending agent.PendingRecord) (agent.Record, error) {
	if err := ctx.Err(); err != nil {
		return agent.Record{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return agent.Record{}, errors.New("journal is closed")
	}
	if pending.Kind == "" {
		return agent.Record{}, errors.New("record kind is required")
	}
	if !pending.Data.IsValid() {
		return agent.Record{}, errors.New("record data is not valid JSON")
	}
	record := agent.Record{
		Sequence: uint64(len(store.records) + 1),
		Time:     time.Now().UTC(),
		Kind:     pending.Kind,
		Data:     pending.Data.Clone(),
	}
	encoded, err := json.Marshal(record, json.Deterministic(true))
	if err != nil {
		return agent.Record{}, fmt.Errorf("encode journal record: %w", err)
	}
	if len(encoded) > productlimits.MaxJournalRecordBytes {
		return agent.Record{}, fmt.Errorf("journal record is %d bytes, limit is %d", len(encoded), productlimits.MaxJournalRecordBytes)
	}
	store.records = append(store.records, record)
	return cloneRecord(record), nil
}

func (store *MemoryStore) Records(ctx context.Context) ([]agent.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil, errors.New("journal is closed")
	}
	records := make([]agent.Record, len(store.records))
	for index, record := range store.records {
		records[index] = cloneRecord(record)
	}
	return records, nil
}

func (store *MemoryStore) HasUserTurn() bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, record := range store.records {
		if record.Kind == agent.RecordRunInputAdded {
			return true
		}
	}
	return false
}

func (store *MemoryStore) Close() error             { return store.close(false) }
func (store *MemoryStore) ClosePruningEmpty() error { return store.close(false) }
func (store *MemoryStore) CloseDiscarding() error   { return store.close(true) }

func (store *MemoryStore) close(discard bool) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if discard {
		store.records = nil
	}
	store.closed = true
	return nil
}
