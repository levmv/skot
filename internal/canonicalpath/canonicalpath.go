// Package canonicalpath provides best-effort canonical path comparison for
// filesystem policy. It resolves the deepest existing ancestor and preserves
// any missing suffix, so callers can compare paths before creating them.
package canonicalpath

import (
	"path/filepath"
	"strings"
)

// Resolve returns an absolute, clean path with every resolvable symlink prefix
// replaced by its target. If no ancestor can be resolved, it returns the
// absolute clean path.
func Resolve(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	path = filepath.Clean(path)
	current := path
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// Contains reports whether path is root itself or canonically below root.
func Contains(root, path string) bool {
	root, path = Resolve(root), Resolve(path)
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
