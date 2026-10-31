package session

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/levmv/skot/internal/sessionnotify"
)

func TestWatchReportsOwnershipAfterFailedDelete(t *testing.T) {
	home := t.TempDir()
	// Another session keeps the shared node alive, so its final completion
	// signal cannot hide a missing notification from the failed deletion.
	keeper, _, err := Create(home)
	if err != nil {
		t.Fatal(err)
	}
	defer keeper.Close()
	id := createCatalogSession(t, home, t.TempDir(), "kept after failed deletion")
	entered, release, deleted := make(chan struct{}), make(chan struct{}, 1), make(chan struct{})
	failure := errors.New("dependent cleanup refused")
	var deleteErr error
	go func() {
		deleteErr = Delete(t.Context(), home, id, func() error {
			close(entered)
			<-release
			return failure
		})
		close(deleted)
	}()
	defer func() {
		close(release)
		select {
		case <-deleted:
		case <-time.After(5 * time.Second):
			t.Error("deletion did not stop")
		}
	}()
	select {
	case <-entered:
	case <-deleted:
		t.Fatalf("deletion skipped dependent cleanup: %v", deleteErr)
	case <-time.After(5 * time.Second):
		t.Fatal("deletion did not acquire the journal")
	}

	ctx, cancel := context.WithCancel(t.Context())
	type observation struct {
		locked bool
		err    error
	}
	observations := make(chan observation, 16)
	watchDone := make(chan struct{})
	var watchErr error
	go func() {
		watchErr = sessionnotify.Watch(ctx, home, func(change sessionnotify.Changes) {
			if !change.Rescan && !slices.Contains(change.IDs, id) {
				return
			}
			snapshot, err := Inspect(ctx, home, id)
			select {
			case observations <- observation{locked: snapshot.Summary.Locked, err: err}:
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
	wait := func(locked bool) {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case observed := <-observations:
				if observed.err != nil {
					t.Fatal(observed.err)
				}
				if observed.locked == locked {
					return
				}
			case <-watchDone:
				t.Fatalf("watch stopped: %v", watchErr)
			case <-timer.C:
				t.Fatalf("no notification observing Locked=%v", locked)
			}
		}
	}
	wait(true) // Initial Rescan observes the journal held by Delete.
	release <- struct{}{}
	select {
	case <-deleted:
		if !errors.Is(deleteErr, failure) {
			t.Fatalf("delete: %v", deleteErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deletion did not finish")
	}
	wait(false) // Only a new callback can update this captured observation.
}
