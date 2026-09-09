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
	if err := store.SetAPIKey(t.Context(), "deepseek", "stored-secret"); err != nil {
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

func TestSecretMaskerRedactsKeysWithSharedPrefix(t *testing.T) {
	for _, secrets := range [][]string{
		{"sk-test-token", "sk-test-token-extended"},
		{"sk-test-token-extended", "sk-test-token"},
	} {
		t.Run(secrets[0], func(t *testing.T) {
			masker := &secretMasker{}
			for _, secret := range secrets {
				masker.Add(secret)
			}
			got := masker.Redact("keys: sk-test-token-extended sk-test-token")
			if got != "keys: [REDACTED] [REDACTED]" {
				t.Fatalf("redacted = %q", got)
			}
		})
	}
}
