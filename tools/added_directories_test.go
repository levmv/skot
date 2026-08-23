package tools

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestAddedDirectoryPolicyResolvesAndCompactsExistingDirectories(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	nested := filepath.Join(outside, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(outside, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	policy, err := NewAddedDirectoryPolicy(workspace, []string{nested, alias, outside, workspace})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(policy.Paths(), []string{resolved}) {
		t.Fatalf("added paths = %#v", policy.Paths())
	}
	if !policy.Contains(filepath.Join(nested, "file")) || policy.Contains(filepath.Join(t.TempDir(), "file")) {
		t.Fatalf("unexpected added-directory containment")
	}
	if _, err := NewAddedDirectoryPolicy(workspace, []string{filepath.Join(outside, "missing")}); err == nil {
		t.Fatal("missing added directory was accepted")
	}
}

func TestAddedDirectoryPolicyRejectsUnsafeValues(t *testing.T) {
	root := t.TempDir()
	for _, values := range [][]string{{""}, {"~someone/shared"}, {string(filepath.Separator)}, {root + "/missing"}} {
		if _, err := NewAddedDirectoryPolicy(root, values); err == nil {
			t.Fatalf("accepted added directories %#v", values)
		}
	}
}
