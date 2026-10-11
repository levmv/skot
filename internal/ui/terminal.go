package ui

import (
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
)

func IsTerminalFile(file *os.File) bool {
	return file != nil && term.IsTerminal(file.Fd())
}

// TerminalText removes terminal controls when out is a terminal. Pipes and
// files receive the original text.
func TerminalText(out io.Writer, text string) string {
	if file, ok := out.(*os.File); ok && IsTerminalFile(file) {
		return sanitizeTerminalText(text)
	}
	return text
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
	var sanitizer terminalTextSanitizer
	return sanitizer.append(strings.ToValidUTF8(text, "�"))
}

type terminalTextState uint8

const (
	terminalTextPlain terminalTextState = iota
	terminalTextEscape
	terminalTextEscapeIntermediate
	terminalTextCSI
	terminalTextOSC
	terminalTextString
	terminalTextOSCEscape
	terminalTextStringEscape
)

// A stream owns one sanitizer until its attempt ends. The runtime emits valid
// UTF-8 deltas; only terminal-control state needs to survive between them.
// Even an unterminated control string uses constant memory.
type terminalTextSanitizer struct {
	state terminalTextState
}

func (sanitizer *terminalTextSanitizer) append(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	for _, r := range text {
		switch sanitizer.state {
		case terminalTextOSC, terminalTextString, terminalTextOSCEscape, terminalTextStringEscape:
			osc := sanitizer.state == terminalTextOSC || sanitizer.state == terminalTextOSCEscape
			escaped := sanitizer.state == terminalTextOSCEscape || sanitizer.state == terminalTextStringEscape
			switch {
			case r == 0x9c || escaped && r == '\\' || osc && r == '\a':
				sanitizer.state = terminalTextPlain
			case r == 0x1b:
				sanitizer.state = terminalTextStringEscape
				if osc {
					sanitizer.state = terminalTextOSCEscape
				}
			default:
				sanitizer.state = terminalTextString
				if osc {
					sanitizer.state = terminalTextOSC
				}
			}
		case terminalTextEscape:
			switch r {
			case '[':
				sanitizer.state = terminalTextCSI
			case ']':
				sanitizer.state = terminalTextOSC
			case 'P', 'X', '^', '_':
				sanitizer.state = terminalTextString
			case 0x1b:
			default:
				sanitizer.state = terminalTextPlain
				if r >= 0x20 && r <= 0x2f {
					sanitizer.state = terminalTextEscapeIntermediate
				}
			}
		case terminalTextCSI, terminalTextEscapeIntermediate:
			finalMin := rune(0x40)
			if sanitizer.state == terminalTextEscapeIntermediate {
				finalMin = 0x30
			}
			if r == 0x1b {
				sanitizer.state = terminalTextEscape
			} else if r >= finalMin && r <= 0x7e {
				sanitizer.state = terminalTextPlain
			}
		case terminalTextPlain:
			switch r {
			case 0x1b:
				sanitizer.state = terminalTextEscape
			case 0x9b:
				sanitizer.state = terminalTextCSI
			case 0x9d:
				sanitizer.state = terminalTextOSC
			case 0x90, 0x98, 0x9e, 0x9f:
				sanitizer.state = terminalTextString
			default:
				if r == '\n' || r == '\t' || r >= 0x20 && !(r >= 0x7f && r <= 0x9f) {
					out.WriteRune(r)
				}
			}
		}
	}
	return out.String()
}
