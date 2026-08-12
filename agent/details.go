package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const maxToolDetailsBytes = 256 * 1024

func normalizeDetails(details []Detail) ([]Detail, error) {
	if len(details) == 0 {
		return nil, nil
	}
	normalized := make([]Detail, len(details))
	total := 0
	for index, detail := range details {
		detail.Kind = strings.TrimSpace(detail.Kind)
		if detail.Kind == "" {
			return nil, fmt.Errorf("detail %d has no kind", index)
		}
		if len(detail.Data) == 0 || !json.Valid(detail.Data) {
			return nil, fmt.Errorf("detail %d (%s) is not valid JSON", index, detail.Kind)
		}
		total += len(detail.Kind) + len(detail.Data)
		if total > maxToolDetailsBytes {
			return nil, errors.New("tool details exceed size limit")
		}
		normalized[index] = Detail{Kind: detail.Kind, Data: append(json.RawMessage(nil), detail.Data...)}
	}
	return normalized, nil
}

func cloneDetails(details []Detail) []Detail {
	if len(details) == 0 {
		return nil
	}
	cloned := make([]Detail, len(details))
	for index, detail := range details {
		cloned[index] = Detail{Kind: detail.Kind, Data: append(json.RawMessage(nil), detail.Data...)}
	}
	return cloned
}
