// Package ignore compiles and matches scoped gitignore-style rule sets.
//
// The package is deliberately filesystem-free. Callers decide which ignore
// files to read, how sources are prioritized, and whether parse diagnostics
// are fatal. Matching is bytewise and case-sensitive. Paths and scopes use
// slash separators, are relative to the matching root, and have no leading or
// trailing slash; the empty scope denotes that root.
package ignore

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
)

// Decision describes the rule responsible for a direct or effective result.
// Matched=false distinguishes an inclusion rule from the absence of a rule,
// which is required when callers compose sources with different priorities.
type Decision struct {
	Matched bool
	Ignored bool
	Pattern string
	Source  string
	Line    int
}

// PatternError describes one rule that could not be compiled. Other valid
// rules in the same input are retained in the returned RuleSet.
type PatternError struct {
	Pattern string
	Source  string
	Line    int
	Message string
}

// Pattern is one rule with caller-supplied provenance. Line may be zero when
// the input did not originate in a line-oriented file.
type Pattern struct {
	// Text is the glob body after line-level comment, negation, escaping, and
	// trailing-space syntax has been parsed.
	Text string
	// Display is the original spelling used in decisions and diagnostics. When
	// empty, CompilePatterns derives it from Text and Negated. Keeping it
	// separate prevents an escaped leading marker from looking like syntax.
	Display string
	// Negated is the action parsed from a leading unescaped '!'.
	Negated bool
	// Source and Line are copied to decisions and diagnostics unchanged.
	Source string
	Line   int
}

func (e PatternError) Error() string {
	location := e.Source
	if e.Line > 0 {
		if location == "" {
			location = fmt.Sprintf("line %d", e.Line)
		} else {
			location = fmt.Sprintf("%s:%d", location, e.Line)
		}
	}
	if location == "" {
		return fmt.Sprintf("invalid ignore pattern %q: %s", e.Pattern, e.Message)
	}
	return fmt.Sprintf("%s: invalid ignore pattern %q: %s", location, e.Pattern, e.Message)
}

// RuleSet is an immutable collection of rules with one directory scope.
// It is safe for concurrent matching.
type RuleSet struct {
	rules []rule
	scope []string
}

type rule struct {
	segments          []segment
	traversalSegments []segment
	basenameOnly      bool
	negated           bool
	dirOnly           bool
	contentsOnly      bool
	pattern           string
	source            string
	line              int
}

const utf8BOM = "\xef\xbb\xbf"

// Compile parses newline-separated gitignore-style patterns. Scope is a
// slash-separated directory relative to the path-matching root; an empty
// scope means that root. Invalid patterns are reported and omitted.
func Compile(data []byte, scope, source string) (*RuleSet, []PatternError) {
	lines := bytes.Split(data, []byte{'\n'})
	patterns := make([]Pattern, 0, len(lines))
	for index, raw := range lines {
		text := strings.TrimSuffix(string(raw), "\r")
		pattern, ok := ParsePatternLine(text, source, index+1)
		if ok {
			patterns = append(patterns, pattern)
		}
	}
	return CompilePatterns(patterns, scope)
}

// ParsePatternLine parses syntax that belongs to one physical ignore-file
// line. Comments and blank lines return ok=false. Keeping this step separate
// from CompilePatterns lets callers safely transform the glob body without
// accidentally interpreting a generated leading '#' or '!' a second time.
func ParsePatternLine(text, source string, line int) (Pattern, bool) {
	if line == 1 {
		text = strings.TrimPrefix(text, utf8BOM)
	}
	text = trimTrailingSpaces(text)
	if text == "" || text[0] == '#' {
		return Pattern{}, false
	}
	pattern := Pattern{Display: text, Source: source, Line: line}
	if text[0] == '!' {
		pattern.Negated = true
		text = text[1:]
	}
	if len(text) >= 2 && text[0] == '\\' && (text[1] == '#' || text[1] == '!') {
		text = text[1:]
	}
	pattern.Text = text
	return pattern, true
}

// CompilePatterns compiles already parsed patterns. It is useful for dialects
// that expand one physical line into several glob bodies while retaining its
// negation state and source location.
func CompilePatterns(patterns []Pattern, scope string) (*RuleSet, []PatternError) {
	var scopeParts []string
	if scope != "" {
		scopeParts = strings.Split(scope, "/")
	}
	rules := make([]rule, 0, len(patterns))
	var diagnostics []PatternError
	for _, input := range patterns {
		compiled, message := compilePattern(input.Text, input.Negated)
		display := input.Display
		if display == "" {
			display = input.Text
			if input.Negated {
				display = "!" + display
			}
		}
		if message != "" {
			diagnostics = append(diagnostics, PatternError{
				Pattern: display,
				Source:  input.Source,
				Line:    input.Line,
				Message: message,
			})
			continue
		}
		compiled.pattern = display
		compiled.source = input.Source
		compiled.line = input.Line
		rules = append(rules, compiled)
	}
	return &RuleSet{rules: rules, scope: scopeParts}, diagnostics
}

