package filesearch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestFilesPositiveGlobsOverrideIgnores(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".git/secret.go", "package secret\n")
	writeTestFile(t, root, ".gitignore", "ignored.go\nignored-dir/\nnested/*.tmp\n")
	writeTestFile(t, root, ".config/settings.go", "package settings\n")
	writeTestFile(t, root, "ignored.go", "package ignored\n")
	writeTestFile(t, root, "ignored-dir/inside.go", "package inside\n")
	writeTestFile(t, root, "nested/drop.tmp", "ignored tmp\n")
	writeTestFile(t, root, "visible.go", "package visible\n")
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		query FilesQuery
		want  []string
	}{
		{
			name:  "matching ignored file",
			query: FilesQuery{Glob: "*.go"},
			want:  []string{".config/settings.go", "ignored.go", "visible.go"},
		},
		{
			name:  "wide glob reopens ignored directory",
			query: FilesQuery{Glob: "*"},
			want: []string{
				".config/settings.go", ".gitignore", "ignored-dir/inside.go",
				"ignored.go", "nested/drop.tmp", "visible.go",
			},
		},
		{
			name:  "scoped descendant glob reopens ignored directory",
			query: FilesQuery{Glob: "ignored-dir/**/*.go"},
			want:  []string{"ignored-dir/inside.go"},
		},
		{
			name:  "scoped glob reopens ignored file",
			query: FilesQuery{Path: "nested", Glob: "*.tmp"},
			want:  []string{"nested/drop.tmp"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := collectFiles(t, searcher, test.query)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("paths = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFilesIgnoresAreRootLocal(t *testing.T) {
	parent := t.TempDir()
	writeTestFile(t, parent, ".gitignore", "*.go\n")
	writeTestFile(t, parent, "workspace/visible.go", "package visible\n")
	searcher, err := New(filepath.Join(parent, "workspace"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Glob: "!*.tmp"})
	if want := []string{"visible.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestFilesSkipsLinkedWorktreeGitFile(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".git", "gitdir: ../metadata/worktrees/example\n")
	writeTestFile(t, root, ".gitignore", "ignored.txt\n")
	writeTestFile(t, root, "ignored.txt", "ignored\n")
	writeTestFile(t, root, "visible.go", "package visible\n")
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Glob: "!*.tmp"})
	want := []string{".gitignore", "visible.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestFilesDoesNotFollowIgnoreFileSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, outside, ".gitignore", "visible.go\n")
	writeTestFile(t, outside, "exclude", "visible.go\n")
	if err := os.Symlink(filepath.Join(outside, ".gitignore"), filepath.Join(root, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".git", "info")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "visible.go", "package visible\n")
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Glob: "!*.tmp"})
	if want := []string{"visible.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestFilesIgnoreNoneStillExcludesGitMetadata(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "ignored.txt\n")
	writeTestFile(t, root, "ignored.txt", "visible when ignores are disabled\n")
	writeTestFile(t, root, ".git/secret.txt", "never visible\n")
	searcher, err := New(root, Options{Ignore: IgnoreNone})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Glob: "!*.tmp"})
	want := []string{".gitignore", "ignored.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}

	got = collectFiles(t, searcher, FilesQuery{Path: ".git", Glob: "*"})
	if len(got) != 0 {
		t.Fatalf("explicit .git query returned %q", got)
	}
}

func TestFilesIgnoreSourcePrecedence(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".git/info/exclude", "info.txt\n")
	writeTestFile(t, root, ".gitignore", "*.txt\n!info.txt\n")
	writeTestFile(t, root, ".ignore", "!dot.txt\n!rg.txt\n")
	writeTestFile(t, root, ".rgignore", "rg.txt\n!nested/keep.txt\n")
	writeTestFile(t, root, "dot.txt", "kept by .ignore\n")
	writeTestFile(t, root, "info.txt", "kept by .gitignore\n")
	writeTestFile(t, root, "other.txt", "ignored by .gitignore\n")
	writeTestFile(t, root, "rg.txt", "ignored again by .rgignore\n")
	writeTestFile(t, root, "nested/.gitignore", "keep.txt\n")
	writeTestFile(t, root, "nested/keep.txt", "root .rgignore wins over nested .gitignore\n")

	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Glob: "!*.tmp"})
	want := []string{
		".gitignore",
		".ignore",
		".rgignore",
		"dot.txt",
		"info.txt",
		"nested/.gitignore",
		"nested/keep.txt",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestFilesNestedIgnoreOverridesParentWithinSource(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "*.tmp\n")
	writeTestFile(t, root, "nested/.gitignore", "!keep.tmp\n")
	writeTestFile(t, root, "nested/keep.tmp", "kept by nested rule\n")
	writeTestFile(t, root, "nested/drop.tmp", "ignored by root rule\n")
	writeTestFile(t, root, "sibling/keep.tmp", "nested rule must not escape its scope\n")

	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Glob: "!*.none"})
	want := []string{".gitignore", "nested/.gitignore", "nested/keep.tmp"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestFilesNegationDoesNotOverrideRulesForChildren(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "*\n!foo\n")
	writeTestFile(t, root, "foo/bar", "still ignored by star\n")

	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := collectFiles(t, searcher, FilesQuery{Glob: "!*.none"}); len(got) != 0 {
		t.Fatalf("paths = %q, want no visible files", got)
	}
}

