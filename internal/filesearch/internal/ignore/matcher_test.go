package ignore

import "testing"

const (
	directoryOnlyPattern = "build/\n"
	doubleStarPattern    = "a/**/b\n"
)

func TestGitignorePatternSemantics(t *testing.T) {
	tests := []struct {
		name     string
		patterns string
		path     string
		isDir    bool
		matched  bool
		ignored  bool
	}{
		{name: "basename", patterns: "*.log\n", path: "deep/app.log", matched: true, ignored: true},
		{name: "anchored root", patterns: "/root.txt\n", path: "root.txt", matched: true, ignored: true},
		{name: "anchored rejects nested", patterns: "/root.txt\n", path: "deep/root.txt"},
		{
			name: "middle slash anchors", patterns: "docs/generated\n", path: "docs/generated/file.go",
			matched: true, ignored: true,
		},
		{name: "middle slash rejects nested", patterns: "docs/generated\n", path: "other/docs/generated"},
		{
			name: "directory only", patterns: directoryOnlyPattern, path: "deep/build", isDir: true,
			matched: true, ignored: true,
		},
		{name: "directory only rejects file", patterns: directoryOnlyPattern, path: "deep/build"},
		{
			name: "directory descendants", patterns: directoryOnlyPattern, path: "deep/build/out.js",
			matched: true, ignored: true,
		},
		{
			name: "double star zero directories", patterns: doubleStarPattern, path: "a/b",
			matched: true, ignored: true,
		},
		{
			name: "double star many directories", patterns: doubleStarPattern, path: "a/x/y/b",
			matched: true, ignored: true,
		},
		{name: "star does not cross slash", patterns: "a/*/b\n", path: "a/x/y/b"},
		{name: "question mark byte", patterns: "?.go\n", path: "a.go", matched: true, ignored: true},
		{name: "question mark is not rune", patterns: "?.go\n", path: "é.go"},
		{
			name: "two question marks match utf8 bytes", patterns: "??.go\n", path: "é.go",
			matched: true, ignored: true,
		},
		{
			name: "escaped wildcard", patterns: `hello\*` + "\n", path: "hello*",
			matched: true, ignored: true,
		},
		{name: "escaped wildcard is literal", patterns: `hello\*` + "\n", path: "hello-world"},
		{
			name: "bracket range", patterns: "file[0-9].txt\n", path: "file5.txt",
			matched: true, ignored: true,
		},
		{
			name: "bracket negation", patterns: "file[!0-9].txt\n", path: "filex.txt",
			matched: true, ignored: true,
		},
		{name: "closing bracket first", patterns: "[]ab]\n", path: "]", matched: true, ignored: true},
		{
			name: "posix class", patterns: "[[:digit:]]*.txt\n", path: "7notes.txt",
			matched: true, ignored: true,
		},
		{
			name: "escaped leading hash", patterns: `\#notes` + "\n", path: "#notes",
			matched: true, ignored: true,
		},
		{name: "comment", patterns: "#notes\n", path: "#notes"},
		{
			name: "trailing spaces stripped", patterns: "notes   \n", path: "notes",
			matched: true, ignored: true,
		},
		{
			name: "escaped trailing space", patterns: `notes\ ` + "\n", path: "notes ",
			matched: true, ignored: true,
		},
		{name: "crlf", patterns: "windows.tmp\r\n", path: "windows.tmp", matched: true, ignored: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rules, diagnostics := Compile([]byte(test.patterns), "", "test.ignore")
			if len(diagnostics) != 0 {
				t.Fatalf("Compile() diagnostics = %v", diagnostics)
			}
			got := rules.Decide(test.path, test.isDir)
			if got.Matched != test.matched || got.Ignored != test.ignored {
				t.Fatalf(
					"Decide(%q, %t) = %+v, want matched=%t ignored=%t",
					test.path, test.isDir, got, test.matched, test.ignored,
				)
			}
		})
	}
}

