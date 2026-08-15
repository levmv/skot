package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/levmv/skot/agent"
)

func TestClearCommandStartsCleanSessionAndInvalidatesTranscript(t *testing.T) {
	fake := &fakeAgent{model: "deepseek/model", sessionID: "session_old", clearID: "session_new"}
	model := testScreenModel(t, fake)
	model.addBlock(screenBlockUser, "old conversation")
	model.composer.history = []string{"old conversation"}
	model.composer.historyIndex = 1
	model.composer.setValue("/clear")

	model, cmd := model.submitInput()
	if cmd != nil {
		t.Fatal("clear unexpectedly became asynchronous")
	}
	if fake.sessionID != "session_new" || !model.renderer.invalidated {
		t.Fatalf("session=%q invalidated=%v", fake.sessionID, model.renderer.invalidated)
	}
	if len(model.composer.history) != 0 || len(model.transcript.blocks) != 1 || model.transcript.blocks[0].text != "new session new" {
		t.Fatalf("history=%#v blocks=%#v", model.composer.history, model.transcript.blocks)
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
