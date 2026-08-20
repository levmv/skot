package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/levmv/skot/agent"
)

func (m *screenModel) startTurn(input string) tea.Cmd {
	m.transcript.startAttempt("")
	events := make(chan tea.Msg, 256)
	turnCtx, cancel := context.WithCancel(m.ctx)
	m.operation = activeOperation{kind: operationTurn, startedAt: time.Now(), cancel: cancel, events: events}
	return tea.Batch(runAgentTurnCmd(turnCtx, m.agent, input, events), waitAgentMsg(events), scheduleTurnTick())
}

func runAgentTurnCmd(ctx context.Context, runtime ConversationAgent, input string, events chan<- tea.Msg) tea.Cmd {
	return func() tea.Msg {
		_, err := runtime.Run(ctx, input, func(event agent.Event) {
			// Keep attempt-reset and final events ordered even after cancellation;
			// otherwise partial streamed output can survive an interrupted turn.
			events <- agentEventMsg{event: event}
		})
		events <- agentDoneMsg{err: err}
		return nil
	}
}

func waitAgentMsg(events <-chan tea.Msg) tea.Cmd {
	if events == nil {
		return nil
	}
	return func() tea.Msg { return <-events }
}

func (m *screenModel) scheduleTranscriptRender() tea.Cmd {
	if m.operation.renderPending {
		return nil
	}
	m.operation.renderPending = true
	ctx := m.ctx
	return func() tea.Msg {
		timer := time.NewTimer(transcriptFrame)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return transcriptRenderMsg{}
		}
	}
}

func scheduleTurnTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return turnTickMsg{} })
}

func (m *screenModel) applyAgentEvent(event agent.Event) {
	switch event.Kind {
	case agent.EventModelAttemptStarted:
		m.transcript.startAttempt(event.AttemptID)
	case agent.EventTextDelta:
		m.transcript.appendAssistant(event.AttemptID, event.Text)
	case agent.EventModelAttemptDiscarded:
		m.operation.modelRetry.pendingPartialRemoved = m.transcript.discardAttempt(event.AttemptID)
		m.operation.modelRetry.pendingFailure = strings.TrimSpace(event.Text)
	case agent.EventModelRetryScheduled:
		m.showModelRetry(event.Text)
	case agent.EventToolStarted:
		m.finishModelRetryNotice()
		if event.Call != nil {
			m.addToolCallAt(*event.Call, time.Now())
		}
	case agent.EventToolFinished:
		if event.Result != nil {
			m.finishTool(*event.Result)
		}
	case agent.EventToolRejected:
		m.finishModelRetryNotice()
		if event.Call != nil && event.Result != nil {
			m.addToolCallAt(*event.Call, time.Now())
			m.finishTool(*event.Result)
		}
	case agent.EventQueuedInputDelivered:
		m.finishModelRetryNotice()
		m.addBlock(screenBlockUser, event.Text)
	case agent.EventBoundaryDelivered, agent.EventContextCompacted, agent.EventToolResultsPruned:
		m.finishModelRetryNotice()
		m.addBlock(screenBlockSystem, event.Text)
	case agent.EventStatus:
		m.addBlock(screenBlockSystem, event.Text)
	case agent.EventRunFinished:
		if event.Status == agent.RunCompleted || event.Status == agent.RunIncomplete {
			m.finishModelRetryNotice()
		} else {
			m.removeModelRetryNotice()
		}
		if event.ToolLimitReached {
			m.addBlock(screenBlockSystem, "tool iteration limit reached; final answer uses completed work only")
		}
		m.transcript.finishAssistant(event.Text)
	}
}

