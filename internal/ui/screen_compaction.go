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
		_, err := m.agent.Compact(operationCtx)
		return compactionDoneMsg{err: err}
	}
}

func (m *screenModel) finishCompaction(message compactionDoneMsg) {
	m.operation.clear()
	m.refreshSessionStatus()
	if message.err != nil {
		m.addBlock(screenBlockError, "compact: "+message.err.Error())
		return
	}
	m.addBlock(screenBlockSystem, "context compacted\n"+formatContextReport(m.sessionStatus.ContextReport))
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
