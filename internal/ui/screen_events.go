package ui

import (
	"context"
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
		result, err := runtime.Run(ctx, input, func(event agent.Event) {
			// Keep attempt-reset and final events ordered even after cancellation;
			// otherwise partial streamed output can survive an interrupted turn.
			events <- agentEventMsg{event: event}
		})
		events <- agentDoneMsg{result: result, err: err}
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
		m.transcript.discardAttempt(event.AttemptID)
		m.addBlock(screenBlockSystem, "interrupted response removed")
	case agent.EventModelRetryScheduled:
		text := event.Text
		if text == "" {
			text = "retrying model request"
		}
		m.addBlock(screenBlockSystem, text)
	case agent.EventToolStarted:
		if event.Call != nil {
			m.addToolCallAt(*event.Call, time.Now())
		}
	case agent.EventToolFinished:
		if event.Result != nil {
			m.finishTool(*event.Result)
		}
	case agent.EventToolRejected:
		if event.Call != nil && event.Result != nil {
			m.addToolCallAt(*event.Call, time.Now())
			m.finishTool(*event.Result)
		}
	case agent.EventQueuedInputDelivered:
		m.addBlock(screenBlockUser, event.Text)
	case agent.EventStatus, agent.EventBoundaryDelivered, agent.EventContextCompacted, agent.EventToolResultsPruned:
		m.addBlock(screenBlockSystem, event.Text)
	case agent.EventRunFinished:
		if event.ToolLimitReached {
			m.addBlock(screenBlockSystem, "tool iteration limit reached; final answer uses completed work only")
		}
		m.transcript.finishAssistant(event.Text)
	}
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

func (transcript *transcriptState) discardAttempt(attemptID string) {
	for index := len(transcript.blocks) - 1; index >= 0; index-- {
		block := transcript.blocks[index]
		if block.kind != screenBlockAssistant || block.attemptID != attemptID {
			continue
		}
		transcript.markBlockDirty(index)
		transcript.blocks = append(transcript.blocks[:index], transcript.blocks[index+1:]...)
		return
	}
}
