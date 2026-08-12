package ui

import (
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/term"
)

func IsTerminalFile(file *os.File) bool {
	return file != nil && term.IsTerminal(file.Fd())
}

func shouldUseStyle(out io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SK_COLOR"))) {
	case "always", "1", "true", "yes", "on":
		return true
	case "never", "0", "false", "no", "off":
		return false
	}
	file, ok := out.(*os.File)
	return ok && IsTerminalFile(file)
}

// sanitizeTerminalText removes control sequences from model, tool, session,
// and workspace text before it reaches a human terminal. JSON output does not
// use this path; encoding/json safely escapes control bytes for consumers.
func sanitizeTerminalText(text string) string {
	text = strings.ToValidUTF8(text, "�")
	var out strings.Builder
	out.Grow(len(text))
	for index := 0; index < len(text); {
		if text[index] == 0x1b {
			index = skipEscapeSequence(text, index)
			continue
		}
		r, size := utf8.DecodeRuneInString(text[index:])
		if size <= 0 {
			size = 1
		}
		switch {
		case r == '\n' || r == '\t':
			out.WriteRune(r)
		case r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f:
			// Drop C0/C1 controls, including BEL, CR, backspace, and their
			// single-codepoint CSI/OSC forms.
		default:
			out.WriteRune(r)
		}
		index += size
	}
	return out.String()
}

func skipEscapeSequence(text string, start int) int {
	index := start + 1
	if index >= len(text) {
		return index
	}
	switch text[index] {
	case '[': // CSI: parameters/intermediates followed by one final byte.
		index++
		for index < len(text) {
			value := text[index]
			index++
			if value >= 0x40 && value <= 0x7e {
				break
			}
		}
		return index
	case ']': // OSC: BEL or ST terminates the payload.
		return skipStringEscape(text, index+1, true)
	case 'P', 'X', '^', '_': // DCS, SOS, PM, APC: terminated by ST.
		return skipStringEscape(text, index+1, false)
	default:
		// Two-byte ESC Fe sequences and charset selectors are harmlessly
		// removed with their introducer.
		return min(index+1, len(text))
	}
}

func skipStringEscape(text string, index int, allowBEL bool) int {
	for index < len(text) {
		if allowBEL && text[index] == '\a' {
			return index + 1
		}
		if text[index] == 0x1b && index+1 < len(text) && text[index+1] == '\\' {
			return index + 2
		}
		index++
	}
	return index
}
