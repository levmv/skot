package ignore

import (
	"strings"
	"testing"
)

func TestSegmentMatcherAgreesWithReferenceStateMachine(t *testing.T) {
	patternAtoms := []string{"a", "b", "?", "*", "[ab]"}
	textAtoms := []byte{'a', 'b'}
	forEachSequence(patternAtoms, 4, func(patternParts []string) {
		patternText := strings.Join(patternParts, "")
		pattern, message := compileSegment(patternText)
		if message != "" {
			t.Fatalf("compileSegment(%q): %s", patternText, message)
		}
		forEachSequence(textAtoms, 5, func(textParts []byte) {
			text := string(textParts)
			want := matchSegmentStateMachine(pattern, text)
			if got := matchSegment(pattern, text); got != want {
				t.Fatalf("matchSegment(%q, %q) = %t, state machine says %t", patternText, text, got, want)
			}
		})
	})
}

func TestPathMatcherAgreesWithReferenceStateMachine(t *testing.T) {
	literalA, message := compileSegment("a")
	if message != "" {
		t.Fatal(message)
	}
	literalB, message := compileSegment("b")
	if message != "" {
		t.Fatal(message)
	}
	patternAtoms := []segment{literalA, literalB, {doubleStar: true}}
	pathAtoms := []string{"a", "b"}
	forEachSequence(patternAtoms, 5, func(pattern []segment) {
		forEachSequence(pathAtoms, 6, func(path []string) {
			wantExact := matchPathStateMachine(pattern, path, false)
			if got := matchSegments(pattern, path); got != wantExact {
				t.Fatalf("matchSegments(%v, %q) = %t, state machine says %t", pattern, path, got, wantExact)
			}
			wantPrefix := matchPathStateMachine(pattern, path, true)
			if got := matchSegmentsPrefix(pattern, path); got != wantPrefix {
				t.Fatalf(
					"matchSegmentsPrefix(%v, %q) = %t, state machine says %t",
					pattern, path, got, wantPrefix,
				)
			}
		})
	})
}

func matchSegmentStateMachine(pattern segment, text string) bool {
	states := make([]bool, len(pattern.tokens)+1)
	states[0] = true
	closeSegmentStars(pattern, states)
	for index := range len(text) {
		next := make([]bool, len(states))
		for state := range pattern.tokens {
			if !states[state] {
				continue
			}
			candidate := pattern.tokens[state]
			switch candidate.kind {
			case tokenStar:
				next[state] = true
			case tokenAny:
				next[state+1] = true
			case tokenLiteral:
				next[state+1] = candidate.value == text[index]
			case tokenClass:
				next[state+1] = pattern.classes[candidate.classIndex].matches(text[index])
			}
		}
		states = next
		closeSegmentStars(pattern, states)
	}
	return states[len(pattern.tokens)]
}

func closeSegmentStars(pattern segment, states []bool) {
	for state := range pattern.tokens {
		if states[state] && pattern.tokens[state].kind == tokenStar {
			states[state+1] = true
		}
	}
}

func matchPathStateMachine(pattern []segment, path []string, allowPrefix bool) bool {
	states := make([]bool, len(pattern)+1)
	states[0] = true
	closeGlobstars(pattern, states)
	if allowPrefix && states[len(pattern)] {
		return true
	}
	for _, part := range path {
		next := make([]bool, len(states))
		for state := range pattern {
			if !states[state] {
				continue
			}
			if pattern[state].doubleStar {
				next[state] = true
			} else if matchSegment(pattern[state], part) {
				next[state+1] = true
			}
		}
		states = next
		closeGlobstars(pattern, states)
		if allowPrefix && states[len(pattern)] {
			return true
		}
	}
	return states[len(pattern)]
}

func closeGlobstars(pattern []segment, states []bool) {
	for state := range pattern {
		if states[state] && pattern[state].doubleStar {
			states[state+1] = true
		}
	}
}

func forEachSequence[T any](alphabet []T, maximumLength int, visit func([]T)) {
	var walk func([]T)
	walk = func(sequence []T) {
		visit(sequence)
		if len(sequence) == maximumLength {
			return
		}
		for _, atom := range alphabet {
			walk(append(sequence, atom))
		}
	}
	walk(nil)
}