func TestSlashInsideBracketExpressionAnchorsPattern(t *testing.T) {
	rules, diagnostics := Compile([]byte("[a/]\n"), "", ".gitignore")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	if got := rules.Decide("a", true); !got.Matched || !got.Ignored {
		t.Fatalf("root decision = %+v, want ignored", got)
	}
	if got := rules.Decide("nested/a", true); got.Matched || got.Ignored {
		t.Fatalf("nested decision = %+v, want unmatched", got)
	}
}

func TestParsedPatternsDoNotReinterpretGeneratedLeadingMarkers(t *testing.T) {
	rules, diagnostics := CompilePatterns([]Pattern{
		{Text: "#hash", Source: ".gitignore", Line: 1},
		{Text: "!bang", Source: ".gitignore", Line: 1},
		{Text: "plain", Source: ".gitignore", Line: 1},
	}, "")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	for _, path := range []string{"#hash", "!bang", "plain"} {
		if got := rules.Decide(path, false); !got.Matched || !got.Ignored {
			t.Errorf("Decide(%q) = %+v, want ignored", path, got)
		}
	}
}

func TestParsePatternLineOwnsLineLevelSyntax(t *testing.T) {
	tests := []struct {
		line    string
		ok      bool
		text    string
		negated bool
	}{
		{line: "# comment"},
		{line: "   "},
		{line: "!keep", ok: true, text: "keep", negated: true},
		{line: `\!literal`, ok: true, text: "!literal"},
		{line: `\#literal`, ok: true, text: "#literal"},
		{line: "{#hash,!bang}", ok: true, text: "{#hash,!bang}"},
	}
	for _, test := range tests {
		got, ok := ParsePatternLine(test.line, "rules", 7)
		if ok != test.ok || got.Text != test.text || (ok && got.Display != test.line) ||
			got.Negated != test.negated {
			t.Errorf(
				"ParsePatternLine(%q) = (%+v, %t), want text=%q negated=%t ok=%t",
				test.line, got, ok, test.text, test.negated, test.ok,
			)
		}
	}
}

func TestDecisionPreservesEscapedLeadingMarkerSpelling(t *testing.T) {
	rules, diagnostics := Compile(
		[]byte("\\!literal\n\\#hash\n"), "", "root/.gitignore",
	)
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	for _, test := range []struct {
		path    string
		pattern string
		line    int
	}{
		{path: "!literal", pattern: `\!literal`, line: 1},
		{path: "#hash", pattern: `\#hash`, line: 2},
	} {
		got := rules.Decide(test.path, false)
		if !got.Matched || !got.Ignored || got.Pattern != test.pattern ||
			got.Source != "root/.gitignore" || got.Line != test.line {
			t.Errorf("Decide(%q) = %+v", test.path, got)
		}
	}
}

func TestMayMatchDescendant(t *testing.T) {
	tests := []struct {
		pattern   string
		directory string
		want      bool
	}{
		{pattern: "*.go", directory: ""},
		{pattern: "*.go", directory: "anywhere"},
		{pattern: "foo/bar.go", directory: "", want: true},
		{pattern: "foo/bar.go", directory: "foo", want: true},
		{pattern: "foo/bar.go", directory: "foo/bar.go"},
		{pattern: "foo/*/bar.go", directory: "foo/x", want: true},
		{pattern: "foo/*/bar.go", directory: "foo/x/y"},
		{pattern: "foo/**/bar.go", directory: "foo/x/y", want: true},
		{pattern: "**/bar.go", directory: "any/depth", want: true},
		{pattern: "foo/**", directory: "foo", want: true},
		{pattern: "foo/**", directory: "foo/x/y", want: true},
		{pattern: "other/file", directory: "foo"},
	}
	for _, test := range tests {
		rules, diagnostics := CompilePatterns([]Pattern{{Text: test.pattern}}, "")
		if len(diagnostics) != 0 {
			t.Fatalf("compile %q: %v", test.pattern, diagnostics)
		}
		if got := rules.MayMatchDescendant(test.directory); got != test.want {
			t.Errorf(
				"MayMatchDescendant(%q) for %q = %t, want %t",
				test.directory, test.pattern, got, test.want,
			)
		}
	}
}

