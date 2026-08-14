package app

import (
	"fmt"
	"slices"
	"strings"
)

const defaultReasoningEffort = ""

func reasoningEffortsForModel(uri string) []string {
	route, err := resolveModelRoute(uri, "", modelRouteOverrides{}, modelRouteEnrichment{})
	if err != nil {
		return []string{defaultReasoningEffort}
	}
	return append([]string(nil), route.ReasoningEfforts...)
}

func normalizeReasoningEffort(uri, effort string) (string, error) {
	return normalizeReasoningEffortForRoute(uri, effort, reasoningEffortsForModel(uri))
}

func normalizeReasoningEffortForRoute(uri, effort string, supported []string) (string, error) {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "default" {
		effort = defaultReasoningEffort
	}
	if slices.Contains(supported, effort) {
		return effort, nil
	}
	return "", fmt.Errorf("reasoning effort %q is unsupported for model %q", effort, strings.TrimSpace(uri))
}
