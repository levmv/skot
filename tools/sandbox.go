package tools

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/levmv/skot/internal/canonicalpath"
)

const (
	ScopeAuto      = "auto"
	ScopeWorkspace = "workspace"
	ScopeMachine   = "machine"
)

// Scope is the filesystem reach selected for model-owned operations.
type Scope string

// Boundary describes the concrete filesystem boundary applied to one
// model-owned process. Scope is always concrete here: auto is resolved by the
// application before a process reaches the tools package.
type Boundary struct {
	Scope     Scope  `json:"scope"`
	Workspace string `json:"workspace"`
	ToolHome  string `json:"tool_home"`
	// ProtectedPaths are inaccessible to model-owned processes under both
	// concrete scopes.
	ProtectedPaths []string `json:"protected_paths,omitempty"`
}

// NeedsBackend reports whether this boundary installs a platform filesystem
// policy. Machine scope without exclusions has no restriction to install.
func (boundary Boundary) NeedsBackend() bool {
	return boundary.Scope == ScopeWorkspace || len(boundary.ProtectedPaths) != 0
}

// ValidateLayout checks the path relationships required by a concrete scope.
func (boundary Boundary) ValidateLayout() error {
	if err := validateConcreteScope(boundary.Scope); err != nil {
		return err
	}
	for _, path := range boundary.ProtectedPaths {
		if canonicalpath.Contains(path, boundary.Workspace) {
			return fmt.Errorf("protected path %s contains the workspace", path)
		}
	}
	if boundary.Scope == ScopeWorkspace && canonicalpath.Contains(boundary.ToolHome, boundary.Workspace) {
		return fmt.Errorf("tool home must not contain the workspace")
	}
	if boundary.Scope == ScopeWorkspace {
		for _, path := range boundary.ProtectedPaths {
			if sandboxPathsOverlap(path, boundary.ToolHome) {
				return fmt.Errorf("protected path %s overlaps tool home", path)
			}
		}
	}
	return nil
}

func NormalizeScope(value string) (Scope, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ScopeAuto, nil
	}
	switch Scope(value) {
	case ScopeAuto, ScopeWorkspace, ScopeMachine:
		return Scope(value), nil
	default:
		return "", fmt.Errorf("unknown filesystem scope %q (want auto, workspace, or machine)", value)
	}
}

func validateConcreteScope(scope Scope) error {
	switch scope {
	case ScopeWorkspace, ScopeMachine:
		return nil
	default:
		return fmt.Errorf("filesystem scope must be concrete, got %q", scope)
	}
}

func RunBoundaryChildIfRequested() bool { return runSandboxChildIfRequested() }

func HardenSupervisor() { hardenSupervisor() }

func BoundaryBackend() string { return sandboxBackend() }

func BoundaryBashCommand(command, workdir string, boundary Boundary) (*exec.Cmd, error) {
	return sandboxedBashCommand(command, workdir, boundary)
}

// DefaultToolHomeRoot returns the disposable payload-data root. It is kept
// outside Skot's private data directory so workspace isolation can grant it
// without exposing supervisor-owned state.
func DefaultToolHomeRoot() (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache directory: %w", err)
	}
	root, err = filepath.Abs(filepath.Join(root, "skot", "tool-home"))
	if err != nil {
		return "", fmt.Errorf("resolve tool home root: %w", err)
	}
	return filepath.Clean(root), nil
}

// WorkspaceToolHome gives every workspace a stable synthetic home without
// storing the workspace path itself in the cache directory name.
func WorkspaceToolHome(root, workspace string) string {
	digest := sha256.Sum256([]byte(workspace))
	return filepath.Join(root, fmt.Sprintf("%x", digest[:8]))
}

// WorkspaceToolTemp is private scratch space inside a workspace's tool home.
// Keeping TMPDIR here avoids granting the machine-wide temporary directories.
func WorkspaceToolTemp(home string) string {
	return filepath.Join(home, "tmp")
}

func ambientBashCommand(command, workdir string) *exec.Cmd {
	cmd := exec.Command("bash", "-lc", command)
	cmd.Dir = workdir
	return cmd
}

func ambientProgramCommand(program string, argv []string, workdir string, environment map[string]string) *exec.Cmd {
	cmd := exec.Command(program, argv[1:]...)
	cmd.Dir = workdir
	cmd.Env = mergeToolEnv(os.Environ(), environment)
	return cmd
}

func resolvedProgramArgv(program string, argv []string) []string {
	resolved := append([]string(nil), argv...)
	if len(resolved) == 0 {
		resolved = []string{program}
	} else {
		resolved[0] = program
	}
	return resolved
}

func minimalToolEnv(home string) []string {
	environment := []string{"HOME=" + home, "TMPDIR=" + WorkspaceToolTemp(home)}
	for _, key := range []string{"PATH", "LANG", "LC_ALL", "TZ"} {
		if value, ok := os.LookupEnv(key); ok {
			environment = append(environment, key+"="+value)
		}
	}
	return environment
}

func sandboxBaseEnv(boundary Boundary) []string {
	if boundary.Scope == ScopeMachine {
		return os.Environ()
	}
	return minimalToolEnv(boundary.ToolHome)
}

func sandboxPathsOverlap(first, second string) bool {
	return canonicalpath.Contains(first, second) || canonicalpath.Contains(second, first)
}

func mergeToolEnv(base []string, extra map[string]string) []string {
	values := make(map[string]string, len(base)+len(extra))
	for _, entry := range base {
		name, value, found := strings.Cut(entry, "=")
		if found {
			values[name] = value
		}
	}
	for name, value := range extra {
		values[name] = value
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+values[name])
	}
	return result
}

func modelProcessEnvironment(base []string, hidden map[string]struct{}, overlay map[string]string) []string {
	if base == nil {
		base = os.Environ()
	}
	filtered := make([]string, 0, len(base))
	for _, entry := range base {
		name, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		_, explicitlyHidden := hidden[name]
		skSetting := strings.HasPrefix(name, "SK_") && !strings.HasPrefix(name, "SK_INTERNAL_")
		if explicitlyHidden || skSetting {
			continue
		}
		filtered = append(filtered, entry)
	}
	return mergeToolEnv(filtered, overlay)
}
