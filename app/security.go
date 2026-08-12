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
	RequestedPolicy string
	Backend         string
	Failure         string
	EffectivePolicy string
	Container       string
}

func (state securityState) snapshot() agent.SandboxSnapshot {
	return agent.SandboxSnapshot{
		RequestedPolicy: state.RequestedPolicy,
		EffectivePolicy: state.EffectivePolicy,
		Backend:         state.Backend,
		Container:       state.Container,
		Network:         "inherited",
	}
}

func buildSecurityStateWithToolHome(ctx context.Context, requested, root, home, toolHome string, protectedSets ...[]string) securityState {
	container := ""
	if requested == workspacetools.SandboxAuto {
		container = detectContainer(ctx)
	}
	effective := resolveSandboxPolicy(requested, container)
	state := securityState{RequestedPolicy: requested, EffectivePolicy: effective, Container: container}
	if effective == workspacetools.SandboxOff {
		return state
	}
	if pathsOverlap(root, home) {
		return unavailableSandbox(state, "Skot home overlaps the workspace; keep private state outside the workspace with -home")
	}
	protected := []string{home}
	if len(protectedSets) > 0 {
		protected = append([]string(nil), protectedSets[0]...)
	}
	for _, path := range protected {
		if securityPathContains(path, root) {
			return unavailableSandbox(state, "protected path contains the workspace: "+path)
		}
	}
	if effective == workspacetools.SandboxWorkspace && (pathsOverlap(home, toolHome) || pathsOverlap(root, toolHome)) {
		return unavailableSandbox(state, "tool home must be outside Skot home and the workspace")
	}
	if effective == workspacetools.SandboxWorkspace {
		for _, path := range protected {
			if pathsOverlap(path, toolHome) {
				return unavailableSandbox(state, "protected path overlaps tool home: "+path)
			}
		}
	}
	backend := workspacetools.SandboxBackend()
	if backend == "" {
		return unavailableSandbox(state, "no platform sandbox is available")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return unavailableSandbox(state, "probe setup failed: "+err.Error())
	}
	probe, err := os.CreateTemp(home, ".sandbox-probe-")
	if err != nil {
		return unavailableSandbox(state, "probe setup failed: "+err.Error())
	}
	probePath := probe.Name()
	defer func() { _ = os.Remove(probePath) }()
	if _, err := probe.WriteString("supervisor-only\n"); err != nil {
		_ = probe.Close()
		return unavailableSandbox(state, "probe setup failed: "+err.Error())
	}
	if err := probe.Close(); err != nil {
		return unavailableSandbox(state, "probe setup failed: "+err.Error())
	}
	quotedProbe := bashQuote(probePath)
	command := "if IFS= read -r _ < " + quotedProbe + " 2>/dev/null; then exit 42; fi; " +
		"if : > " + quotedProbe + " 2>/dev/null; then exit 43; fi; " +
		"if rm -f -- " + quotedProbe + " 2>/dev/null; then exit 44; fi; exit 0"
	cmd, err := workspacetools.SandboxedBashCommand(command, root, workspacetools.Sandbox{
		Policy: effective, Workspace: root, ToolHome: toolHome, ProtectedPaths: protected,
	})
	if err != nil {
		return unavailableSandbox(state, "sandbox command failed: "+err.Error())
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
				return unavailableSandbox(state, "sandbox integrity probe changed private state: "+readErr.Error())
			}
			return unavailableSandbox(state, "sandbox integrity probe changed private state")
		}
		state.Backend = backend
		return state
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch exit.ExitCode() {
		case 42:
			return unavailableSandbox(state, "sandbox probe read private state")
		case 43:
			return unavailableSandbox(state, "sandbox probe truncated private state")
		case 44:
			return unavailableSandbox(state, "sandbox probe removed private state")
		}
	}
	reason := "sandbox probe failed: " + err.Error()
	if detail := strings.TrimSpace(diagnostic.String()); detail != "" {
		reason += ": " + detail
	}
	return unavailableSandbox(state, reason)
}

func unavailableSandbox(state securityState, probe string) securityState {
	state.Failure = probe
	return state
}

func resolveSandboxPolicy(requested, container string) string {
	if requested != workspacetools.SandboxAuto {
		return requested
	}
	if container != "" {
		return workspacetools.SandboxMasked
	}
	return workspacetools.SandboxWorkspace
}

func (state securityState) Summary() string {
	text := "sandbox: " + state.EffectivePolicy
	var detail []string
	if state.RequestedPolicy == workspacetools.SandboxAuto {
		detail = append(detail, "auto")
	}
	if state.Container != "" {
		detail = append(detail, state.Container)
	}
	if len(detail) != 0 {
		text += " (" + strings.Join(detail, ", ") + ")"
	}
	return text + " · network: inherited"
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

func pathsOverlap(first, second string) bool {
	return securityPathContains(first, second) || securityPathContains(second, first)
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
	if state.EffectivePolicy == workspacetools.SandboxOff {
		if state.RequestedPolicy != "" && state.RequestedPolicy != workspacetools.SandboxOff {
			return errors.New("sandbox off must be selected explicitly")
		}
		return nil
	}
	if state.Backend != "" {
		return nil
	}
	return fmt.Errorf("%s sandbox is unavailable: %s", state.EffectivePolicy, state.Failure)
}