func TestFilesHigherPriorityDirectoryNegationDoesNotMaskChildRule(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "foo/\n*.tmp\n")
	writeTestFile(t, root, ".ignore", "!foo/\n")
	writeTestFile(t, root, "foo/a.txt", "visible\n")
	writeTestFile(t, root, "foo/a.tmp", "ignored by the child-level rule\n")

	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Glob: "!*.none"})
	want := []string{".gitignore", ".ignore", "foo/a.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestFilesExplicitStartCarriesIgnoredAncestorState(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "ignored/\n")
	writeTestFile(t, root, "ignored/sub/file.txt", "inside ignored ancestor\n")

	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := collectFiles(t, searcher, FilesQuery{Path: "ignored/sub", Glob: "!*.none"}); len(got) != 0 {
		t.Fatalf("ordinary query paths = %q, want none", got)
	}
	got := collectFiles(t, searcher, FilesQuery{Path: "ignored/sub", Glob: "*"})
	if want := []string{"ignored/sub/file.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("overriding query paths = %q, want %q", got, want)
	}
}

func TestFilesDoesNotReadIgnoreFilesBelowExcludedDirectory(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "ignored/\n")
	writeTestFile(t, root, "ignored/.gitignore", "invalid[\n")
	writeTestFile(t, root, "ignored/sub/.gitignore", "also-invalid[\n")
	writeTestFile(t, root, "ignored/sub/inside.go", "package inside\n")

	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	queries := []FilesQuery{
		{Glob: "ignored/**/*.go"},
		{Path: "ignored/sub", Glob: "ignored/sub/*.go"},
	}
	for _, query := range queries {
		got := collectFiles(t, searcher, query)
		if want := []string{"ignored/sub/inside.go"}; !reflect.DeepEqual(got, want) {
			t.Errorf("Files(%+v) = %q, want %q", query, got, want)
		}
	}
}

