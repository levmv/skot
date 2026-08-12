package tools

import (
	"os"
	"path/filepath"
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

func TestProcessLayerRejectsUnresolvedAuto(t *testing.T) {
	if err := validateConcreteSandboxPolicy(SandboxAuto); err == nil {
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
