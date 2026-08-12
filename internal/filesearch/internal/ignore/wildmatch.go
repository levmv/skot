package ignore

type tokenKind uint8

const (
	tokenLiteral tokenKind = iota
	tokenAny
	tokenStar
	tokenClass
)

type token struct {
	kind       tokenKind
	value      byte
	classIndex int
}

type byteClass struct {
	bits    [4]uint64
	negated bool
}

func (c *byteClass) add(value byte) {
	c.bits[value/64] |= uint64(1) << (value % 64)
}

func (c *byteClass) addRange(low, high byte) {
	for value := low; ; value++ {
		c.add(value)
		if value == high {
			return
		}
	}
}

func (c byteClass) matches(value byte) bool {
	present := c.bits[value/64]&(uint64(1)<<(value%64)) != 0
	if c.negated {
		return !present
	}
	return present
}

type segment struct {
	doubleStar bool
	tokens     []token
	classes    []byteClass
}

func compileSegment(pattern string) (segment, string) {
	compiled := segment{tokens: make([]token, 0, len(pattern))}
	for index := 0; index < len(pattern); {
		switch pattern[index] {
		case '\\':
			if index+1 >= len(pattern) {
				return segment{}, "trailing backslash"
			}
			compiled.tokens = append(compiled.tokens, token{kind: tokenLiteral, value: pattern[index+1]})
			index += 2
		case '?':
			compiled.tokens = append(compiled.tokens, token{kind: tokenAny})
			index++
		case '*':
			if len(compiled.tokens) == 0 || compiled.tokens[len(compiled.tokens)-1].kind != tokenStar {
				compiled.tokens = append(compiled.tokens, token{kind: tokenStar})
			}
			index++
		case '[':
			class, next, message := compileClass(pattern, index)
			if message != "" {
				return segment{}, message
			}
			classIndex := len(compiled.classes)
			compiled.classes = append(compiled.classes, class)
			compiled.tokens = append(compiled.tokens, token{kind: tokenClass, classIndex: classIndex})
			index = next
		default:
			compiled.tokens = append(compiled.tokens, token{kind: tokenLiteral, value: pattern[index]})
			index++
		}
	}
	return compiled, ""
}

// matchSegments uses the same bounded two-pointer strategy as ordinary glob
// matching. A ** segment is the only construct that may consume separators.
func matchSegments(pattern []segment, path []string) bool {
	return matchSegmentsMode(pattern, path, false)
}

// matchSegmentsPrefix reports whether pattern matches any prefix of path,
// including the empty prefix. It performs the prefix search as part of the
// ordinary bounded globstar scan instead of retrying every possible prefix.
func matchSegmentsPrefix(pattern []segment, path []string) bool {
	return matchSegmentsMode(pattern, path, true)
}

// mayMatchSegmentDescendant reports whether path is a prefix of at least one
// match that contains another segment. It is the segment-level counterpart to
// shell completion: a ** may absorb more of the existing prefix, while an
// unmatched pattern suffix can be supplied by future descendants.
func mayMatchSegmentDescendant(pattern []segment, path []string) bool {
	patternIndex, pathIndex := 0, 0
	starPattern, starPath := -1, -1
	for pathIndex < len(path) {
		if patternIndex < len(pattern) && pattern[patternIndex].doubleStar {
			starPattern = patternIndex
			starPath = pathIndex
			patternIndex++
			continue
		}
		if patternIndex < len(pattern) && matchSegment(pattern[patternIndex], path[pathIndex]) {
			patternIndex++
			pathIndex++
			continue
		}
		if starPattern >= 0 && starPath < len(path) {
			starPath++
			pathIndex = starPath
			patternIndex = starPattern + 1
			continue
		}
		return false
	}
	// A remaining segment can be matched by an appended path component. If the
	// current path already consumed the pattern, a previously encountered **
	// can instead absorb more input and leave its suffix for appended components.
	return patternIndex < len(pattern) || starPattern >= 0
}

func matchSegmentsMode(pattern []segment, path []string, allowPrefix bool) bool {
	patternIndex, pathIndex := 0, 0
	starPattern, starPath := -1, -1
	for pathIndex < len(path) {
		if allowPrefix && patternIndex == len(pattern) {
			return true
		}
		if patternIndex < len(pattern) && pattern[patternIndex].doubleStar {
			starPattern = patternIndex
			starPath = pathIndex
			patternIndex++
			continue
		}
		if patternIndex < len(pattern) && matchSegment(pattern[patternIndex], path[pathIndex]) {
			patternIndex++
			pathIndex++
			continue
		}
		if starPattern >= 0 && starPath < len(path) {
			starPath++
			pathIndex = starPath
			patternIndex = starPattern + 1
			continue
		}
		return false
	}
	for patternIndex < len(pattern) && pattern[patternIndex].doubleStar {
		patternIndex++
	}
	return patternIndex == len(pattern)
}

