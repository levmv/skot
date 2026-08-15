package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/toolpolicy"
)

const footerSeparator = " · "

func (m *screenModel) refreshSessionStatus() {
	m.sessionStatus = m.agent.SessionStatus()
}

func (m screenModel) footerLine() string {
	model := strings.TrimSpace(m.agent.CurrentModel())
	effort := strings.TrimSpace(m.agent.CurrentReasoningEffort())
	if model == "" {
		// Config only applies before the agent publishes its own selection, and
		// it covers both fields: an empty effort is a valid choice (the default),
		// so it cannot decide the fallback on its own.
		model = strings.TrimSpace(m.config.ModelURI)
		effort = strings.TrimSpace(m.config.ReasoningEffort)
	}
	if effort != "" {
		model += " " + effort
	}

	toolSet := strings.TrimSpace(m.agent.CurrentToolSet())
	if toolSet == "" {
		toolSet = strings.TrimSpace(m.config.ToolSet)
	}
	// The footer names the tool set only when it departs from the default, the
	// same way it stays silent about a default reasoning effort. Both are there
	// to surface a deliberate choice, and the default is not one.
	if toolSet == toolpolicy.ToolSetDefault {
		toolSet = ""
	}

	// The tool set sits away from the model, where a bare "default" next to a
	// model name read as part of the selection. Root goes last because it is
	// the one part that gets truncated, which keeps the rest at a stable width.
	beforeRoot := compactFooterParts(
		model,
		compactContextStatus(m.sessionStatus.ContextReport),
		compactUsage(m.sessionStatus.Usage),
		toolSet,
	)
	root := sanitizeTerminalText(strings.TrimSpace(m.config.Root))
	root = truncateFooterRoot(root, beforeRoot, "", m.contentWidth())

	return m.mutedStyle.Render(compactFooterParts(beforeRoot, root))
}

func compactFooterParts(parts ...string) string {
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = sanitizeTerminalText(strings.TrimSpace(part)); part != "" {
			result = append(result, part)
		}
	}
	return strings.Join(result, footerSeparator)
}

func truncateFooterRoot(root, before, after string, width int) string {
	if root == "" {
		return ""
	}
	full := compactFooterParts(before, root, after)
	if visibleLen(full) <= width {
		return root
	}
	fixed := compactFooterParts(before, after)
	available := width - visibleLen(fixed)
	if fixed != "" {
		available -= visibleLen(footerSeparator)
	}
	if available <= 0 {
		return ""
	}
	return truncateMiddle(root, available)
}

func truncateMiddle(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if visibleLen(value) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	contentWidth := width - 1
	leftWidth := (contentWidth + 1) / 2
	rightWidth := contentWidth - leftWidth
	left := ansi.Truncate(value, leftWidth, "")
	right := ""
	if rightWidth > 0 {
		right = ansi.TruncateLeft(value, visibleLen(value)-rightWidth, "")
	}
	return left + "…" + right
}

func compactContextStatus(report agent.ContextReport) string {
	if report.Window <= 0 {
		return ""
	}
	// InputLimit is the automatic-compaction boundary, so 100% means the next
	// request has exhausted its usable budget rather than the provider window.
	limit := report.InputLimit
	if limit <= 0 {
		limit = report.Window
	}
	percent := int(float64(max(0, report.TotalInputTokens))*100/float64(limit) + 0.5)
	if percent == 0 && report.TotalInputTokens > 0 {
		// Rounding to zero beside a visible token count reads as a
		// contradiction, so a used-but-tiny context says so directly.
		return "ctx <1%"
	}
	return fmt.Sprintf("ctx ~%d%%", percent)
}

func compactUsage(usage agent.ModelUsage) string {
	if usage.InputTokens == 0 && usage.CachedInputTokens == 0 && usage.OutputTokens == 0 {
		return ""
	}
	sent := "↑" + formatTokenCount(usage.InputTokens)
	if usage.CachedInputTokens > 0 {
		// Cached input is part of the input on every backend, not additional to
		// it. The parentheses carry that on their own, so the figure inside
		// needs no arrow. No space either: spaces separate the arrows from each
		// other, so one here would leave the parenthetical floating between them.
		sent += "(" + formatTokenCount(usage.CachedInputTokens) + ")"
	}
	return sent + " ↓" + formatTokenCount(usage.OutputTokens)
}

func formatTokenCount(tokens int) string {
	tokens = max(0, tokens)
	if tokens < 1_000 {
		return strconv.Itoa(tokens)
	}
	divisor, suffix := 1_000, "k"
	switch {
	case tokens >= 999_950_000:
		divisor, suffix = 1_000_000_000, "b"
	case tokens >= 999_950:
		divisor, suffix = 1_000_000, "m"
	}
	value := float64(tokens) / float64(divisor)
	formatted := strconv.FormatFloat(value, 'f', 1, 64)
	formatted = strings.TrimSuffix(formatted, ".0")
	return formatted + suffix
}
