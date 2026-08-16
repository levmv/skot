package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/levmv/skot/agent"
	workspacetools "github.com/levmv/skot/tools"
)

type securityState struct {
	RequestedScope     workspacetools.Scope
	Backend            string
	Failure            string
	EffectiveScope     workspacetools.Scope
	Container          string
	ProtectedPathCount int
	BackendRequired    bool
}

func (state securityState) snapshot() agent.ScopeSnapshot {
	return agent.ScopeSnapshot{
		RequestedScope:     string(state.RequestedScope),
		EffectiveScope:     string(state.EffectiveScope),
		ProtectedPathCount: state.ProtectedPathCount,
		Backend:            state.Backend,
		Container:          state.Container,
		Network:            "inherited",
	}
}

func buildSecurityStateWithToolHome(ctx context.Context, requested workspacetools.Scope, root, toolHome string, protectedSets ...[]string) securityState {
	container := ""
	if requested == workspacetools.ScopeAuto {
		container = detectContainer(ctx)
	}
	effective := resolveScope(requested, container)
	var protected []string
	if len(protectedSets) > 0 {
		protected = append([]string(nil), protectedSets[0]...)
	}
	state := securityState{
		RequestedScope: requested, EffectiveScope: effective, Container: container,
		ProtectedPathCount: len(protected),
	}
	boundary := workspacetools.Boundary{
		Scope: effective, Workspace: root, ToolHome: toolHome, ProtectedPaths: protected,
	}
	state.BackendRequired = boundary.NeedsBackend()
	if err := boundary.ValidateLayout(); err != nil {
		return unavailableBoundary(state, err.Error())
	}
	if !state.BackendRequired {
		return state
	}
	backend := workspacetools.BoundaryBackend()
	if backend == "" {
		return unavailableBoundary(state, "no platform filesystem boundary is available")
	}
	probe, probeBoundary, err := createBoundaryProbe(boundary)
	if err != nil {
		return unavailableBoundary(state, "probe setup failed: "+err.Error())
	}
	probePath := probe.Name()
	defer func() { _ = os.Remove(probePath) }()
	if _, err := probe.WriteString("supervisor-only\n"); err != nil {
		_ = probe.Close()
		return unavailableBoundary(state, "probe setup failed: "+err.Error())
	}
	if err := probe.Close(); err != nil {
		return unavailableBoundary(state, "probe setup failed: "+err.Error())
	}
	quotedProbe := bashQuote(probePath)
	command := "if IFS= read -r _ < " + quotedProbe + " 2>/dev/null; then exit 42; fi; " +
		"if : > " + quotedProbe + " 2>/dev/null; then exit 43; fi; " +
		"if rm -f -- " + quotedProbe + " 2>/dev/null; then exit 44; fi; exit 0"
	cmd, err := workspacetools.BoundaryBashCommand(command, root, probeBoundary)
	if err != nil {
		return unavailableBoundary(state, "boundary command failed: "+err.Error())
	}
	commandEnv := cmd.Env
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd = exec.CommandContext(probeCtx, cmd.Path, cmd.Args[1:]...)
	cmd.Dir = root
	cmd.Env = commandEnv
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	err = cmd.Run()
	if err == nil {
		if raw, readErr := os.ReadFile(probePath); readErr != nil || string(raw) != "supervisor-only\n" {
			if readErr != nil {
				return unavailableBoundary(state, "filesystem probe changed supervisor state: "+readErr.Error())
			}
			return unavailableBoundary(state, "filesystem probe changed supervisor state")
		}
		state.Backend = backend
		return state
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch exit.ExitCode() {
		case 42:
			return unavailableBoundary(state, "filesystem probe read protected state")
		case 43:
			return unavailableBoundary(state, "filesystem probe truncated protected state")
		case 44:
			return unavailableBoundary(state, "filesystem probe removed protected state")
		}
	}
	reason := "filesystem probe failed: " + err.Error()
	if detail := strings.TrimSpace(diagnostic.String()); detail != "" {
		reason += ": " + detail
	}
	return unavailableBoundary(state, reason)
}

func createBoundaryProbe(boundary workspacetools.Boundary) (*os.File, workspacetools.Boundary, error) {
	var failures []string
	seen := make(map[string]struct{})
	directories := []string{os.TempDir(), boundary.Workspace}
	if boundary.Scope == workspacetools.ScopeWorkspace && len(boundary.ProtectedPaths) != 0 {
		directories[0], directories[1] = directories[1], directories[0]
	}
	for _, directory := range directories {
		directory = strings.TrimSpace(directory)
		if directory == "" {
			continue
		}
		if absolute, err := filepath.Abs(directory); err == nil {
			directory = filepath.Clean(absolute)
		}
		if _, exists := seen[directory]; exists {
			continue
		}
		seen[directory] = struct{}{}
		probe, err := os.CreateTemp(directory, ".skot-boundary-probe-")
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		probeBoundary := boundary
		probeBoundary.ProtectedPaths = append(append([]string(nil), boundary.ProtectedPaths...), probe.Name())
		if err := probeBoundary.ValidateLayout(); err == nil {
			return probe, probeBoundary, nil
		} else {
			failures = append(failures, err.Error())
		}
		_ = probe.Close()
		_ = os.Remove(probe.Name())
	}
	return nil, workspacetools.Boundary{}, errors.New(strings.Join(failures, "; "))
}

