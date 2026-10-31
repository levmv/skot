package app_test

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/levmv/skot/app"
)

// This helper uses only the public API. Keeping the executor in a real process
// exercises Unix IPC, registration locks, and kernel cleanup on SIGKILL on both
// Linux and macOS.
func TestSessionNotificationExecutor(t *testing.T) {
	if os.Getenv("SKOT_NOTIFICATION_EXECUTOR") != "1" {
		return
	}
	var application *app.Application
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var command struct{ Operation, ID string }
		err := json.Unmarshal(scanner.Bytes(), &command)
		id := command.ID
		if err == nil {
			switch command.Operation {
			case "open", "resume":
				application, err = app.Open(context.Background(), app.Config{
					Home: os.Getenv("SKOT_NOTIFICATION_HOME"), Root: os.Getenv("SKOT_NOTIFICATION_ROOT"),
					Interactive: true, DisableInteractivePreferences: true,
					ModelURI: "deepseek/session-test", ModelExplicit: true,
					BaseURL: os.Getenv("SKOT_NOTIFICATION_PROVIDER"), ContextWindow: 32 * 1024,
					ToolSet: "none", ToolSetExplicit: true, Scope: app.ScopeMachine,
					Resume: command.Operation == "resume", ResumePrefix: id,
				})
				if err == nil {
					id = application.SessionID()
				}
			case "run":
				_, err = application.Run(context.Background(), "a public session notification", nil)
			case "clear":
				id, err = application.ClearSession(context.Background())
			case "close":
				err = application.Close()
				application = nil
			case "delete":
				err = app.DeleteSession(context.Background(), os.Getenv("SKOT_NOTIFICATION_HOME"), id)
			case "quit":
				if application != nil {
					_ = application.Close()
				}
				os.Exit(0)
			default:
				err = fmt.Errorf("unknown command %q", command.Operation)
			}
		}
		reply := struct{ ID, Error string }{ID: id}
		if err != nil {
			reply.Error = err.Error()
		}
		data, _ := json.Marshal(reply)
		fmt.Println(string(data))
	}
	if application != nil {
		_ = application.Close()
	}
	os.Exit(0)
}

type notificationExecutor struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
}

func startNotificationExecutor(t *testing.T, home, root, provider string) *notificationExecutor {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSessionNotificationExecutor$")
	cmd.Env = append(os.Environ(), "SKOT_NOTIFICATION_EXECUTOR=1", "SKOT_NOTIFICATION_HOME="+home,
		"SKOT_NOTIFICATION_ROOT="+root, "SKOT_NOTIFICATION_PROVIDER="+provider, "DEEPSEEK_API_KEY=notification-test-key",
		"TMPDIR="+t.TempDir())
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return &notificationExecutor{cmd: cmd, stdin: stdin, stdout: bufio.NewScanner(stdout)}
}

func (executor *notificationExecutor) command(t *testing.T, operation, id string) string {
	t.Helper()
	data, _ := json.Marshal(struct{ Operation, ID string }{operation, id})
	if _, err := executor.stdin.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	type result struct{ ID, Error string }
	reply := make(chan result, 1)
	go func() {
		var value result
		if !executor.stdout.Scan() {
			value.Error = fmt.Sprintf("executor stopped: %v", executor.stdout.Err())
		} else if err := json.Unmarshal(executor.stdout.Bytes(), &value); err != nil {
			value.Error = err.Error()
		}
		reply <- value
	}()
	select {
	case value := <-reply:
		if value.Error != "" {
			t.Fatal(value.Error)
		}
		return value.ID
	case <-time.After(10 * time.Second):
		t.Fatal("executor command timed out: " + operation)
		return ""
	}
}

type sessionObserver struct {
	changes chan app.SessionChanges
	done    chan error
	cancel  context.CancelFunc
}

