package filesearch

import (
	"regexp"
	"testing"
	"unicode/utf8"
)

func TestRegexpPrefilterPreservesMatches(t *testing.T) {
	tests := []struct {
		pattern string
		lines   []string
	}{
		{pattern: `needle|absent`, lines: []string{"ordinary", "has needle", "absent here"}},
		{pattern: `(?:alpha|beta).*suffix`, lines: []string{"alpha suffix", "beta and suffix", "alpha only", "suffix only"}},
		{pattern: `prefix(?:one|two)`, lines: []string{"prefixone", "prefixtwo", "prefixthree"}},
		{pattern: `(?:foo)?bar`, lines: []string{"bar", "foobar", "foo"}},
		{pattern: `(?:foo)+bar`, lines: []string{"foobar", "foofoobar", "bar"}},
		{pattern: `(?i)needle`, lines: []string{"NEEDLE", "needle", "ordinary"}},
		{pattern: `^start.*end$`, lines: []string{"start and end", "before start and end", "start and end after"}},
		{pattern: `日本|école`, lines: []string{"日本語", "une école", "ordinary"}},
		{pattern: `foo\.(go|txt)`, lines: []string{"foo.go", "foo.txt", "fooXgo"}},
	}
	for _, test := range tests {
		t.Run(test.pattern, func(t *testing.T) {
			compiled := regexp.MustCompile(test.pattern)
			literals := regexpRequiredLiterals(test.pattern)
			for _, line := range test.lines {
				want := compiled.MatchString(line)
				got := (len(literals) == 0 || containsRequiredLiteral([]byte(line), literals)) && compiled.MatchString(line)
				if got != want {
					t.Errorf("line %q: prefiltered match=%t, regexp match=%t, literals=%q", line, got, want, literals)
				}
			}
		})
	}
}

func TestRegexpPrefilterBounded(t *testing.T) {
	pattern := `a|b|c|d|e|f|g|h|i|j|k|l|m|n|o|p|q`
	if literals := regexpRequiredLiterals(pattern); len(literals) != 0 {
		t.Fatalf("oversized prefilter = %q", literals)
	}
}

func TestFoldLiteralMatcherPreservesRegexpSemantics(t *testing.T) {
	tests := []struct {
		pattern string
		lines   []string
	}{
		{pattern: `(?i)needle`, lines: []string{"ordinary", "NEEDLE", "Needle here", "needles"}},
		{pattern: `(?i)kelvin`, lines: []string{"KELVIN", "kelvin", "ordinary"}},
		{pattern: `(?i)s`, lines: []string{"ſ", "S", "s", "x"}},
		{pattern: `(?i)école`, lines: []string{"ÉCOLE", "école", "ecole"}},
	}
	for _, test := range tests {
		t.Run(test.pattern, func(t *testing.T) {
			matcher := regexpFoldLiteral(test.pattern)
			if matcher == nil {
				t.Fatal("fold literal matcher was not built")
			}
			compiled := regexp.MustCompile(test.pattern)
			for _, line := range test.lines {
				if got, want := matcher.matches([]byte(line)), compiled.MatchString(line); got != want {
					t.Errorf("line %q: fold literal match=%t, regexp match=%t", line, got, want)
				}
			}
		})
	}
	for _, pattern := range []string{`(?i)^needle`, `(?i)needle|other`, `needle`} {
		if matcher := regexpFoldLiteral(pattern); matcher != nil {
			t.Errorf("regexpFoldLiteral(%q) unexpectedly returned a matcher", pattern)
		}
	}
}

func FuzzRegexpOptimizersPreserveMatches(f *testing.F) {
	for _, seed := range []struct {
		pattern string
		line    string
	}{
		{pattern: `needle|absent`, line: "ordinary text"},
		{pattern: `(?:alpha|beta).*suffix`, line: "beta with suffix"},
		{pattern: `(?i)needle`, line: "NEEDLE"},
		{pattern: `(?i)kelvin`, line: "KELVIN"},
		{pattern: `foo(?:bar)?baz`, line: "foobaz"},
		{pattern: `日本|école`, line: "une école"},
	} {
		f.Add(seed.pattern, seed.line)
	}
	f.Fuzz(func(t *testing.T, pattern, line string) {
		if !utf8.ValidString(line) {
			return
		}
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return
		}
		want := compiled.MatchString(line)
		if matcher := regexpFoldLiteral(pattern); matcher != nil {
			if got := matcher.matches([]byte(line)); got != want {
				t.Fatalf("fold matcher for %q on %q = %t, want %t", pattern, line, got, want)
			}
		}
		if literals := regexpRequiredLiterals(pattern); want && len(literals) > 0 && !containsRequiredLiteral([]byte(line), literals) {
			t.Fatalf("prefilter for %q rejected matching line %q; literals=%q", pattern, line, literals)
		}
	})
}
