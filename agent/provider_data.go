package agent

import (
	"encoding/json"
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

func normalizeProviderData(entries []ProviderData) ([]ProviderData, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	if len(entries) > maxProviderDataEntries {
		return nil, fmt.Errorf("provider data has %d entries, limit is %d", len(entries), maxProviderDataEntries)
	}
	normalized := make([]ProviderData, len(entries))
	seen := make(map[string]struct{}, len(entries))
	total := 0
	for index, entry := range entries {
		kind := strings.TrimSpace(entry.Kind)
		if kind == "" {
			return nil, fmt.Errorf("provider data %d has no kind", index)
		}
		if _, exists := seen[kind]; exists {
			return nil, fmt.Errorf("provider data kind %q is duplicated", kind)
		}
		seen[kind] = struct{}{}
		if len(entry.Data) == 0 || !json.Valid(entry.Data) {
			return nil, fmt.Errorf("provider data %d (%s) is not valid JSON", index, kind)
		}
		total += len(kind) + len(entry.Data)
		if total > maxProviderDataBytes {
			return nil, errors.New("provider data exceeds size limit")
		}
		normalized[index] = ProviderData{Kind: kind, Data: append(json.RawMessage(nil), entry.Data...)}
	}
	return normalized, nil
}

func validateProviderData(entries []ProviderData) error {
	normalized, err := normalizeProviderData(entries)
	if err != nil {
		return err
	}
	for index := range entries {
		if entries[index].Kind != normalized[index].Kind {
			return fmt.Errorf("provider data %d kind is not normalized", index)
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
		cloned[index] = ProviderData{Kind: entry.Kind, Data: append(json.RawMessage(nil), entry.Data...)}
	}
	return cloned
}
