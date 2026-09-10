package ui

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/levmv/skot/agent"
)

func TestClearCommandStartsCleanSessionAndInvalidatesTranscript(t *testing.T) {
	for _, working := range []bool{false, true} {
		name := "idle"
		if working {
			name = "working"
		}
		t.Run(name, func(t *testing.T) {
			fake := &fakeAgent{model: "deepseek/model", sessionID: "session_old", clearID: "session_new", queued: []string{"next"}}
			model := testScreenModel(t, fake)
			model.addBlock(screenBlockUser, "old conversation")
			model.composer.history = []string{"old conversation"}
			model.composer.historyIndex = 1
			model.composer.setValue("/clear")
			turnCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if working {
				model.operation = activeOperation{kind: operationTurn, cancel: cancel}
			}

			model, cmd := model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
			if working {
				if cmd != nil || turnCtx.Err() == nil || fake.sessionID != "session_old" {
					t.Fatal("clear did not cancel and wait for the old turn")
				}
				model, cmd = model.update(agentDoneMsg{err: context.Canceled})
			}
			if cmd == nil || fake.sessionID != "session_old" {
				t.Fatal("clear did not defer session replacement")
			}
			model, cmd = model.update(cmd())
			if cmd != nil || model.operation.kind != operationNone || len(fake.queued) != 0 {
				t.Fatalf("clear resumed old work: command=%v operation=%#v queue=%q", cmd, model.operation, fake.queued)
			}
			if fake.sessionID != "session_new" || !model.renderer.invalidated || model.quitting {
				t.Fatalf("session=%q invalidated=%v quitting=%v", fake.sessionID, model.renderer.invalidated, model.quitting)
			}
			if len(model.composer.history) != 0 || len(model.transcript.blocks) != 1 || model.transcript.blocks[0].text != "new session new" {
				t.Fatalf("history=%#v blocks=%#v", model.composer.history, model.transcript.blocks)
			}
			model, _ = model.update(tea.PasteMsg{Content: "new task"})
			if model.composer.value() != "new task" {
				t.Fatal("new session did not accept input")
			}
		})
	}
}

type blockingClearAgent struct {
	Agent
	mu      sync.RWMutex
	started chan struct{}
	release chan struct{}
}

func (client *blockingClearAgent) ClearSession(context.Context) (string, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	close(client.started)
	<-client.release
	return "session_new", nil
}

func (client *blockingClearAgent) CurrentModel() string {
	client.mu.RLock()
	defer client.mu.RUnlock()
	return client.Agent.CurrentModel()
}

func (client *blockingClearAgent) SessionStatus() agent.SessionStatus {
	client.mu.RLock()
	defer client.mu.RUnlock()
	return client.Agent.SessionStatus()
}

func TestControlCForcesExitDuringSessionReplacement(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{sessionID: "session_old"})
	client := &blockingClearAgent{Agent: model.agent, started: make(chan struct{}), release: make(chan struct{})}
	model.agent = client
	model.composer.setValue("/clear")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(io.Discard),
		tea.WithoutRenderer(), tea.WithoutSignalHandler())
	defer func() {
		close(client.release)
		program.Kill()
	}()
	type result struct {
		model tea.Model
		err   error
	}
	done := make(chan result, 1)
	go func() {
		final, err := program.Run()
		done <- result{model: final, err: err}
	}()
	go func() {
		program.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
		select {
		case <-client.started:
		case <-ctx.Done():
			return
		}
		program.Send(turnTickMsg{})
		program.Send(tea.WindowSizeMsg{Width: 60, Height: 24})
		program.Send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	}()
	select {
	case final := <-done:
		if final.err != nil {
			t.Fatal(final.err)
		}
		if !errors.Is(final.model.(screenModel).exitErr, context.Canceled) {
			t.Fatal("Ctrl+C did not interrupt session replacement")
		}
	case <-ctx.Done():
		t.Fatal("Ctrl+C kept waiting for session replacement")
	}
}

func TestResumePickerDefersSwitchAndLoadsSelectedHistoryBelowScrollback(t *testing.T) {
	now := time.Now()
	fake := &fakeAgent{
		model: "deepseek/current", sessionID: "session_current", resumeID: "session_resumed",
		resumeNotices: []string{"durable job job-corrupt is unobservable and was left untouched"},
		sessions: []SessionSummary{
			{ID: "session_current", Title: "Current work", UpdatedAt: now},
			{ID: "session_resumed", Title: "Continue useful work", UpdatedAt: now.Add(-4 * time.Minute)},
		},
	}
	model := testScreenModel(t, fake)
	model.composer.setValue("/resume")
	model, cmd := model.submitInput()
	if cmd != nil || model.picker.kind != pickerSession || len(model.picker.items) != 1 {
		t.Fatalf("resume picker = %#v, cmd=%v", model.picker, cmd)
	}
	if model.picker.items[0].label != "Continue useful work" || model.picker.items[0].description != "4m ago" {
		t.Fatalf("picker item = %#v", model.picker.items[0])
	}

	model, cmd = model.handleKey(tea.KeyPressMsg{Text: "1", Code: '1', BaseCode: '1'})
	if cmd == nil || fake.resumeArg != "" {
		t.Fatalf("session switched before picker frame: arg=%q cmd=%v", fake.resumeArg, cmd)
	}
	fake.state = agent.State{Items: []agent.Item{
		{Kind: agent.ItemUserText, Text: "restored prompt"},
		{Kind: agent.ItemAssistantText, Text: "restored answer"},
	}}
	message := cmd()
	model, _ = model.update(message)
	if fake.resumeArg != "session_resumed" || !model.renderer.appendPending {
		t.Fatalf("resume arg=%q appendPending=%v", fake.resumeArg, model.renderer.appendPending)
	}
	texts := make([]string, 0, len(model.transcript.blocks))
	for _, block := range model.transcript.blocks {
		texts = append(texts, block.text)
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "resumed session resumed") || !strings.Contains(joined, "resume warning: durable job job-corrupt is unobservable") || !strings.Contains(joined, "restored prompt") || !strings.Contains(joined, "restored answer") {
		t.Fatalf("resumed transcript = %q", joined)
	}
	if len(model.composer.history) != 1 || model.composer.history[0] != "restored prompt" {
		t.Fatalf("resumed history = %#v", model.composer.history)
	}
}

func TestDirectResumeReportsFailureWithoutReplacingTranscript(t *testing.T) {
	fake := &fakeAgent{model: "deepseek/model", resumeErr: context.Canceled}
	model := testScreenModel(t, fake)
	before := len(model.transcript.blocks)
	model.composer.setValue("/resume missing")
	model, cmd := model.submitInput()
	if cmd == nil {
		t.Fatal("direct resume did not schedule a switch")
	}
	model, _ = model.update(cmd())
	if model.renderer.appendPending || len(model.transcript.blocks) != before+1 || !strings.Contains(model.transcript.blocks[len(model.transcript.blocks)-1].text, "context canceled") {
		t.Fatalf("failed resume state: append=%v blocks=%#v", model.renderer.appendPending, model.transcript.blocks)
	}
}
