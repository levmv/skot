package ui

import "testing"

func TestSanitizeTerminalTextRemovesCSIAndStringControls(t *testing.T) {
	input := "safe\x1b[31mred\x1b[0m " +
		"\x1b]52;c;Y2xpcGJvYXJk\aafter " +
		"\x1bP1;2|device-control\x1b\\done\b\r"
	want := "safered after done"
	if got := sanitizeTerminalText(input); got != want {
		t.Fatalf("sanitizeTerminalText() = %q, want %q", got, want)
	}
}
