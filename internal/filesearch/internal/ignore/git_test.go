package ignore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

type gitIgnoreCase struct {
	path  string
	isDir bool
}

func TestMatcherAgreesWithGitCheckIgnore(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	patterns := "*.log\n!important.log\n/build/\n/root-only\na/**/cache\ndata/**\nfile[0-9].tmp\n"
	tests := []gitIgnoreCase{
		{path: "app.log"},
		{path: "important.log"},
		{path: "deep/app.log"},
		{path: "build", isDir: true},
		{path: "build/out.go"},
		{path: "root-only"},
		{path: "deep/root-only"},
		{path: "a/cache"},
		{path: "a/x/y/cache"},
		{path: "data", isDir: true},
		{path: "data/value.json"},
		{path: "file7.tmp"},
		{path: "filex.tmp"},
		{path: "src/main.go"},
	}
	assertGitAgreement(t, git, patterns, tests)
}

func TestEscapedPathSeparatorsAgreeWithGitCheckIgnore(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	tests := []struct {
		name     string
		patterns string
		paths    []gitIgnoreCase
	}{
		{
			name: "ordinary escaped separator", patterns: `foo\/bar` + "\n",
			paths: []gitIgnoreCase{{path: "foo/bar"}, {path: "deep/foo/bar"}, {path: "foo/other"}},
		},
		{
			name: "wildcard before escaped separator", patterns: `*\/tail` + "\n",
			paths: []gitIgnoreCase{{path: "one/tail"}, {path: "deep/one/tail"}, {path: "tail"}},
		},
		{
			name: "multiple escaped separators", patterns: `foo\/bar\/baz` + "\n",
			paths: []gitIgnoreCase{{path: "foo/bar/baz"}, {path: "other/foo/bar/baz"}},
		},
		{
			name: "escaped leading separator is not an anchor", patterns: `\/foo` + "\n",
			paths: []gitIgnoreCase{{path: "foo"}, {path: "deep/foo"}},
		},
		{
			name: "separator inside bracket expression", patterns: `foo[\/]bar` + "\n",
			paths: []gitIgnoreCase{{path: "foo/bar"}, {path: "fooXbar"}},
		},
	}
	if runtime.GOOS != "windows" {
		tests = append(tests, struct {
			name     string
			patterns string
			paths    []gitIgnoreCase
		}{
			name: "even backslash parity", patterns: `foo\\/bar` + "\n",
			paths: []gitIgnoreCase{{path: `foo\/bar`}, {path: "foo/bar"}},
		})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertGitAgreement(t, git, test.patterns, test.paths)
		})
	}
}

