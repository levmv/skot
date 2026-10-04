package modelconfig

import (
	"fmt"
	"slices"
	"strings"
)

const DefaultReasoningEffort = ""

func ReasoningEfforts(uri string) []string {
	route, err := Resolve(uri, "", Overrides{}, Enrichment{})
	if err != nil {
		return []string{DefaultReasoningEffort}
	}
	return append([]string(nil), route.ReasoningEfforts...)
}

func NormalizeReasoningEffort(uri, effort string) (string, error) {
	return normalizeReasoningEffortForRoute(uri, effort, ReasoningEfforts(uri))
}

func normalizeReasoningEffortForRoute(uri, effort string, supported []string) (string, error) {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "default" {
		effort = DefaultReasoningEffort
	}
	if slices.Contains(supported, effort) {
		return effort, nil
	}
	return "", fmt.Errorf("reasoning effort %q is unsupported for model %q", effort, strings.TrimSpace(uri))
}
