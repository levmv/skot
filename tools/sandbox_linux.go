//go:build linux

package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	"golang.org/x/sys/unix"
)

const sandboxChildArg = "__sk_sandbox_process"

const (
	envSandboxArgv   = "SK_INTERNAL_SANDBOX_ARGV"
	envSandboxConfig = "SK_INTERNAL_SANDBOX_CONFIG"
)

func runSandboxChildIfRequested() bool {
	if len(os.Args) < 2 || os.Args[1] != sandboxChildArg {
		return false
	}
	runSandboxedProcess()
	return true
}

func sandboxBackend() string { return "landlock" }

func sandboxedBashCommand(command, workdir string, boundary Boundary) (*exec.Cmd, error) {
	if err := validateConcreteScope(boundary.Scope); err != nil {
		return nil, err
	}
	if !boundary.NeedsBackend() {
		return ambientBashCommand(command, workdir), nil
	}
	bash := systemBashPath()
	if bash == "" {
		return nil, errors.New("system Bash is unavailable")
	}
	return sandboxedProgramCommand(bash, []string{bash, "-lc", command}, workdir, boundary, nil)
}

func sandboxedProgramCommand(program string, argv []string, workdir string, boundary Boundary, environment map[string]string) (*exec.Cmd, error) {
	argv = resolvedProgramArgv(program, argv)
	if err := validateConcreteScope(boundary.Scope); err != nil {
		return nil, err
	}
	if !boundary.NeedsBackend() {
		return ambientProgramCommand(program, argv, workdir, environment), nil
	}
	encodedArgv, err := json.Marshal(argv)
	if err != nil {
		return nil, fmt.Errorf("encode boundary argv: %w", err)
	}
	encodedSandbox, err := json.Marshal(boundary)
	if err != nil {
		return nil, fmt.Errorf("encode boundary configuration: %w", err)
	}
	cmd := exec.Command("/proc/self/exe", sandboxChildArg)
	cmd.Dir = workdir
	cmd.Env = sandboxControlEnv(encodedArgv, encodedSandbox, boundary, environment)
	return cmd, nil
}

func sandboxControlEnv(argv, encodedSandbox []byte, boundary Boundary, environment map[string]string) []string {
	return append(mergeToolEnv(sandboxBaseEnv(boundary), environment),
		envSandboxArgv+"="+string(argv),
		envSandboxConfig+"="+string(encodedSandbox),
	)
}

func runSandboxedProcess() {
	var argv []string
	if err := json.Unmarshal([]byte(os.Getenv(envSandboxArgv)), &argv); err != nil || len(argv) == 0 || argv[0] == "" {
		fmt.Fprintln(os.Stderr, "filesystem boundary: invalid program arguments")
		os.Exit(126)
	}
	var boundary Boundary
	if err := json.Unmarshal([]byte(os.Getenv(envSandboxConfig)), &boundary); err != nil {
		fmt.Fprintln(os.Stderr, "filesystem boundary: invalid configuration")
		os.Exit(126)
	}
	environment := sandboxChildEnv(os.Environ())
	runtime.LockOSThread()
	if err := applyToolLandlock(boundary); err != nil {
		fmt.Fprintf(os.Stderr, "filesystem boundary: %v\n", err)
		os.Exit(126)
	}
	if err := syscall.Exec(argv[0], argv, environment); err != nil {
		fmt.Fprintf(os.Stderr, "filesystem boundary exec %s: %v\n", argv[0], err)
		os.Exit(126)
	}
}

func sandboxChildEnv(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(name, "SK_INTERNAL_") {
			result = append(result, entry)
		}
	}
	return result
}

func systemBashPath() string {
	for _, path := range []string{"/bin/bash", "/usr/bin/bash", "/run/current-system/sw/bin/bash"} {
		if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return path
		}
	}
	return ""
}

var sandboxReadOnlyDirs = []string{
	"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32",
	"/etc", "/nix", "/opt", "/snap", "/run/systemd/resolve",
}

func sandboxWritableDirs(boundary Boundary) []string {
	return []string{boundary.Workspace, boundary.ToolHome}
}

func applyToolLandlock(boundary Boundary) error {
	switch boundary.Scope {
	case ScopeWorkspace:
		readDirs, readFiles, err := landlockPathsExcept(sandboxReadOnlyDirs, boundary.ProtectedPaths)
		if err != nil {
			return err
		}
		writeDirs, writeFiles, err := landlockPathsExcept(sandboxWritableDirs(boundary), boundary.ProtectedPaths)
		if err != nil {
			return err
		}
		var deviceFiles []string
		for _, path := range []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty"} {
			if !protectedBy(boundary.ProtectedPaths, path) {
				deviceFiles = append(deviceFiles, path)
			}
		}
		rules := []landlock.Rule{
			landlock.RODirs(readDirs...).IgnoreIfMissing(),
			landlock.ROFiles(readFiles...).IgnoreIfMissing(),
			landlock.RWDirs(writeDirs...).WithRefer().IgnoreIfMissing(),
			landlock.RWFiles(append(writeFiles, deviceFiles...)...).IgnoreIfMissing(),
		}
		// ABI V3 is required so an ungranted file cannot still be truncated.
		return landlock.V3.RestrictPaths(rules...)
	case ScopeMachine:
		dirs, files, err := landlockPathsExcept([]string{string(filepath.Separator)}, boundary.ProtectedPaths)
		if err != nil {
			return err
		}
		rules := []landlock.Rule{
			landlock.RWDirs(dirs...).WithRefer().IgnoreIfMissing(),
			landlock.RWFiles(files...).IgnoreIfMissing(),
		}
		return landlock.V3.RestrictPaths(rules...)
	default:
		return fmt.Errorf("unsupported concrete filesystem scope %q", boundary.Scope)
	}
}

// landlockPathsExcept converts allow roots minus protected subtrees into the
// concrete sibling grants Landlock requires. Landlock has no deny rule, so an
// ancestor containing a protected path cannot itself be granted.
func landlockPathsExcept(roots, protected []string) (dirs, files []string, err error) {
	seen := make(map[string]struct{})
	var add func(string, string) error
	add = func(path, boundary string) error {
		path = canonicalSandboxPath(path)
		if !pathContains(boundary, path) {
			return nil
		}
		if protectedBy(protected, path) {
			return nil
		}
		if _, exists := seen[path]; exists {
			return nil
		}
		seen[path] = struct{}{}
		containsProtected := false
		for _, denied := range protected {
			if pathContains(path, denied) {
				containsProtected = true
				break
			}
		}
		info, statErr := os.Stat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		if statErr != nil {
			return fmt.Errorf("inspect boundary path %s: %w", path, statErr)
		}
		if !containsProtected || !info.IsDir() {
			if info.IsDir() {
				dirs = append(dirs, path)
			} else {
				files = append(files, path)
			}
			return nil
		}
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			return fmt.Errorf("enumerate boundary path %s: %w", path, readErr)
		}
		for _, entry := range entries {
			candidate := filepath.Join(path, entry.Name())
			info, inspectErr := os.Lstat(candidate)
			if inspectErr != nil || info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			if err := add(candidate, boundary); err != nil {
				return err
			}
		}
		return nil
	}
	for _, root := range roots {
		boundary := canonicalSandboxPath(root)
		if err := add(boundary, boundary); err != nil {
			return nil, nil, err
		}
	}
	sort.Strings(dirs)
	sort.Strings(files)
	return dirs, files, nil
}

func hardenSupervisor() { _ = unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) }
