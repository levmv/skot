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
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	// Cleaned paths contain no parent components: if a component prefix cannot
	// be resolved, no longer prefix can be resolved either. Search byte offsets
	// rounded to component boundaries instead of cleaning every shrinking path.
	rootEnd := len(filepath.VolumeName(path))
	if filepath.IsAbs(path) {
		rootEnd++
	}
	bestEnd := -1
	var best string
	for low, high := 0, len(path); low < high; {
		middle := low + (high-low)/2
		end := max(rootEnd, strings.LastIndexByte(path[:middle+1], filepath.Separator))
		resolved, err := filepath.EvalSymlinks(path[:end])
		if err == nil {
			if end > bestEnd {
				bestEnd, best = end, resolved
			}
			low = middle + 1
		} else {
			high = middle
		}
	}
	if bestEnd < 0 {
		return path
	}
	// Rebuild the missing suffix once; per-component Join is quadratic too.
	return filepath.Join(best, strings.TrimPrefix(path[bestEnd:], string(filepath.Separator)))
}

// Contains reports whether path is root itself or canonically below root.
func Contains(root, path string) bool {
	root, path = Resolve(root), Resolve(path)
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
