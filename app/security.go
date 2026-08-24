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
	"github.com/levmv/skot/internal/canonicalpath"
	workspacetools "github.com/levmv/skot/tools"
)

type securityState struct {
	Scope           workspacetools.Scope
	AddedPaths      []string
	ProtectedPaths  []string
	Backend         string
	Failure         string
	BackendRequired bool
}

func (state securityState) snapshot() agent.ScopeSnapshot {
	return agent.ScopeSnapshot{
		Scope:          string(state.Scope),
		AddedPaths:     append([]string(nil), state.AddedPaths...),
		ProtectedPaths: append([]string(nil), state.ProtectedPaths...),
		Backend:        state.Backend,
		Network:        "inherited",
	}
}

func newSecurityState(scope workspacetools.Scope, addedPaths, protectedPaths []string) securityState {
	return securityState{
		Scope:          scope,
		AddedPaths:     append([]string(nil), addedPaths...),
		ProtectedPaths: append([]string(nil), protectedPaths...),
	}
}

func buildProcessSecurityState(ctx context.Context, state securityState, root, toolHome string) securityState {
	boundary := workspacetools.Boundary{
		Scope: state.Scope, Workspace: root, ToolHome: toolHome,
		AddedPaths: state.AddedPaths, ProtectedPaths: state.ProtectedPaths,
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
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
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

// protectedPathsNotice explains the one consequence of a protected path which
// is impossible to guess. Landlock has no deny rule, so a directory holding a
// protected entry cannot be granted to a process at all: listing it or creating
// entries in it fails for Bash and program tools, while the built-in file tools
// still reach everything the policy allows.
func protectedPathsNotice(state securityState, root string) string {
	if state.Backend != "landlock" {
		return ""
	}
	reachableRoots := append([]string{root}, state.AddedPaths...)
	for _, path := range state.ProtectedPaths {
		if state.Scope == workspacetools.ScopeWorkspace {
			reachable := false
			for _, reachableRoot := range reachableRoots {
				if canonicalpath.Contains(reachableRoot, path) && !canonicalpath.Contains(path, reachableRoot) {
					reachable = true
					break
				}
			}
			if !reachable {
				continue
			}
		}
		return "bash and program tools cannot list " + filepath.Dir(path) +
			" or the directories above it, because a protected path sits inside; the built-in ls and read still can"
	}
	return ""
}

func landlockProtectedPathsNeedBuiltInLS(backend string, paths []string) bool {
	return backend == "landlock" && len(paths) != 0
}

func (state securityState) Summary() string {
	text := "scope: " + string(state.Scope)
	// Machine scope already reaches the whole filesystem, so added directories
	// grant nothing and naming them here would describe reach they do not add.
	// Going back to workspace prints the list again, so nothing is lost.
	if len(state.AddedPaths) != 0 && state.Scope != workspacetools.ScopeMachine {
		text += " · added paths: " + strings.Join(state.AddedPaths, ", ")
	}
	if len(state.ProtectedPaths) != 0 {
		text += " · protected paths: " + strings.Join(state.ProtectedPaths, ", ")
	}
	return text
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
	message := fmt.Sprintf("%s scope is unavailable: %s", state.Scope, state.Failure)
	if state.Scope == workspacetools.ScopeWorkspace && len(state.ProtectedPaths) == 0 {
		message += "; set -scope=machine (or SK_SCOPE=machine) to run without a filesystem boundary"
	} else if state.Scope == workspacetools.ScopeMachine && len(state.ProtectedPaths) != 0 {
		message += "; protected_paths require an active filesystem boundary even in machine scope"
	}
	return errors.New(message)
}
