package filesearch

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSearchRepositoryIgnoresAndIncludeOverrides(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".git/metadata.txt", "needle git metadata\n")
	writeTestFile(t, root, ".gitignore", "ignored.txt\nignored-dir/\n")
	writeTestFile(t, root, ".config/settings.go", "needle hidden\n")
	writeTestFile(t, root, "ignored.txt", "needle ignored file\n")
	writeTestFile(t, root, "ignored-dir/inside.go", "needle ignored directory\n")
	writeTestFile(t, root, "visible.go", "Alpha visible\nneedle visible\n")
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		query SearchQuery
		want  []Match
	}{
		{
			name:  "default ignores with hidden files",
			query: SearchQuery{Pattern: "needle"},
			want: []Match{
				{Path: ".config/settings.go", Line: 1, Text: "needle hidden"},
				{Path: "visible.go", Line: 2, Text: "needle visible"},
			},
		},
		{
			name:  "matching include reopens ignored file",
			query: SearchQuery{Pattern: "needle", Include: "*.txt"},
			want:  []Match{{Path: "ignored.txt", Line: 1, Text: "needle ignored file"}},
		},
		{
			name:  "wide include reopens ignored directory",
			query: SearchQuery{Pattern: "needle", Include: "*"},
			want: []Match{
				{Path: ".config/settings.go", Line: 1, Text: "needle hidden"},
				{Path: "ignored-dir/inside.go", Line: 1, Text: "needle ignored directory"},
				{Path: "ignored.txt", Line: 1, Text: "needle ignored file"},
				{Path: "visible.go", Line: 2, Text: "needle visible"},
			},
		},
		{
			name:  "inline case insensitive regexp",
			query: SearchQuery{Pattern: "(?i)^alpha"},
			want:  []Match{{Path: "visible.go", Line: 1, Text: "Alpha visible"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := collectMatches(t, searcher, test.query)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("matches = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestSearchLineSemantics(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "lines.txt", "alpha alpha\r\n\r\nlast-no-newline")
	searcher, err := New(root, Options{Ignore: IgnoreNone})
	if err != nil {
		t.Fatal(err)
	}
	matches := collectMatches(t, searcher, SearchQuery{Pattern: `alpha|^$|last-no-newline$`})
	want := []Match{
		{Path: "lines.txt", Line: 1, Text: "alpha alpha"},
		{Path: "lines.txt", Line: 2, Text: ""},
		{Path: "lines.txt", Line: 3, Text: "last-no-newline"},
	}
	if !reflect.DeepEqual(matches, want) {
		t.Fatalf("matches = %#v, want %#v", matches, want)
	}
}

func TestSearchMatchesFullLineBeforeUTF8Preview(t *testing.T) {
	root := t.TempDir()
	line := strings.Repeat("é", 200) + "needle"
	writeTestFile(t, root, "long.txt", line+"\n")
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	matches := collectMatches(t, searcher, SearchQuery{Path: "long.txt", Pattern: "needle", PreviewBytes: 5})
	want := []Match{{Path: "long.txt", Line: 1, Text: "éé", LineTruncated: true}}
	if !reflect.DeepEqual(matches, want) {
		t.Fatalf("matches = %#v, want %#v", matches, want)
	}
	if !utf8.ValidString(matches[0].Text) || len(matches[0].Text) > 5 {
		t.Fatalf("preview is not a valid bounded UTF-8 string: %q", matches[0].Text)
	}

	matches = collectMatches(t, searcher, SearchQuery{Path: "long.txt", Pattern: "needle"})
	if len(matches) != 1 || matches[0].Text != line || matches[0].LineTruncated {
		t.Fatalf("complete-line search = %#v", matches)
	}

	asciiLine := strings.Repeat("x", 350) + "needle"
	writeTestFile(t, root, "ascii-long.txt", asciiLine+"\n")
	matches = collectMatches(t, searcher, SearchQuery{Path: "ascii-long.txt", Pattern: "needle", PreviewBytes: 300})
	want = []Match{{Path: "ascii-long.txt", Line: 1, Text: strings.Repeat("x", 300), LineTruncated: true}}
	if !reflect.DeepEqual(matches, want) {
		t.Fatalf("300-byte preview = %#v, want %#v", matches, want)
	}
}

func TestSearchSkipsWholeBinaryAndInvalidUTF8Files(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "valid.txt", "needle valid\n")
	writeTestBytes(t, root, "binary.txt", []byte("needle before NUL\n\x00binary"))
	writeTestBytes(t, root, "invalid.txt", append([]byte("needle before invalid\n"), 0xff))
	searcher, err := New(root, Options{Ignore: IgnoreNone})
	if err != nil {
		t.Fatal(err)
	}
	var matches []Match
	stats, err := searcher.Search(context.Background(), SearchQuery{Pattern: "needle"}, func(match Match) error {
		matches = append(matches, match)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Match{{Path: "valid.txt", Line: 1, Text: "needle valid"}}
	if !reflect.DeepEqual(matches, want) {
		t.Fatalf("matches = %#v, want %#v", matches, want)
	}
	if stats.FilesVisited != 3 || stats.FilesSkipped != 2 || stats.Results != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestSearchSkipsOversizedLineAndContinues(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "long.txt", "hit\nxxxxx\nhit2\n")
	var matches []Match
	operation := searchOperation{
		regexp:       regexp.MustCompile("hit"),
		literal:      []byte("hit"),
		maxLineBytes: 4,
		visit: func(match Match) error {
			matches = append(matches, match)
			return nil
		},
		checkBuffer: make([]byte, textCheckBufferBytes+utf8.UTFMax),
		reader:      bufio.NewReaderSize(strings.NewReader(""), textCheckBufferBytes),
	}
	skipped, err := operation.scanFile(context.Background(), filepath.Join(root, "long.txt"), "long.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := []Match{
		{Path: "long.txt", Line: 1, Text: "hit"},
		{Path: "long.txt", Line: 3, Text: "hit2"},
	}
	if skipped || !reflect.DeepEqual(matches, want) {
		t.Fatalf("skipped=%t matches=%#v, want %#v", skipped, matches, want)
	}
	if operation.stats.OversizedLinesSkipped != 1 || operation.stats.Results != 2 {
		t.Fatalf("stats = %+v", operation.stats)
	}
}

func TestSearchUTF8RuneAcrossValidationBuffer(t *testing.T) {
	root := t.TempDir()
	line := strings.Repeat("x", textCheckBufferBytes-1) + "é needle"
	writeTestFile(t, root, "boundary.txt", line+"\n")
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	matches := collectMatches(t, searcher, SearchQuery{Pattern: "needle", PreviewBytes: 4})
	want := []Match{{Path: "boundary.txt", Line: 1, Text: "xxxx", LineTruncated: true}}
	if !reflect.DeepEqual(matches, want) {
		t.Fatalf("matches = %#v, want %#v", matches, want)
	}
}

func TestSearchExplicitIgnoredFileAndInclude(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".git/HEAD", "ref: refs/heads/main\n")
	writeTestFile(t, root, ".gitignore", "ignored.txt\n")
	writeTestFile(t, root, "ignored.txt", "needle explicit\n")
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	matches := collectMatches(t, searcher, SearchQuery{Path: "ignored.txt", Pattern: "needle"})
	want := []Match{{Path: "ignored.txt", Line: 1, Text: "needle explicit"}}
	if !reflect.DeepEqual(matches, want) {
		t.Fatalf("matches = %#v, want %#v", matches, want)
	}
	matches = collectMatches(t, searcher, SearchQuery{Path: "ignored.txt", Pattern: "needle", Include: "*.go"})
	if len(matches) != 0 {
		t.Fatalf("non-matching include returned %#v", matches)
	}
}

func TestSearchStopCallbackErrorAndCancellation(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "many.txt", "needle one\nneedle two\nneedle three\n")
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	stats, err := searcher.Search(context.Background(), SearchQuery{Pattern: "needle"}, func(Match) error {
		calls++
		if calls == 2 {
			return errors.Join(errors.New("enough"), ErrStop)
		}
		return nil
	})
	if err != nil || calls != 2 || stats.Results != 1 || !stats.Stopped {
		t.Fatalf("error=%v calls=%d stats=%+v", err, calls, stats)
	}

	wantErr := errors.New("sink failed")
	stats, err = searcher.Search(
		context.Background(), SearchQuery{Pattern: "needle"},
		func(Match) error { return wantErr },
	)
	if !errors.Is(err, wantErr) || stats.Results != 0 || stats.Stopped {
		t.Fatalf("error=%v stats=%+v", err, stats)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stats, err = searcher.Search(ctx, SearchQuery{Pattern: "needle"}, func(Match) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || stats.Results != 1 {
		t.Fatalf("cancellation error=%v stats=%+v", err, stats)
	}
}

func TestSearchExplicitFileSymlinkPolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, root, "real.txt", "needle real\n")
	writeTestFile(t, root, "real-dir/inside.txt", "needle inside\n")
	writeTestFile(t, outside, "outside.txt", "needle outside\n")
	if err := os.Symlink(filepath.Join(root, "real.txt"), filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "outside.txt"), filepath.Join(root, "outside.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real-dir"), filepath.Join(root, "dir-alias")); err != nil {
		t.Fatal(err)
	}
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	matches := collectMatches(t, searcher, SearchQuery{Path: "alias.txt", Pattern: "needle"})
	want := []Match{{Path: "alias.txt", Line: 1, Text: "needle real"}}
	if !reflect.DeepEqual(matches, want) {
		t.Fatalf("matches = %#v, want %#v", matches, want)
	}
	matches = collectMatches(t, searcher, SearchQuery{Path: "dir-alias", Pattern: "needle"})
	want = []Match{{Path: "dir-alias/inside.txt", Line: 1, Text: "needle inside"}}
	if !reflect.DeepEqual(matches, want) {
		t.Fatalf("directory symlink matches = %#v, want %#v", matches, want)
	}
	matches = collectMatches(t, searcher, SearchQuery{Pattern: "needle"})
	want = []Match{
		{Path: "real-dir/inside.txt", Line: 1, Text: "needle inside"},
		{Path: "real.txt", Line: 1, Text: "needle real"},
	}
	if !reflect.DeepEqual(matches, want) {
		t.Fatalf("recursive matches = %#v, want %#v", matches, want)
	}
	if _, err := searcher.Search(
		context.Background(),
		SearchQuery{Path: "outside.txt", Pattern: "needle"},
		func(Match) error { return nil },
	); err == nil {
		t.Fatal("outside-root explicit symlink unexpectedly succeeded")
	}
}

func TestSearchExplicitSymlinkCannotAliasGitMetadata(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available on Windows")
	}
	root := t.TempDir()
	writeTestFile(t, root, ".git/secret.txt", "needle secret\n")
	if err := os.Symlink(filepath.Join(root, ".git"), filepath.Join(root, "metadata")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		filepath.Join(root, ".git", "secret.txt"),
		filepath.Join(root, "metadata.txt"),
	); err != nil {
		t.Fatal(err)
	}
	searcher, err := New(root, Options{Ignore: IgnoreNone})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"metadata", "metadata.txt"} {
		if got := collectMatches(t, searcher, SearchQuery{Path: path, Pattern: "needle"}); len(got) != 0 {
			t.Errorf("metadata alias %q returned %#v", path, got)
		}
	}
}

