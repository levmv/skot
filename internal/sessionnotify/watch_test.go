package sessionnotify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/levmv/skot/internal/filelock"
)

func TestWatchReconcilesSessionsReleasedDuringReconnect(t *testing.T) {
	home := t.TempDir()
	dir, err := registry(home)
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
	defer os.RemoveAll(runtime)
	record := registration{Version: protocolVersion, Instance: id, Address: filepath.Join(runtime, "s")}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: record.Address, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	registrationFile, err := openPrivateFile(filepath.Join(dir, id+".json"), os.O_CREATE|os.O_EXCL|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer registrationFile.Close()
	if err := filelock.Acquire(registrationFile); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registrationFile.Write(data); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	var sourceLocked atomic.Bool
	sourceLocked.Store(true)
	a := "session_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	b := "session_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	type observation struct {
		change Changes
		locked bool
	}
	observations := make(chan observation, 16)
	watchDone := make(chan struct{})
	var watchErr error
	go func() {
		cachedLocked := true
		watchErr = Watch(ctx, home, func(change Changes) {
			if change.Rescan || slices.Contains(change.IDs, a) {
				cachedLocked = sourceLocked.Load()
			}
			select {
			case observations <- observation{change: change, locked: cachedLocked}:
			case <-ctx.Done():
			}
		})
		close(watchDone)
	}()
	defer func() {
		cancel()
		select {
		case <-watchDone:
			if !errors.Is(watchErr, context.Canceled) {
				t.Errorf("watch stopped: %v", watchErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("watch did not stop")
		}
	}()
	accept := func() *net.UnixConn {
		t.Helper()
		if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		conn, err := listener.AcceptUnix()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	snapshot := func(conn *net.UnixConn, owned []string) {
		t.Helper()
		data, err := json.Marshal(frame{Version: protocolVersion, Instance: id, IDs: owned, Owned: owned})
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(append(data, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(match func(observation) bool) {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case observed := <-observations:
				if match(observed) {
					return
				}
			case <-watchDone:
				t.Fatalf("watch stopped: %v", watchErr)
			case <-timer.C:
				t.Fatal("session cache was not reconciled")
			}
		}
	}

	// Control handshake delivery so A is released after the client has already
	// processed the gap's Rescan, but before the replacement snapshot arrives.
	first := accept()
	snapshot(first, []string{a, b})
	wait(func(observed observation) bool { return observed.change.Rescan && observed.locked })
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	wait(func(observed observation) bool { return observed.change.Rescan && observed.locked })
	second := accept()
	sourceLocked.Store(false)
	snapshot(second, []string{b})
	wait(func(observed observation) bool { return !observed.locked })
}
