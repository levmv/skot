package sessionnotify

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levmv/skot/internal/filelock"
)

const testSessionID = "session_0123456789abcdef0123456789abcdef"

func TestPendingOverflowRequestsReconciliation(t *testing.T) {
	queue := newPending()
	for index := range maxPending + 1 {
		queue.add(Changes{IDs: []string{fmt.Sprintf("session_%032x", index)}})
	}
	if changes := queue.take(); !changes.Rescan || len(changes.IDs) != 0 {
		t.Fatalf("overflow: %+v", changes)
	}
	queue.add(Changes{IDs: []string{testSessionID, testSessionID}})
	if changes := queue.take(); changes.Rescan || len(changes.IDs) != 1 || changes.IDs[0] != testSessionID {
		t.Fatalf("next batch: %+v", changes)
	}
}

func TestPublisherSnapshotIdleAndSlowSubscriber(t *testing.T) {
	home := t.TempDir()
	lease := Begin(home)
	if lease == nil {
		t.Fatal("publisher unavailable")
	}
	defer lease.Close()
	lease.Own(testSessionID)
	conn, err := net.Dial("unix", lease.node.record.Address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	initial, err := readFrame(scanner, lease.node.record.Instance)
	if err != nil || len(initial.Owned) != 1 || initial.Owned[0] != testSessionID || len(initial.IDs) != 1 {
		t.Fatalf("initial ownership: %+v, %v", initial, err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if scanner.Scan() {
		t.Fatalf("unsolicited idle frame: %s", scanner.Text())
	}
	if failure, ok := errors.AsType[net.Error](scanner.Err()); !ok || !failure.Timeout() {
		t.Fatalf("idle read: %v", scanner.Err())
	}
	// Stop reading the socket entirely, force sustained publication, and verify
	// that network backpressure never reaches the journal's publication path.
	started := time.Now()
	for range 100_000 {
		lease.Change(testSessionID)
	}
	if time.Since(started) >= ioTimeout {
		t.Fatal("slow subscriber blocked publication")
	}
}

func TestRegistrationCollectionPreservesLiveNodes(t *testing.T) {
	home := t.TempDir()
	lease := Begin(home)
	if lease == nil {
		t.Fatal("publisher unavailable")
	}
	defer lease.Close()
	n := lease.node
	for range 8 {
		record, alive, err := inspectRegistration(n.dir, n.record.Instance+".json")
		if err != nil || !alive || record != n.record {
			t.Fatalf("live registration: %+v, %v, %v", record, alive, err)
		}
	}
	// An arbitrary socket path in a stale record is never cleanup authority.
	victim := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("f", 32)
	data, _ := json.Marshal(registration{Version: protocolVersion, Instance: id, Address: victim})
	path := filepath.Join(n.dir, id+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, alive, err := inspectRegistration(n.dir, id+".json"); err != nil || alive {
		t.Fatalf("stale record: alive=%v, %v", alive, err)
	}
	if data, err := os.ReadFile(victim); err != nil || string(data) != "keep" {
		t.Fatalf("registration removed unrelated state: %q, %v", data, err)
	}
	if _, err := os.Stat(n.record.Address); err != nil {
		t.Fatalf("live socket removed: %v", err)
	}
}

func TestRegistrationCollectionRemovesInterruptedStagingRuntime(t *testing.T) {
	for _, contents := range []string{"", `{"version":1,"instance":`} {
		t.Run(fmt.Sprintf("bytes=%d", len(contents)), func(t *testing.T) {
			dir, err := registry(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var random [16]byte
			if _, err := rand.Read(random[:]); err != nil {
				t.Fatal(err)
			}
			id := hex.EncodeToString(random[:])
			runtime := runtimeDirectory(id)
			if err := os.Mkdir(runtime, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(runtime) })
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(runtime, "s"), Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			// Preserve the socket pathname as the kernel does on process death.
			listener.SetUnlinkOnClose(false)
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			name := "." + id + ".tmp"
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, alive, err := inspectRegistration(dir, name); err != nil || alive {
				t.Fatalf("staging registration: alive=%v, %v", alive, err)
			}
			for _, removed := range []string{runtime, path} {
				if _, err := os.Lstat(removed); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stale state remains at %s: %v", removed, err)
				}
			}
		})
	}
}

func TestWatchReconnectsToSharedLiveNode(t *testing.T) {
	home := t.TempDir()
	first, second := Begin(home), Begin(home)
	if first == nil || second == nil {
		t.Fatal("publisher unavailable")
	}
	defer first.Close()
	defer second.Close()
	if first.node != second.node {
		t.Fatal("sessions in one process/home did not share their node")
	}
	first.Own(testSessionID)
	otherID := "session_ffffffffffffffffffffffffffffffff"
	second.Own(otherID)
	ctx, cancel := context.WithCancel(t.Context())
	changes, done := make(chan Changes, 1024), make(chan error, 1)
	go func() { done <- Watch(ctx, home, func(change Changes) { changes <- change }) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("watch did not stop")
		}
	}()
	wait := func(match func(Changes) bool) {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case change := <-changes:
				if match(change) {
					return
				}
			case <-timer.C:
				t.Fatal("change not delivered")
			}
		}
	}
	wait(func(change Changes) bool { return change.Rescan })
	first.Close()
	wait(func(change Changes) bool { return len(change.IDs) == 1 && change.IDs[0] == testSessionID })
	n := second.node
	n.mu.Lock()
	for client := range n.clients {
		_ = client.conn.Close()
	}
	n.mu.Unlock()
	wait(func(change Changes) bool { return change.Rescan })
	second.Change(otherID)
	// Recovery's reconciliation can subsume IDs queued during reconnection.
	wait(func(change Changes) bool { return change.Rescan || len(change.IDs) == 1 && change.IDs[0] == otherID })
	if _, alive, err := inspectRegistration(n.dir, n.record.Instance+".json"); err != nil || !alive {
		t.Fatalf("disconnection removed a live registration: %v, %v", alive, err)
	}
}

