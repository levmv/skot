package ui

import (
	"strings"
	"testing"
)

func TestSanitizeTerminalTextRemovesCSIAndStringControls(t *testing.T) {
	input := "safe\x1b[31mred\x1b[0m " +
		"\x1b]52;c;Y2xpcGJvYXJk\aafter " +
		"\x1bP1;2|device-control\x1b\\done\b\r"
	want := "safered after done"
	if got := sanitizeTerminalText(input); got != want {
		t.Fatalf("sanitizeTerminalText() = %q, want %q", got, want)
	}
}

func TestAssistantStreamSanitizesAcrossChunks(t *testing.T) {
	type chunk struct{ text, want string }
	for _, test := range []struct {
		name   string
		chunks []chunk
	}{
		{"CSI", []chunk{
			{"До \x1b", "До "},
			{"[3", "До "},
			{"1mкрасный\x1b[0", "До красный"},
			{"m\n\t世界 🙂", "До красный\n\t世界 🙂"},
		}},
		{"OSC with BEL", []chunk{
			{"before\x1b]0;hidden", "before"},
			{" title", "before"},
			{"\aafter", "beforeafter"},
		}},
		{"OSC with ST", []chunk{
			{"before\x1b]52;c;hidden", "before"},
			{" clipboard\x1b", "before"},
			{"\\after", "beforeafter"},
		}},
		{"DCS", []chunk{
			{"before\x1bP1;2|hidden\a", "before"},
			{" payload\x1b", "before"},
			{"\\after", "beforeafter"},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var transcript transcriptState
			transcript.startAttempt("response")
			for index, chunk := range test.chunks {
				transcript.appendAssistant("response", chunk.text)
				var got strings.Builder
				for _, block := range transcript.blocks {
					got.WriteString(block.text)
				}
				if got.String() != chunk.want {
					t.Fatalf("after chunk %d: streamed text = %q, want %q", index+1, got.String(), chunk.want)
				}
			}
		})
	}
}

func TestAssistantControlDoesNotCarryIntoNextAttempt(t *testing.T) {
	var transcript transcriptState
	transcript.startAttempt("first")
	transcript.appendAssistant("first", "\x1b]0;unfinished title")
	transcript.startAttempt("next")
	transcript.appendAssistant("next", "visible response")
	if len(transcript.blocks) != 1 || transcript.blocks[0].text != "visible response" {
		t.Fatalf("next attempt transcript = %+v", transcript.blocks)
	}
}
