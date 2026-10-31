package session

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/model"
	"golang.org/x/sys/unix"
)

func TestInspectLeavesLiveAndInterruptedWorkUntouched(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	store, id, err := Create(home)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	appendCatalogRecord(t, store, agent.RecordSessionStarted, agent.SessionStartedRecord{SchemaVersion: agent.JournalSchemaVersion, SessionID: id, Workspace: workspace})
	appendCatalogRecord(t, store, agent.RecordModelSelected, model.ReplayContext{Backend: "test", Provider: "test", Model: "model", Epoch: "epoch"})
	appendCatalogRecord(t, store, agent.RecordRunStarted, agent.RunStartedRecord{RunID: "run"})
	appendCatalogRecord(t, store, agent.RecordRunInputAdded, agent.RunInputAddedRecord{RunID: "run", Text: "Inspect me"})
	appendCatalogRecord(t, store, agent.RecordModelResponse, agent.ModelResponseRecord{
		RunID: "run", Backend: "test", Model: "model", Epoch: "epoch",
		Items: []model.Item{{Kind: model.ItemToolCall, ResponseID: "response", ToolCall: &model.ToolCall{ID: "call", Name: "read", RawArguments: `{}`}}},
	})
	path := filepath.Join(home, "sessions", id, journalName)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	// Stop a writer halfway through a new record. The observer must ignore the
	// unfinished line without repairing it or reconciling the pending tool.
	data, _ := json.Marshal(agent.ToolResultRecord{RunID: "run", Result: model.ToolResult{CallID: "call", Content: model.TextContent("done")}})
	record, _ := json.Marshal(agent.Record{Sequence: 6, Time: time.Now().UTC(), Kind: agent.RecordToolResult, Data: data})
	writer, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	cut := len(record) / 2
	if _, err := writer.Write(record[:cut]); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	snapshot, err := Inspect(t.Context(), home, id)
	if err != nil || !snapshot.Summary.Locked || snapshot.State.LastSequence != 5 || len(snapshot.State.PendingTools) != 1 || len(snapshot.State.ActiveRuns) != 1 {
		t.Fatalf("live snapshot: %+v, %v", snapshot, err)
	}
	after, _ := os.ReadFile(path)
	afterInfo, _ := os.Stat(path)
	if !bytes.Equal(before, after) || !info.ModTime().Equal(afterInfo.ModTime()) || afterInfo.Mode().Perm() != 0o640 {
		t.Fatal("inspection changed the journal")
	}
	if opened, err := OpenManaged(home, id); !errors.Is(err, ErrLocked) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("inspection released the owner's lock: %v", err)
	}
	if _, err := writer.Write(append(record[cut:], '\n')); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err = Inspect(t.Context(), home, id)
	if err != nil || snapshot.Summary.Locked || snapshot.State.LastSequence != 6 || len(snapshot.State.PendingTools) != 0 || len(snapshot.State.ActiveRuns) != 1 {
		t.Fatalf("unowned unfinished snapshot: %+v, %v", snapshot, err)
	}
	if _, err := Inspect(t.Context(), home, ShortID(id)); err == nil {
		t.Fatal("inspection accepted an ID prefix")
	}
}

