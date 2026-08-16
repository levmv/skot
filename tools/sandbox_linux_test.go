//go:build linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
)

func TestLinuxSandboxDoesNotGrantSharedTemp(t *testing.T) {
	for _, dir := range sandboxWritableDirs(Boundary{Workspace: "/workspace", ToolHome: "/tool-home"}) {
		if dir == "/tmp" || dir == "/var/tmp" {
			t.Fatalf("shared temporary directory is writable: %q", dir)
		}
	}
}

func TestSandboxedProgramDisappearingAfterPreflightIsFatal(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "vanishing")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewProcessManager(root, t.TempDir(), t.TempDir(), ScopeWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	declaration := normalizedProgramTool(t, ProgramTool{Name: "vanishing", Description: "vanishes", Command: []string{"./vanishing"}})
	resolved, err := manager.ResolveProgramTools([]ProgramTool{declaration})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	output, err := resolved[0].Tool.Run(context.Background(), `{}`)
	if !errors.Is(err, agent.ErrToolFatal) || !strings.Contains(output.Content, "status: failed") {
		t.Fatalf("output/error = %q / %v", output.Content, err)
	}
}

func TestSandboxedProgramGetsDeclaredButNotAmbientEnvironment(t *testing.T) {
	root := t.TempDir()
	toolHome := t.TempDir()
	if err := os.Mkdir(WorkspaceToolTemp(toolHome), 0o700); err != nil {
		t.Fatal(err)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is unavailable")
	}
	t.Setenv("SK_PROGRAM_OUTSIDE", "must-not-leak")
	cmd, err := sandboxedProgramCommand(sh, []string{sh, "-c", "env"}, root, Boundary{
		Scope: ScopeWorkspace, Workspace: root, ProtectedPaths: []string{t.TempDir()}, ToolHome: toolHome,
	}, map[string]string{"SK_PROGRAM_DECLARED": "visible"})
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandboxed program failed: %v: %s", err, output)
	}
	text := string(output)
	if strings.Contains(text, "must-not-leak") || !strings.Contains(text, "SK_PROGRAM_DECLARED=visible") {
		t.Fatalf("program environment = %q", text)
	}
}

