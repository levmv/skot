package canonicalpath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePreservesMissingSuffixBelowSymlink(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got := Resolve(filepath.Join(alias, "missing", "leaf"))
	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(resolvedReal, "missing", "leaf")
	if got != want {
		t.Fatalf("resolved path = %q; want %q", got, want)
	}
}

func TestContainsUsesCanonicalBoundary(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	outside := filepath.Join(parent, "outside")
	for _, path := range []string{root, outside} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	outward := filepath.Join(root, "outward")
	inward := filepath.Join(outside, "inward")
	if err := os.Symlink(outside, outward); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(root, inward); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	separator := string(filepath.Separator)
	cleanedChild := root + separator + "nested" + separator + ".." + separator + "child"
	escapedSibling := root + separator + ".." + separator + filepath.Base(outside)

	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "equal", path: root, want: true},
		{name: "child", path: filepath.Join(root, "child"), want: true},
		{name: "cleaned child", path: cleanedChild, want: true},
		{name: "dotdot escape", path: escapedSibling, want: false},
		{name: "parent", path: parent, want: false},
		{name: "sibling prefix", path: root + "-other", want: false},
		{name: "outward symlink", path: filepath.Join(outward, "missing"), want: false},
		{name: "inward symlink", path: filepath.Join(inward, "missing"), want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Contains(root, test.path); got != test.want {
				t.Fatalf("Contains(%q, %q) = %v; want %v", root, test.path, got, test.want)
			}
		})
	}
}
