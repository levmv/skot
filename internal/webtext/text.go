package webtext

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var whitespace = regexp.MustCompile(`\s+`)

func Compact(text string, limit int) string {
	text = strings.TrimSpace(whitespace.ReplaceAllString(Sanitize(text), " "))
	return Truncate(text, limit)
}

func Sanitize(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, text)
}

func Truncate(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.ValidString(text[:cut]) {
		cut--
	}
	return text[:cut] + "\n[…truncated…]"
}