func observeSessions(t *testing.T, home string, callback func(app.SessionChanges)) *sessionObserver {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	watch := &sessionObserver{changes: make(chan app.SessionChanges, 1024), done: make(chan error, 1), cancel: cancel}
	go func() {
		watch.done <- app.WatchSessions(ctx, home, func(changes app.SessionChanges) {
			if callback != nil {
				callback(changes)
			}
			watch.changes <- changes
		})
	}()
	t.Cleanup(func() { watch.stop(t) })
	watch.wait(t, func(changes app.SessionChanges) bool { return changes.Rescan })
	return watch
}

func (watch *sessionObserver) stop(t *testing.T) {
	t.Helper()
	if watch.cancel == nil {
		return
	}
	watch.cancel()
	watch.cancel = nil
	select {
	case err := <-watch.done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("WatchSessions returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("WatchSessions did not stop")
	}
}

func (watch *sessionObserver) wait(t *testing.T, match func(app.SessionChanges) bool) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case changes := <-watch.changes:
			if match(changes) {
				return
			}
		case err := <-watch.done:
			t.Fatalf("WatchSessions stopped: %v", err)
		case <-timer.C:
			t.Fatal("session change was not delivered")
		}
	}
}

func (watch *sessionObserver) session(t *testing.T, home, id string, match func(app.SessionSnapshot, error) bool) {
	t.Helper()
	watch.wait(t, func(changes app.SessionChanges) bool {
		if !changes.Rescan && !slices.Contains(changes.IDs, id) {
			return false
		}
		snapshot, err := app.ReadSession(t.Context(), home, id)
		return match(snapshot, err)
	})
}

func notificationProvider(t *testing.T) string {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCompositionResponse(t, w, "done")
	}))
	t.Cleanup(provider.Close)
	return provider.URL
}

func TestWatchSessionsAcrossProcesses(t *testing.T) {
	// Deliberately longer than either platform's Unix socket address capacity.
	home := filepath.Join(t.TempDir(), strings.Repeat("long-home-", 16), "skot")
	root, provider := t.TempDir(), notificationProvider(t)
	executor := startNotificationExecutor(t, home, root, provider)
	id := executor.command(t, "open", "")
	executor.command(t, "run", "")
	watch := observeSessions(t, home, nil) // executor first
	second := observeSessions(t, home, nil)
	if snapshot, err := app.ReadSession(t.Context(), home, id); err != nil || !snapshot.Summary.Locked || len(snapshot.State.Blocks) != 1 {
		t.Fatalf("initial snapshot: %+v, %v", snapshot, err)
	}
	watch.stop(t)
	watch = observeSessions(t, home, nil) // subscriber restart, idle executor
	executor.command(t, "run", "")
	watch.session(t, home, id, func(s app.SessionSnapshot, err error) bool {
		return err == nil && s.Summary.Locked && len(s.State.Blocks) == 2
	})
	nextID := executor.command(t, "clear", "")
	watch.session(t, home, id, func(s app.SessionSnapshot, err error) bool {
		next, nextErr := app.ReadSession(t.Context(), home, nextID)
		return err == nil && !s.Summary.Locked && nextErr == nil && next.Summary.Locked && next.Summary.Title == ""
	})
	if catalog, err := app.ListSessions(t.Context(), home); err != nil || slices.ContainsFunc(catalog, func(s app.SessionSummary) bool { return s.ID == nextID }) {
		t.Fatalf("empty draft appeared in the catalog: %+v, %v", catalog, err)
	}
	executor.command(t, "close", "")
	watch.session(t, home, nextID, func(_ app.SessionSnapshot, err error) bool { return errors.Is(err, os.ErrNotExist) })
	executor.command(t, "delete", id) // no open Application
	for _, current := range []*sessionObserver{watch, second} {
		current.session(t, home, id, func(_ app.SessionSnapshot, err error) bool { return errors.Is(err, os.ErrNotExist) })
	}
	// subscriber first; same home, different executor
	other := startNotificationExecutor(t, home, root, provider)
	otherID := other.command(t, "open", "")
	other.command(t, "run", "")
	watch.session(t, home, otherID, func(s app.SessionSnapshot, err error) bool { return err == nil && s.Summary.Locked })
	if err := other.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	watch.session(t, home, otherID, func(s app.SessionSnapshot, err error) bool { return err == nil && !s.Summary.Locked })
	// Ownership can move immediately after a crash. EOF must not overwrite
	// the new executor's actual Locked state.
	other = startNotificationExecutor(t, home, root, provider)
	other.command(t, "resume", otherID)
	watch.session(t, home, otherID, func(s app.SessionSnapshot, err error) bool { return err == nil && s.Summary.Locked })
	if err := other.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	other = startNotificationExecutor(t, home, root, provider)
	other.command(t, "resume", otherID)
	other.command(t, "run", "")
	watch.session(t, home, otherID, func(s app.SessionSnapshot, err error) bool {
		return err == nil && s.Summary.Locked && len(s.State.Blocks) == 2
	})
	other.command(t, "close", "")
	watch.session(t, home, otherID, func(s app.SessionSnapshot, err error) bool { return err == nil && !s.Summary.Locked })
}

