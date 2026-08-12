package app

import (
	"strings"
	"sync"

	"github.com/levmv/skot/internal/state"
)

type secretMasker struct {
	mu      sync.RWMutex
	secrets []string
}

func newSecretMasker(store *state.Store, extra ...string) *secretMasker {
	masker := &secretMasker{}
	for _, provider := range providerCredentialCatalog {
		if token, _, err := credentialForProvider(store, provider.name); err == nil {
			masker.Add(token)
		}
	}
	for _, secret := range extra {
		masker.Add(secret)
	}
	return masker
}

func (masker *secretMasker) Add(secret string) {
	if masker == nil {
		return
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return
	}
	masker.mu.Lock()
	defer masker.mu.Unlock()
	for _, known := range masker.secrets {
		if known == secret {
			return
		}
	}
	masker.secrets = append(masker.secrets, secret)
}

func (masker *secretMasker) Redact(text string) string {
	if masker == nil {
		return text
	}
	masker.mu.RLock()
	defer masker.mu.RUnlock()
	for _, secret := range masker.secrets {
		text = strings.ReplaceAll(text, secret, "[REDACTED]")
	}
	return text
}
