//go:build unix

package skills

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestHostPolicyAppliesBeforeDiscoveryAndAgainOnLoad(t *testing.T) {
	root := t.TempDir()
	path := writeSkill(t, root, "review", "Allowed instructions.\n")
	blocked := writeSkill(t, root, "blocked", "Never read.\n")
	// A denied malformed file must report permission, not a parser error.
	writeFile(t, blocked, "not a skill")
	denied := blocked
	allow := func(_ Source, path string) bool { return path != denied }
	catalog := DiscoverWithOptions([]Source{{Path: root}}, DiscoveryOptions{AllowPath: allow})
	if len(catalog.Entries) != 1 || len(catalog.Diagnostics) != 1 || !errors.Is(catalog.Diagnostics[0].Err, os.ErrPermission) {
		t.Fatalf("catalog = %#v", catalog)
	}
	entry, err := catalog.Resolve("review")
	if err != nil {
		t.Fatal(err)
	}
	denied = path
	if _, err := entry.Load(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("updated policy ignored: %v", err)
	}
	// Internal symlink targets also pass through the host's policy.
	target := filepath.Join(filepath.Dir(path), "private.md")
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, "private.md", path)
	denied = target
	if _, err := entry.Load(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("protected symlink target loaded: %v", err)
	}
	// Replacing the package with a link must also recheck its target directory.
	if err := os.Rename(filepath.Dir(path), filepath.Dir(path)+"-old"); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, filepath.Dir(blocked), filepath.Dir(path))
	denied = filepath.Dir(blocked)
	if _, err := entry.Load(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("protected package directory loaded: %v", err)
	}
}