// matchSegment keeps only the most recent '*' checkpoint. On a mismatch that
// star consumes one more byte and the literal suffix is retried. This is the
// standard bounded greedy glob scan: constant auxiliary space and no
// recursive or exponential backtracking.
func matchSegment(pattern segment, text string) bool {
	patternIndex, textIndex := 0, 0
	starPattern, starText := -1, -1
	for textIndex < len(text) {
		if patternIndex < len(pattern.tokens) {
			candidate := pattern.tokens[patternIndex]
			switch candidate.kind {
			case tokenStar:
				starPattern = patternIndex
				starText = textIndex
				patternIndex++
				continue
			case tokenAny:
				patternIndex++
				textIndex++
				continue
			case tokenLiteral:
				if candidate.value == text[textIndex] {
					patternIndex++
					textIndex++
					continue
				}
			case tokenClass:
				if pattern.classes[candidate.classIndex].matches(text[textIndex]) {
					patternIndex++
					textIndex++
					continue
				}
			}
		}
		if starPattern >= 0 && starText < len(text) {
			starText++
			textIndex = starText
			patternIndex = starPattern + 1
			continue
		}
		return false
	}
	for patternIndex < len(pattern.tokens) && pattern.tokens[patternIndex].kind == tokenStar {
		patternIndex++
	}
	return patternIndex == len(pattern.tokens)
}

func compileClass(pattern string, start int) (byteClass, int, string) {
	var class byteClass
	index := start + 1
	if index >= len(pattern) {
		return byteClass{}, 0, "unclosed bracket expression"
	}
	if pattern[index] == '!' || pattern[index] == '^' {
		class.negated = true
		index++
	}
	first := true
	for index < len(pattern) {
		if pattern[index] == ']' && !first {
			return class, index + 1, ""
		}
		first = false
		if pattern[index] == '[' && index+1 < len(pattern) && pattern[index+1] == ':' {
			name, next, ok := readPOSIXClass(pattern, index)
			if ok {
				if !addPOSIXClass(&class, name) {
					return byteClass{}, 0, "unknown POSIX class [:" + name + ":]"
				}
				index = next
				continue
			}
		}
		low, next := readClassByte(pattern, index)
		index = next
		if index+1 < len(pattern) && pattern[index] == '-' && pattern[index+1] != ']' {
			high, after := readClassByte(pattern, index+1)
			if low <= high {
				class.addRange(low, high)
			} else {
				// Git's wildmatch keeps the first endpoint matched even when the
				// range is reversed (for example, [z-a] still matches z).
				class.add(low)
			}
			index = after
			continue
		}
		class.add(low)
	}
	return byteClass{}, 0, "unclosed bracket expression"
}

func readClassByte(pattern string, index int) (byte, int) {
	if pattern[index] == '\\' && index+1 < len(pattern) {
		return pattern[index+1], index + 2
	}
	return pattern[index], index + 1
}

func readPOSIXClass(pattern string, start int) (string, int, bool) {
	for index := start + 2; index+1 < len(pattern); index++ {
		if pattern[index] == ':' && pattern[index+1] == ']' {
			return pattern[start+2 : index], index + 2, true
		}
	}
	return "", start, false
}

func addPOSIXClass(class *byteClass, name string) bool {
	// Git wildmatch defines these classes over bytes using ASCII predicates,
	// independently of the process locale.
	var predicate func(byte) bool
	switch name {
	case "alnum":
		predicate = isAlnum
	case "alpha":
		predicate = isAlpha
	case "blank":
		predicate = isBlank
	case "cntrl":
		predicate = isControl
	case "digit":
		predicate = isDigit
	case "graph":
		predicate = isGraph
	case "lower":
		predicate = isLower
	case "print":
		predicate = isPrint
	case "punct":
		predicate = isPunctuation
	case "space":
		predicate = isSpace
	case "upper":
		predicate = isUpper
	case "xdigit":
		predicate = isHexDigit
	default:
		return false
	}
	for value := byte(0); ; value++ {
		if predicate(value) {
			class.add(value)
		}
		if value == 0xff {
			break
		}
	}
	return true
}

func isAlpha(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func isDigit(value byte) bool {
	return value >= '0' && value <= '9'
}

func isAlnum(value byte) bool {
	return isAlpha(value) || isDigit(value)
}

func isBlank(value byte) bool {
	return value == ' ' || value == '\t'
}

func isControl(value byte) bool {
	return value < 0x20 || value == 0x7f
}

func isGraph(value byte) bool {
	return value > 0x20 && value < 0x7f
}

func isLower(value byte) bool {
	return value >= 'a' && value <= 'z'
}

func isPrint(value byte) bool {
	return value >= 0x20 && value < 0x7f
}

func isPunctuation(value byte) bool {
	return isGraph(value) && !isAlpha(value) && !isDigit(value)
}

func isSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n' || value == '\r' || value == '\f' || value == '\v'
}

func isUpper(value byte) bool {
	return value >= 'A' && value <= 'Z'
}

func isHexDigit(value byte) bool {
	return isDigit(value) || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}
