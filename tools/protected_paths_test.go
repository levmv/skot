package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
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
	}, true)
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
	policy.SetEnabled(false)
	if policy.Protects(filepath.Join(root, "private")) {
		t.Fatal("disabled policy still protects paths")
	}
}

func TestProcessManagerSwitchesProtectedPathsWithSandbox(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	secret := filepath.Join(root, "private", "secret.txt")
	mustWriteFile(t, secret, "secret\n")
	policy, err := NewProtectedPathPolicy(root, []string{state, "private"}, false)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProcessManager(root, state, t.TempDir(), SandboxOff, policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if _, _, err := manager.workspace.resolveReadableFile("private/secret.txt"); err != nil {
		t.Fatalf("off read rejected: %v", err)
	}
	if err := manager.SetSandboxAfter(SandboxWorkspace, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.workspace.resolveReadableFile("private/secret.txt"); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("workspace read error = %v", err)
	}
	userOutput, err := manager.RunShell(context.Background(), "cat private/secret.txt")
	if err != nil || !strings.Contains(userOutput.Content, "secret") {
		t.Fatalf("user shell output/error = %q / %v", userOutput.Content, err)
	}
	if err := manager.SetSandboxAfter(SandboxOff, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.workspace.resolveReadableFile("private/secret.txt"); err != nil {
		t.Fatalf("restored off read rejected: %v", err)
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
	policy, err := NewProtectedPathPolicy(root, []string{state, "private"}, true)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProcessManager(root, state, t.TempDir(), SandboxWorkspace, policy)
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

func TestConfiguredProgramBecomesUnavailableAfterSandboxSwitch(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	program := filepath.Join(root, "private", "program")
	if err := os.MkdirAll(filepath.Dir(program), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf visible\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := NewProtectedPathPolicy(root, []string{state, "private"}, false)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProcessManager(root, state, t.TempDir(), SandboxOff, policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	declaration := normalizedProgramTool(t, ProgramTool{
		Name: "private_program", Description: "protected program", Command: []string{"./private/program"},
	})
	resolved, err := manager.ResolveProgramTools([]ProgramTool{declaration})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetSandboxAfter(SandboxMasked, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := resolved[0].Tool.Run(context.Background(), `{}`); !errors.Is(err, agent.ErrToolFatal) || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("protected program output error = %v", err)
	}
}

func TestProtectedPathPolicyRejectsUnsafeValues(t *testing.T) {
	root := t.TempDir()
	for _, values := range [][]string{{""}, {"~someone/private"}, {string(filepath.Separator)}} {
		if _, err := NewProtectedPathPolicy(root, values, true); err == nil {
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
	policy, err := NewProtectedPathPolicy(root, []string{state, parent}, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewProcessManager(root, state, t.TempDir(), SandboxMasked, policy)
	if err == nil || !strings.Contains(err.Error(), "contains the workspace") {
		t.Fatalf("NewProcessManager error = %v", err)
	}
}