func TestMayMatchDescendantFromRuleSetScope(t *testing.T) {
	rules, diagnostics := CompilePatterns([]Pattern{{Text: "foo/bar.go"}}, "nested")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	if !rules.MayMatchDescendant("nested") {
		t.Fatal("scope directory should have a matching descendant")
	}
	if rules.MayMatchDescendant("") {
		t.Fatal("directory outside the rule scope should not match")
	}
}

func TestLastMatchingRuleWinsWithProvenance(t *testing.T) {
	rules, diagnostics := Compile([]byte("*.log\n!important.log\n"), "", "root/.gitignore")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	got := rules.Decide("important.log", false)
	if !got.Matched || got.Ignored || got.Pattern != "!important.log" ||
		got.Source != "root/.gitignore" || got.Line != 2 {
		t.Fatalf("decision = %+v", got)
	}
	if ordinary := rules.Decide("ordinary.log", false); !ordinary.Matched ||
		!ordinary.Ignored || ordinary.Line != 1 {
		t.Fatalf("ordinary decision = %+v", ordinary)
	}
	if unmatched := rules.Decide("main.go", false); unmatched.Matched || unmatched.Ignored {
		t.Fatalf("unmatched decision = %+v", unmatched)
	}
}

func TestNegationAppliesAtItsOwnPathLevel(t *testing.T) {
	rules, diagnostics := Compile([]byte("*\n!foo\n"), "", ".gitignore")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	if got := rules.Decide("foo", true); !got.Matched || got.Ignored || got.Pattern != "!foo" {
		t.Fatalf("foo decision = %+v", got)
	}
	if got := rules.Decide("foo/bar", false); !got.Matched || !got.Ignored || got.Pattern != "*" {
		t.Fatalf("foo/bar decision = %+v", got)
	}
	if got := rules.DecideDirect("foo/bar", false); !got.Matched || !got.Ignored || got.Pattern != "*" {
		t.Fatalf("foo/bar direct decision = %+v", got)
	}
}

func TestNegationCannotReincludeBelowIgnoredParent(t *testing.T) {
	rules, diagnostics := Compile([]byte("foo/\n!foo/bar\n"), "", ".gitignore")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	if got := rules.DecideDirect("foo/bar", false); !got.Matched || got.Ignored {
		t.Fatalf("direct decision = %+v, want matching negation", got)
	}
	if got := rules.Decide("foo/bar", false); !got.Matched || !got.Ignored || got.Pattern != "foo/" {
		t.Fatalf("effective decision = %+v, want ignored by parent", got)
	}
}

func TestEffectiveDecisionKeepsExcludedAncestorProvenance(t *testing.T) {
	rules, diagnostics := Compile([]byte("foo/\nfoo/bar\n"), "", ".gitignore")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	got := rules.Decide("foo/bar", false)
	if !got.Matched || !got.Ignored || got.Pattern != "foo/" || got.Line != 1 {
		t.Fatalf("decision = %+v, want excluded ancestor provenance", got)
	}
}

func TestDirectoryNegationReincludesDescendantsWithoutOtherMatches(t *testing.T) {
	rules, diagnostics := Compile([]byte("foo/\n!foo/\n"), "", ".gitignore")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	if got := rules.Decide("foo/child.txt", false); !got.Matched || got.Ignored || got.Pattern != "!foo/" {
		t.Fatalf("decision = %+v", got)
	}
}

