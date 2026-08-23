package ui

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPathCompletionOffersDirectoriesInTheTypedForm(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	for _, name := range []string{"shared", "shared-library", ".hidden", "notes"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "shared.txt"), []byte("file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "shared", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	separator := string(filepath.Separator)

	for _, test := range []struct {
		name  string
		typed string
		want  []string
	}{
		{name: "empty offers nothing", typed: ""},
		{
			name: "relative prefix keeps its form", typed: "shar",
			want: []string{"shared" + separator, "shared-library" + separator},
		},
		{
			name: "trailing separator lists what is inside", typed: "shared" + separator,
			want: []string{filepath.Join("shared", "nested") + separator},
		},
		{
			name: "hidden directories wait to be named", typed: ".hid",
			want: []string{".hidden" + separator},
		},
		{
			name: "absolute prefix stays absolute", typed: filepath.Join(root, "not"),
			want: []string{filepath.Join(root, "notes") + separator},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := pathCompletionCandidates(&pathCompletionCache{}, root, test.typed); !slices.Equal(got, test.want) {
				t.Fatalf("candidates = %#v, want %#v", got, test.want)
			}
		})
	}

	// A regular file is not a candidate, and a symlinked directory is.
	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := pathCompletionCandidates(&pathCompletionCache{}, root, "link"); !slices.Equal(got, []string{"linked" + separator}) {
		t.Fatalf("symlink candidates = %#v", got)
	}
}

func TestPathCompletionReadsOneDirectoryOnce(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cache := &pathCompletionCache{}
	if got := pathCompletionCandidates(cache, root, "a"); len(got) != 1 {
		t.Fatalf("candidates = %#v", got)
	}
	// Typing on inside the same directory must not read it again: the listing
	// answers every further prefix.
	if err := os.Mkdir(filepath.Join(root, "alpine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := pathCompletionCandidates(cache, root, "al"); len(got) != 1 {
		t.Fatalf("cached candidates = %#v", got)
	}
	cache.reset()
	if got := pathCompletionCandidates(cache, root, "al"); len(got) != 2 {
		t.Fatalf("candidates after reset = %#v", got)
	}
}
