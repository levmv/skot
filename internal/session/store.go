package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/levmv/skot/agent"
	productlimits "github.com/levmv/skot/internal/limits"
)

type Store struct {
	mu             sync.Mutex
	file           *os.File
	path           string
	records        []agent.Record
	failed         error
	maxRecordBytes int
	dirty          bool
	tailRepaired   bool
}

var ErrLocked = errors.New("session is already open")

const initialJournalBufferBytes = 4 * 1024

func Open(path string) (*Store, error) {
	path = filepath.Clean(path)
	if path == "." {
		return nil, errors.New("journal path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create journal directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect journal permissions: %w", err)
	}
	if permissions := info.Mode().Perm(); permissions&0o077 != 0 {
		_ = file.Close()
		return nil, fmt.Errorf("journal permissions %04o grant access to group or other users; expected 0600 or stricter", permissions)
	}
	if err := acquireStoreLock(file); err != nil {
		_ = file.Close()
		if errors.Is(err, errStoreWouldBlock) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("lock journal: %w", err)
	}
	tailRepaired, err := repairIncompleteTail(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	records, err := readRecords(file, productlimits.MaxJournalRecordBytes)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &Store{
		file: file, path: path, records: records,
		maxRecordBytes: productlimits.MaxJournalRecordBytes,
		dirty:          tailRepaired,
		tailRepaired:   tailRepaired,
	}, nil
}

func (store *Store) Append(ctx context.Context, pending agent.PendingRecord) (agent.Record, error) {
	if err := ctx.Err(); err != nil {
		return agent.Record{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.file == nil {
		return agent.Record{}, errors.New("journal is closed")
	}
	if store.failed != nil {
		return agent.Record{}, fmt.Errorf("journal is unavailable after an earlier write failure: %w", store.failed)
	}
	if pending.Kind == "" {
		return agent.Record{}, errors.New("record kind is required")
	}
	if !json.Valid(pending.Data) {
		return agent.Record{}, errors.New("record data is not valid JSON")
	}

	record := agent.Record{
		Sequence: uint64(len(store.records) + 1),
		Time:     time.Now().UTC(),
		Kind:     pending.Kind,
		Data:     append(json.RawMessage(nil), pending.Data...),
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return agent.Record{}, fmt.Errorf("encode journal record: %w", err)
	}
	if len(encoded) > store.maxRecordBytes {
		return agent.Record{}, fmt.Errorf("journal record is %d bytes, limit is %d", len(encoded), store.maxRecordBytes)
	}
	encoded = append(encoded, '\n')
	written, err := store.file.Write(encoded)
	if written > 0 {
		store.dirty = true
	}
	if err == nil && written != len(encoded) {
		err = io.ErrShortWrite
	}
	if err != nil {
		store.failed = err
		return agent.Record{}, fmt.Errorf("persist journal record: %w", err)
	}
	store.records = append(store.records, record)
	return cloneRecord(record), nil
}

func (store *Store) Records(ctx context.Context) ([]agent.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.file == nil {
		return nil, errors.New("journal is closed")
	}
	if store.failed != nil {
		return nil, fmt.Errorf("journal is unavailable after an earlier write failure: %w", store.failed)
	}
	records := make([]agent.Record, len(store.records))
	for index, record := range store.records {
		records[index] = cloneRecord(record)
	}
	return records, nil
}

func (store *Store) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.file == nil {
		return nil
	}
	return store.closeLocked(true)
}

func (store *Store) TailRepaired() bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.tailRepaired
}

// HasUserTurn reports whether the journal contains a submitted user input.
// Inspecting the record kinds keeps the answer valid for both live and reopened
// sessions without depending on a caller's replay snapshot.
func (store *Store) HasUserTurn() bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, record := range store.records {
		if record.Kind == agent.RecordRunInputAdded {
			return true
		}
	}
	return false
}

// ClosePruningEmpty closes a newly-created managed session and removes its
// journal and directory if no record was ever written.
func (store *Store) ClosePruningEmpty() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.file == nil {
		return nil
	}
	prune := len(store.records) == 0 && store.failed == nil
	path := store.path
	err := store.closeLocked(!prune)
	if err != nil || !prune {
		return err
	}
	if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("remove empty session journal: %w", removeErr)
	}
	if removeErr := os.Remove(filepath.Dir(path)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("remove empty session directory: %w", removeErr)
	}
	return nil
}

// CloseDiscarding closes a provisional managed session and removes its
// journal regardless of whether records were written. The containing session
// directory is removed only when it is otherwise empty.
func (store *Store) CloseDiscarding() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.file == nil {
		return nil
	}
	path := store.path
	if err := store.closeLocked(false); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove provisional session journal: %w", err)
	}
	if err := os.Remove(filepath.Dir(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove provisional session directory: %w", err)
	}
	return nil
}

func (store *Store) closeLocked(syncChanges bool) error {
	var syncErr error
	if syncChanges && store.dirty {
		syncErr = store.file.Sync()
	}
	closeErr := store.file.Close()
	store.file = nil
	return errors.Join(syncErr, closeErr)
}

// repairIncompleteTail removes only a final fragment not terminated by a
// newline. Complete but malformed JSONL records remain errors in readRecords;
// repair is deliberately not a general corruption recovery mechanism.
func repairIncompleteTail(file *os.File) (bool, error) {
	info, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("stat journal before tail repair: %w", err)
	}
	size := info.Size()
	if size == 0 {
		return false, nil
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], size-1); err != nil {
		return false, fmt.Errorf("read journal terminator: %w", err)
	}
	if last[0] == '\n' {
		return false, nil
	}

	const blockBytes = 64 * 1024
	truncateAt := int64(0)
	for end := size; end > 0; {
		start := max(int64(0), end-blockBytes)
		block := make([]byte, end-start)
		if _, err := file.ReadAt(block, start); err != nil {
			return false, fmt.Errorf("scan incomplete journal tail: %w", err)
		}
		if index := bytes.LastIndexByte(block, '\n'); index >= 0 {
			truncateAt = start + int64(index) + 1
			break
		}
		end = start
	}
	if err := file.Truncate(truncateAt); err != nil {
		return false, fmt.Errorf("repair incomplete journal tail: %w", err)
	}
	return true, nil
}

func readRecords(file *os.File, maxRecordBytes int) ([]agent.Record, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek journal: %w", err)
	}
	scanner := bufio.NewScanner(file)
	initialBufferBytes := initialJournalBufferBytes
	if maxRecordBytes+1 < initialBufferBytes {
		initialBufferBytes = maxRecordBytes + 1
	}
	scanner.Buffer(make([]byte, initialBufferBytes), maxRecordBytes+1)
	var records []agent.Record
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			return nil, fmt.Errorf("empty journal record at line %d", len(records)+1)
		}
		var record agent.Record
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("decode journal record at line %d: %w", len(records)+1, err)
		}
		want := uint64(len(records) + 1)
		if record.Sequence != want {
			return nil, fmt.Errorf("journal sequence at line %d is %d, want %d", len(records)+1, record.Sequence, want)
		}
		if record.Time.IsZero() || record.Kind == "" || !json.Valid(record.Data) {
			return nil, fmt.Errorf("invalid journal record at line %d", len(records)+1)
		}
		records = append(records, cloneRecord(record))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read journal: %w", err)
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return nil, fmt.Errorf("seek journal end: %w", err)
	}
	return records, nil
}

func cloneRecord(record agent.Record) agent.Record {
	record.Data = append(json.RawMessage(nil), record.Data...)
	return record
}
