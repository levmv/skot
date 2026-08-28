package state

import (
	"fmt"
	"strings"
)

const (
	DisplayCompact  = "compact"
	DisplayDetailed = "detailed"
	DisplayFull     = "full"
)

// NormalizeDisplayProfile validates the interactive transcript presentation.
func NormalizeDisplayProfile(value string) (string, error) {
	switch value = strings.ToLower(strings.TrimSpace(value)); value {
	case "", DisplayCompact:
		return DisplayCompact, nil
	case DisplayDetailed:
		return DisplayDetailed, nil
	case DisplayFull:
		return DisplayFull, nil
	default:
		return "", fmt.Errorf("invalid display profile %q; expected compact, detailed, or full", value)
	}
}
