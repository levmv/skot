package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/levmv/skot/internal/canonicalpath"
)

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
	protection *ProtectedPathPolicy
}

// NewFilesystemAccess creates one shared authority state.
func NewFilesystemAccess(root string, scope Scope, protection *ProtectedPathPolicy) (*FilesystemAccess, error) {
	root, err := ResolveWorkspaceRoot(root)
	if err != nil {
		return nil, err
	}
	if protection == nil {
		protection, err = NewProtectedPathPolicy(root, nil)
		if err != nil {
			return nil, err
		}
	}
	policy := &filesystemPolicy{scope: scope, workspace: root, protection: protection}
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

func (access *FilesystemAccess) policyForScope(scope Scope) (*filesystemPolicy, error) {
	current := access.snapshot()
	next := &filesystemPolicy{
		scope: scope, workspace: current.workspace, protection: current.protection,
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
		ToolHome: toolHome, ProtectedPaths: policy.protection.Paths(),
	}
}

func (policy *filesystemPolicy) checkScope(path string) error {
	path = canonicalpath.Resolve(path)
	if policy.scope == ScopeMachine || isWithinRoot(policy.workspace, path) {
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