func TestResolvedBareProgramRunsThroughSandboxChild(t *testing.T) {
	manager, err := NewProcessManager(t.TempDir(), t.TempDir(), t.TempDir(), ScopeWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	declaration := normalizedProgramTool(t, ProgramTool{
		Name: "sandbox_probe", Description: "sandbox probe",
		Command: []string{"sh", "-c", "cat"},
	})
	resolved, err := manager.ResolveProgramTools([]ProgramTool{declaration})
	if err != nil {
		t.Fatal(err)
	}
	output, err := resolved[0].Tool.Run(context.Background(), `{"inside":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.Content, `{"inside":true}`) {
		t.Fatalf("program output = %q", output.Content)
	}
}

func TestSandboxedBashNestedWorkdirCanAccessWorkspace(t *testing.T) {
	root := t.TempDir()
	workdir := filepath.Join(root, "nested")
	if err := os.Mkdir(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("workspace data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	toolHome := t.TempDir()
	cmd, err := sandboxedBashCommand("cat ../source.txt > ../copy.txt", workdir, Boundary{
		Scope: ScopeWorkspace, Workspace: root, ProtectedPaths: []string{t.TempDir()}, ToolHome: toolHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed command failed: %v: %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(root, "copy.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "workspace data\n" {
		t.Fatalf("copy = %q", data)
	}
}

func TestSandboxedToolTempStaysInsideToolHome(t *testing.T) {
	root := t.TempDir()
	toolHome := t.TempDir()
	if err := os.Mkdir(WorkspaceToolTemp(toolHome), 0o700); err != nil {
		t.Fatal(err)
	}
	outside, err := os.CreateTemp("", "skot-sandbox-outside-")
	if err != nil {
		t.Fatal(err)
	}
	outsidePath := outside.Name()
	if _, err := outside.WriteString("not readable"); err != nil {
		t.Fatal(err)
	}
	if err := outside.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outsidePath) })

	command := `printf scratch > "$TMPDIR/scratch.txt"; ` +
		"if cat " + shellQuoteForTest(outsidePath) + " >/dev/null 2>&1; then exit 41; fi"
	cmd, err := sandboxedBashCommand(command, root, Boundary{
		Scope: ScopeWorkspace, Workspace: root, ProtectedPaths: []string{t.TempDir()}, ToolHome: toolHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed command failed: %v: %s", err, output)
	}
	if data, err := os.ReadFile(filepath.Join(WorkspaceToolTemp(toolHome), "scratch.txt")); err != nil || string(data) != "scratch" {
		t.Fatalf("private scratch = %q, %v", data, err)
	}
}

func TestMachineProtectedPathsPreserveAmbientHome(t *testing.T) {
	workspace, stateHome, toolHome := t.TempDir(), t.TempDir(), t.TempDir()
	ambientHome := t.TempDir()
	t.Setenv("HOME", ambientHome)
	secret := filepath.Join(stateHome, "auth.json")
	if err := os.WriteFile(secret, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	aliasRoot := t.TempDir()
	stateAlias := filepath.Join(aliasRoot, "state-alias")
	if err := os.Symlink(stateHome, stateAlias); err != nil {
		t.Fatal(err)
	}
	aliasSecret := filepath.Join(stateAlias, "auth.json")
	procSecret := "/proc/self/root" + secret
	parentProcSecret := fmt.Sprintf("/proc/%d/root%s", os.Getpid(), secret)
	command := "if cat " + shellQuoteForTest(secret) + " >/dev/null 2>&1; then exit 41; fi; " +
		"if : > " + shellQuoteForTest(secret) + " 2>/dev/null; then exit 42; fi; " +
		"if rm -f -- " + shellQuoteForTest(secret) + " 2>/dev/null; then exit 43; fi; " +
		"if cat " + shellQuoteForTest(aliasSecret) + " >/dev/null 2>&1; then exit 44; fi; " +
		"if cat " + shellQuoteForTest(procSecret) + " >/dev/null 2>&1; then exit 45; fi; " +
		"if cat " + shellQuoteForTest(parentProcSecret) + " >/dev/null 2>&1; then exit 46; fi; " +
		`printf '%s' "$HOME" > observed-home; printf masked > workspace-write`
	cmd, err := sandboxedBashCommand(command, workspace, Boundary{
		Scope: ScopeMachine, Workspace: workspace, ProtectedPaths: []string{stateHome}, ToolHome: toolHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("masked command failed: %v: %s", err, output)
	}
	if raw, err := os.ReadFile(secret); err != nil || string(raw) != "secret\n" {
		t.Fatalf("private state = %q, %v", raw, err)
	}
	if raw, err := os.ReadFile(filepath.Join(workspace, "observed-home")); err != nil || string(raw) != ambientHome {
		t.Fatalf("masked HOME = %q, %v", raw, err)
	}
	if raw, err := os.ReadFile(filepath.Join(workspace, "workspace-write")); err != nil || string(raw) != "masked" {
		t.Fatalf("masked workspace write = %q, %v", raw, err)
	}
}

func TestMachineProtectedPathsSurviveSupervisedLaunch(t *testing.T) {
	workspace, stateHome, toolRoot := t.TempDir(), t.TempDir(), t.TempDir()
	secret := filepath.Join(stateHome, "events.jsonl")
	if err := os.WriteFile(secret, []byte("journal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := NewProtectedPathPolicy(workspace, []string{stateHome})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProcessManager(workspace, stateHome, toolRoot, ScopeMachine, policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	command := "if cat " + shellQuoteForTest(secret) + " >/dev/null 2>&1; then exit 41; fi; printf masked"
	started := runProcessResult(t, manager.bash, bashArgs{Command: command, Background: true})
	metadata := processResultForTest(t, started)
	result := runProcessResult(t, manager.job, jobArgs{Action: "wait", JobID: metadata.JobID, TimeoutSeconds: 10})
	finished := processResultForTest(t, result)
	if finished.Status != ProcessCompleted || !strings.Contains(result.Content, "masked") {
		t.Fatalf("supervised masked result = %#v / %q", finished, result.Content)
	}
	if raw, err := os.ReadFile(secret); err != nil || string(raw) != "journal\n" {
		t.Fatalf("private state = %q, %v", raw, err)
	}
}

func TestFullSandboxCannotTruncatePrivateState(t *testing.T) {
	workspace, stateHome, toolHome := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Mkdir(WorkspaceToolTemp(toolHome), 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(stateHome, "auth.json")
	if err := os.WriteFile(secret, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := "if : > " + shellQuoteForTest(secret) + " 2>/dev/null; then exit 41; fi; exit 0"
	cmd, err := sandboxedBashCommand(command, workspace, Boundary{
		Scope: ScopeWorkspace, Workspace: workspace, ProtectedPaths: []string{stateHome}, ToolHome: toolHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("full sandbox command failed: %v: %s", err, output)
	}
	if raw, err := os.ReadFile(secret); err != nil || string(raw) != "secret\n" {
		t.Fatalf("private state was truncated: %q, %v", raw, err)
	}
}

func TestWorkspaceSandboxCarvesProtectedWorkspacePaths(t *testing.T) {
	workspace, stateHome, toolRoot := t.TempDir(), t.TempDir(), t.TempDir()
	private := filepath.Join(workspace, "private")
	public := filepath.Join(workspace, "public")
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "outside.txt")
	secret := filepath.Join(private, "secret.txt")
	if err := os.Mkdir(private, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(public, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideFile, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	policy, err := NewProtectedPathPolicy(workspace, []string{stateHome, "private"})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProcessManager(workspace, stateHome, toolRoot, ScopeWorkspace, policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	command := "if cat " + shellQuoteForTest(secret) + " >/dev/null 2>&1; then exit 41; fi; " +
		"if : > " + shellQuoteForTest(secret) + " 2>/dev/null; then exit 42; fi; " +
		"if cat escape/outside.txt >/dev/null 2>&1; then exit 43; fi; " +
		"if : > escape/outside.txt 2>/dev/null; then exit 44; fi; printf visible > public/result.txt"
	result := runProcessResult(t, manager.bash, bashArgs{Command: command})
	metadata := processResultForTest(t, result)
	if metadata.Status != ProcessCompleted {
		t.Fatalf("workspace protected result = %#v / %q", metadata, result.Content)
	}
	if raw, err := os.ReadFile(secret); err != nil || string(raw) != "secret\n" {
		t.Fatalf("protected file = %q, %v", raw, err)
	}
	if raw, err := os.ReadFile(filepath.Join(public, "result.txt")); err != nil || string(raw) != "visible" {
		t.Fatalf("public file = %q, %v", raw, err)
	}
	if raw, err := os.ReadFile(outsideFile); err != nil || string(raw) != "outside\n" {
		t.Fatalf("outside file = %q, %v", raw, err)
	}
}

func TestMachineScopeHidesMultipleProtectedPaths(t *testing.T) {
	workspace, stateHome, toolRoot := t.TempDir(), t.TempDir(), t.TempDir()
	otherRoot := t.TempDir()
	protectedDir := filepath.Join(otherRoot, "private")
	protectedFile := filepath.Join(otherRoot, "single.txt")
	if err := os.Mkdir(protectedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(stateHome, "auth.json"), filepath.Join(protectedDir, "nested.txt"), protectedFile} {
		if err := os.WriteFile(path, []byte("secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := NewProtectedPathPolicy(workspace, []string{stateHome, protectedDir, protectedFile})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProcessManager(workspace, stateHome, toolRoot, ScopeMachine, policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	command := ""
	for index, path := range []string{filepath.Join(stateHome, "auth.json"), filepath.Join(protectedDir, "nested.txt"), protectedFile} {
		command += fmt.Sprintf("if cat %s >/dev/null 2>&1; then exit %d; fi; ", shellQuoteForTest(path), 41+index)
	}
	command += "printf visible > public.txt"
	result := runProcessResult(t, manager.bash, bashArgs{Command: command})
	metadata := processResultForTest(t, result)
	if metadata.Status != ProcessCompleted {
		t.Fatalf("masked protected result = %#v / %q", metadata, result.Content)
	}
}

func TestStateHomeInsideWorkspaceHasNoSpecialStatus(t *testing.T) {
	for _, scope := range []Scope{ScopeWorkspace, ScopeMachine} {
		t.Run(string(scope), func(t *testing.T) {
			workspace := t.TempDir()
			stateHome := filepath.Join(workspace, ".skot")
			public := filepath.Join(workspace, "project")
			if err := os.MkdirAll(stateHome, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(public, 0o755); err != nil {
				t.Fatal(err)
			}
			secret := filepath.Join(stateHome, "auth.json")
			if err := os.WriteFile(secret, []byte("secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			toolHomeRoot := filepath.Join(workspace, ".cache", "skot", "tool-home")
			manager, err := NewProcessManager(workspace, stateHome, toolHomeRoot, scope)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Close() })

			command := "cat .skot/auth.json > project/result.txt; printf visible > root-created"
			result := runProcessResult(t, manager.bash, bashArgs{Command: command})
			metadata := processResultForTest(t, result)
			if metadata.Status != ProcessCompleted {
				t.Fatalf("nested state result = %#v / %q", metadata, result.Content)
			}
			if raw, err := os.ReadFile(secret); err != nil || string(raw) != "secret\n" {
				t.Fatalf("private state = %q, %v", raw, err)
			}
			if raw, err := os.ReadFile(filepath.Join(public, "result.txt")); err != nil || string(raw) != "secret\n" {
				t.Fatalf("public result = %q, %v", raw, err)
			}
			if raw, err := os.ReadFile(filepath.Join(workspace, "root-created")); err != nil || string(raw) != "visible" {
				t.Fatalf("workspace root entry = %q, %v", raw, err)
			}
		})
	}
}

func shellQuoteForTest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