func TestFilesGitignoreUTF8BOM(t *testing.T) {
	root := t.TempDir()
	writeTestBytes(t, root, ".gitignore", []byte("\xef\xbb\xbffoo\n"))
	writeTestFile(t, root, "foo", "ignored\n")

	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Glob: "!*.none"})
	if want := []string{".gitignore"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestFilesTrailingDoubleStarAllowsNestedReinclude(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "data/**\n")
	writeTestFile(t, root, "data/.gitignore", "!keep.txt\n")
	writeTestFile(t, root, "data/keep.txt", "kept by nested rule\n")
	writeTestFile(t, root, "data/drop.txt", "ignored by root rule\n")

	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Glob: "!*.none"})
	want := []string{".gitignore", "data/keep.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestFilesTrailingDoubleStarDirectoryDoesNotIgnoreBaseContents(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "foo/**/\n")
	writeTestFile(t, root, "foo/root.txt", "visible directly below foo\n")
	writeTestFile(t, root, "foo/sub/drop.txt", "ignored below a descendant directory\n")
	writeTestFile(t, root, "outside.txt", "visible outside foo\n")

	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Glob: "!*.none"})
	want := []string{".gitignore", "foo/root.txt", "outside.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestFilesManyStarGlobstarMatchesZeroOrManyDirectories(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "ignored/\n")
	writeTestFile(t, root, "ignored/inside.go", "package inside\n")
	writeTestFile(t, root, "ignored/x/y/inside.go", "package inside\n")
	writeTestFile(t, root, "ignored/x/y/other.txt", "not selected\n")

	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Glob: "ignored/***/inside.go"})
	want := []string{"ignored/inside.go", "ignored/x/y/inside.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestFilesIgnoreBraceAlternativesAndCRLF(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "*.{log,tmp}\r\n!keep.log\r\n# unmatched { in a comment\r\n")
	writeTestFile(t, root, "drop.log", "ignored\n")
	writeTestFile(t, root, "drop.tmp", "ignored\n")
	writeTestFile(t, root, "keep.log", "re-included\n")
	writeTestFile(t, root, "visible.go", "visible\n")
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Glob: "!*.md"})
	want := []string{".gitignore", "keep.log", "visible.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestFilesReportsIgnorePatternLocation(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "valid.tmp\ninvalid[\n")
	writeTestFile(t, root, "visible.go", "package visible\n")
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = searcher.Files(context.Background(), FilesQuery{Glob: "*.go"}, func(File) error { return nil })
	if err == nil || !strings.Contains(err.Error(), ".gitignore:2") || !strings.Contains(err.Error(), "invalid[") {
		t.Fatalf("ignore diagnostic = %v", err)
	}
}

func TestFilesStopAndCallbackError(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		writeTestFile(t, root, name, name)
	}
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}

	calls := 0
	stats, err := searcher.Files(context.Background(), FilesQuery{Glob: "*.go"}, func(File) error {
		calls++
		if calls == 2 {
			return errors.Join(errors.New("limit reached"), ErrStop)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || stats.Results != 1 || !stats.Stopped {
		t.Fatalf("calls=%d stats=%+v", calls, stats)
	}

	wantErr := errors.New("sink failed")
	stats, err = searcher.Files(context.Background(), FilesQuery{Glob: "*.go"}, func(File) error {
		return wantErr
	})
	if !errors.Is(err, wantErr) || stats.Results != 0 || stats.Stopped {
		t.Fatalf("error=%v stats=%+v", err, stats)
	}
}

func TestFilesUsesGlobalLexicalPathOrder(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a/inside.go", "package inside\n")
	writeTestFile(t, root, "a.go", "package a\n")
	writeTestFile(t, root, "a0.go", "package a0\n")
	searcher, err := New(root, Options{Ignore: IgnoreNone})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Glob: "*.go"})
	want := []string{"a.go", "a/inside.go", "a0.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want global lexical order %q", got, want)
	}
}

