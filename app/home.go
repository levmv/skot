package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveHome returns an absolute, cleaned Skot data directory. An empty value
// selects .skot below the current user's home directory.
func ResolveHome(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home: %w", err)
		}
		value = filepath.Join(home, ".skot")
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve Skot home: %w", err)
	}
	return filepath.Clean(abs), nil
}