func TestWatchFailsWhenDiscoveryIsRemoved(t *testing.T) {
	home := t.TempDir()
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() {
		first := true
		done <- Watch(t.Context(), home, func(Changes) {
			if first {
				first = false
				close(ready)
			}
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("watch setup timed out")
	}
	if err := os.Remove(filepath.Join(home, "session-notifications", signalName)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("broken discovery succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("broken discovery was not reported")
	}
}

// Model kernel descriptor-close ordering deterministically in a real process:
// IPC and registration ownership disappear while the journal stays locked.
func TestNotificationCloseOrderingExecutor(t *testing.T) {
	if os.Getenv("SKOT_NOTIFICATION_CLOSE_ORDER") != "1" {
		return
	}
	home := os.Getenv("SKOT_NOTIFICATION_HOME")
	path := filepath.Join(home, "sessions", testSessionID, "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		panic(err)
	}
	journal, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		panic(err)
	}
	if err := filelock.Acquire(journal); err != nil {
		panic(err)
	}
	lease := Begin(home)
	if lease == nil {
		panic("publisher unavailable")
	}
	lease.Own(testSessionID)
	fmt.Println("ready")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		switch scanner.Text() {
		case "disconnect":
			n := lease.node
			_ = n.listener.Close()
			n.mu.Lock()
			for client := range n.clients {
				_ = client.conn.Close()
			}
			n.mu.Unlock()
			_ = n.file.Close()
			fmt.Println("disconnected")
		case "release":
			_ = journal.Close()
			fmt.Println("released")
		case "quit":
			os.Exit(0)
		}
	}
	os.Exit(0)
}

func startCloseOrderingExecutor(t *testing.T, home string) (io.WriteCloser, *bufio.Scanner) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestNotificationCloseOrderingExecutor$")
	cmd.Env = append(os.Environ(), "SKOT_NOTIFICATION_CLOSE_ORDER=1", "SKOT_NOTIFICATION_HOME="+home)
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
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatal("executor did not start")
	}
	return stdin, scanner
}

func TestWatchRechecksJournalAfterSocketEOF(t *testing.T) {
	home := t.TempDir()
	stdin, scanner := startCloseOrderingExecutor(t, home)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	changes, done := make(chan bool, 1024), make(chan error, 1)
	go func() {
		done <- Watch(ctx, home, func(Changes) {
			locked, err := managedLocked(home, testSessionID)
			if err != nil {
				panic(err)
			}
			changes <- locked
		})
	}()
	wait := func(wantLocked bool) {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case locked := <-changes:
				if locked == wantLocked {
					return
				}
			case err := <-done:
				t.Fatalf("Watch stopped: %v", err)
			case <-timer.C:
				t.Fatalf("no notification with Locked=%v", wantLocked)
			}
		}
	}
	wait(true)
	_, _ = fmt.Fprintln(stdin, "disconnect")
	if !scanner.Scan() || scanner.Text() != "disconnected" {
		t.Fatal("executor did not disconnect")
	}
	wait(true) // EOF callback still observes the journal owned.
	_, _ = fmt.Fprintln(stdin, "release")
	if !scanner.Scan() || scanner.Text() != "released" {
		t.Fatal("executor did not release journal")
	}
	wait(false) // Requires a later event-driven ownership probe, not EOF alone.
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not stop")
	}
}

func TestWatchDiscoversNewOwnerDuringSlowCallback(t *testing.T) {
	home := t.TempDir()
	stdin, scanner := startCloseOrderingExecutor(t, home)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready, blocked, resume := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	var blockOnce sync.Once
	go func() {
		first := true
		done <- Watch(ctx, home, func(Changes) {
			if first {
				first = false
				close(ready)
				return
			}
			blockOnce.Do(func() {
				close(blocked)
				select {
				case <-resume:
				case <-ctx.Done():
				}
			})
		})
	}()
	await := func(signal <-chan struct{}) {
		t.Helper()
		select {
		case <-signal:
		case err := <-done:
			t.Fatalf("watch stopped: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("watch did not reach callback")
		}
	}
	await(ready)
	_, _ = fmt.Fprintln(stdin, "disconnect")
	if !scanner.Scan() || scanner.Text() != "disconnected" {
		t.Fatal("executor did not disconnect")
	}
	await(blocked)
	_, _ = fmt.Fprintln(stdin, "release")
	if !scanner.Scan() || scanner.Text() != "released" {
		t.Fatal("executor did not release journal")
	}
	journal, err := os.OpenFile(filepath.Join(home, "sessions", testSessionID, "events.jsonl"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if err := filelock.Acquire(journal); err != nil {
		t.Fatal(err)
	}
	successor := Begin(home)
	if successor == nil {
		t.Fatal("successor unavailable")
	}
	defer func() {
		_ = journal.Close()
		successor.Close()
	}()
	successor.Own(testSessionID)
	// Keep the callback blocked beyond the event-driven recovery budget.
	// Discovery must still recognize the successor and stop ownership probes.
	timer := time.NewTimer(ioTimeout + 100*time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-done:
		t.Fatalf("watch stopped: %v", err)
	case <-timer.C:
	}
	close(resume)
	select {
	case err := <-done:
		t.Fatalf("slow callback prevented ownership recovery: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not stop")
	}
}