// Decide returns the effective action for path and the rule that established
// it. Directory decisions are inherited by descendants, while a negation can
// only reinclude a path whose parent is visible.
func (s *RuleSet) Decide(path string, isDir bool) Decision {
	if s == nil || path == "" {
		return Decision{}
	}
	pathParts, ok := s.relativePathParts(strings.Split(path, "/"))
	if !ok {
		return Decision{}
	}
	var result Decision
	for end := 1; end <= len(pathParts); end++ {
		direct := s.decideDirect(pathParts[:end], end < len(pathParts) || isDir)
		if !direct.Matched {
			continue
		}
		if direct.Ignored {
			// Git does not inspect descendants of an excluded directory. No
			// deeper rule can change the effective action or its provenance.
			return direct
		}
		result = direct
	}
	return result
}

// DecideDirect returns the last rule that matches path itself, without
// inheriting a decision from any parent directory. This is useful to compose
// multiple rule sources or to carry directory state during a filesystem walk.
// Path must be a normalized, slash-separated relative path.
func (s *RuleSet) DecideDirect(path string, isDir bool) Decision {
	if s == nil || path == "" {
		return Decision{}
	}
	return s.DecideDirectParts(strings.Split(path, "/"), isDir)
}

// DecideDirectParts is DecideDirect for callers that already have a path
// split into slash-separated segments. The segments are still interpreted
// relative to the RuleSet's configured scope.
func (s *RuleSet) DecideDirectParts(pathParts []string, isDir bool) Decision {
	if s == nil {
		return Decision{}
	}
	relative, ok := s.relativePathParts(pathParts)
	if !ok {
		return Decision{}
	}
	return s.decideDirect(relative, isDir)
}

// DecideDirectRelativeParts matches a path that the caller has already made
// relative to this RuleSet's scope. It lets a filesystem walker reuse its
// active scope stack without rechecking every ancestor prefix.
func (s *RuleSet) DecideDirectRelativeParts(pathParts []string, isDir bool) Decision {
	if s == nil || len(pathParts) == 0 {
		return Decision{}
	}
	return s.decideDirect(pathParts, isDir)
}

// MayMatchDescendant reports whether any slash-bearing rule can match a path
// strictly below directory. Basename-only rules intentionally return false:
// callers would otherwise have to reopen every ignored directory for a glob
// such as "*.go". Directory may equal the RuleSet scope; an empty directory
// denotes the matching root.
func (s *RuleSet) MayMatchDescendant(directory string) bool {
	if s == nil {
		return false
	}
	var directoryParts []string
	if directory != "" {
		directoryParts = strings.Split(directory, "/")
	}
	pathParts, ok := s.relativeParts(directoryParts)
	if !ok {
		return false
	}
	for index := range s.rules {
		candidate := &s.rules[index]
		if candidate.basenameOnly {
			continue
		}
		if mayMatchSegmentDescendant(candidate.traversalSegments, pathParts) {
			return true
		}
	}
	return false
}

func (s *RuleSet) relativePathParts(pathParts []string) ([]string, bool) {
	relative, ok := s.relativeParts(pathParts)
	return relative, ok && len(relative) > 0
}

func (s *RuleSet) relativeParts(pathParts []string) ([]string, bool) {
	if len(pathParts) < len(s.scope) {
		return nil, false
	}
	for index, part := range s.scope {
		if pathParts[index] != part {
			return nil, false
		}
	}
	return pathParts[len(s.scope):], true
}

func (s *RuleSet) decideDirect(pathParts []string, isDir bool) Decision {
	for index := range slices.Backward(s.rules) {
		candidate := &s.rules[index]
		if !candidate.matchesDirect(pathParts, isDir) {
			continue
		}
		return Decision{
			Matched: true,
			Ignored: !candidate.negated,
			Pattern: candidate.pattern,
			Source:  candidate.source,
			Line:    candidate.line,
		}
	}
	return Decision{}
}

