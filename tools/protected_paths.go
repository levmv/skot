package tools

import (
	"fmt"

	"github.com/levmv/skot/internal/canonicalpath"
)

// ProtectedPathPolicy is the immutable shared filesystem deny-list for
// model-owned operations.
type ProtectedPathPolicy struct {
	paths []string
}

// NewProtectedPathPolicy resolves literal protected paths. Absolute paths are
// used as-is, ~/ is relative to the user's real home, and other paths are
// relative to the workspace root. Missing suffixes are allowed so a path stays
// protected if it is created later.
func NewProtectedPathPolicy(root string, values []string) (*ProtectedPathPolicy, error) {
	root, err := ResolveWorkspaceRoot(root)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(values))
	for index, value := range values {
		resolved, err := ResolvePolicyPath(root, value)
		if err != nil {
			return nil, fmt.Errorf("protected path %d: %w", index+1, err)
		}
		paths = append(paths, resolved)
	}
	paths = compactPolicyPaths(paths)
	return &ProtectedPathPolicy{paths: paths}, nil
}

// Paths returns the canonical effective list.
func (policy *ProtectedPathPolicy) Paths() []string {
	if policy == nil {
		return nil
	}
	return append([]string(nil), policy.paths...)
}

// Protects reports whether path itself is inside a protected tree. An ancestor
// of a protected path is not protected; directory operations may inspect that
// ancestor while filtering the protected child.
func (policy *ProtectedPathPolicy) Protects(path string) bool {
	if policy == nil {
		return false
	}
	return protectedBy(policy.paths, path)
}

func protectedBy(protected []string, path string) bool {
	path = canonicalpath.Resolve(path)
	for _, denied := range protected {
		if canonicalpath.Contains(denied, path) {
			return true
		}
	}
	return false
}

func (policy *ProtectedPathPolicy) contains(path string) bool {
	if policy == nil {
		return false
	}
	return protectedBy(policy.paths, path)
}
