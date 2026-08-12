package ignore

import (
	"strings"
	"testing"
)

func FuzzCompileAndDecide(f *testing.F) {
	for _, seed := range []struct {
		patterns string
		scope    string
		path     string
	}{
		{patterns: "*.go\n!generated.go\n", path: "src/main.go"},
		{patterns: doubleStarPattern, scope: "src", path: "src/a/x/b"},
		{patterns: "[[:digit:]]?.txt\n", path: "7x.txt"},
		{patterns: "unterminated[\n", path: "unterminated["},
		{patterns: "\x00\n", path: "file"},
	} {
		f.Add(seed.patterns, seed.scope, seed.path, false)
	}
	f.Fuzz(func(_ *testing.T, patterns, scope, path string, isDir bool) {
		rules, _ := Compile([]byte(patterns), scope, "fuzz")
		_ = rules.Decide(path, isDir)
	})
}

func FuzzMatchSegmentsPrefix(f *testing.F) {
	for _, seed := range []struct {
		pattern string
		path    string
	}{
		{pattern: "**/never", path: "a/b/never/file"},
		{pattern: "a/**/b", path: "a/x/y/b/tail"},
		{pattern: "**", path: "anything"},
		{pattern: "literal", path: "other/path"},
	} {
		f.Add(seed.pattern, seed.path)
	}
	f.Fuzz(func(t *testing.T, pattern, path string) {
		if pattern == "" {
			t.Skip()
		}
		compiled, message := compilePattern(pattern, false)
		if message != "" || compiled.basenameOnly || compiled.contentsOnly {
			t.Skip()
		}
		parts := strings.Split(path, "/")
		want := false
		for end := 0; end <= len(parts); end++ {
			if matchSegments(compiled.segments, parts[:end]) {
				want = true
				break
			}
		}
		if got := matchSegmentsPrefix(compiled.segments, parts); got != want {
			t.Fatalf("matchSegmentsPrefix(%q, %q) = %t, want %t", pattern, path, got, want)
		}
	})
}

func FuzzMayMatchSegmentDescendant(f *testing.F) {
	for _, seed := range []struct {
		pattern string
		path    string
	}{
		{pattern: "foo/bar", path: "foo"},
		{pattern: "foo/*/bar", path: "foo/x"},
		{pattern: "foo/**/bar", path: "foo/x/y"},
		{pattern: "**/bar", path: "anything/deep"},
		{pattern: "foo/**", path: "foo/x/y"},
	} {
		f.Add(seed.pattern, seed.path)
	}
	f.Fuzz(func(t *testing.T, pattern, path string) {
		if pattern == "" || path == "" {
			t.Skip()
		}
		compiled, message := compilePattern(pattern, false)
		if message != "" || compiled.basenameOnly {
			t.Skip()
		}
		parts := strings.Split(path, "/")
		got := mayMatchSegmentDescendant(compiled.traversalSegments, parts)
		want := mayMatchSegmentDescendantReference(compiled.traversalSegments, parts)
		if got != want {
			t.Fatalf("mayMatchSegmentDescendant(%q, %q) = %t, want %t", pattern, path, got, want)
		}
	})
}

func mayMatchSegmentDescendantReference(pattern []segment, path []string) bool {
	states := make([]bool, len(pattern)+1)
	states[0] = true
	closeGlobstars := func() {
		for index, candidate := range pattern {
			if states[index] && candidate.doubleStar {
				states[index+1] = true
			}
		}
	}
	closeGlobstars()
	for _, part := range path {
		next := make([]bool, len(states))
		for index, candidate := range pattern {
			if !states[index] {
				continue
			}
			if candidate.doubleStar {
				next[index] = true
			} else if matchSegment(candidate, part) {
				next[index+1] = true
			}
		}
		states = next
		closeGlobstars()
	}
	for index := range len(pattern) {
		if states[index] {
			return true
		}
	}
	return false
}
