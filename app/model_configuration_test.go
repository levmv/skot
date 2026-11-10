package app

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
)

func TestConfiguredModelsSurviveSelectionAndSessionChanges(t *testing.T) {
	const upper = "router/Vendor/Model:fast"
	const lower = "router/Vendor/model:fast"
	t.Setenv("SKOT_TEST_ROUTER_KEY", "router-secret")
	t.Setenv("OPENAI_API_KEY", "unrelated-secret")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var wire struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
		}
		if err := json.UnmarshalRead(request.Body, &wire); err != nil {
			t.Error(err)
		}
		if request.URL.Path != "/v1/chat/completions" || request.Header.Get("Authorization") != "Bearer router-secret" ||
			!slices.Contains([]string{"Vendor/Model:fast", "Vendor/model:fast"}, wire.Model) || wire.MaxTokens != 4096 {
			t.Errorf("configured request: %s, model=%q, max=%d", request.URL, wire.Model, wire.MaxTokens)
		}
		writeChildTestAnswer(t, writer, wire.Model)
	}))
	t.Cleanup(server.Close)
	home, root := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"agent_models":["router/Vendor/Model:fast"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeModels := func(window int) {
		t.Helper()
		raw := fmt.Sprintf(`{"providers":{"router":{
			"base_url":%q,"api":"chat_completions","api_key_env":"SKOT_TEST_ROUTER_KEY",
			"models":{
				"Vendor/Model:fast":{"name":"My model","context_window":%d,"max_output_tokens":4096,"reasoning_efforts":["high","max"]},
				"Vendor/model:fast":{"context_window":128000,"max_output_tokens":4096}
			}
		}}}`, server.URL+"/v1", window)
		if err := os.WriteFile(filepath.Join(home, "models.json"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeModels(900_000)
	config := childTestConfig(home, root, "", true)
	config.ModelURI, config.ContextWindow = upper, 0
	config.RetryBudget = time.Second
	application, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	choices := application.ModelChoices()
	for _, uri := range []string{DefaultModelURI, upper, lower} {
		if !slices.ContainsFunc(choices, func(choice ModelChoice) bool { return choice.URI == uri }) {
			t.Fatalf("model %q absent from choices", uri)
		}
	}
	if !slices.ContainsFunc(choices, func(choice ModelChoice) bool {
		return choice.URI == upper && choice.Name == "My model" && slices.Contains(choice.ReasoningEfforts, "max")
	}) {
		t.Fatal("configured display name or reasoning choices were lost")
	}
	run := func(wantModel string, wantWindow int) {
		t.Helper()
		result, err := application.Run(t.Context(), "hello", nil)
		if err != nil || result.Answer != strings.TrimPrefix(wantModel, "router/") {
			t.Fatalf("configured run: %+v, %v", result, err)
		}
		saved, err := application.State(t.Context())
		if err != nil || saved.Configured == nil || saved.Configured.RuntimePolicy.ContextWindow != wantWindow {
			t.Fatalf("configured context window: %+v, %v", saved.Configured, err)
		}
	}
	run(upper, 900_000)
	parentID := application.SessionID()
	if err := application.SwitchModel(t.Context(), lower, "", ""); err != nil {
		t.Fatal(err)
	}
	run(lower, 128_000)
	tool := childTestTool(t, application)
	started := runChildTestTool(t, tool, parentID, childToolArgs{Action: "start", Prompt: "child", Model: upper})
	var child childSnapshot
	for _, detail := range started.Details {
		if detail.Kind == childDetailKind {
			if err := json.Unmarshal(detail.Data, &child); err != nil {
				t.Fatal(err)
			}
		}
	}
	if child.AgentID == "" {
		t.Fatalf("child did not start: %s", started.Content.Text())
	}
	checked := runChildTestTool(t, tool, parentID, childToolArgs{Action: "check", IDs: []string{child.AgentID}, Wait: "all"})
	if !strings.Contains(checked.Content.Text(), "Vendor/Model:fast") {
		t.Fatalf("child: %s", checked.Content.Text())
	}
	if err := application.SwitchModel(t.Context(), upper, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := application.ClearSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	run(upper, 900_000)
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	// A new configuration replaces remembered metadata when reopening the session.
	writeModels(800_000)
	config.Resume, config.ResumePrefix = true, parentID
	application, err = Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	run(upper, 800_000)
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, "models.json")); err != nil {
		t.Fatal(err)
	}
	application, err = Open(t.Context(), config)
	if err != nil {
		t.Fatalf("removed provider must not prevent reading saved sessions: %v", err)
	}
	choices = application.ModelChoices()
	for _, uri := range []string{upper, lower} {
		if !slices.ContainsFunc(choices, func(choice ModelChoice) bool { return choice.URI == uri && choice.Unavailable }) {
			t.Fatalf("saved model %q lost after removing its provider", uri)
		}
	}
}

func TestConfiguredProviderCredentialsStayPrivate(t *testing.T) {
	const environment = "SKOT_TEST_PRIVATE_ROUTER_KEY"
	var expected atomic.Value
	expected.Store("environment-secret")
	t.Setenv(environment, "environment-secret")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+expected.Load().(string) {
			t.Error("provider used the wrong credential")
		}
		writeChildTestAnswer(t, writer, "echo "+expected.Load().(string))
	}))
	t.Cleanup(server.Close)
	config := Config{
		Home: t.TempDir(), Root: t.TempDir(), Scope: ScopeMachine, ScopeExplicit: true,
		ModelURI: "router/test", ModelExplicit: true, ToolSet: ToolSetNone, ToolSetExplicit: true,
		Providers: map[string]ProviderConfig{"router": {
			BaseURL: new(server.URL), API: new("chat_completions"), APIKeyEnv: new(environment),
		}},
	}
	application, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	bashIndex := slices.IndexFunc(application.config.tools, func(tool agent.Tool) bool { return tool.Spec.Name == "bash" })
	if bashIndex < 0 {
		t.Fatal("bash tool is absent")
	}
	output, err := application.config.tools[bashIndex].Run(agent.WithToolSessionID(t.Context(), application.SessionID()), `{"command":"printf '%s' \"${SKOT_TEST_PRIVATE_ROUTER_KEY-absent}\""}`)
	if err != nil || !strings.Contains(output.Content.Text(), "absent") {
		t.Fatalf("model-owned process inherited the API key: %s, %v", output.Content.Text(), err)
	}
	run := func() {
		t.Helper()
		result, err := application.Run(t.Context(), "hello", nil)
		if err != nil || !strings.HasPrefix(result.Answer, "echo ") || strings.Contains(result.Answer, "secret") {
			t.Fatalf("unredacted provider output: %+v, %v", result, err)
		}
	}
	run()
	t.Setenv(environment, "")
	expected.Store("stored-secret")
	if err := application.Login(t.Context(), "router", "stored-secret"); err != nil {
		t.Fatal(err)
	}
	run()
}