func (r *rule) matchesDirect(pathParts []string, isDir bool) bool {
	if len(pathParts) == 0 {
		return false
	}
	if r.basenameOnly {
		if r.dirOnly && !isDir {
			return false
		}
		candidate := r.segments[0]
		return candidate.doubleStar || matchSegment(candidate, pathParts[len(pathParts)-1])
	}
	if r.contentsOnly {
		// A trailing all-star globstar segment matches paths inside the preceding
		// directory, but not that directory itself. With a trailing slash, only
		// descendant directories match directly; their decision is inherited by files.
		if r.dirOnly && !isDir {
			return false
		}
		// Match any proper path prefix, including the root prefix for patterns
		// such as **/**. Limiting the input to len(pathParts)-1 keeps the base
		// directory itself outside the trailing globstar's match.
		return matchSegmentsPrefix(r.segments, pathParts[:len(pathParts)-1])
	}
	if r.dirOnly {
		return isDir && matchSegments(r.segments, pathParts)
	}
	return matchSegments(r.segments, pathParts)
}

func compilePattern(pattern string, negated bool) (rule, string) {
	var compiled rule
	compiled.negated = negated
	if pattern == "" || pattern == "/" {
		return rule{}, "empty pattern"
	}
	if strings.IndexByte(pattern, 0) >= 0 {
		return rule{}, "NUL is not allowed"
	}
	if len(pattern) > 1 && pattern[len(pattern)-1] == '/' {
		compiled.dirOnly = true
		pattern = pattern[:len(pattern)-1]
	}
	anchored := pattern[0] == '/'
	if anchored {
		pattern = pattern[1:]
		if pattern == "" {
			return rule{}, "empty pattern"
		}
	}
	rawSegments, slashBearing, message := splitPatternSegments(pattern)
	if message != "" {
		return rule{}, message
	}
	// Git treats any slash as making a pattern relative to its rule scope,
	// including a slash inside a bracket expression. The trailing directory
	// marker was removed above, so a basename directory rule such as "build/"
	// remains unanchored.
	anchored = anchored || slashBearing
	compiled.basenameOnly = !anchored
	contentsOnly := len(rawSegments) >= 2 && isGlobstarSegment(rawSegments[len(rawSegments)-1])
	segments := make([]segment, 0, len(rawSegments))
	for _, rawSegment := range rawSegments {
		if isGlobstarSegment(rawSegment) {
			segments = append(segments, segment{doubleStar: true})
			continue
		}
		compiledSegment, segmentMessage := compileSegment(rawSegment)
		if segmentMessage != "" {
			return rule{}, segmentMessage
		}
		segments = append(segments, compiledSegment)
	}
	if contentsOnly {
		// Direct matching drops the trailing globstar to keep the base directory
		// unmatched. Traversal feasibility still needs the original form because
		// descendants below that base can match.
		compiled.contentsOnly = true
		compiled.traversalSegments = collapseDoubleStars(append([]segment(nil), segments...))
		segments = segments[:len(segments)-1]
		compiled.segments = collapseDoubleStars(segments)
	} else {
		segments = collapseDoubleStars(segments)
		compiled.traversalSegments = segments
		compiled.segments = segments
	}
	return compiled, ""
}

// splitPatternSegments recognizes escaped separators before compiling the
// individual wildmatch segments. Git treats an escaped slash as a path
// separator too, while a slash inside a bracket expression remains part of
// that expression. Other escapes are preserved for compileSegment.
func splitPatternSegments(pattern string) ([]string, bool, string) {
	slashCount := strings.Count(pattern, "/")
	segments := make([]string, 0, slashCount+1)
	start := 0
	for index := 0; index < len(pattern); {
		switch pattern[index] {
		case '\\':
			if index+1 >= len(pattern) {
				index++
				continue
			}
			if pattern[index+1] == '/' {
				segments = append(segments, pattern[start:index])
				index += 2
				start = index
				continue
			}
			index += 2
		case '/':
			segments = append(segments, pattern[start:index])
			index++
			start = index
		case '[':
			_, next, message := compileClass(pattern, index)
			if message != "" {
				return nil, false, message
			}
			index = next
		default:
			index++
		}
	}
	segments = append(segments, pattern[start:])
	return segments, slashCount > 0, ""
}

func isGlobstarSegment(segment string) bool {
	if len(segment) < 2 {
		return false
	}
	for index := range len(segment) {
		if segment[index] != '*' {
			return false
		}
	}
	return true
}

func collapseDoubleStars(segments []segment) []segment {
	if len(segments) < 2 {
		return segments
	}
	// Adjacent globstars are equivalent to one and keeping them would create
	// redundant fallback points in the matcher.
	collapsed := segments[:1]
	for _, candidate := range segments[1:] {
		if candidate.doubleStar && collapsed[len(collapsed)-1].doubleStar {
			continue
		}
		collapsed = append(collapsed, candidate)
	}
	return collapsed
}

func trimTrailingSpaces(text string) string {
	end := len(text)
	for end > 0 && text[end-1] == ' ' {
		backslashes := 0
		for index := end - 2; index >= 0 && text[index] == '\\'; index-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			break
		}
		end--
	}
	return text[:end]
}