func unavailableBoundary(state securityState, probe string) securityState {
	state.Failure = probe
	return state
}

func protectedWorkspaceNotice(state securityState, root string, paths []string) string {
	if !landlockProtectedPathsLimitWorkspaceRoot(state.Backend, root, paths) {
		return ""
	}
	return "A protected path is inside the workspace; Bash and program tools may be unable to list or create entries in its ancestor directories on Linux; use the built-in file tools for those operations"
}

func landlockProtectedPathsLimitWorkspaceRoot(backend, root string, paths []string) bool {
	if backend != "landlock" {
		return false
	}
	for _, path := range paths {
		if securityPathContains(root, path) && !securityPathContains(path, root) {
			return true
		}
	}
	return false
}

func resolveScope(requested workspacetools.Scope, container string) workspacetools.Scope {
	if requested != workspacetools.ScopeAuto {
		return requested
	}
	if container != "" {
		return workspacetools.ScopeMachine
	}
	return workspacetools.ScopeWorkspace
}

func (state securityState) Summary() string {
	text := "scope: " + string(state.EffectiveScope)
	var detail []string
	if state.RequestedScope == workspacetools.ScopeAuto {
		detail = append(detail, "auto")
	}
	if state.Container != "" {
		detail = append(detail, state.Container)
	}
	if len(detail) != 0 {
		text += " (" + strings.Join(detail, ", ") + ")"
	}
	if state.ProtectedPathCount != 0 {
		text += fmt.Sprintf(" · protected paths: %d", state.ProtectedPathCount)
	}
	return text
}

func canonicalSecurityPath(path string) string {
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

func securityPathContains(root, path string) bool {
	root, path = canonicalSecurityPath(root), canonicalSecurityPath(path)
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func detectContainer(ctx context.Context) string {
	for _, marker := range []struct {
		path string
		id   string
	}{
		{path: "/run/.containerenv", id: "podman"},
		{path: "/.dockerenv", id: "docker"},
	} {
		if _, err := os.Stat(marker.path); err == nil {
			return marker.id
		}
	}
	for _, source := range []string{"/run/systemd/container", "/proc/1/environ"} {
		if raw, err := os.ReadFile(source); err == nil {
			if id := trustedContainerID(raw); id != "" {
				return id
			}
		}
	}
	if raw, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		if id := cgroupContainerID(raw); id != "" {
			return id
		}
	}
	detectCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	for _, path := range []string{
		"/usr/bin/systemd-detect-virt",
		"/bin/systemd-detect-virt",
		"/run/current-system/sw/bin/systemd-detect-virt",
	} {
		if raw, err := exec.CommandContext(detectCtx, path, "--container").Output(); err == nil {
			return trustedContainerID(raw)
		}
	}
	return ""
}

func cgroupContainerID(raw []byte) string {
	for _, line := range strings.Split(string(raw), "\n") {
		firstColon := strings.IndexByte(line, ':')
		if firstColon < 0 {
			continue
		}
		secondColon := strings.IndexByte(line[firstColon+1:], ':')
		if secondColon < 0 {
			continue
		}
		path := line[firstColon+1+secondColon+1:]
		parts := strings.Split(strings.ToLower(path), "/")
		for index, part := range parts {
			switch part {
			case "kubepods", "kubepods.slice":
				return "kubernetes"
			case "docker", "containerd":
				if index+1 < len(parts) && containerID(parts[index+1]) {
					return part
				}
			}
			for _, runtime := range []struct {
				prefix string
				id     string
			}{
				{prefix: "docker-", id: "docker"},
				{prefix: "libpod-", id: "podman"},
				{prefix: "cri-containerd-", id: "containerd"},
			} {
				if strings.HasPrefix(part, runtime.prefix) && strings.HasSuffix(part, ".scope") &&
					containerID(strings.TrimSuffix(strings.TrimPrefix(part, runtime.prefix), ".scope")) {
					return runtime.id
				}
			}
		}
	}
	return ""
}

func containerID(value string) bool {
	if len(value) < 12 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func trustedContainerID(raw []byte) string {
	for _, field := range strings.FieldsFunc(string(raw), func(r rune) bool {
		return r == 0 || r == '\n' || r == '\r'
	}) {
		field = strings.ToLower(strings.TrimSpace(field))
		field = strings.TrimPrefix(field, "container=")
		switch field {
		case "docker", "podman", "containerd", "lxc", "systemd-nspawn":
			return field
		}
	}
	return ""
}

func bashQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func validateSecurity(state securityState) error {
	if !state.BackendRequired {
		return nil
	}
	if state.Backend != "" {
		return nil
	}
	message := fmt.Sprintf("%s scope is unavailable: %s", state.EffectiveScope, state.Failure)
	if state.EffectiveScope == workspacetools.ScopeWorkspace && state.ProtectedPathCount == 0 {
		message += "; set -scope=machine (or SK_SCOPE=machine) to run without a filesystem boundary"
	}
	return errors.New(message)
}
