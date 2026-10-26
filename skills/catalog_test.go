//go:build unix

package skills

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDiscoverResolvesNamesBySourceOrderAndPreservesExplicitPaths(t *testing.T) {
	project, user := t.TempDir(), t.TempDir()
	projectPath := writeSkill(t, project, "review", "Project procedure.\n")
	userPath := writeSkill(t, user, "review", "Personal procedure.\n")
	alias := filepath.Join(t.TempDir(), "alias")
	mustSymlink(t, project, alias)
	mustSymlink(t, "review", filepath.Join(project, "review-alias"))
	catalog := Discover([]Source{
		{Path: project, Label: "project"},
		{Path: user, Label: "user"},
		{Path: alias, Label: "alias"},
	})
	if len(catalog.Entries) != 2 || len(catalog.Diagnostics) != 0 {
		t.Fatalf("catalog = %#v", catalog)
	}
	if entry, err := catalog.Resolve("review"); err != nil || entry.Path != projectPath {
		t.Fatalf("preferred name = %#v, %v", entry, err)
	}
	reversed := Discover([]Source{{Path: user, Label: "user"}, {Path: project, Label: "project"}})
	if entry, err := reversed.Resolve("review"); err != nil || entry.Path != userPath {
		t.Fatalf("reversed source order = %#v, %v", entry, err)
	}
	for _, test := range []struct {
		path, source, body string
	}{
		{projectPath, "project", "Project procedure.\n"},
		{userPath, "user", "Personal procedure.\n"},
	} {
		entry, err := catalog.Resolve(test.path)
		if err != nil || entry.Source.Label != test.source {
			t.Fatalf("resolve %s: %#v, %v", test.path, entry, err)
		}
		document, err := entry.Load()
		if err != nil || document.Body != test.body {
			t.Fatalf("load %s: %#v, %v", test.path, document, err)
		}
	}
	if _, err := catalog.Resolve(filepath.Join(t.TempDir(), "SKILL.md")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown path: %v", err)
	}
}

func TestDiscoverReportsBrokenPackagesWithoutHidingValidOnes(t *testing.T) {
	root := t.TempDir()
	valid := writeSkill(t, root, "review", "Instructions.\n")
	writeFile(t, filepath.Join(root, "broken", "SKILL.md"), "---\nname: broken\n---\n")
	writeFile(t, filepath.Join(root, "mismatch", "SKILL.md"), manifest("other", "Instructions.\n"))
	writeFile(t, filepath.Join(root, "README.md"), "Not a skill.\n")
	writeFile(t, filepath.Join(root, "flat.md"), manifest("flat", "Not a package.\n"))
	writeFile(t, filepath.Join(root, "empty", "README.md"), "Not a skill.\n")
	writeSkill(t, filepath.Join(root, "collection"), "nested", "Not a direct child.\n")
	notDirectory := filepath.Join(t.TempDir(), "file")
	writeFile(t, notDirectory, "Not a source directory.\n")
	catalog := Discover([]Source{
		{Path: filepath.Join(t.TempDir(), "missing")},
		{Path: notDirectory},
		{Path: root},
	})
	if len(catalog.Entries) != 1 || catalog.Entries[0].Metadata.Name != "review" || len(catalog.Diagnostics) != 3 {
		t.Fatalf("catalog = %#v", catalog)
	}
	for _, diagnostic := range catalog.Diagnostics {
		if diagnostic.Path == "" || diagnostic.Err == nil {
			t.Fatalf("incomplete diagnostic: %#v", diagnostic)
		}
	}
	outside := writeSkill(t, t.TempDir(), "outside", "Outside sources.\n")
	selected := DiscoverWithOptions([]Source{{Path: root}}, DiscoveryOptions{Paths: []string{valid, outside}})
	if len(selected.Entries) != 1 || selected.Entries[0].Path != valid || len(selected.Diagnostics) != 0 {
		t.Fatal("selective discovery read unrelated manifests or authorized an unregistered path")
	}
}

