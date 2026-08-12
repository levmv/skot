package filesearch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIgnoreMatcherRestoreDropsScopedRuleSets(t *testing.T) {
	matcher := newIgnoreMatcher()
	addTestRule(t, matcher.gitRules, "root.txt", "")
	checkpoint := matcher.checkpoint()
	addTestRule(t, matcher.gitRules, "*.tmp", "nested")
	addTestRule(t, matcher.ignoreRules, "dot.txt", "nested")
	addTestRule(t, matcher.ripgrepRules, "rg.txt", "nested")
	if !matcher.ignored("nested/drop.tmp", false) {
		t.Fatal("scoped rule was not active before restore")
	}

	matcher.restore(checkpoint)
	if got := matcher.checkpoint(); got != checkpoint {
		t.Fatalf("checkpoint after restore = %+v, want %+v", got, checkpoint)
	}
	if matcher.ignored("nested/drop.tmp", false) {
		t.Fatal("scoped rule remained active after restore")
	}
	if !matcher.ignored("root.txt", false) {
		t.Fatal("root rule was removed by scoped restore")
	}
	assertClearedRuleSets(t, matcher.gitRules, checkpoint.gitRules)
	assertClearedRuleSets(t, matcher.ignoreRules, checkpoint.ignoreRules)
	assertClearedRuleSets(t, matcher.ripgrepRules, checkpoint.ripgrepRules)
}

func addTestRule(t *testing.T, matcher *ruleMatcher, pattern, scope string) {
	t.Helper()
	diagnostics := matcher.addPatternList([]matcherPattern{{
		text: pattern, display: pattern, line: 1,
	}}, scope, "test")
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
}

func assertClearedRuleSets(t *testing.T, matcher *ruleMatcher, start int) {
	t.Helper()
	storage := matcher.sets[:cap(matcher.sets)]
	for index := start; index < len(storage); index++ {
		if storage[index].rules != nil {
			t.Fatalf("discarded rule set %d is still retained", index)
		}
	}
}

func TestReadIgnorePatternsBoundsMemoryInputs(t *testing.T) {
	t.Run("maximum line is accepted", func(t *testing.T) {
		input := strings.Repeat("x", maxPatternBytes)
		patterns, err := readIgnorePatterns(strings.NewReader(input), ".gitignore")
		if err != nil || len(patterns) != 1 {
			t.Fatalf("patterns=%d error=%v", len(patterns), err)
		}
	})

	t.Run("long line with newline is accepted", func(t *testing.T) {
		input := strings.Repeat("x", maxPatternBytes) + "\n"
		patterns, err := readIgnorePatterns(strings.NewReader(input), ".gitignore")
		if err != nil || len(patterns) != 1 {
			t.Fatalf("patterns=%d error=%v", len(patterns), err)
		}
	})

	t.Run("long line crosses initial buffer", func(t *testing.T) {
		input := strings.Repeat("x", initialIgnoreBufferBytes+1) + "\nnext\n"
		patterns, err := readIgnorePatterns(strings.NewReader(input), ".gitignore")
		if err != nil || len(patterns) != 2 {
			t.Fatalf("patterns=%d error=%v", len(patterns), err)
		}
	})

	t.Run("oversized line", func(t *testing.T) {
		input := strings.Repeat("x", maxPatternBytes+1)
		_, err := readIgnorePatterns(strings.NewReader(input), ".gitignore")
		if err == nil || !strings.Contains(err.Error(), ".gitignore:1 exceeds 65535 bytes") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("oversized file", func(t *testing.T) {
		line := "#" + strings.Repeat("x", maxPatternBytes-2) + "\n"
		input := strings.Repeat(line, maxIgnoreFileBytes/len(line)+1)
		_, err := readIgnorePatterns(strings.NewReader(input), ".gitignore")
		if err == nil || !strings.Contains(err.Error(), "exceeds 8388608 bytes") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("too many expanded patterns", func(t *testing.T) {
		input := strings.Repeat("ordinary\n", maxIgnorePatterns+1)
		_, err := readIgnorePatterns(strings.NewReader(input), ".gitignore")
		if err == nil || !strings.Contains(err.Error(), "exceeds 65536 expanded patterns") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("expanded text is bounded", func(t *testing.T) {
		line := strings.Repeat("x", maxPatternBytes-len("{a,b}")) + "{a,b}\n"
		expandedLineBytes := 2 * (maxPatternBytes - len("{a,b}") + 1)
		input := strings.Repeat(line, maxExpandedPatternBytes/expandedLineBytes+1)
		_, err := readIgnorePatterns(strings.NewReader(input), ".gitignore")
		if err == nil || !strings.Contains(err.Error(), "exceeds 1048576 expanded pattern bytes") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestIgnoreMatcherRejectsOversizedSparseFileFromMetadata(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".gitignore")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, maxIgnoreFileBytes+1); err != nil {
		t.Fatal(err)
	}
	matcher := newIgnoreMatcher()
	err := matcher.addFile(matcher.gitRules, path, "")
	if err == nil || !strings.Contains(err.Error(), "exceeds 8388608 bytes") {
		t.Fatalf("error = %v", err)
	}
}
