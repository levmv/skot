package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/levmv/skot/app"
)

func (fake *fakeAgent) BeginBrowserLogin(context.Context, string) (app.BrowserLogin, error) {
	if fake.browserLogin == nil {
		return nil, errors.New("browser login unavailable")
	}
	return fake.browserLogin, fake.loginErr
}

type fakeBrowserLogin struct {
	submitted  string
	waitErr    error
	completed  bool
	closed     bool
	onComplete func()
}

func (*fakeBrowserLogin) AuthorizationURL() string {
	return "https://auth.openai.com/oauth/authorize?state=test-state"
}
func (login *fakeBrowserLogin) SubmitRedirect(value string) error {
	login.submitted = value
	return nil
}
func (login *fakeBrowserLogin) Wait() error { return login.waitErr }
func (login *fakeBrowserLogin) Close()      { login.closed = true }
func (login *fakeBrowserLogin) Complete(context.Context) error {
	if login.closed {
		return context.Canceled
	}
	login.completed = true
	if login.onComplete != nil {
		login.onComplete()
	}
	return nil
}

func browserLoginScreen(t *testing.T) (screenModel, *fakeAgent, *fakeBrowserLogin) {
	t.Helper()
	fake := &fakeAgent{
		model: "deepseek/model", knownModels: []string{"openai-codex/gpt-6-astra"},
		providers: []ProviderStatus{
			{Name: "deepseek", Source: "auth store"},
			{Name: "openai-codex", Source: "none", BrowserLogin: true},
		},
	}
	login := &fakeBrowserLogin{onComplete: func() { fake.providers[1].Source = "auth store" }}
	fake.browserLogin = login
	return testScreenModel(t, fake), fake, login
}

func TestBrowserLoginURLKeepsItsTargetAcrossWrappedLines(t *testing.T) {
	model, _, login := browserLoginScreen(t)
	model.resize(30, 24)
	model.composer.setValue("/login openai-codex")
	model, _ = model.submitInput()
	t.Cleanup(model.cancelLogin)

	var visibleURL strings.Builder
	linkedRows := 0
	for _, line := range model.transcript.lines {
		if !strings.Contains(line, "\x1b]8;") {
			continue
		}
		if !strings.Contains(line, login.AuthorizationURL()) {
			t.Fatalf("wrapped link does not point to the full login URL: %q", line)
		}
		linkedRows++
		visibleURL.WriteString(strings.TrimSpace(ansi.Strip(line)))
	}
	if linkedRows < 2 || visibleURL.String() != login.AuthorizationURL() {
		t.Fatal("wrapped link lost part of the visible login URL")
	}
}

func TestBrowserLoginSwitchesModelOnlyAfterSavingSuccessfulLogin(t *testing.T) {
	model, fake, login := browserLoginScreen(t)
	model.composer.setValue("/model openai-codex/gpt-6-astra")
	model, command := model.submitInput()
	if command == nil || model.browserLogin != login || fake.model != "deepseek/model" || login.completed {
		t.Fatal("model selection did not start a staged browser login")
	}
	const redirect = "http://localhost:1455/auth/callback?code=private-authorization-code&state=test-state"
	model.secret.SetValue(redirect)
	if strings.Contains(strings.Join(model.markedEditorLines(), "\n"), "private-authorization-code") {
		t.Fatal("authorization code rendered in clear text")
	}
	model, _ = model.submitInput()
	if login.submitted != redirect || fake.loginToken != "" {
		t.Fatal("callback was not sent to browser login")
	}
	model, save := model.update(command())
	if save == nil || login.completed || fake.model != "deepseek/model" {
		t.Fatal("browser result was not staged for asynchronous storage")
	}
	model, _ = model.update(tea.PasteMsg{Content: redirect})
	model, _ = model.update(save())
	if !login.completed || !login.closed || fake.model != "openai-codex/gpt-6-astra" || model.loginProvider != "" {
		t.Fatal("successful login did not save and switch to the selected model")
	}
	for _, block := range model.transcript.blocks {
		if strings.Contains(block.text, "private-authorization-code") {
			t.Fatal("callback leaked into transcript")
		}
	}
	if model.composer.value() != "" || strings.Contains(strings.Join(model.composer.history, "\n"), "private-authorization-code") {
		t.Fatal("callback leaked into input history")
	}
}

func TestCancelledBrowserLoginCannotSaveAfterAnotherLoginStarts(t *testing.T) {
	model, fake, first := browserLoginScreen(t)
	model.openModelPicker()
	command := model.selectModel(modelSelection{uri: "openai-codex/gpt-6-astra"}, model.picker)
	model.closePicker()
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	if !first.closed || first.completed || fake.model != "deepseek/model" || model.picker.kind != pickerModel {
		t.Fatal("cancellation did not restore the model picker without changing credentials")
	}
	second := &fakeBrowserLogin{}
	fake.browserLogin = second
	model.closePicker()
	_ = model.startProviderLogin("openai-codex", modelSelection{}, pickerState{})
	model, _ = model.update(command())
	if first.completed || second.completed || model.browserLogin != second || fake.model != "deepseek/model" {
		t.Fatal("an abandoned completion interfered with the new login")
	}
	model.cancelLogin()
}

func TestBrowserLoginCanBeCancelledWhileSavingCredentials(t *testing.T) {
	model, fake, login := browserLoginScreen(t)
	command := model.selectModel(modelSelection{uri: "openai-codex/gpt-6-astra"}, pickerState{})
	model, save := model.update(command())
	if save == nil || !model.maintenanceOperation().isMaintenance() {
		t.Fatal("credential storage did not keep a cancellable operation")
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	model, _ = model.update(save())
	if login.completed || !login.closed || fake.model != "deepseek/model" || model.maintenanceOperation().isMaintenance() {
		t.Fatal("cancelled storage saved credentials or changed the model")
	}
}

func TestFailedBrowserLoginPreservesCredentialsAndModel(t *testing.T) {
	model, fake, login := browserLoginScreen(t)
	fake.providers[1].Source = "auth store"
	login.waitErr = errors.New("authorization declined")
	model.composer.setValue("/login openai-codex")
	model, command := model.submitInput()
	if command == nil {
		t.Fatal("explicit login did not allow reauthorization")
	}
	model, _ = model.update(command())
	if login.completed || !login.closed || fake.providers[1].Source != "auth store" || fake.model != "deepseek/model" || model.loginProvider != "" {
		t.Fatal("failed reauthorization changed credentials or model")
	}
}