func TestFilesDirectGlobDoesNotInheritDirectoryMatches(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "foo/inside.txt", "not selected by the literal foo glob\n")
	writeTestFile(t, root, "foo/.filesearch-match-end", "must not collide with matcher internals\n")
	writeTestFile(t, root, "other/foo", "selected basename\n")
	writeTestFile(t, root, "other/keep.txt", "ordinary file\n")
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}

	got := collectFiles(t, searcher, FilesQuery{Glob: "foo"})
	if want := []string{"other/foo"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("literal glob paths = %q, want %q", got, want)
	}
	got = collectFiles(t, searcher, FilesQuery{Glob: "foo/**"})
	if want := []string{"foo/.filesearch-match-end", "foo/inside.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("descendant glob paths = %q, want %q", got, want)
	}
	got = collectFiles(t, searcher, FilesQuery{Glob: "!foo"})
	if want := []string{"other/keep.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("negative directory glob paths = %q, want %q", got, want)
	}
}

func TestFilesUnicodeGlob(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.go", "é.go", "日本.go"} {
		writeTestFile(t, root, name, name)
	}
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		pattern string
		want    []string
	}{
		{pattern: "?.go", want: []string{"a.go"}},
		{pattern: "??.go", want: []string{"é.go"}},
		{pattern: "*.go", want: []string{"a.go", "é.go", "日本.go"}},
	}
	for _, test := range cases {
		t.Run(test.pattern, func(t *testing.T) {
			got := collectFiles(t, searcher, FilesQuery{Glob: test.pattern})
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("paths = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFilesCancellation(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "visible.go", "package visible\n")
	writeTestFile(t, root, "z.go", "package z\n")
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, err = searcher.Files(ctx, FilesQuery{Glob: "*"}, func(File) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("error=%v called=%t", err, called)
	}

	ctx, cancel = context.WithCancel(context.Background())
	stats, err := searcher.Files(ctx, FilesQuery{Glob: "*"}, func(File) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || stats.Results != 1 {
		t.Fatalf("mid-walk cancellation error=%v stats=%+v", err, stats)
	}
}

func TestFilesValidatesQueries(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "file.txt", "content\n")
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tests := []FilesQuery{
		{Glob: ""},
		{Glob: "!"},
		{Glob: "foo{bar,baz"},
		{Path: "../outside", Glob: "*"},
		{Path: filepath.Join(root, "file.txt"), Glob: "*"},
		{Path: "file.txt", Glob: "*"},
	}
	for _, query := range tests {
		if _, err := searcher.Files(context.Background(), query, func(File) error { return nil }); err == nil {
			t.Errorf("Files(%+v) unexpectedly succeeded", query)
		}
	}
	if _, err := searcher.Files(context.Background(), FilesQuery{Glob: "*"}, nil); err == nil {
		t.Error("nil callback unexpectedly succeeded")
	}
	if _, err := New(root, Options{Ignore: IgnoreMode(99)}); err == nil {
		t.Error("invalid IgnoreMode unexpectedly succeeded")
	}
}

func TestCompileDirectPatternBoundsInput(t *testing.T) {
	if _, err := compileDirectPattern(strings.Repeat("x", maxPatternBytes)); err != nil {
		t.Fatalf("maximum-size glob: %v", err)
	}
	if _, err := compileDirectPattern(strings.Repeat("x", maxPatternBytes+1)); err == nil ||
		!strings.Contains(err.Error(), "glob exceeds 65535 bytes") {
		t.Fatalf("oversized glob error = %v", err)
	}
	alternatives := strings.TrimSuffix(strings.Repeat("a,", 17), ",")
	prefix := strings.Repeat("x", maxPatternBytes-len(alternatives)-2)
	if _, err := compileDirectPattern(prefix + "{" + alternatives + "}"); err == nil ||
		!strings.Contains(err.Error(), "glob expansion exceeds 1048576 bytes") {
		t.Fatalf("oversized expansion error = %v", err)
	}
}

func TestFilesDoesNotFollowDiscoveredSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, root, "real/in-root.go", "package real\n")
	writeTestFile(t, outside, "outside.go", "package outside\n")
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "outside.go"), filepath.Join(root, "linked-file.go")); err != nil {
		t.Fatal(err)
	}
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Glob: "*.go"})
	if want := []string{"real/in-root.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}

	// An explicitly named in-root directory symlink is allowed, but a symlink
	// that resolves outside the root is rejected by the rooted path policy.
	got = collectFiles(t, searcher, FilesQuery{Path: "linked-dir", Glob: "*.go"})
	if want := []string{"linked-dir/in-root.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit symlink paths = %q, want %q", got, want)
	}
	if _, err := searcher.Files(
		context.Background(),
		FilesQuery{Path: "linked-file.go", Glob: "*"},
		func(File) error { return nil },
	); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("outside symlink error = %v", err)
	}
}

