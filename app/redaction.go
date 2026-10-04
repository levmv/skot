package app

import (
	"slices"
	"strings"
	"sync"

	"github.com/levmv/skot/internal/modelconfig"
)

type secretMasker struct {
	mu       sync.RWMutex
	secrets  []string
	replacer *strings.Replacer
}

func newSecretMasker(store modelconfig.CredentialStore, extra ...string) *secretMasker {
	masker := &secretMasker{}
	if tokens, err := modelconfig.StoredCodexTokens(store); err == nil {
		modelconfig.MaskCodexTokens(masker, tokens)
	}
	for _, provider := range modelconfig.CredentialCatalog {
		if token, _, err := modelconfig.CredentialForProvider(store, provider.Name, true); err == nil {
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
	if slices.Contains(masker.secrets, secret) {
		return
	}
	masker.secrets = append(masker.secrets, secret)
	// Prefer the complete key when another key is its prefix. Replacer scans
	// the original text once, without matching against inserted markers.
	slices.SortFunc(masker.secrets, func(a, b string) int { return len(b) - len(a) })
	pairs := make([]string, 0, 2*len(masker.secrets))
	for _, value := range masker.secrets {
		pairs = append(pairs, value, "[REDACTED]")
	}
	masker.replacer = strings.NewReplacer(pairs...)
}

func (masker *secretMasker) Redact(text string) string {
	if masker == nil {
		return text
	}
	masker.mu.RLock()
	defer masker.mu.RUnlock()
	if masker.replacer == nil {
		return text
	}
	return masker.replacer.Replace(text)
}
