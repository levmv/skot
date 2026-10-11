package ui

import (
	"bytes"
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/levmv/skot/agent"
	modelapi "github.com/levmv/skot/model"
)

func attachProgramStatus(t *testing.T, model *screenModel) *bytes.Buffer {
	t.Helper()
	out := &bytes.Buffer{}
	model.programStatus = &programStatusReporter{out: out}
	if err := model.programStatus.report(model.terminalProgramStatus()); err != nil {
		t.Fatal(err)
	}
	return out
}

func updateProgramStatusScreen(model *screenModel, msg tea.Msg) tea.Cmd {
	next, cmd := model.Update(msg)
	*model = next.(screenModel)
	return cmd
}

func TestProgramStatusFollowsTurnsWithoutFinishingBetweenQueuedInputs(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	out := attachProgramStatus(t, &model)
	model.composer.setValue("first request")
	updateProgramStatusScreen(&model, tea.KeyPressMsg{Code: tea.KeyEnter})
	updateProgramStatusScreen(&model, agentEventMsg{event: agent.Event{Kind: agent.EventTextDelta, Text: "hello"}})
	model.composer.setValue("queued request")
	updateProgramStatusScreen(&model, tea.KeyPressMsg{Code: tea.KeyEnter})
	updateProgramStatusScreen(&model, agentDoneMsg{})
	updateProgramStatusScreen(&model, turnTickMsg{})
	const initial = "\x1b]7501;app=skot:state=idle\x1b\\\x1b]7501;app=skot:state=working\x1b\\"
	if got := out.String(); got != initial {
		t.Fatalf("status while processing the queue = %q", got)
	}
	updateProgramStatusScreen(&model, agentDoneMsg{})
	updateProgramStatusScreen(&model, turnTickMsg{})
	updateProgramStatusScreen(&model, tea.FocusMsg{})
	want := initial + "\x1b]7501;app=skot:state=done\x1b\\"
	if got := out.String(); got != want {
		t.Fatalf("completed status = %q, want %q", got, want)
	}
	updateProgramStatusScreen(&model, tea.PasteMsg{Content: "next request"})
	want += "\x1b]7501;app=skot:state=idle\x1b\\"
	if got := out.String(); got != want {
		t.Fatalf("status after user input = %q, want %q", got, want)
	}
	updateProgramStatusScreen(&model, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	want += "\x1b]7501;app=skot:state=clear\x1b\\"
	if got := out.String(); got != want || !model.quitting {
		t.Fatalf("status on exit = %q, quitting = %t", got, model.quitting)
	}
}

func TestProgramStatusDistinguishesFailureFromCancellation(t *testing.T) {
	for _, test := range []struct {
		name, state string
		err         error
	}{
		{"failure", "error", errors.New("model unavailable")},
		{"cancelled", "idle", context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := testScreenModel(t, &fakeAgent{})
			out := attachProgramStatus(t, &model)
			model.composer.setValue("request")
			updateProgramStatusScreen(&model, tea.KeyPressMsg{Code: tea.KeyEnter})
			out.Reset()
			updateProgramStatusScreen(&model, agentDoneMsg{err: test.err})
			want := "\x1b]7501;app=skot:state=" + test.state + "\x1b\\"
			if got := out.String(); got != want {
				t.Fatalf("status = %q, want %q", got, want)
			}
		})
	}
}

func TestProgramStatusReflectsShellExitStatus(t *testing.T) {
	for _, test := range []struct {
		name, processStatus, state string
		exitCode                   *int
		err                        error
	}{
		{"success", agent.ProcessCompleted, "done", new(0), nil},
		{"nonzero exit", agent.ProcessFailed, "error", new(7), nil},
		{"timeout", agent.ProcessTimedOut, "error", nil, nil},
		{"cancelled", "", "idle", nil, context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := testScreenModel(t, &fakeAgent{})
			out := attachProgramStatus(t, &model)
			model.composer.setValue("! command")
			updateProgramStatusScreen(&model, tea.KeyPressMsg{Code: tea.KeyEnter})
			out.Reset()
			result := modelapi.ToolResult{Error: test.err != nil}
			if test.processStatus != "" {
				result.Details = []modelapi.Detail{processDetailForTest(t, agent.ProcessResult{
					Status: test.processStatus, ExitCode: test.exitCode, UserInitiated: true,
				})}
			}
			updateProgramStatusScreen(&model, shellDoneMsg{result: result, err: test.err})
			want := "\x1b]7501;app=skot:state=" + test.state + "\x1b\\"
			if got := out.String(); got != want {
				t.Fatalf("shell status = %q, want %q", got, want)
			}
		})
	}
}

func TestProgramStatusWaitsForBrowserAuthentication(t *testing.T) {
	model, _, _ := browserLoginScreen(t)
	out := attachProgramStatus(t, &model)
	model.composer.setValue("/login openai-codex")
	wait := updateProgramStatusScreen(&model, tea.KeyPressMsg{Code: tea.KeyEnter})
	if wait == nil {
		t.Fatal("browser login did not start")
	}
	save := updateProgramStatusScreen(&model, wait())
	if save == nil {
		t.Fatal("browser login did not begin saving credentials")
	}
	updateProgramStatusScreen(&model, save())
	want := "\x1b]7501;app=skot:state=idle\x1b\\" +
		"\x1b]7501;app=skot:state=blocked:kind=auth\x1b\\" +
		"\x1b]7501;app=skot:state=working\x1b\\" +
		"\x1b]7501;app=skot:state=idle\x1b\\"
	if got := out.String(); got != want {
		t.Fatalf("login statuses = %q, want %q", got, want)
	}
}

func TestProgramStatusWaitsForCustomModelContext(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{
		model:    "deepseek/model",
		modelErr: &modelapi.ContextWindowRequiredError{URI: "deepseek/custom-model"},
	})
	out := attachProgramStatus(t, &model)
	model.composer.setValue("/model deepseek/custom-model")
	updateProgramStatusScreen(&model, tea.KeyPressMsg{Code: tea.KeyEnter})
	updateProgramStatusScreen(&model, tea.KeyPressMsg{Code: tea.KeyEsc})
	want := "\x1b]7501;app=skot:state=idle\x1b\\" +
		"\x1b]7501;app=skot:state=blocked:kind=question\x1b\\" +
		"\x1b]7501;app=skot:state=idle\x1b\\"
	if got := out.String(); got != want {
		t.Fatalf("model selection statuses = %q, want %q", got, want)
	}
}