func TestFilesPinsValidatedExplicitDirectorySymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, root, "real/inside.go", "package inside\n")
	writeTestFile(t, outside, "outside.go", "package outside\n")
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(root, "real"), alias); err != nil {
		t.Fatal(err)
	}
	searcher, err := New(root, Options{Ignore: IgnoreNone})
	if err != nil {
		t.Fatal(err)
	}
	directory, err := searcher.resolveDirectory("alias")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatal(err)
	}
	direct, err := compileDirectPattern("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []walkedFile
	walker := fileWalker{
		searcher: searcher,
		query:    directory,
		direct:   direct,
		visit: func(file walkedFile) error {
			files = append(files, file)
			return nil
		},
	}
	if _, err := walker.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []walkedFile{{
		abs:  filepath.Join(searcher.Root(), "real", "inside.go"),
		path: "alias/inside.go",
	}}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("files = %#v, want %#v", files, want)
	}
}

func TestFilesExplicitSymlinkCannotAliasGitMetadata(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available on Windows")
	}
	root := t.TempDir()
	writeTestFile(t, root, ".git/secret.txt", "secret\n")
	if err := os.Symlink(filepath.Join(root, ".git"), filepath.Join(root, "metadata")); err != nil {
		t.Fatal(err)
	}
	searcher, err := New(root, Options{Ignore: IgnoreNone})
	if err != nil {
		t.Fatal(err)
	}
	if got := collectFiles(t, searcher, FilesQuery{Path: "metadata", Glob: "*"}); len(got) != 0 {
		t.Fatalf("metadata alias returned %q", got)
	}
}

func TestNewRejectsRootInsideGitMetadata(t *testing.T) {
	root := t.TempDir()
	metadata := filepath.Join(root, ".git", "objects")
	if err := os.MkdirAll(metadata, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := New(metadata, Options{Ignore: IgnoreNone}); err == nil ||
		!strings.Contains(err.Error(), "root is inside Git metadata") {
		t.Fatalf("metadata root error = %v", err)
	}
}

func TestFilesLoadsIgnoresThroughValidatedExplicitSymlinkAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available on Windows")
	}
	root := t.TempDir()
	writeTestFile(t, root, "real/.gitignore", "*.tmp\n")
	writeTestFile(t, root, "real/sub/drop.tmp", "ignored\n")
	writeTestFile(t, root, "real/sub/keep.go", "package keep\n")
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Path: "alias/sub", Glob: "!*.none"})
	want := []string{"alias/sub/keep.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestFilesDirectBraceAlternativesKeepLeadingMarkersLiteral(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"#hash", "!bang", "plain", "other"} {
		writeTestFile(t, root, path, "fixture\n")
	}
	searcher, err := New(root, Options{Ignore: IgnoreNone})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		glob string
		want []string
	}{
		{glob: "#hash", want: []string{"#hash"}},
		{glob: "{#hash,plain}", want: []string{"#hash", "plain"}},
		{glob: "{!bang,plain}", want: []string{"!bang", "plain"}},
		{glob: `\!bang`, want: []string{"!bang"}},
	}
	for _, test := range tests {
		if got := collectFiles(t, searcher, FilesQuery{Glob: test.glob}); !reflect.DeepEqual(got, test.want) {
			t.Errorf("glob %q paths = %q, want %q", test.glob, got, test.want)
		}
	}
}

