package tools

import (
	"fmt"
	"os"

	"github.com/levmv/skot/internal/canonicalpath"
)

// AddedDirectoryPolicy is the immutable list of directory trees which extend
// workspace scope for model-owned operations.
type AddedDirectoryPolicy struct {
	paths []string
}

// NewAddedDirectoryPolicy resolves added directories. Absolute paths are used
// as-is, ~/ is relative to the user's real home, and other paths are relative
// to the workspace root. Every value must name an existing directory.
func NewAddedDirectoryPolicy(root string, values []string) (*AddedDirectoryPolicy, error) {
	root, err := ResolveWorkspaceRoot(root)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(values))
	for index, value := range values {
		value, err = resolveAddedDirectory(root, value)
		if err != nil {
			return nil, fmt.Errorf("added directory %d: %w", index+1, err)
		}
		if canonicalpath.Contains(root, value) {
			continue
		}
		paths = append(paths, value)
	}
	return &AddedDirectoryPolicy{paths: compactPolicyPaths(paths)}, nil
}

func resolveAddedDirectory(root, value string) (string, error) {
	resolved, err := ResolvePolicyPath(root, value)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory: %s", resolved)
	}
	return resolved, nil
}

// Paths returns the canonical effective list.
func (policy *AddedDirectoryPolicy) Paths() []string {
	if policy == nil {
		return nil
	}
	return append([]string(nil), policy.paths...)
}

// Contains reports whether path is inside an added directory tree.
func (policy *AddedDirectoryPolicy) Contains(path string) bool {
	if policy == nil {
		return false
	}
	for _, root := range policy.paths {
		if canonicalpath.Contains(root, path) {
			return true
		}
	}
	return false
}
