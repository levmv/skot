package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/levmv/skot/agent"
)

const footerSeparator = " · "

func (m *screenModel) refreshSessionStatus() {
	m.sessionStatus = m.agent.SessionStatus()
}

func (m screenModel) footerLine() string {
	model := strings.TrimSpace(m.agent.CurrentModel())
	if model == "" {
		model = strings.TrimSpace(m.config.ModelURI)
	}
	toolSet := strings.TrimSpace(m.agent.CurrentToolSet())
	if toolSet == "" {
		toolSet = strings.TrimSpace(m.config.ToolSet)
	}

	beforeRoot := compactFooterParts(model, toolSet)
	afterRoot := compactFooterParts(
		compactContextStatus(m.sessionStatus.ContextReport),
		compactUsage(m.sessionStatus.Usage),
	)
	root := sanitizeTerminalText(strings.TrimSpace(m.config.Root))
	root = truncateFooterRoot(root, beforeRoot, afterRoot, m.contentWidth())

	return m.mutedStyle.Render(compactFooterParts(beforeRoot, root, afterRoot))
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
	return fmt.Sprintf("context ~%d%%", percent)
}

func compactUsage(usage agent.ModelUsage) string {
	if usage.InputTokens == 0 && usage.CachedInputTokens == 0 && usage.OutputTokens == 0 {
		return ""
	}
	parts := []string{"↑" + formatTokenCount(usage.InputTokens)}
	if usage.CachedInputTokens > 0 {
		parts = append(parts, "↻"+formatTokenCount(usage.CachedInputTokens))
	}
	parts = append(parts, "↓"+formatTokenCount(usage.OutputTokens))
	return strings.Join(parts, " ")
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
