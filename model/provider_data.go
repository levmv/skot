package model

import (
	"errors"
	"fmt"
	"strings"
)

const (
	maxProviderDataEntries = 64
	// Provider state arrives inside a model completion which is independently
	// bounded at 16 MiB. Keep the same per-item ceiling so a crafted journal
	// cannot turn many small opaque fields into an unbounded allocation.
	maxProviderDataBytes = 16 << 20
)

// ValidateProviderData checks the kinds, JSON payloads, and size of stored state.
func ValidateProviderData(entries []ProviderData) error {
	if len(entries) > maxProviderDataEntries {
		return fmt.Errorf("provider data has %d entries, limit is %d", len(entries), maxProviderDataEntries)
	}
	seen := make(map[string]struct{}, len(entries))
	total := 0
	for index, entry := range entries {
		kind := strings.TrimSpace(entry.Kind)
		if kind == "" {
			return fmt.Errorf("provider data %d has no kind", index)
		}
		if entry.Kind != kind {
			return fmt.Errorf("provider data %d kind is not normalized", index)
		}
		if _, exists := seen[kind]; exists {
			return fmt.Errorf("provider data kind %q is duplicated", kind)
		}
		seen[kind] = struct{}{}
		if len(entry.Data) == 0 || !entry.Data.IsValid() {
			return fmt.Errorf("provider data %d (%s) is not valid JSON", index, kind)
		}
		total += len(kind) + len(entry.Data)
		if total > maxProviderDataBytes {
			return errors.New("provider data exceeds size limit")
		}
	}
	return nil
}

func cloneProviderData(entries []ProviderData) []ProviderData {
	if len(entries) == 0 {
		return nil
	}
	cloned := make([]ProviderData, len(entries))
	for index, entry := range entries {
		cloned[index] = ProviderData{Kind: entry.Kind, Data: entry.Data.Clone()}
	}
	return cloned
}
