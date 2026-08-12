package app

import (
	"fmt"
	"slices"
	"strings"
)

const defaultReasoningEffort = ""

// The current Chat Completions providers share the small capability surface
// that Cy exposed. This stays a function so a backend can narrow or extend it
// later without teaching the TUI provider-specific rules.
func reasoningEffortsForModel(string) []string {
	return []string{defaultReasoningEffort, "high"}
}

func normalizeReasoningEffort(uri, effort string) (string, error) {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "default" {
		effort = defaultReasoningEffort
	}
	if slices.Contains(reasoningEffortsForModel(uri), effort) {
		return effort, nil
	}
	return "", fmt.Errorf("reasoning effort %q is unsupported for model %q", effort, strings.TrimSpace(uri))
}