func TestLoadReadsCurrentContentAndInvocationMetadata(t *testing.T) {
	root := t.TempDir()
	path := writeSkill(t, root, "review", "Old instructions.\n")
	catalog := Discover([]Source{{Path: root}})
	entry, err := catalog.Resolve("review")
	if err != nil {
		t.Fatal(err)
	}
	raw := "---\nname: review\ndescription: Updated review.\ndisable-model-invocation: true\n---\nNew instructions.\n"
	if err := os.Rename(filepath.Dir(path), filepath.Dir(path)+"-old"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, raw)
	document, err := entry.Load()
	if err != nil || document.Raw != raw || !document.Metadata.DisableModelInvocation {
		t.Fatalf("current document = %#v, %v", document, err)
	}
	if entry.Metadata.DisableModelInvocation || entry.Metadata.Description != "Review changes." {
		t.Fatalf("discovery snapshot was changed: %#v", entry.Metadata)
	}
	writeFile(t, path, "---\nname: review\ndescription: [\n---\n")
	if _, err := entry.Load(); err == nil {
		t.Fatal("invalid edited manifest was accepted")
	}
}

func TestDiscoverReportsOversizedDocuments(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "review", strings.Repeat("x", MaxDocumentBytes))
	catalog := Discover([]Source{{Path: root}})
	if len(catalog.Entries) != 0 || len(catalog.Diagnostics) != 1 ||
		catalog.Diagnostics[0].Path != filepath.Join(root, "review", "SKILL.md") || !strings.Contains(catalog.Diagnostics[0].Err.Error(), "exceeds") {
		t.Fatalf("oversized document discovery: %#v", catalog)
	}
}

func TestDiscoveryAllowsPackageLinksAndConfinesManifests(t *testing.T) {
	root := t.TempDir()
	inside := writeSkill(t, root, "review", "Instructions.\n")
	external := writeSkill(t, t.TempDir(), "outside", "Outside source.\n")
	mustSymlink(t, filepath.Dir(external), filepath.Join(root, "outside"))
	writeFile(t, filepath.Join(root, "linked", "placeholder"), "")
	mustSymlink(t, "../review/SKILL.md", filepath.Join(root, "linked", "SKILL.md"))
	catalog := Discover([]Source{{Path: root}})
	if len(catalog.Entries) != 2 || len(catalog.Diagnostics) != 1 {
		t.Fatalf("package and manifest links: %#v", catalog)
	}
	linked, err := catalog.Resolve("outside")
	if err != nil {
		t.Fatal(err)
	}
	if document, err := linked.Load(); err != nil || document.Body != "Outside source.\n" {
		t.Fatalf("linked package: %#v, %v", document, err)
	}
	denied := DiscoverWithOptions([]Source{{Path: root}}, DiscoveryOptions{AllowPath: func(_ Source, path string) bool {
		return path != filepath.Dir(external)
	}})
	if len(denied.Entries) != 1 || len(denied.Diagnostics) != 2 {
		t.Fatalf("host policy ignored for an external package: %#v", denied)
	}
	entry, err := catalog.Resolve("review")
	if err != nil {
		t.Fatal(err)
	}
	// A relative symlink inside the package remains a valid manifest.
	if err := os.Rename(inside, filepath.Join(filepath.Dir(inside), "instructions.md")); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, "instructions.md", inside)
	if _, err := entry.Load(); err != nil {
		t.Fatalf("internal manifest symlink: %v", err)
	}
	if err := os.Remove(inside); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, external, inside)
	if _, err := entry.Load(); err == nil {
		t.Fatal("manifest escaped its package after discovery")
	}
}

func TestManifestMustBeARegularFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := Discover([]Source{{Path: root}})
	if len(catalog.Entries) != 0 || len(catalog.Diagnostics) != 1 {
		t.Fatalf("FIFO discovery: %#v", catalog)
	}
}

func manifest(name, body string) string {
	return "---\nname: " + name + "\ndescription: Review changes.\n---\n" + body
}

func writeSkill(t *testing.T, root, name, body string) string {
	t.Helper()
	path := filepath.Join(root, name, "SKILL.md")
	writeFile(t, path, manifest(name, body))
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
}
