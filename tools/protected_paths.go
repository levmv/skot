package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ProtectedPathPolicy is the shared filesystem deny-list for model-owned
// operations. Paths are immutable after construction; enforcement can be
// toggled when the application switches between a sandboxed policy and off.
type ProtectedPathPolicy struct {
	mu      sync.RWMutex
	enabled bool
	paths   []string
}

// NewProtectedPathPolicy resolves literal protected paths. Absolute paths are
// used as-is, ~/ is relative to the user's real home, and other paths are
// relative to the workspace root. Missing suffixes are allowed so a path stays
// protected if it is created later.
func NewProtectedPathPolicy(root string, values []string, enabled bool) (*ProtectedPathPolicy, error) {
	root, err := ResolveWorkspaceRoot(root)
	if err != nil {
		return nil, err
	}
	var userHome string
	resolveUserHome := func() (string, error) {
		if userHome != "" {
			return userHome, nil
		}
		userHome, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home for protected paths: %w", err)
		}
		return userHome, nil
	}
	paths := make([]string, 0, len(values))
	for index, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("protected path %d is empty", index+1)
		}
		switch {
		case value == "~":
			userHome, err = resolveUserHome()
			if err != nil {
				return nil, err
			}
			value = userHome
		case strings.HasPrefix(value, "~"+string(filepath.Separator)) || strings.HasPrefix(value, "~/"):
			userHome, err = resolveUserHome()
			if err != nil {
				return nil, err
			}
			value = filepath.Join(userHome, value[2:])
		case strings.HasPrefix(value, "~"):
			return nil, fmt.Errorf("protected path %q uses an unsupported home expansion", value)
		case !filepath.IsAbs(value):
			value = filepath.Join(root, value)
		}
		value = canonicalSandboxPath(value)
		if filepath.Dir(value) == value {
			return nil, errors.New("the filesystem root cannot be a protected path")
		}
		paths = append(paths, value)
	}
	paths = compactProtectedPaths(paths)
	return &ProtectedPathPolicy{enabled: enabled, paths: paths}, nil
}

func compactProtectedPaths(paths []string) []string {
	sort.Slice(paths, func(i, j int) bool {
		if len(paths[i]) != len(paths[j]) {
			return len(paths[i]) < len(paths[j])
		}
		return paths[i] < paths[j]
	})
	compacted := make([]string, 0, len(paths))
	for _, path := range paths {
		covered := false
		for _, parent := range compacted {
			if pathContains(parent, path) {
				covered = true
				break
			}
		}
		if !covered {
			compacted = append(compacted, path)
		}
	}
	sort.Strings(compacted)
	return compacted
}

// Paths returns the canonical effective list, including Skot state when the
// application constructed the policy. It does not depend on Enabled.
func (policy *ProtectedPathPolicy) Paths() []string {
	if policy == nil {
		return nil
	}
	policy.mu.RLock()
	defer policy.mu.RUnlock()
	return append([]string(nil), policy.paths...)
}

func (policy *ProtectedPathPolicy) SetEnabled(enabled bool) {
	if policy == nil {
		return
	}
	policy.mu.Lock()
	policy.enabled = enabled
	policy.mu.Unlock()
}

func (policy *ProtectedPathPolicy) Enabled() bool {
	if policy == nil {
		return false
	}
	policy.mu.RLock()
	defer policy.mu.RUnlock()
	return policy.enabled
}

// Protects reports whether path itself is inside a protected tree. An ancestor
// of a protected path is not protected; directory operations may inspect that
// ancestor while filtering the protected child.
func (policy *ProtectedPathPolicy) Protects(path string) bool {
	if policy == nil {
		return false
	}
	policy.mu.RLock()
	defer policy.mu.RUnlock()
	if !policy.enabled {
		return false
	}
	return protectedBy(policy.paths, path)
}

func protectedBy(protected []string, path string) bool {
	path = canonicalSandboxPath(path)
	for _, denied := range protected {
		if pathContains(denied, path) {
			return true
		}
	}
	return false
}

func (policy *ProtectedPathPolicy) contains(path string) bool {
	if policy == nil {
		return false
	}
	policy.mu.RLock()
	defer policy.mu.RUnlock()
	return protectedBy(policy.paths, path)
}