func TestInspectDoesNotPreventSessionOwnership(t *testing.T) {
	home := t.TempDir()
	id := createCatalogSession(t, home, t.TempDir(), "concurrent observers")
	var stop atomic.Bool
	var readers sync.WaitGroup
	defer func() {
		stop.Store(true)
		readers.Wait()
	}()
	for range 4 {
		readers.Go(func() {
			for !stop.Load() {
				if _, err := Inspect(t.Context(), home, id); err != nil {
					t.Errorf("inspect: %v", err)
					return
				}
			}
		})
	}
	for range 2_000 {
		store, err := OpenManaged(home, id)
		if err != nil {
			t.Fatalf("passive observers prevented opening the session: %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCatalogReportsMetadataErrorsAndInspectValidatesHistory(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	good := createCatalogSession(t, home, workspace, "good")
	broken := createCatalogSession(t, home, workspace, "broken")
	path := filepath.Join(home, "sessions", broken, journalName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("not json\n")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	future, futureID, err := Create(home)
	if err != nil {
		t.Fatal(err)
	}
	appendCatalogRecord(t, future, agent.RecordSessionStarted, agent.SessionStartedRecord{SchemaVersion: agent.JournalSchemaVersion + 1, SessionID: futureID, Workspace: workspace})
	future.Close()
	for id, fragment := range map[string]string{broken: "decode journal record", futureID: "unsupported journal schema"} {
		if _, err := Inspect(t.Context(), home, id); err == nil || !strings.Contains(err.Error(), fragment) {
			t.Fatalf("inspect %s: %v", id, err)
		}
	}
	entries, err := InspectAll(t.Context(), home)
	if err != nil || len(entries) != 3 {
		t.Fatalf("catalog: %+v, %v", entries, err)
	}
	for _, entry := range entries {
		if (entry.ID == futureID) != (entry.Error != "") {
			t.Fatalf("wrong diagnostic: %+v", entry)
		}
	}
	local, err := List(home, workspace)
	if err != nil || !reflect.DeepEqual(local, entries) {
		t.Fatalf("workspace catalog disagrees with global catalog: %+v, %v", local, err)
	}
	for _, id := range []string{broken, futureID} {
		if err := Delete(t.Context(), home, id, nil); err != nil {
			t.Fatalf("delete unreadable session: %v", err)
		}
	}
	// A passive reader must also reject non-regular files without waiting on
	// them or changing an unrelated target's permissions.
	if err := os.Remove(filepath.Join(home, "sessions", good, journalName)); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"symlink", "fifo"} {
		path := filepath.Join(home, "sessions", good, journalName)
		if kind == "symlink" {
			err = os.Symlink(filepath.Join(t.TempDir(), "outside"), path)
		} else {
			err = unix.Mkfifo(path, 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Inspect(t.Context(), home, good); err == nil {
			t.Fatalf("accepted %s journal", kind)
		}
		os.Remove(path)
	}
}

func TestDeleteCleansUpWithoutParentJournal(t *testing.T) {
	for _, parentExists := range []bool{false, true} {
		t.Run(fmt.Sprint(parentExists), func(t *testing.T) {
			home := t.TempDir()
			id := "session_" + strings.Repeat("a", 32)
			dir := filepath.Join(home, "sessions", id)
			if parentExists {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			failure := errors.New("cleanup failed")
			if err := Delete(t.Context(), home, id, func() error { return failure }); !errors.Is(err, failure) {
				t.Fatalf("cleanup was skipped: %v", err)
			}
			called := false
			if err := Delete(t.Context(), home, id, func() error {
				called = true
				opened, err := OpenManaged(home, id)
				if opened != nil {
					opened.Close()
				}
				if !errors.Is(err, ErrLocked) && !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("resume allowed during cleanup: %v", err)
				}
				return nil
			}); err != nil || !called {
				t.Fatalf("retry cleanup: called=%v, err=%v", called, err)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("orphaned directory remains: %v", err)
			}
		})
	}
}

func TestDeleteExcludesResumeAndRejectsAlreadyUnlinkedJournal(t *testing.T) {
	home := t.TempDir()
	id := createCatalogSession(t, home, t.TempDir(), "delete")
	path := filepath.Join(home, "sessions", id, journalName)
	// Model an opener which obtained the descriptor just before deletion.
	file, err := openManagedJournal(home, id, os.O_RDWR|os.O_APPEND)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := Delete(t.Context(), home, id, func() error {
		opened, err := OpenManaged(home, id)
		if opened != nil {
			opened.Close()
		}
		if !errors.Is(err, ErrLocked) {
			t.Fatalf("resume during deletion: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(file, path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("opened an unlinked journal: %v", err)
	}
	if _, err := OpenManaged(home, id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resume resurrected a deleted session: %v", err)
	}
	if err := Delete(t.Context(), home, id, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted directory recreated: %v", err)
	}
}
