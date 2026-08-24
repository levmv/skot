package ui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/levmv/skot/agent"
)

func (m *screenModel) startShell(command string, private bool) tea.Cmd {
	operationCtx, cancel := context.WithCancel(m.ctx)
	m.operation = activeOperation{kind: operationShell, startedAt: time.Now(), cancel: cancel}
	m.transcript.appendBlock(screenBlock{
		kind: screenBlockTool,
		text: sanitizeTerminalText(shellCommandDisplay(command, private)),
		tool: &toolBlock{
			name: "bash", startedAt: m.operation.startedAt, shell: &shellMeta{private: private},
		},
	})
	return tea.Batch(runShellCmd(operationCtx, m.agent, command, private), scheduleTurnTick())
}

func runShellCmd(ctx context.Context, runtime ShellAgent, command string, private bool) tea.Cmd {
	return func() tea.Msg {
		var result agent.ToolResult
		var err error
		if private {
			result, err = runtime.RunPrivateShell(ctx, command)
		} else {
			result, err = runtime.RunShell(ctx, command)
		}
		return shellDoneMsg{result: result, err: err}
	}
}

func (m *screenModel) finishShell(result agent.ToolResult, runErr error, finishedAt time.Time) {
	m.operation.clear()
	m.transcript.finishShell(result, runErr, finishedAt)
}

func (transcript *transcriptState) finishShell(result agent.ToolResult, runErr error, finishedAt time.Time) {
	for index := range slices.Backward(transcript.blocks) {
		block := &transcript.blocks[index]
		if block.kind != screenBlockTool || block.tool == nil || block.tool.shell == nil || block.tool.done {
			continue
		}
		tool := block.tool
		transcript.markBlockDirty(index)
		tool.done = true
		tool.elapsed = max(time.Duration(0), finishedAt.Sub(tool.startedAt))
		tool.failed = runErr != nil && !errors.Is(runErr, context.Canceled)
		tool.output = processOutputFromContent(result.Content.Text())
		for _, detail := range result.Details {
			if process, ok := agent.ProcessResultFromDetail(detail); ok {
				tool.process = &process
				tool.failed = process.Status != agent.ProcessCompleted
			}
		}
		if tool.process == nil && strings.TrimSpace(result.Content.Text()) != "" {
			tool.output = sanitizeTerminalText(result.Content.Text())
		}
		if runErr != nil && tool.process == nil {
			block.text += ": " + compactSingleLine(runErr.Error(), 180)
		}
		return
	}
}
