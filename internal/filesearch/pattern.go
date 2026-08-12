package filesearch

import (
	"errors"
	"fmt"
	"strings"
)

const (
	maxPatternBytes         = 64*1024 - 1
	maxExpandedPatternBytes = 1 * 1024 * 1024
	maxBraceExpansions      = 128
)

type directPattern struct {
	include bool
	dirOnly bool
	matcher *ruleMatcher
}

func compileDirectPattern(raw string) (*directPattern, error) {
	if len(raw) > maxPatternBytes {
		return nil, fmt.Errorf("filesearch: glob exceeds %d bytes", maxPatternBytes)
	}
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("filesearch: glob is required")
	}
	if strings.IndexByte(raw, 0) >= 0 || strings.ContainsAny(raw, "\r\n") {
		return nil, errors.New("filesearch: glob contains an invalid character")
	}

	include := true
	pattern := raw
	if strings.HasPrefix(pattern, "!") {
		include = false
		pattern = pattern[1:]
		if pattern == "" {
			return nil, errors.New("filesearch: negative glob has no pattern")
		}
	}
	dirOnly := strings.HasSuffix(pattern, "/")
	if dirOnly {
		pattern = strings.TrimSuffix(pattern, "/")
		if pattern == "" {
			return nil, errors.New("filesearch: directory glob has no pattern")
		}
	}
	expanded, err := expandBraces(pattern)
	if err != nil {
		return nil, fmt.Errorf("filesearch: compile glob %q: %w", raw, err)
	}
	matcher := newRuleMatcher()
	patterns := make([]matcherPattern, 0, len(expanded))
	expandedBytes := 0
	for _, candidate := range expanded {
		if len(candidate) > maxExpandedPatternBytes-expandedBytes {
			return nil, fmt.Errorf(
				"filesearch: glob expansion exceeds %d bytes",
				maxExpandedPatternBytes,
			)
		}
		expandedBytes += len(candidate)
		patterns = append(patterns, matcherPattern{text: candidate, display: candidate})
	}
	if patternErrs := matcher.addPatternList(patterns, "", ""); len(patternErrs) > 0 {
		return nil, fmt.Errorf("filesearch: compile glob %q: %s", raw, patternErrs[0].message)
	}
	return &directPattern{
		include: include,
		dirOnly: dirOnly,
		matcher: matcher,
	}, nil
}

func (p *directPattern) matches(path string, isDir bool) bool {
	if p.dirOnly && !isDir {
		return false
	}
	return p.matcher.matchesDirect(path, isDir)
}

// mayMatchDescendant reports whether a slash-bearing query explicitly names
// a path below this directory. Basename-only globs retain the established
// filesearch behavior: they can reopen a matching ignored file, but do not
// force traversal through every ignored directory in the tree.
func (p *directPattern) mayMatchDescendant(path string) bool {
	return p.include && p.matcher.mayMatchDescendant(path)
}

func expandBraces(pattern string) ([]string, error) {
	results := []string{pattern}
	for {
		expandedAny := false
		next := make([]string, 0, len(results))
		for _, current := range results {
			open, closeIndex, alternatives, ok, err := firstBraceGroup(current)
			if err != nil {
				return nil, err
			}
			if !ok {
				next = append(next, current)
				continue
			}
			expandedAny = true
			for _, alternative := range alternatives {
				if alternative == "" {
					return nil, errors.New("empty brace alternatives are not supported")
				}
				next = append(next, current[:open]+alternative+current[closeIndex+1:])
				if len(next) > maxBraceExpansions {
					return nil, fmt.Errorf("brace expansion exceeds %d alternatives", maxBraceExpansions)
				}
			}
		}
		results = next
		if !expandedAny {
			return results, nil
		}
	}
}

// firstBraceGroup finds the first group that actually contains alternatives.
// A comma-free outer pair is transparent, so {{a,b}} expands the nested group
// first. The explicit stack also bounds call depth for adversarial input.
func firstBraceGroup(pattern string) (
	open, closeIndex int,
	alternatives []string,
	ok bool,
	err error,
) {
	type braceFrame struct {
		open        int
		hasComma    bool
		nestedOpen  int
		nestedClose int
		hasNested   bool
	}
	var stack []braceFrame
	escaped := false
	for index := 0; index < len(pattern); index++ {
		char := pattern[index]
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' {
			escaped = true
			continue
		}
		if char == '[' {
			classEnd := bracketExpressionEnd(pattern, index)
			if classEnd < 0 {
				break
			}
			index = classEnd
			continue
		}
		switch char {
		case '{':
			stack = append(stack, braceFrame{open: index})
		case ',':
			if len(stack) > 0 {
				stack[len(stack)-1].hasComma = true
			}
		case '}':
			if len(stack) == 0 {
				return 0, 0, nil, false, errors.New("unmatched closing brace")
			}
			frame := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			candidateOpen, candidateClose, hasCandidate :=
				frame.nestedOpen, frame.nestedClose, frame.hasNested
			if frame.hasComma {
				candidateOpen, candidateClose, hasCandidate = frame.open, index, true
			}
			if len(stack) == 0 {
				if !hasCandidate {
					continue
				}
				parts := splitBraceAlternatives(pattern[candidateOpen+1 : candidateClose])
				return candidateOpen, candidateClose, parts, true, nil
			}
			parent := &stack[len(stack)-1]
			if hasCandidate && !parent.hasNested {
				parent.nestedOpen = candidateOpen
				parent.nestedClose = candidateClose
				parent.hasNested = true
			}
		}
	}
	if len(stack) != 0 {
		return 0, 0, nil, false, errors.New("unmatched opening brace")
	}
	return 0, 0, nil, false, nil
}

func splitBraceAlternatives(body string) []string {
	var parts []string
	start := 0
	depth := 0
	escaped := false
	for index := 0; index < len(body); index++ {
		char := body[index]
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' {
			escaped = true
			continue
		}
		if char == '[' {
			classEnd := bracketExpressionEnd(body, index)
			if classEnd < 0 {
				break
			}
			index = classEnd
			continue
		}
		switch char {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[start:index])
				start = index + 1
			}
		}
	}
	parts = append(parts, body[start:])
	return parts
}

// bracketExpressionEnd skips bracket contents while looking for brace syntax.
// It mirrors the bracket boundaries recognized by the matcher, including a
// literal leading ']' and nested POSIX classes such as [[:digit:]].
func bracketExpressionEnd(pattern string, start int) int {
	index := start + 1
	if index >= len(pattern) {
		return -1
	}
	if pattern[index] == '!' || pattern[index] == '^' {
		index++
	}
	// A closing bracket in the first content position is a literal member.
	if index < len(pattern) && pattern[index] == ']' {
		index++
	}
	for index < len(pattern) {
		switch pattern[index] {
		case '\\':
			index += 2
		case '[':
			if index+1 < len(pattern) && pattern[index+1] == ':' {
				if next := posixBracketClassEnd(pattern, index); next >= 0 {
					index = next
					continue
				}
			}
			index++
		case ']':
			return index
		default:
			index++
		}
	}
	return -1
}

func posixBracketClassEnd(pattern string, start int) int {
	for index := start + 2; index+1 < len(pattern); index++ {
		if pattern[index] == ':' && pattern[index+1] == ']' {
			return index + 2
		}
	}
	return -1
}