func TestSearchValidatesQuery(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "file.txt", "content\n")
	searcher, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tests := []SearchQuery{
		{},
		{Pattern: "("},
		{Pattern: strings.Repeat("x", maxPatternBytes+1)},
		{Pattern: "content", PreviewBytes: -1},
		{Pattern: "content", Path: "missing"},
		{Pattern: "content", Include: "{,txt}"},
	}
	for _, query := range tests {
		if _, err := searcher.Search(context.Background(), query, func(Match) error { return nil }); err == nil {
			t.Errorf("Search(%+v) unexpectedly succeeded", query)
		}
	}
	if _, err := searcher.Search(context.Background(), SearchQuery{Pattern: "content"}, nil); err == nil {
		t.Error("nil callback unexpectedly succeeded")
	}
}

func TestReadBoundedLine(t *testing.T) {
	reader := bufio.NewReaderSize(strings.NewReader("four\r\nfive5\nsix\n"), 2)
	line, err := readBoundedLine(context.Background(), reader, 4)
	if err != nil || string(trimLineEnding(line)) != "four" {
		t.Fatalf("first line=%q error=%v", line, err)
	}
	if _, err := readBoundedLine(context.Background(), reader, 4); !errors.Is(err, errLineTooLong) {
		t.Fatalf("long-line error=%v", err)
	}
	line, err = readBoundedLine(context.Background(), reader, 4)
	if err != nil || string(trimLineEnding(line)) != "six" {
		t.Fatalf("line after oversized line=%q error=%v", line, err)
	}
	reader = bufio.NewReaderSize(strings.NewReader("five5"), 2)
	if _, err := readBoundedLine(context.Background(), reader, 4); !errors.Is(err, errLineTooLong) ||
		!errors.Is(err, io.EOF) {
		t.Fatalf("oversized final line error=%v", err)
	}
}

func collectMatches(t *testing.T, searcher *Searcher, query SearchQuery) []Match {
	t.Helper()
	var matches []Match
	if _, err := searcher.Search(context.Background(), query, func(match Match) error {
		matches = append(matches, match)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return matches
}

func writeTestBytes(t *testing.T, root, path string, content []byte) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func FuzzPreviewLine(f *testing.F) {
	for _, text := range []string{"", "ascii", "ééé", "日本語", "needle"} {
		f.Add(text, uint8(3))
	}
	f.Fuzz(func(t *testing.T, text string, rawLimit uint8) {
		if !utf8.ValidString(text) {
			return
		}
		limit := int(rawLimit)
		preview, truncated := previewLine(text, limit)
		if !utf8.ValidString(preview) {
			t.Fatalf("invalid UTF-8 preview %q", preview)
		}
		if limit > 0 && len(preview) > limit {
			t.Fatalf("preview length %d exceeds limit %d", len(preview), limit)
		}
		if truncated != (limit > 0 && len(text) > limit) {
			t.Fatalf("truncated=%t for text length %d and limit %d", truncated, len(text), limit)
		}
	})
}
