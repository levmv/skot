package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStoreExcludesLegacyFlockOwners(t *testing.T) {
	home := t.TempDir()
	id := createCatalogSession(t, home, t.TempDir(), "shared session")
	legacy, err := os.OpenFile(filepath.Join(home, "sessions", id, journalName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if err := unix.Flock(int(legacy.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if store, err := OpenManaged(home, id); !errors.Is(err, ErrLocked) {
		if store != nil {
			store.Close()
		}
		t.Fatalf("opened a legacy owner's journal: %v", err)
	}
	if err := Delete(t.Context(), home, id, nil); !errors.Is(err, ErrLocked) {
		t.Fatalf("deleted a legacy owner's session: %v", err)
	}
	if err := unix.Flock(int(legacy.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	store, err := OpenManaged(home, id)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := unix.Flock(int(legacy.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("legacy executor acquired an owned journal: %v", err)
	}
}