func TestScopedRuleSet(t *testing.T) {
	rules, diagnostics := Compile(
		[]byte("*.tmp\n/local\n"), "src/generated", "src/generated/.gitignore",
	)
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	tests := []struct {
		path string
		want bool
	}{
		{path: "src/generated/cache.tmp", want: true},
		{path: "src/generated/deep/cache.tmp", want: true},
		{path: "src/generated/local", want: true},
		{path: "src/cache.tmp"},
		{path: "other/src/generated/cache.tmp"},
	}
	for _, test := range tests {
		if got := rules.Decide(test.path, test.path == "src/generated/local").Ignored; got != test.want {
			t.Errorf("Decide(%q).Ignored = %t, want %t", test.path, got, test.want)
		}
	}
}

func TestScopedRulesDoNotMatchTheirContainingDirectory(t *testing.T) {
	rules, diagnostics := Compile([]byte("**\n"), "nested", "nested/.gitignore")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	if got := rules.DecideDirect("nested", true); got.Matched {
		t.Fatalf("scope directory decision = %+v", got)
	}
	if got := rules.DecideDirect("nested/file", false); !got.Matched || !got.Ignored {
		t.Fatalf("scoped child decision = %+v", got)
	}
}

func TestScopedDirectPartsMatchWithoutRepeatingScopeWork(t *testing.T) {
	rules, diagnostics := Compile([]byte("*.tmp\n"), "nested/deep", "nested/deep/.gitignore")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	fullPath := []string{"nested", "deep", "more", "file.tmp"}
	if got := rules.DecideDirectParts(fullPath, false); !got.Matched || !got.Ignored {
		t.Fatalf("full-path decision = %+v", got)
	}
	if got := rules.DecideDirectRelativeParts(fullPath[2:], false); !got.Matched || !got.Ignored {
		t.Fatalf("relative-path decision = %+v", got)
	}
	if got := rules.DecideDirectRelativeParts([]string{"more", "file.go"}, false); got.Matched {
		t.Fatalf("non-matching relative decision = %+v", got)
	}
}

func TestCompileReportsAndSkipsInvalidPatterns(t *testing.T) {
	rules, diagnostics := Compile(
		[]byte("valid.log\nunterminated[\n[[:bogus:]]\ntrailing\\\nalso-valid\n"), "", "rules",
	)
	if len(diagnostics) != 3 {
		t.Fatalf("diagnostics = %v, want 3", diagnostics)
	}
	for index, wantLine := range []int{2, 3, 4} {
		if diagnostics[index].Line != wantLine || diagnostics[index].Source != "rules" {
			t.Errorf("diagnostic[%d] = %+v, want line %d", index, diagnostics[index], wantLine)
		}
	}
	if !rules.Decide("valid.log", false).Ignored || !rules.Decide("also-valid", false).Ignored {
		t.Fatal("valid rules surrounding diagnostics were not retained")
	}
}

func TestPureDoubleStarDirectoryPattern(t *testing.T) {
	rules, diagnostics := Compile([]byte("**/\n"), "", "")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	if !rules.Decide("deep/dir", true).Ignored {
		t.Fatal("**/ did not match a directory")
	}
	if rules.DecideDirect("deep/dir/file", false).Matched {
		t.Fatal("**/ directly matched a file")
	}
	if !rules.Decide("deep/dir/file", false).Ignored {
		t.Fatal("**/ directory decision did not propagate to its descendant")
	}
}

func TestTrailingDoubleStarDirectoryMatchesOnlyDescendantDirectories(t *testing.T) {
	rules, diagnostics := Compile([]byte("foo/**/\n"), "", ".gitignore")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	if got := rules.DecideDirect("foo", true); got.Matched {
		t.Fatalf("base directory decision = %+v", got)
	}
	if got := rules.Decide("foo/root.txt", false); got.Matched {
		t.Fatalf("base child decision = %+v", got)
	}
	if got := rules.DecideDirect("foo/sub", true); !got.Matched || !got.Ignored {
		t.Fatalf("descendant directory decision = %+v", got)
	}
	if got := rules.Decide("foo/sub/file.txt", false); !got.Matched || !got.Ignored {
		t.Fatalf("descendant file decision = %+v", got)
	}
}

