package tools

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const (
	SandboxAuto      = "auto"
	SandboxWorkspace = "workspace"
	SandboxMasked    = "masked"
	SandboxOff       = "off"
)

// Sandbox describes the concrete filesystem boundary applied to one
// model-owned process. Policy is always concrete here: auto is resolved by the
// application before a process reaches the tools package.
type Sandbox struct {
	Policy    string `json:"policy"`
	Workspace string `json:"workspace"`
	ToolHome  string `json:"tool_home"`
	// ProtectedPaths are inaccessible to model-owned processes under both
	// workspace and masked policies.
	ProtectedPaths []string `json:"protected_paths,omitempty"`
}

// ValidateLayout checks the path relationships required by a concrete
// sandbox policy.
func (sandbox Sandbox) ValidateLayout() error {
	if err := validateConcreteSandboxPolicy(sandbox.Policy); err != nil {
		return err
	}
	if sandbox.Policy == SandboxOff {
		return nil
	}
	for _, path := range sandbox.ProtectedPaths {
		if pathContains(path, sandbox.Workspace) {
			return fmt.Errorf("protected path %s contains the workspace", path)
		}
	}
	if sandbox.Policy == SandboxWorkspace && pathContains(sandbox.ToolHome, sandbox.Workspace) {
		return fmt.Errorf("tool home must not contain the workspace")
	}
	if sandbox.Policy == SandboxWorkspace {
		for _, path := range sandbox.ProtectedPaths {
			if sandboxPathsOverlap(path, sandbox.ToolHome) {
				return fmt.Errorf("protected path %s overlaps tool home", path)
			}
		}
	}
	return nil
}

func NormalizeSandboxPolicy(policy string) (string, error) {
	policy = strings.ToLower(strings.TrimSpace(policy))
	if policy == "" {
		policy = SandboxAuto
	}
	switch policy {
	case SandboxAuto, SandboxWorkspace, SandboxMasked, SandboxOff:
		return policy, nil
	default:
		return "", fmt.Errorf("unknown sandbox policy %q (want auto, workspace, masked, or off)", policy)
	}
}

func validateConcreteSandboxPolicy(policy string) error {
	switch policy {
	case SandboxWorkspace, SandboxMasked, SandboxOff:
		return nil
	default:
		return fmt.Errorf("sandbox policy must be concrete, got %q", policy)
	}
}

func RunSandboxChildIfRequested() bool { return runSandboxChildIfRequested() }

func HardenSupervisor() { hardenSupervisor() }

func SandboxBackend() string { return sandboxBackend() }

func SandboxedBashCommand(command, workdir string, sandbox Sandbox) (*exec.Cmd, error) {
	return sandboxedBashCommand(command, workdir, sandbox)
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

func sandboxBaseEnv(sandbox Sandbox) []string {
	if sandbox.Policy == SandboxMasked || sandbox.Policy == SandboxOff {
		return os.Environ()
	}
	return minimalToolEnv(sandbox.ToolHome)
}

func canonicalSandboxPath(path string) string {
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

func pathContains(root, path string) bool {
	root = canonicalSandboxPath(root)
	path = canonicalSandboxPath(path)
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func sandboxPathsOverlap(first, second string) bool {
	return pathContains(first, second) || pathContains(second, first)
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
