package app_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/app"
	"github.com/levmv/skot/tools"
)

func TestSessionCatalogReadResumeAndDelete(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "fake-session-test-key")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCompositionResponse(t, w, "done")
	}))
	defer provider.Close()
	home := filepath.Join(t.TempDir(), "skot")
	if sessions, err := app.ListSessions(t.Context(), home); err != nil || len(sessions) != 0 {
		t.Fatalf("missing catalog: %+v, %v", sessions, err)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("listing created a home: %v", err)
	}
	if err := app.DeleteSession(t.Context(), home, "session_00000000000000000000000000000000"); err != nil {
		t.Fatalf("delete from missing home: %v", err)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deletion created a home: %v", err)
	}
	config := app.Config{
		Home: home, Root: t.TempDir(), Interactive: true,
		ModelURI: "deepseek/session-test", ModelExplicit: true,
		BaseURL: provider.URL, ContextWindow: 32 * 1024,
		ToolSet: "none", ToolSetExplicit: true, Scope: app.ScopeMachine,
	}
	first, err := app.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := first.Run(t.Context(), "first workspace", nil); err != nil {
		t.Fatal(err)
	}
	otherConfig := config
	otherConfig.Root = t.TempDir()
	second, err := app.Open(t.Context(), otherConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := second.Run(t.Context(), "second workspace", nil); err != nil {
		t.Fatal(err)
	}
	all, err := app.ListSessions(t.Context(), home)
	if err != nil || len(all) != 2 || all[0].Workspace == all[1].Workspace {
		t.Fatalf("global catalog: %+v, %v", all, err)
	}
	local, err := first.ListSessions()
	if err != nil || len(local) != 1 || local[0].ID != first.SessionID() || !local[0].Locked {
		t.Fatalf("workspace catalog: %+v, %v", local, err)
	}
	id := first.SessionID()
	before, err := app.ReadSession(t.Context(), home, id)
	if err != nil || !before.Summary.Locked || len(before.State.Blocks) != 1 || before.State.Blocks[0].Status != agent.RunCompleted {
		t.Fatalf("read open session: %+v, %v", before, err)
	}
	if err := app.DeleteSession(t.Context(), home, id); !errors.Is(err, app.ErrSessionBusy) {
		t.Fatalf("delete open session: %v", err)
	}
	config.Resume, config.ResumePrefix = true, id
	config.Root = ""
	if opened, err := app.Open(t.Context(), config); !errors.Is(err, app.ErrSessionBusy) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("resume open session: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := app.ReadSession(t.Context(), home, id)
	if err != nil || after.Summary.Locked || after.State.LastSequence != before.State.LastSequence {
		t.Fatalf("read after close: %+v, %v", after, err)
	}
	wrongRoot := config
	wrongRoot.Root = otherConfig.Root
	if opened, err := app.Open(t.Context(), wrongRoot); err == nil {
		opened.Close()
		t.Fatal("resumed a session in a different explicit workspace")
	}
	if _, err := second.ResumeSession(t.Context(), id); err == nil {
		t.Fatal("existing application switched to another workspace")
	}
	resumed, err := app.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Root() != before.Summary.Workspace {
		resumed.Close()
		t.Fatalf("resumed workspace = %q, want %q", resumed.Root(), before.Summary.Workspace)
	}
	if _, err := resumed.Run(t.Context(), "continued from another frontend", nil); err != nil {
		resumed.Close()
		t.Fatal(err)
	}
	resumed.Close()
	after, err = app.ReadSession(t.Context(), home, id)
	if err != nil || len(after.State.Blocks) != 2 {
		t.Fatalf("continued history: %+v, %v", after, err)
	}
	workspaceFile := filepath.Join(before.Summary.Workspace, "keep.txt")
	if err := os.WriteFile(workspaceFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := app.DeleteSession(t.Context(), home, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := app.ReadSession(t.Context(), home, id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read deleted session: %v", err)
	}
	if content, err := os.ReadFile(workspaceFile); err != nil || string(content) != "keep" {
		t.Fatalf("workspace changed: %q, %v", content, err)
	}
	all, err = app.ListSessions(t.Context(), home)
	if err != nil || len(all) != 1 || all[0].ID != second.SessionID() {
		t.Fatalf("catalog after deletion: %+v, %v", all, err)
	}
}

func TestDeleteSessionRefusesDetachedWorkerWithoutStoppingIt(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "fake-session-test-key")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCompositionResponse(t, w, "done")
	}))
	defer provider.Close()
	home, root := t.TempDir(), t.TempDir()
	application, err := app.Open(t.Context(), app.Config{
		Home: home, Root: root, Interactive: true, ToolSet: "none", Scope: app.ScopeMachine,
		ModelURI: "deepseek/session-test", ModelExplicit: true,
		BaseURL: provider.URL, ContextWindow: 32 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if _, err := application.Run(t.Context(), "start", nil); err != nil {
		t.Fatal(err)
	}
	id := application.SessionID()
	manager, err := tools.NewProcessManager(root, home, t.TempDir(), tools.ScopeMachine)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx := agent.WithToolSessionID(t.Context(), id)
	catalog := manager.Tools()
	bash := catalog[slices.IndexFunc(catalog, func(tool agent.Tool) bool { return tool.Spec.Name == "bash" })]
	job := catalog[slices.IndexFunc(catalog, func(tool agent.Tool) bool { return tool.Spec.Name == "job" })]
	started, err := bash.Run(ctx, `{"command":"sleep 30","background":true,"timeout":0}`)
	if err != nil || len(manager.RunningDetachedJobs(id)) != 1 {
		t.Fatalf("start worker: %+v, %v", started, err)
	}
	jobID := manager.RunningDetachedJobs(id)[0].ID
	stop := func() error {
		_, err := job.Run(agent.WithToolSessionID(context.Background(), id), `{"action":"stop","job_id":"`+jobID+`"}`)
		return err
	}
	defer func() { _ = stop() }()
	journal := filepath.Join(home, "sessions", id, "events.jsonl")
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	if err := app.DeleteSession(t.Context(), home, id); !errors.Is(err, app.ErrSessionBusy) {
		t.Fatalf("delete with detached worker: %v", err)
	}
	if len(manager.RunningDetachedJobs(id)) != 1 {
		t.Fatal("deletion stopped the worker")
	}
	if _, err := os.Stat(journal); err != nil {
		t.Fatalf("busy deletion removed history: %v", err)
	}
	// Recovery must still check surviving jobs when the parent directory has
	// already disappeared, rather than treating deletion as finished.
	if err := os.RemoveAll(filepath.Dir(journal)); err != nil {
		t.Fatal(err)
	}
	if err := app.DeleteSession(t.Context(), home, id); !errors.Is(err, app.ErrSessionBusy) {
		t.Fatalf("delete orphaned active worker: %v", err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = app.DeleteSession(t.Context(), home, id)
		if !errors.Is(err, app.ErrSessionBusy) || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(home, "jobs")); err != nil || len(entries) != 0 {
		t.Fatalf("orphaned jobs survived deletion: %v, %v", entries, err)
	}
}
