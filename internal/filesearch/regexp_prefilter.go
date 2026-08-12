package filesearch

import (
	"bytes"
	"regexp/syntax"
	"unicode"
	"unicode/utf8"
)

const maxRegexpPrefilterAlternatives = 16

type foldLiteralMatcher struct {
	runes []rune
	ascii []byte
}

// regexpFoldLiteral recognizes an unanchored literal under case folding. An
// all-ASCII line can use the byte path; other lines use Unicode SimpleFold so
// uncommon equivalents such as the Kelvin sign keep standard regexp behavior.
func regexpFoldLiteral(pattern string) *foldLiteralMatcher {
	expression, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil
	}
	expression = expression.Simplify()
	if expression.Op != syntax.OpLiteral || expression.Flags&syntax.FoldCase == 0 || len(expression.Rune) == 0 {
		return nil
	}
	matcher := &foldLiteralMatcher{runes: expression.Rune}
	matcher.ascii = make([]byte, len(expression.Rune))
	for index, value := range expression.Rune {
		if value < 0 || value >= utf8.RuneSelf {
			matcher.ascii = nil
			break
		}
		matcher.ascii[index] = lowerASCII(byte(value))
	}
	return matcher
}

func (m *foldLiteralMatcher) matches(line []byte) bool {
	if m.ascii != nil && isASCII(line) {
		return containsFoldASCII(line, m.ascii)
	}
	for offset := 0; offset < len(line); {
		if matchFoldRunes(line[offset:], m.runes) {
			return true
		}
		_, size := utf8.DecodeRune(line[offset:])
		offset += size
	}
	return false
}

func isASCII(data []byte) bool {
	for _, value := range data {
		if value >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func containsFoldASCII(line, lowercaseLiteral []byte) bool {
	if len(lowercaseLiteral) > len(line) {
		return false
	}
	for start := 0; start <= len(line)-len(lowercaseLiteral); start++ {
		if lowerASCII(line[start]) != lowercaseLiteral[0] {
			continue
		}
		matched := true
		for index := 1; index < len(lowercaseLiteral); index++ {
			if lowerASCII(line[start+index]) != lowercaseLiteral[index] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func lowerASCII(value byte) byte {
	if value >= 'A' && value <= 'Z' {
		return value + ('a' - 'A')
	}
	return value
}

func matchFoldRunes(line []byte, literal []rune) bool {
	for _, want := range literal {
		if len(line) == 0 {
			return false
		}
		got, size := utf8.DecodeRune(line)
		if !runesEqualFold(got, want) {
			return false
		}
		line = line[size:]
	}
	return true
}

func runesEqualFold(left, right rune) bool {
	if left == right {
		return true
	}
	for folded := unicode.SimpleFold(left); folded != left; folded = unicode.SimpleFold(folded) {
		if folded == right {
			return true
		}
	}
	return false
}

// regexpRequiredLiterals returns a bounded set of literals where every match
// must contain at least one set member. An empty result means that no safe,
// useful prefilter was found.
func regexpRequiredLiterals(pattern string) [][]byte {
	expression, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil
	}
	return requiredLiterals(expression.Simplify())
}

func requiredLiterals(expression *syntax.Regexp) [][]byte {
	switch expression.Op {
	case syntax.OpLiteral:
		if expression.Flags&syntax.FoldCase != 0 || len(expression.Rune) == 0 {
			return nil
		}
		return [][]byte{[]byte(string(expression.Rune))}
	case syntax.OpCapture:
		return requiredLiterals(expression.Sub[0])
	case syntax.OpConcat:
		var best [][]byte
		for _, child := range expression.Sub {
			best = betterLiteralSet(best, requiredLiterals(child))
		}
		return best
	case syntax.OpAlternate:
		var combined [][]byte
		for _, child := range expression.Sub {
			literals := requiredLiterals(child)
			if len(literals) == 0 || len(combined)+len(literals) > maxRegexpPrefilterAlternatives {
				return nil
			}
			combined = appendUniqueLiterals(combined, literals)
		}
		return combined
	case syntax.OpPlus:
		return requiredLiterals(expression.Sub[0])
	case syntax.OpRepeat:
		if expression.Min > 0 {
			return requiredLiterals(expression.Sub[0])
		}
	}
	return nil
}

func betterLiteralSet(current, candidate [][]byte) [][]byte {
	if len(candidate) == 0 {
		return current
	}
	if len(current) == 0 {
		return candidate
	}
	// Every concatenated child is mandatory, so either set is safe. Prefer the
	// one whose least selective alternative is longer, then the smaller set.
	currentMin := shortestLiteral(current)
	candidateMin := shortestLiteral(candidate)
	if candidateMin > currentMin || (candidateMin == currentMin && len(candidate) < len(current)) {
		return candidate
	}
	return current
}

func shortestLiteral(literals [][]byte) int {
	shortest := len(literals[0])
	for _, literal := range literals[1:] {
		shortest = min(shortest, len(literal))
	}
	return shortest
}

func appendUniqueLiterals(destination, source [][]byte) [][]byte {
	for _, candidate := range source {
		duplicate := false
		for _, existing := range destination {
			if bytes.Equal(existing, candidate) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			destination = append(destination, candidate)
		}
	}
	return destination
}

func containsRequiredLiteral(line []byte, literals [][]byte) bool {
	for _, literal := range literals {
		if bytes.Contains(line, literal) {
			return true
		}
	}
	return false
}
