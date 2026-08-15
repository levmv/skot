package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeSandboxPolicyValues(t *testing.T) {
	for input, want := range map[string]string{
		"": SandboxAuto, "AUTO": SandboxAuto, "masked": SandboxMasked,
		"workspace": SandboxWorkspace, "off": SandboxOff,
	} {
		got, err := NormalizeSandboxPolicy(input)
		if err != nil || got != want {
			t.Fatalf("NormalizeSandboxPolicy(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := NormalizeSandboxPolicy("require"); err == nil {
		t.Fatal("NormalizeSandboxPolicy accepted an unknown policy")
	}
}

func TestSandboxLayoutRejectsUnresolvedAuto(t *testing.T) {
	if err := (Sandbox{Policy: SandboxAuto}).ValidateLayout(); err == nil {
		t.Fatal("process layer accepted unresolved auto policy")
	}
}

func TestMaskedDoesNotValidateUnusedToolHome(t *testing.T) {
	workspace, stateHome := t.TempDir(), t.TempDir()
	manager, err := NewProcessManager(workspace, stateHome, workspace, SandboxMasked)
	if err != nil {
		t.Fatalf("masked depends on unused tool home: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
}

func TestProcessManagerAllowsStateHomeInsideWorkspace(t *testing.T) {
	for _, policy := range []string{SandboxWorkspace, SandboxMasked} {
		t.Run(policy, func(t *testing.T) {
			workspace := t.TempDir()
			stateHome := filepath.Join(workspace, ".skot")
			toolHomeRoot := filepath.Join(workspace, ".cache", "skot", "tool-home")
			manager, err := NewProcessManager(workspace, stateHome, toolHomeRoot, policy)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Close() })
		})
	}
}

func TestSandboxLayoutRejectsUnsafeContainment(t *testing.T) {
	workspaceParent := t.TempDir()
	workspace := filepath.Join(workspaceParent, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		sandbox   Sandbox
		wantError string
	}{
		{
			name: "protected path contains workspace",
			sandbox: Sandbox{
				Policy: SandboxWorkspace, Workspace: workspace, ToolHome: t.TempDir(),
				ProtectedPaths: []string{workspaceParent},
			},
			wantError: "contains the workspace",
		},
		{
			name: "tool home contains workspace",
			sandbox: Sandbox{
				Policy: SandboxWorkspace, Workspace: workspace, ToolHome: workspaceParent,
				ProtectedPaths: []string{t.TempDir()},
			},
			wantError: "tool home must not contain",
		},
		{
			name: "protected path overlaps tool home",
			sandbox: Sandbox{
				Policy: SandboxWorkspace, Workspace: t.TempDir(), ToolHome: filepath.Join(workspaceParent, "tool-home"),
				ProtectedPaths: []string{workspaceParent},
			},
			wantError: "overlaps tool home",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.sandbox.ValidateLayout()
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("ValidateLayout error = %v", err)
			}
		})
	}
}

func TestCanonicalSandboxPathResolvesExistingSymlinkPrefix(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real-cache")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "cache-alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	got := canonicalSandboxPath(filepath.Join(alias, "skot", "tool-home"))
	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(resolvedReal, "skot", "tool-home")
	if got != want {
		t.Fatalf("canonical path = %q; want %q", got, want)
	}
}
