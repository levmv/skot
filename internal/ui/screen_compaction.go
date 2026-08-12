package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/levmv/skot/agent"
)

func (m *screenModel) startCompaction() tea.Cmd {
	operationCtx, cancel := context.WithCancel(m.ctx)
	m.operation = activeOperation{kind: operationCompaction, startedAt: time.Now(), cancel: cancel}
	return func() tea.Msg {
		record, err := m.agent.Compact(operationCtx, 1)
		var report agent.ContextReport
		if err == nil {
			report, err = m.agent.ContextReport(operationCtx)
		}
		return compactionDoneMsg{record: record, report: report, err: err}
	}
}

func (m *screenModel) finishCompaction(message compactionDoneMsg) {
	m.operation.clear()
	if message.err != nil {
		m.addBlock(screenBlockError, "compact: "+message.err.Error())
		return
	}
	m.addBlock(screenBlockSystem, fmt.Sprintf(
		"context compacted through journal sequence %d\n%s",
		message.record.CoveredThroughSequence,
		formatContextReport(message.report),
	))
}

func formatContextReport(report agent.ContextReport) string {
	var headline string
	if report.Window > 0 {
		headline = fmt.Sprintf("context: %s / %s input tokens · %s available · %s model window",
			formatTokenCount(report.TotalInputTokens),
			formatTokenCount(report.InputLimit),
			formatTokenCount(report.AvailableInputTokens),
			formatTokenCount(report.Window),
		)
	} else {
		headline = fmt.Sprintf("context: %s estimated input tokens · model window unknown", formatTokenCount(report.TotalInputTokens))
	}
	parts := []string{
		"instructions " + formatTokenCount(report.InstructionTokens),
		"tools " + formatTokenCount(report.ToolTokens),
		"summary " + formatTokenCount(report.SummaryTokens),
		"history " + formatTokenCount(report.HistoryTokens),
		"queued " + formatTokenCount(report.PendingTokens),
	}
	return headline + "\n" + strings.Join(parts, " · ") + fmt.Sprintf("\ncompactions %d · tool prunings %d", report.CompactionCount, report.ToolPruningCount)
}

func formatTokenCount(tokens int) string {
	if tokens < 1000 {
		return fmt.Sprintf("%d", tokens)
	}
	if tokens < 10_000 {
		return fmt.Sprintf("%.1fk", float64(tokens)/1000)
	}
	return fmt.Sprintf("%dk", tokens/1000)
}
