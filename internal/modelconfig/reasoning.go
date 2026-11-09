package modelconfig

import (
	"fmt"
	"slices"
	"strings"
)

const DefaultReasoningEffort = ""

// A nil supported list leaves value validation to the provider.
func normalizeReasoningEffortForRoute(uri, effort string, supported []string) (string, error) {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "default" {
		effort = DefaultReasoningEffort
	}
	if supported == nil || slices.Contains(supported, effort) {
		return effort, nil
	}
	return "", fmt.Errorf("reasoning effort %q is unsupported for model %q", effort, strings.TrimSpace(uri))
}
