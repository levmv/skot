package ui

import (
	"fmt"
	"strings"
	"time"
)

func formatDuration(duration time.Duration) string {
	duration = max(time.Duration(0), duration)
	if duration < time.Second {
		return fmt.Sprintf("%dms", duration.Milliseconds())
	}
	if duration < 10*time.Second {
		return fmt.Sprintf("%.1fs", duration.Seconds())
	}
	if duration < time.Minute {
		return fmt.Sprintf("%.0fs", duration.Seconds())
	}
	minutes := int(duration / time.Minute)
	seconds := int(duration/time.Second) % 60
	return fmt.Sprintf("%dm%02ds", minutes, seconds)
}

func formatTurnDuration(duration time.Duration) string {
	seconds := max(int64(0), int64(duration/time.Second))
	hours := seconds / 3600
	minutes := seconds % 3600 / 60
	seconds %= 60
	switch {
	case hours > 0:
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	case minutes > 0:
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

func (m *screenModel) finishTurnChanges() {
	if summary := formatChangedFiles(m.operation.changedPaths); summary != "" {
		m.addBlock(screenBlockSystem, summary)
	}
	m.operation.changedPaths = nil
}

func (m *screenModel) finishTurnDuration(now time.Time) {
	if !m.operation.isTurn() || m.operation.startedAt.IsZero() {
		return
	}
	duration := max(time.Duration(0), now.Sub(m.operation.startedAt))
	m.appendBlock(screenBlock{kind: screenBlockDuration, duration: duration})
}

func (m screenModel) renderDurationLine(duration time.Duration) string {
	// The rule separates turns, so it starts at the screen edge instead of
	// inside the transcript gutter: an indented divider reads as one more
	// piece of content rather than a boundary between them.
	width := transcriptGutter + m.contentWidth()
	line := "─ Worked for " + formatTurnDuration(duration) + " "
	if pad := width - visibleLen(line); pad > 0 {
		line += strings.Repeat("─", pad)
	} else {
		line = truncateANSI(line, width)
	}
	return m.mutedStyle.Render(line)
}
