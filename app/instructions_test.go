package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	workspacetools "github.com/levmv/skot/tools"
)

func TestLoadInstructionsUsesCurrentHierarchy(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(original) }()

	root := t.TempDir()
	nested := filepath.Join(root, "pkg", "feature")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "AGENTS.md"), []byte("nested rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}
	prompts, err := loadInstructions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 || !strings.Contains(prompts[0], "root rules") || !strings.Contains(prompts[1], "pkg/AGENTS.md") || !strings.Contains(prompts[1], "nested rules") {
		t.Fatalf("prompts = %#v", prompts)
	}
	got := effectiveInstructions("custom", root, prompts)
	if !strings.HasPrefix(got, "custom\n\n") || !strings.Contains(got, "root rules") {
		t.Fatalf("effective instructions = %q", got)
	}
	// A custom prompt without the token stays exactly as its author wrote it.
	if strings.Contains(got, root) {
		t.Fatalf("custom prompt gained an unrequested workspace root: %q", got)
	}
	if substituted := effectiveInstructions("work in {{workspace_root}} only", root, nil); substituted != "work in "+root+" only" {
		t.Fatalf("substituted prompt = %q", substituted)
	}
	if standard := effectiveInstructions("", root, nil); !strings.Contains(standard, "Your workspace root is "+root+".") ||
		strings.Contains(standard, workspaceRootToken) {
		t.Fatalf("default prompt = %q", standard)
	}
}

func TestLoadInstructionsRejectsSymlinkOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "AGENTS.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := loadInstructions(root); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("load error = %v", err)
	}
}

func TestLoadInstructionsAcceptsCanonicalWorkspaceAlias(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(original) }()

	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	nested := filepath.Join(real, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "AGENTS.md"), []byte("alias rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Chdir(filepath.Join(alias, "nested")); err != nil {
		t.Fatal(err)
	}
	prompts, err := loadInstructions(alias)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 1 || !strings.Contains(prompts[0], "alias rules") {
		t.Fatalf("prompts = %#v", prompts)
	}
}

func TestLoadInstructionsSkipsProtectedDirectPathAndAlias(t *testing.T) {
	root := t.TempDir()
	protected := filepath.Join(root, "private")
	if err := os.Mkdir(protected, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(protected, "AGENTS.md"), []byte("private rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(protected, "AGENTS.md"), filepath.Join(root, "AGENTS.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	policy, err := workspacetools.NewProtectedPathPolicy(root, []string{"private"}, true)
	if err != nil {
		t.Fatal(err)
	}
	prompts, err := loadInstructions(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 0 {
		t.Fatalf("protected instructions = %#v", prompts)
	}
	policy.SetEnabled(false)
	prompts, err = loadInstructions(root, policy)
	if err != nil || len(prompts) != 1 || !strings.Contains(prompts[0], "private rules") {
		t.Fatalf("off instructions/error = %#v / %v", prompts, err)
	}
}
