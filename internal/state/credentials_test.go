package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCredentialUpdatesAcrossStoresPreserveOtherProviders(t *testing.T) {
	home := t.TempDir()
	first, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.SetAPIKey(t.Context(), "openai", "old-key"); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var group sync.WaitGroup
	group.Go(func() {
		err := first.UpdateCredential(t.Context(), "openai", func(profile CredentialProfile) (CredentialProfile, error) {
			close(entered)
			<-release
			profile.Payload = []byte(`{"token":"new-key"}`)
			return profile, nil
		})
		if err != nil {
			t.Error(err)
		}
	})
	<-entered
	group.Go(func() {
		if err := second.SetAPIKey(t.Context(), "deepseek", "other-key"); err != nil {
			t.Error(err)
		}
	})
	// A queued mutation must be cancellable without running its callback.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err = second.UpdateCredential(ctx, "openai", func(profile CredentialProfile) (CredentialProfile, error) {
		t.Error("cancelled lock waiter mutated credentials")
		return CredentialProfile{}, nil
	})
	close(release)
	group.Wait()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled update = %v", err)
	}
	for provider, expected := range map[string]string{"openai": "new-key", "deepseek": "other-key"} {
		actual, ok, err := first.APIKey(provider)
		if err != nil || !ok || actual != expected {
			t.Fatalf("lost %s credential", provider)
		}
	}
}

func TestCredentialLockRejectsSymlinks(t *testing.T) {
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "unrelated")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "store.lock")); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAPIKey(t.Context(), "openai", "key"); err == nil {
		t.Fatal("accepted symlinked credential lock")
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
		t.Fatal("modified the lock symlink target")
	}
}

func TestCorruptCredentialStoreIsNotOverwritten(t *testing.T) {
	for _, damage := range []string{"truncated", "modified"} {
		t.Run(damage, func(t *testing.T) {
			home := t.TempDir()
			store, err := OpenCredentials(home)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetAPIKey(t.Context(), "example", "existing-key"); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, "store.bin")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if damage == "truncated" {
				raw = raw[:1]
			} else {
				raw[len(raw)-1] ^= 1
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			legacy := filepath.Join(home, "auth.json")
			if err := os.WriteFile(legacy, []byte(`{"profiles":{},"defaults":{}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenCredentials(home); err == nil {
				t.Fatal("opened a damaged store or fell back to plaintext")
			}
			if _, err := os.Stat(legacy); err != nil {
				t.Fatal("failed migration removed the legacy store")
			}
			if err := store.SetAPIKey(t.Context(), "other", "new-key"); err == nil {
				t.Fatal("replaced a damaged store during login")
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, raw) {
				t.Fatal("damaged store was overwritten")
			}
		})
	}
}

func TestCredentialMigrationRemovesPlaintextAndPreservesNewerTokens(t *testing.T) {
	for _, existingStore := range []bool{false, true} {
		name := "legacy only"
		if existingStore {
			name = "already published"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			want := "legacy-key"
			if existingStore {
				store, err := OpenCredentials(home)
				if err != nil {
					t.Fatal(err)
				}
				want = "refreshed-key"
				if err := store.SetAPIKey(t.Context(), "example", want); err != nil {
					t.Fatal(err)
				}
			}
			legacy := filepath.Join(home, "auth.json")
			if err := os.WriteFile(legacy, []byte(`{"profiles":{"work":{"provider":"example","kind":"api_key","payload":{"token":"legacy-key"}}},"defaults":{"example":"work"}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := OpenCredentials(home)
			if err != nil {
				t.Fatal(err)
			}
			if token, ok, err := store.APIKey("example"); err != nil || !ok || token != want {
				t.Fatalf("migrated API key = %q, %v, %v", token, ok, err)
			}
			if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("plaintext credentials remain: %v", err)
			}
			if raw, err := os.ReadFile(filepath.Join(home, "store.bin")); err != nil || bytes.Contains(raw, []byte(want)) {
				t.Fatal("migration did not persist opaque credentials")
			}
		})
	}
}
