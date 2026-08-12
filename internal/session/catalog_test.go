package session

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
)

func TestCatalogListsWorkspaceSessionsAndResolvesPrefix(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "workspace")
	firstID := createCatalogSession(t, home, workspace, "  first   task  \nignored")
	time.Sleep(10 * time.Millisecond)
	secondID := createCatalogSession(t, home, workspace, strings.Repeat("д", 80))
	_ = createCatalogSession(t, home, filepath.Join(t.TempDir(), "other"), "other task")

	summaries, err := List(home, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 || summaries[0].ID != secondID || summaries[1].ID != firstID {
		t.Fatalf("summaries = %#v", summaries)
	}
	if len([]rune(summaries[0].Title)) != 72 || !strings.HasSuffix(summaries[0].Title, "…") {
		t.Fatalf("truncated title = %q", summaries[0].Title)
	}
	resolved, err := Resolve(home, workspace, ShortID(firstID))
	if err != nil || resolved.ID != firstID || resolved.Title != "first task" {
		t.Fatalf("resolved = %#v, err = %v", resolved, err)
	}
	if got := ShortID(firstID); strings.HasPrefix(got, "session_") || len(got) != 12 {
		t.Fatalf("ShortID() = %q", got)
	}
	latest, err := Resolve(home, workspace, "")
	if err != nil || latest.ID != secondID {
		t.Fatalf("latest = %#v, err = %v", latest, err)
	}
}

func TestCatalogRejectsCrossWorkspaceAndAmbiguousPrefix(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	firstID := createCatalogSession(t, home, workspace, "one")
	secondID := createCatalogSession(t, home, workspace, "two")
	common := "session_"
	if !strings.HasPrefix(firstID, common) || !strings.HasPrefix(secondID, common) {
		t.Fatal("unexpected generated IDs")
	}
	if _, err := Resolve(home, workspace, common); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous Resolve() error = %v", err)
	}
	if _, err := Resolve(home, t.TempDir(), firstID); err == nil || !strings.Contains(err.Error(), "no session") {
		t.Fatalf("cross-workspace Resolve() error = %v", err)
	}
}

func TestCatalogSkipsUnsupportedSessionSchema(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	store, id, err := Create(home)
	if err != nil {
		t.Fatal(err)
	}
	appendCatalogRecord(t, store, agent.RecordSessionStarted, agent.SessionStartedRecord{
		SchemaVersion: agent.JournalSchemaVersion + 1,
		SessionID:     id,
		Workspace:     workspace,
	})
	appendCatalogRecord(t, store, agent.RecordRunInputAdded, agent.RunInputAddedRecord{RunID: "future-run", Text: "future session"})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(home, workspace, ShortID(id)); err == nil || !strings.Contains(err.Error(), "no session") {
		t.Fatalf("Resolve() error = %v", err)
	}
}

func createCatalogSession(t *testing.T, home, workspace, input string) string {
	t.Helper()
	store, id, err := Create(home)
	if err != nil {
		t.Fatal(err)
	}
	appendCatalogRecord(t, store, agent.RecordSessionStarted, agent.SessionStartedRecord{SchemaVersion: agent.JournalSchemaVersion, SessionID: id, Workspace: workspace})
	appendCatalogRecord(t, store, agent.RecordModelSelected, agent.ModelSelectedRecord{Backend: "test", Model: "model", Epoch: "epoch"})
	appendCatalogRecord(t, store, agent.RecordRunStarted, agent.RunStartedRecord{RunID: "run"})
	appendCatalogRecord(t, store, agent.RecordRunInputAdded, agent.RunInputAddedRecord{RunID: "run", Text: input})
	appendCatalogRecord(t, store, agent.RecordRunFinished, agent.RunFinishedRecord{RunID: "run", Status: agent.RunCompleted})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return id
}

func appendCatalogRecord(t *testing.T, store *Store, kind agent.RecordKind, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(context.Background(), agent.PendingRecord{Kind: kind, Data: data}); err != nil {
		t.Fatal(err)
	}
}
