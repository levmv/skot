package filesearch

import (
	"strings"

	ignore "github.com/levmv/skot/internal/filesearch/internal/ignore"
)

// ruleMatcher is the only seam between filesearch and the internal matcher
// implementation. No matcher type or behavior is exposed to callers or to
// traversal code.
type ruleMatcher struct {
	sets []scopedRuleSet
}

// scopedRuleSet records the scope as a segment count because sets are kept on
// the active ancestor stack. The walker has already established that every
// active scope is a prefix, so matching can slice pathParts without comparing
// that prefix again for every rule set.
type scopedRuleSet struct {
	rules      *ignore.RuleSet
	scopeDepth int
}

type matcherPatternError struct {
	pattern string
	message string
	line    int
}

type matcherPattern struct {
	text    string
	display string
	negated bool
	line    int
}

func newRuleMatcher() *ruleMatcher {
	return &ruleMatcher{}
}

func (m *ruleMatcher) addPatternList(patterns []matcherPattern, dir, source string) []matcherPatternError {
	inputs := make([]ignore.Pattern, 0, len(patterns))
	for _, pattern := range patterns {
		inputs = append(inputs, ignore.Pattern{
			Text:    pattern.text,
			Display: pattern.display,
			Negated: pattern.negated,
			Source:  source,
			Line:    pattern.line,
		})
	}
	set, diagnostics := ignore.CompilePatterns(inputs, dir)
	return m.addSet(set, diagnostics, pathDepth(dir))
}

func parseMatcherPatternLine(text string, line int) (matcherPattern, bool) {
	parsed, ok := ignore.ParsePatternLine(text, "", line)
	if !ok {
		return matcherPattern{}, false
	}
	return matcherPattern{
		text:    parsed.Text,
		display: parsed.Display,
		negated: parsed.Negated,
		line:    line,
	}, true
}

func (m *ruleMatcher) addSet(
	set *ignore.RuleSet,
	diagnostics []ignore.PatternError,
	scopeDepth int,
) []matcherPatternError {
	m.sets = append(m.sets, scopedRuleSet{rules: set, scopeDepth: scopeDepth})
	if len(diagnostics) == 0 {
		return nil
	}
	errs := make([]matcherPatternError, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		errs = append(errs, matcherPatternError{
			pattern: diagnostic.Pattern,
			message: diagnostic.Message,
			line:    diagnostic.Line,
		})
	}
	return errs
}

func (m *ruleMatcher) matchesDirect(path string, isDir bool) bool {
	if path == "" {
		return false
	}
	matched, _ := m.directDecisionParts(strings.Split(path, "/"), isDir)
	return matched
}

func (m *ruleMatcher) mayMatchDescendant(path string) bool {
	for _, candidate := range m.sets {
		if candidate.rules.MayMatchDescendant(path) {
			return true
		}
	}
	return false
}

func (m *ruleMatcher) directDecisionParts(pathParts []string, isDir bool) (bool, bool) {
	for index := len(m.sets) - 1; index >= 0; index-- {
		candidate := m.sets[index]
		if len(pathParts) <= candidate.scopeDepth {
			continue
		}
		result := candidate.rules.DecideDirectRelativeParts(pathParts[candidate.scopeDepth:], isDir)
		if result.Matched {
			return true, result.Ignored
		}
	}
	return false, false
}

func pathDepth(path string) int {
	if path == "" {
		return 0
	}
	return strings.Count(path, "/") + 1
}

func (m *ruleMatcher) restore(length int) {
	// Clear discarded pointers so a long-lived walker cannot retain completed
	// sibling rule sets through the backing array.
	clear(m.sets[length:])
	m.sets = m.sets[:length]
}
