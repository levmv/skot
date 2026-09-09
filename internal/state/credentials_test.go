package state

import (
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
	if err := os.Symlink(outside, filepath.Join(home, "auth.lock")); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAPIKey(t.Context(), "openai", "key"); err == nil {
		t.Fatal("accepted symlinked credential lock")
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
		t.Fatal("modified the lock symlink target")
	}
}
