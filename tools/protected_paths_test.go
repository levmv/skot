package tools

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestProtectedPathPolicyResolvesAndCompactsPaths(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	external := filepath.Join(t.TempDir(), "external-secret")
	policy, err := NewProtectedPathPolicy(root, []string{
		"private",
		"private/nested",
		external,
		"~/home-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		canonicalSandboxPath(filepath.Join(home, "home-secret")),
		canonicalSandboxPath(filepath.Join(root, "private")),
		canonicalSandboxPath(external),
	}
	slices.Sort(want)
	if got := policy.Paths(); !slices.Equal(got, want) {
		t.Fatalf("paths = %#v; want %#v", got, want)
	}
	if !policy.Protects(filepath.Join(root, "private", "future.txt")) {
		t.Fatal("missing descendant is not protected")
	}
}

func TestProcessManagerKeepsProtectedPathsAcrossScopeSwitch(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	secret := filepath.Join(root, "private", "secret.txt")
	mustWriteFile(t, secret, "secret\n")
	policy, err := NewProtectedPathPolicy(root, []string{state, "private"})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProcessManager(root, state, t.TempDir(), ScopeMachine, policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if _, _, err := manager.workspace.resolveReadableFile("private/secret.txt"); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("machine read error = %v", err)
	}
	if err := manager.SetScopeAfter(ScopeWorkspace, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.workspace.resolveReadableFile("private/secret.txt"); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("workspace read error = %v", err)
	}
	userOutput, err := manager.RunShell(context.Background(), "cat private/secret.txt")
	if err != nil || !strings.Contains(userOutput.Content, "secret") {
		t.Fatalf("user shell output/error = %q / %v", userOutput.Content, err)
	}
	if err := manager.SetScopeAfter(ScopeMachine, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.workspace.resolveReadableFile("private/secret.txt"); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("restored machine read error = %v", err)
	}
}

func TestConfiguredProgramCannotResolveInsideProtectedPath(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	program := filepath.Join(root, "private", "program")
	if err := os.MkdirAll(filepath.Dir(program), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(program, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := NewProtectedPathPolicy(root, []string{state, "private"})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProcessManager(root, state, t.TempDir(), ScopeWorkspace, policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	declaration := normalizedProgramTool(t, ProgramTool{
		Name: "private_program", Description: "protected program", Command: []string{"./private/program"},
	})
	if _, err := manager.ResolveProgramTools([]ProgramTool{declaration}); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("ResolveProgramTools error = %v", err)
	}
}

func TestConfiguredProgramProtectionDoesNotDependOnScope(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	program := filepath.Join(root, "private", "program")
	if err := os.MkdirAll(filepath.Dir(program), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf visible\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := NewProtectedPathPolicy(root, []string{state, "private"})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProcessManager(root, state, t.TempDir(), ScopeMachine, policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	declaration := normalizedProgramTool(t, ProgramTool{
		Name: "private_program", Description: "protected program", Command: []string{"./private/program"},
	})
	if _, err := manager.ResolveProgramTools([]ProgramTool{declaration}); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("protected program resolution error = %v", err)
	}
}

func TestProtectedPathPolicyRejectsUnsafeValues(t *testing.T) {
	root := t.TempDir()
	for _, values := range [][]string{{""}, {"~someone/private"}, {string(filepath.Separator)}} {
		if _, err := NewProtectedPathPolicy(root, values); err == nil {
			t.Fatalf("accepted protected paths %#v", values)
		}
	}
}

func TestProtectedPathPolicyRejectsPathContainingWorkspace(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	policy, err := NewProtectedPathPolicy(root, []string{state, parent})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewProcessManager(root, state, t.TempDir(), ScopeMachine, policy)
	if err == nil || !strings.Contains(err.Error(), "contains the workspace") {
		t.Fatalf("NewProcessManager error = %v", err)
	}
}

// A protected path may itself be a symlink, for example a `.env` linked into a
// shared secrets directory. The policy resolves it and protects the target, so
// every name for that file is protected, not just the configured one.
func TestProtectedSymlinkProtectsItsTargetUnderEveryName(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "real.txt"), "credential\n")
	mustWriteFile(t, filepath.Join(root, "ordinary.txt"), "ordinary contents\n")
	if err := os.Symlink("real.txt", filepath.Join(root, "secret.env")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	policy, err := NewProtectedPathPolicy(root, []string{"secret.env"})
	if err != nil {
		t.Fatal(err)
	}
	tools, _, err := NewWorkspaceToolsWithProtection(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"secret.env", "real.txt"} {
		args := `{"path":"` + path + `"}`
		if _, err := runTool(tools, "read", args); err == nil || !strings.Contains(err.Error(), "protected") {
			t.Fatalf("read(%s) = %v", path, err)
		}
	}
	if _, err := runTool(tools, "read", `{"path":"ordinary.txt"}`); err != nil {
		t.Fatalf("unrelated file became unreadable: %v", err)
	}
}
