package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/levmv/skot/internal/canonicalpath"
)

// ResolvePolicyPath canonicalizes one user-supplied filesystem-policy path.
// A bare "~" or a "~/" prefix is the user's real home, a relative path starts
// at the workspace root, and the result is canonical. The path need not exist,
// so a policy may name a file which is only created later; callers which
// require an existing directory check that themselves.
func ResolvePolicyPath(root, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("path is empty")
	}
	switch {
	case value == "~":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home: %w", err)
		}
		value = home
	case strings.HasPrefix(value, "~/") || strings.HasPrefix(value, "~"+string(filepath.Separator)):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home: %w", err)
		}
		value = filepath.Join(home, value[2:])
	case strings.HasPrefix(value, "~"):
		return "", fmt.Errorf("path %q uses an unsupported home expansion", value)
	case !filepath.IsAbs(value):
		value = filepath.Join(root, value)
	}
	resolved := canonicalpath.Resolve(value)
	if filepath.Dir(resolved) == resolved {
		return "", errors.New("the filesystem root cannot be a policy path")
	}
	return resolved, nil
}

// compactPolicyPaths sorts canonical paths and drops every entry an earlier
// one already contains, so a policy holds each tree once.
func compactPolicyPaths(paths []string) []string {
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
			if canonicalpath.Contains(parent, path) {
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

// FilesystemAccess owns the atomically published filesystem policy shared by
// built-in file tools and model-owned process launches.
type FilesystemAccess struct {
	current atomic.Pointer[filesystemPolicy]
}

// filesystemPolicy is immutable after publication. Workspace remains the
// primary relative-path and default-search base even when scope is machine.
type filesystemPolicy struct {
	scope      Scope
	workspace  string
	additions  *AddedDirectoryPolicy
	protection *ProtectedPathPolicy
}

// NewFilesystemAccess creates one shared authority state for built-in file
// tools and model-owned process launches. A nil policy is an empty one.
func NewFilesystemAccess(root string, scope Scope, additions *AddedDirectoryPolicy, protection *ProtectedPathPolicy) (*FilesystemAccess, error) {
	root, err := ResolveWorkspaceRoot(root)
	if err != nil {
		return nil, err
	}
	if additions == nil {
		additions, err = NewAddedDirectoryPolicy(root, nil)
		if err != nil {
			return nil, err
		}
	}
	if protection == nil {
		protection, err = NewProtectedPathPolicy(root, nil)
		if err != nil {
			return nil, err
		}
	}
	policy := &filesystemPolicy{scope: scope, workspace: root, additions: additions, protection: protection}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	access := &FilesystemAccess{}
	access.current.Store(policy)
	return access, nil
}

func (access *FilesystemAccess) snapshot() *filesystemPolicy {
	if access == nil {
		panic("tools: nil filesystem access")
	}
	policy := access.current.Load()
	if policy == nil {
		panic("tools: uninitialized filesystem access")
	}
	return policy
}

func (access *FilesystemAccess) policyFor(scope Scope, additions *AddedDirectoryPolicy, protection *ProtectedPathPolicy) (*filesystemPolicy, error) {
	current := access.snapshot()
	next := &filesystemPolicy{
		scope: scope, workspace: current.workspace, additions: additions, protection: protection,
	}
	if err := next.validate(); err != nil {
		return nil, err
	}
	return next, nil
}

func (policy *filesystemPolicy) validate() error {
	if policy == nil {
		return errors.New("filesystem policy is nil")
	}
	if err := validateScope(policy.scope); err != nil {
		return err
	}
	if policy.workspace == "" || !filepath.IsAbs(policy.workspace) {
		return errors.New("filesystem policy workspace must be absolute")
	}
	if policy.protection == nil {
		return errors.New("filesystem policy protection is nil")
	}
	if policy.additions == nil {
		return errors.New("filesystem policy additions are nil")
	}
	for _, path := range policy.protection.Paths() {
		if canonicalpath.Contains(path, policy.workspace) {
			return fmt.Errorf("protected path %s contains the workspace", path)
		}
	}
	return nil
}

func (policy *filesystemPolicy) workspaceOnly() *filesystemPolicy {
	if policy.scope == ScopeWorkspace {
		return policy
	}
	copy := *policy
	copy.scope = ScopeWorkspace
	return &copy
}

// processBoundary materializes the process-side enforcement of this same
// user-path policy. ToolHome is process runtime substrate, not an additional
// root for built-in file operations.
func (policy *filesystemPolicy) processBoundary(toolHome string) Boundary {
	return Boundary{
		Scope: policy.scope, Workspace: policy.workspace,
		ToolHome: toolHome, AddedPaths: policy.additions.Paths(), ProtectedPaths: policy.protection.Paths(),
	}
}

func (policy *filesystemPolicy) checkScope(path string) error {
	path = canonicalpath.Resolve(path)
	if policy.scope == ScopeMachine || isWithinRoot(policy.workspace, path) || policy.additions.Contains(path) {
		return nil
	}
	return fmt.Errorf("path is outside workspace scope: %s", policy.displayPath(path))
}

func (policy *filesystemPolicy) checkProtected(path, display string) error {
	if policy.protection.Protects(path) {
		return fmt.Errorf("path is protected: %s", display)
	}
	return nil
}

func (policy *filesystemPolicy) resolvePath(path string) (string, string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "."
	}
	abs := filepath.Clean(path)
	if !filepath.IsAbs(abs) {
		abs = filepath.Clean(filepath.Join(policy.workspace, abs))
	}
	if err := policy.checkScope(abs); err != nil {
		return "", "", err
	}
	return abs, policy.displayPath(abs), nil
}

func (policy *filesystemPolicy) resolveExistingPath(path string, enforceProtection bool) (string, string, os.FileInfo, error) {
	lexical, display, err := policy.resolvePath(path)
	if err != nil {
		return "", "", nil, err
	}
	if enforceProtection {
		if err := policy.checkProtected(lexical, display); err != nil {
			return "", "", nil, err
		}
	}
	resolved, err := filepath.EvalSymlinks(lexical)
	if err != nil {
		return "", "", nil, err
	}
	if err := policy.checkScope(resolved); err != nil {
		return "", "", nil, fmt.Errorf("symlink target %w", err)
	}
	display = policy.displayActionPath(lexical, resolved)
	if enforceProtection {
		if err := policy.checkProtected(resolved, display); err != nil {
			return "", "", nil, err
		}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", "", nil, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return "", "", nil, fmt.Errorf("%s is not a regular file or directory", display)
	}
	return filepath.Clean(resolved), display, info, nil
}

func (policy *filesystemPolicy) resolveReadableFile(path string) (string, string, error) {
	abs, display, info, err := policy.resolveExistingPath(path, true)
	if err != nil {
		return "", "", err
	}
	if info.IsDir() {
		return "", "", fmt.Errorf("%s is a directory", display)
	}
	return abs, display, nil
}

func (policy *filesystemPolicy) displayActionPath(lexical, resolved string) string {
	if isWithinRoot(policy.workspace, lexical) && isWithinRoot(policy.workspace, resolved) {
		return policy.displayPath(lexical)
	}
	return filepath.ToSlash(filepath.Clean(resolved))
}

func (policy *filesystemPolicy) displayPath(path string) string {
	path = filepath.Clean(path)
	if !isWithinRoot(policy.workspace, path) {
		return filepath.ToSlash(path)
	}
	relative, err := filepath.Rel(policy.workspace, path)
	if err != nil || relative == "." {
		return "."
	}
	return filepath.ToSlash(relative)
}