func TestNegationAgreesWithGitCheckIgnore(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	t.Run("reincluded directory does not reinclude matching children", func(t *testing.T) {
		assertGitAgreement(t, git, "*\n!foo\n", []gitIgnoreCase{
			{path: "foo", isDir: true},
			{path: "foo/bar"},
		})
	})
	t.Run("ignored parent blocks child negation", func(t *testing.T) {
		assertGitAgreement(t, git, "foo/\n!foo/bar\n", []gitIgnoreCase{
			{path: "foo", isDir: true},
			{path: "foo/bar"},
		})
	})
	t.Run("reincluded directory carries visible state", func(t *testing.T) {
		assertGitAgreement(t, git, "foo/\n!foo/\n", []gitIgnoreCase{
			{path: "foo", isDir: true},
			{path: "foo/bar"},
		})
	})
	t.Run("trailing double star permits direct child negation", func(t *testing.T) {
		assertGitAgreement(t, git, "data/**\n!data/keep\n", []gitIgnoreCase{
			{path: "data", isDir: true},
			{path: "data/drop"},
			{path: "data/keep"},
		})
	})
	t.Run("trailing double star still blocks deep negation", func(t *testing.T) {
		assertGitAgreement(t, git, "data/**\n!data/sub/keep\n", []gitIgnoreCase{
			{path: "data/sub", isDir: true},
			{path: "data/sub/keep"},
		})
	})
	t.Run("UTF-8 BOM", func(t *testing.T) {
		assertGitAgreement(t, git, "\xef\xbb\xbffoo\n", []gitIgnoreCase{{path: "foo"}})
	})
	t.Run("pure double star directory pattern", func(t *testing.T) {
		assertGitAgreement(t, git, "**/\n", []gitIgnoreCase{
			{path: "root.txt"},
			{path: "deep/file.txt"},
		})
	})
	t.Run("trailing double star directory pattern", func(t *testing.T) {
		assertGitAgreement(t, git, "foo/**/\n", []gitIgnoreCase{
			{path: "foo", isDir: true},
			{path: "foo/root.txt"},
			{path: "foo/sub", isDir: true},
			{path: "foo/sub/file.txt"},
		})
	})
	t.Run("leading globstar before trailing double star", func(t *testing.T) {
		assertGitAgreement(t, git, "**/never/**\n", []gitIgnoreCase{
			{path: "never", isDir: true},
			{path: "never/file"},
			{path: "a/never", isDir: true},
			{path: "a/never/file"},
			{path: "a/never/sub/file"},
			{path: "other/file"},
			{path: "a/other/never.txt"},
		})
	})
	t.Run("reversed bracket ranges", func(t *testing.T) {
		assertGitAgreement(t, git, "[z-a]\n", []gitIgnoreCase{
			{path: "z"},
			{path: "a"},
			{path: "m"},
		})
		assertGitAgreement(t, git, "[!z-a]\n", []gitIgnoreCase{
			{path: "z"},
			{path: "a"},
			{path: "m"},
		})
	})
	t.Run("standalone runs of three or more stars", func(t *testing.T) {
		assertGitAgreement(t, git, "a/***/b\ndata/****\n", []gitIgnoreCase{
			{path: "a/b"},
			{path: "a/x/b"},
			{path: "a/x/y/b"},
			{path: "a/x/y/c"},
			{path: "data", isDir: true},
			{path: "data/file"},
			{path: "data/sub/file"},
		})
	})
	t.Run("slash inside bracket expression anchors pattern", func(t *testing.T) {
		assertGitAgreement(t, git, "[a/]\n", []gitIgnoreCase{
			{path: "a", isDir: true},
			{path: "nested/a", isDir: true},
		})
	})
	if runtime.GOOS != "windows" {
		t.Run("backslash parity before trailing space", func(t *testing.T) {
			assertGitAgreement(t, git, "foo\\\\ \n", []gitIgnoreCase{{path: `foo\`}})
		})
	}
}

func assertGitAgreement(t *testing.T, git, patterns string, tests []gitIgnoreCase) {
	t.Helper()
	rules, diagnostics := Compile([]byte(patterns), "", ".gitignore")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	root := t.TempDir()
	gitEnv := append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	command := exec.CommandContext(context.Background(), git, "init", "-q")
	command.Dir = root
	command.Env = gitEnv
	if output, initErr := command.CombinedOutput(); initErr != nil {
		t.Fatalf("git init: %v: %s", initErr, output)
	}
	if writeErr := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(patterns), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	for _, test := range tests {
		absolute := filepath.Join(root, filepath.FromSlash(test.path))
		if test.isDir {
			if mkdirErr := os.MkdirAll(absolute, 0o750); mkdirErr != nil {
				t.Fatal(mkdirErr)
			}
		} else {
			if mkdirErr := os.MkdirAll(filepath.Dir(absolute), 0o750); mkdirErr != nil {
				t.Fatal(mkdirErr)
			}
			if writeErr := os.WriteFile(absolute, []byte("fixture\n"), 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
		}
		command = exec.CommandContext(context.Background(), git, "check-ignore", "--no-index", "-q", "--", test.path)
		command.Dir = root
		command.Env = gitEnv
		gitErr := command.Run()
		gitIgnored := gitErr == nil
		var exitErr *exec.ExitError
		if gitErr != nil && (!errors.As(gitErr, &exitErr) || exitErr.ExitCode() != 1) {
			t.Fatalf("git check-ignore %q: %v", test.path, gitErr)
		}
		if got := rules.Decide(test.path, test.isDir).Ignored; got != gitIgnored {
			t.Errorf("Decide(%q, %t).Ignored = %t, git says %t", test.path, test.isDir, got, gitIgnored)
		}
	}
}