func TestWatchSessionsAccumulatesDuringSlowSnapshot(t *testing.T) {
	home, root, provider := t.TempDir(), t.TempDir(), notificationProvider(t)
	watch := observeSessions(t, home, nil)
	executor := startNotificationExecutor(t, home, root, provider)
	id := executor.command(t, "open", "")
	executor.command(t, "run", "")
	executor.command(t, "close", "")
	watch.session(t, home, id, func(s app.SessionSnapshot, err error) bool { return err == nil && !s.Summary.Locked })
	watch.stop(t)
	entered, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	defer close(release)
	changes, done := make(chan app.SessionChanges, 1024), make(chan error, 1)
	go func() {
		first := true
		done <- app.WatchSessions(ctx, home, func(change app.SessionChanges) {
			if first {
				first = false
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			changes <- change
		})
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("watch setup timed out")
	}
	// The callback is blocked throughout these short node lifetimes. No socket
	// connection is required for their final signal to survive discovery delay.
	executor.command(t, "resume", id)
	executor.command(t, "run", "")
	executor.command(t, "close", "")
	executor.command(t, "delete", id)
	created := executor.command(t, "open", "")
	executor.command(t, "run", "")
	executor.command(t, "close", "")
	select {
	case release <- struct{}{}:
	case <-time.After(time.Second):
		t.Fatal("callback did not resume")
	}
	slow := &sessionObserver{changes: changes, done: done, cancel: cancel}
	t.Cleanup(func() { slow.stop(t) })
	<-changes // initial Rescan was computed before the short operations
	slow.wait(t, func(change app.SessionChanges) bool { return change.Rescan })
	if _, err := app.ReadSession(t.Context(), home, id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted session: %v", err)
	}
	if snapshot, err := app.ReadSession(t.Context(), home, created); err != nil || snapshot.Summary.Locked || len(snapshot.State.Blocks) != 1 {
		t.Fatalf("short-lived session: %+v, %v", snapshot, err)
	}
}

func TestNotificationFailureDoesNotUndoSessionWrites(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "session-notifications"), []byte("unavailable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.WatchSessions(t.Context(), home, func(app.SessionChanges) { t.Error("broken subscription called back") }); err == nil {
		t.Fatal("broken subscription succeeded")
	}
	executor := startNotificationExecutor(t, home, t.TempDir(), notificationProvider(t))
	id := executor.command(t, "open", "")
	executor.command(t, "run", "")
	executor.command(t, "close", "")
	if snapshot, err := app.ReadSession(t.Context(), home, id); err != nil || snapshot.Summary.Locked || len(snapshot.State.Blocks) != 1 {
		t.Fatalf("saved state after notification failure: %+v, %v", snapshot, err)
	}
	executor.command(t, "delete", id)
	if _, err := app.ReadSession(t.Context(), home, id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deletion after notification failure: %v", err)
	}
}