func TestReversedBracketRangeKeepsFirstEndpoint(t *testing.T) {
	tests := []struct {
		patterns string
		path     string
		ignored  bool
	}{
		{patterns: "[z-a]\n", path: "z", ignored: true},
		{patterns: "[z-a]\n", path: "a"},
		{patterns: "[!z-a]\n", path: "z"},
		{patterns: "[!z-a]\n", path: "m", ignored: true},
	}
	for _, test := range tests {
		rules, diagnostics := Compile([]byte(test.patterns), "", ".gitignore")
		if len(diagnostics) != 0 {
			t.Fatal(diagnostics)
		}
		if got := rules.Decide(test.path, false).Ignored; got != test.ignored {
			t.Errorf("Decide(%q) ignored = %t, want %t for %q", test.path, got, test.ignored, test.patterns)
		}
	}
}

func TestStandaloneManyStarSegmentActsAsGlobstar(t *testing.T) {
	rules, diagnostics := Compile([]byte("a/***/b\n"), "", ".gitignore")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	for _, path := range []string{"a/b", "a/x/b", "a/x/y/b"} {
		if got := rules.DecideDirect(path, false); !got.Matched || !got.Ignored {
			t.Errorf("DecideDirect(%q) = %+v, want ignored", path, got)
		}
	}
	if got := rules.DecideDirect("a/x/y/c", false); got.Matched {
		t.Fatalf("non-matching decision = %+v", got)
	}
}

func TestTrailingManyStarSegmentMatchesOnlyContents(t *testing.T) {
	rules, diagnostics := Compile([]byte("data/****\n"), "", ".gitignore")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	if got := rules.DecideDirect("data", true); got.Matched {
		t.Fatalf("base directory decision = %+v", got)
	}
	for _, path := range []string{"data/file", "data/sub/file"} {
		if got := rules.DecideDirect(path, false); !got.Matched || !got.Ignored {
			t.Errorf("DecideDirect(%q) = %+v, want ignored", path, got)
		}
	}
}

func TestLeadingGlobstarBeforeTrailingDoubleStarMatchesOnlyContents(t *testing.T) {
	rules, diagnostics := Compile([]byte("**/never/**\n"), "", ".gitignore")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	for _, path := range []string{"never", "a/never"} {
		if got := rules.DecideDirect(path, true); got.Matched {
			t.Errorf("base directory %q decision = %+v", path, got)
		}
	}
	for _, path := range []string{"never/file", "a/never/file", "a/never/sub/file"} {
		if got := rules.DecideDirect(path, false); !got.Matched || !got.Ignored {
			t.Errorf("DecideDirect(%q) = %+v, want ignored", path, got)
		}
	}
	for _, path := range []string{"other/file", "a/other/never.txt"} {
		if got := rules.DecideDirect(path, false); got.Matched {
			t.Errorf("non-matching path %q decision = %+v", path, got)
		}
	}
}

func TestCompileStripsUTF8BOM(t *testing.T) {
	rules, diagnostics := Compile([]byte("\xef\xbb\xbffoo\n"), "", ".gitignore")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	if got := rules.Decide("foo", false); !got.Matched || !got.Ignored || got.Pattern != "foo" {
		t.Fatalf("decision = %+v", got)
	}
}

func TestTrailingSpaceEscapeUsesBackslashParity(t *testing.T) {
	rules, diagnostics := Compile([]byte("foo\\\\ \n"), "", ".gitignore")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	if got := rules.Decide(`foo\`, false); !got.Matched || !got.Ignored {
		t.Fatalf("decision = %+v", got)
	}
	if got := rules.Decide(`foo\ `, false); got.Matched {
		t.Fatalf("escaped-space decision = %+v", got)
	}
}