func TestFilesIgnoreBraceAlternativesKeepLeadingMarkersLiteral(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "{#hash,!bang,plain}\n")
	for _, path := range []string{"#hash", "!bang", "plain", "visible"} {
		writeTestFile(t, root, path, "fixture\n")
	}
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Glob: "!*.none"})
	want := []string{".gitignore", "visible"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestExpandBraces(t *testing.T) {
	for _, test := range []struct {
		pattern string
		want    []string
	}{
		{pattern: "**/*.{go,txt}", want: []string{"**/*.go", "**/*.txt"}},
		{pattern: "{{a,b}}", want: []string{"{a}", "{b}"}},
		{pattern: "x{{a,b}}y", want: []string{"x{a}y", "x{b}y"}},
		{pattern: "{a,{b,c}}", want: []string{"a", "b", "c"}},
		{pattern: "{{a,b},{c,d}}", want: []string{"a", "b", "c", "d"}},
	} {
		got, err := expandBraces(test.pattern)
		if err != nil {
			t.Fatalf("expandBraces(%q): %v", test.pattern, err)
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("expandBraces(%q) = %q, want %q", test.pattern, got, test.want)
		}
	}
	if _, err := expandBraces("{a,b"); err == nil {
		t.Error("unmatched brace unexpectedly succeeded")
	}
	if _, err := expandBraces("{,a}"); err == nil {
		t.Error("empty alternative unexpectedly succeeded")
	}
}

func TestExpandBracesHandlesDeepLiteralWrappersIteratively(t *testing.T) {
	const depth = 10_000
	opening := strings.Repeat("{", depth)
	closing := strings.Repeat("}", depth)
	got, err := expandBraces(opening + "{a,b}" + closing)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{opening + "a" + closing, opening + "b" + closing}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expansion lengths = %d, want two %d-byte patterns", len(got), len(want[0]))
	}
}

func TestFilesSupportsEscapedPathSeparators(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "foo/bar.txt", "fixture\n")
	writeTestFile(t, root, "other/bar.txt", "fixture\n")
	searcher, err := New(root, Options{Ignore: IgnoreNone})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Glob: `foo\/bar.txt`})
	if want := []string{"foo/bar.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestFilesSlashInsideBracketExpressionAnchorsGlob(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a", "root\n")
	writeTestFile(t, root, "nested/a", "nested\n")
	searcher, err := New(root, Options{Ignore: IgnoreNone})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Glob: "[a/]"})
	if want := []string{"a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestFilesSupportsEscapedPathSeparatorsInIgnoreFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", `foo\/bar.txt`+"\n")
	writeTestFile(t, root, "foo/bar.txt", "fixture\n")
	writeTestFile(t, root, "foo/keep.txt", "fixture\n")
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectFiles(t, searcher, FilesQuery{Glob: "!*.none"})
	want := []string{".gitignore", "foo/keep.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestExpandBracesSkipsClosingBracketAtStartOfClass(t *testing.T) {
	for _, pattern := range []string{"[]{,}]", "[]a{b,c}]", "[!]a{b,c}]"} {
		got, err := expandBraces(pattern)
		if err != nil {
			t.Fatalf("expandBraces(%q): %v", pattern, err)
		}
		if want := []string{pattern}; !reflect.DeepEqual(got, want) {
			t.Errorf("expandBraces(%q) = %q, want unchanged", pattern, got)
		}
	}
	compiled, err := compileDirectPattern("[]{,}]")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"]", "{", ",", "}"} {
		if !compiled.matches(path, false) {
			t.Errorf("bracket expression did not match %q", path)
		}
	}
}

func collectFiles(t *testing.T, searcher *Searcher, query FilesQuery) []string {
	t.Helper()
	var paths []string
	if _, err := searcher.Files(context.Background(), query, func(file File) error {
		paths = append(paths, file.Path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return paths
}

func writeTestFile(t *testing.T, root, path, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
