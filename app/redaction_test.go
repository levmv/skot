package app

import (
	"testing"

	"github.com/levmv/skot/internal/state"
)

func TestSecretMaskerLoadsStoredAndEnvironmentCredentials(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAPIKey("deepseek", "stored-secret"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "environment-secret")
	masker := newSecretMasker(store, "extra-secret")
	got := masker.Redact("stored-secret environment-secret extra-secret public")
	if got != "[REDACTED] [REDACTED] [REDACTED] public" {
		t.Fatalf("redacted = %q", got)
	}
	masker.Add("new-secret")
	if got := masker.Redact("new-secret"); got != "[REDACTED]" {
		t.Fatalf("added secret = %q", got)
	}
}