// showModelRetry keeps retry progress compact. Repeated failures update one
// status block, which becomes a short recovery note after a successful response.
func (m *screenModel) showModelRetry(retryText string) {
	retry := &m.operation.modelRetry
	retryText = strings.TrimSpace(retryText)
	if retryText == "" {
		retryText = "retrying model request"
	}
	retry.count++
	retry.lastFailure = compactModelFailure(retry.pendingFailure)
	retry.partialRemoved = retry.partialRemoved || retry.pendingPartialRemoved
	if retry.count > 1 {
		retryText += fmt.Sprintf(" after %d model errors", retry.count)
	}
	text := retryText
	if retry.lastFailure != "" {
		text += ": " + retry.lastFailure
	}
	if retry.pendingPartialRemoved {
		text += " (partial response removed)"
	}
	text = sanitizeTerminalText(text)

	index := retry.blockIndex
	if retry.visible && index >= 0 && index < len(m.transcript.blocks) && m.transcript.blocks[index].kind == screenBlockSystem {
		m.transcript.markBlockDirty(index)
		m.transcript.blocks[index].text = text
	} else {
		m.addBlock(screenBlockSystem, text)
		retry.blockIndex = len(m.transcript.blocks) - 1
		retry.visible = true
	}
	retry.pendingFailure = ""
	retry.pendingPartialRemoved = false
}

func compactModelFailure(text string) string {
	const maxWidth = 160
	text = strings.Join(strings.Fields(sanitizeTerminalText(text)), " ")
	if visibleLen(text) <= maxWidth {
		return text
	}
	return truncateANSI(text, maxWidth-1) + "…"
}

func (m *screenModel) finishModelRetryNotice() {
	retry := &m.operation.modelRetry
	index := retry.blockIndex
	if !retry.visible || index < 0 || index >= len(m.transcript.blocks) || m.transcript.blocks[index].kind != screenBlockSystem {
		m.resetModelRetryGroup()
		return
	}
	text := fmt.Sprintf("model recovered after %d retries", retry.count)
	if retry.count == 1 {
		text = "model recovered after 1 retry"
	}
	if retry.lastFailure != "" {
		text += ": " + retry.lastFailure
	}
	if retry.partialRemoved {
		text += " (partial response removed)"
	}
	m.transcript.markBlockDirty(index)
	m.transcript.blocks[index].text = sanitizeTerminalText(text)
	m.resetModelRetryGroup()
}

func (m *screenModel) removeModelRetryNotice() {
	retry := &m.operation.modelRetry
	index := retry.blockIndex
	if retry.visible && index >= 0 && index < len(m.transcript.blocks) && m.transcript.blocks[index].kind == screenBlockSystem {
		m.transcript.markBlockDirty(index)
		m.transcript.blocks = append(m.transcript.blocks[:index], m.transcript.blocks[index+1:]...)
	}
	m.resetModelRetryGroup()
}

func (m *screenModel) resetModelRetryGroup() {
	retry := &m.operation.modelRetry
	retry.blockIndex = 0
	retry.visible = false
	retry.count = 0
	retry.lastFailure = ""
	retry.partialRemoved = false
}

func (transcript *transcriptState) startAttempt(attemptID string) {
	transcript.currentAttempt = attemptID
}

func (transcript *transcriptState) appendAssistant(attemptID, delta string) {
	delta = sanitizeTerminalText(delta)
	if delta == "" {
		return
	}
	for index := len(transcript.blocks) - 1; index >= 0; index-- {
		block := &transcript.blocks[index]
		if block.kind == screenBlockAssistant && block.attemptID == attemptID {
			transcript.markBlockDirty(index)
			block.text += delta
			return
		}
	}
	transcript.appendBlock(screenBlock{kind: screenBlockAssistant, text: delta, attemptID: attemptID})
}

func (transcript *transcriptState) finishAssistant(text string) {
	text = sanitizeTerminalText(text)
	if strings.TrimSpace(text) == "" {
		return
	}
	for index := len(transcript.blocks) - 1; index >= 0; index-- {
		block := &transcript.blocks[index]
		if block.kind == screenBlockAssistant && block.attemptID == transcript.currentAttempt {
			if block.text != text {
				transcript.markBlockDirty(index)
				block.text = text
			}
			return
		}
	}
	transcript.appendBlock(screenBlock{kind: screenBlockAssistant, text: text, attemptID: transcript.currentAttempt})
}

func (transcript *transcriptState) discardAttempt(attemptID string) bool {
	for index := len(transcript.blocks) - 1; index >= 0; index-- {
		block := transcript.blocks[index]
		if block.kind != screenBlockAssistant || block.attemptID != attemptID {
			continue
		}
		transcript.markBlockDirty(index)
		transcript.blocks = append(transcript.blocks[:index], transcript.blocks[index+1:]...)
		return true
	}
	return false
}
