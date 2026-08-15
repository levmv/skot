package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/session"
	"github.com/levmv/skot/internal/state"
	"github.com/levmv/skot/internal/toolpolicy"
	workspacetools "github.com/levmv/skot/tools"
)

func TestMain(m *testing.M) {
	if workspacetools.RunSandboxChildIfRequested() {
		return
	}
	if workspacetools.RunJobWorkerIfRequested() {
		return
	}
	cache, err := os.MkdirTemp("", "skot-main-test-cache-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("XDG_CACHE_HOME", cache)
	code := m.Run()
	_ = os.RemoveAll(cache)
	os.Exit(code)
}

func TestHelpListsAnthropicMessagesAsImplemented(t *testing.T) {
	var stderr bytes.Buffer
	err := run(context.Background(), []string{"-help"}, bytes.NewReader(nil), io.Discard, &stderr)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(stderr.String(), "implemented: chat_completions, responses, anthropic_messages") {
		t.Fatalf("help = %q", stderr.String())
	}
}

func TestVerboseEmitterReportsDurableStatusEvents(t *testing.T) {
	var output bytes.Buffer
	emit := verboseEmitter(true, &output)
	for _, event := range []agent.Event{
		{Kind: agent.EventBoundaryDelivered, Text: "background work completed", Sequence: 4},
		{Kind: agent.EventContextCompacted, Text: "context compacted", Sequence: 5},
		{Kind: agent.EventToolResultsPruned, Text: "pruned old tool results", Sequence: 6},
		{Kind: agent.EventToolRejected, Call: &agent.ToolCall{Name: "read"}, Result: &agent.ToolResult{Content: "iteration limit", Error: true}, Sequence: 7},
		{Kind: agent.EventRunFinished, Status: agent.RunCompleted, ToolLimitReached: true, Sequence: 8},
	} {
		emit(event)
	}
	for _, want := range []string{"sk: background work completed\n", "sk: context compacted\n", "sk: pruned old tool results\n", "sk: tool read rejected: iteration limit\n", "sk: completed (tool iteration limit reached)\n"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("verbose output %q does not contain %q", output.String(), want)
		}
	}
}

func TestRunVerbosePrintsRunUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(writer, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":5,\"total_tokens\":17,\"prompt_tokens_details\":{\"cached_tokens\":4},\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("DEEPSEEK_API_KEY", "secret")
	clearMainWebCredentials(t)
	home, root := t.TempDir(), t.TempDir()
	journal := filepath.Join(t.TempDir(), "session.jsonl")
	for _, prompt := range []string{"first", "second"} {
		var stdout, stderr bytes.Buffer
		if err := run(context.Background(), []string{
			"-model", "deepseek/test-model", "-base-url", server.URL,
			"-home", home, "-root", root, "-journal", journal, "-v", prompt,
		}, bytes.NewReader(nil), &stdout, &stderr); err != nil {
			t.Fatal(err)
		}
		if stdout.String() != "done\n" {
			t.Fatalf("%s stdout = %q", prompt, stdout.String())
		}
		// The reasoning share is part of completion, not an extra cost; a route
		// which reports it must carry it through to the run summary.
		if !strings.Contains(stderr.String(), "[usage: prompt=12 cached=4 completion=5 reasoning=3 total=17]\n") {
			t.Fatalf("%s stderr = %q", prompt, stderr.String())
		}
	}
}

func TestRunJSONWritesOneVersionedResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(writer, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":5,\"total_tokens\":17,\"prompt_tokens_details\":{\"cached_tokens\":4},\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()
	clearMainWebCredentials(t)

	var stdout bytes.Buffer
	err := run(context.Background(), []string{
		"-model", "deepseek/test-model", "-base-url", server.URL,
		"-home", t.TempDir(), "-root", t.TempDir(), "-save-session", "-json", "task",
	}, bytes.NewReader(nil), &stdout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"tool_set":"default"`)) || bytes.Contains(stdout.Bytes(), []byte(`"profile"`)) {
		t.Fatalf("JSON result uses an unexpected tool set field: %s", stdout.String())
	}
	decoder := json.NewDecoder(&stdout)
	var result jsonResult
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("decode JSON result: %v; output=%q", err, stdout.String())
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("JSON output contains more than one value: %v", err)
	}
	if result.Version != jsonResultVersion || result.Reply != "done" || result.Status != agent.RunCompleted ||
		result.RunID == "" || result.SessionID == "" || result.Error != "" || result.DurationMillis < 0 ||
		result.Model != "deepseek/test-model" || result.ReasoningEffort != "default" ||
		result.ToolSet != toolpolicy.ToolSetDefault || result.ModelAttempts != 1 {
		t.Fatalf("JSON result = %#v", result)
	}
	if result.Usage.InputTokens != 12 || result.Usage.CachedInputTokens != 4 ||
		result.Usage.OutputTokens != 5 || result.Usage.ReasoningTokens != 3 || result.Usage.TotalTokens != 17 {
		t.Fatalf("JSON usage = %#v", result.Usage)
	}
}

func TestRunJSONOmitsEphemeralSessionAndReportsIncompleteRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	clearMainWebCredentials(t)

	var stdout bytes.Buffer
	runErr := run(context.Background(), []string{
		"-model", "deepseek/test-model", "-base-url", server.URL,
		"-home", t.TempDir(), "-root", t.TempDir(), "-json", "task",
	}, bytes.NewReader(nil), &stdout, io.Discard)
	if !errors.Is(runErr, agent.ErrRunIncomplete) {
		t.Fatalf("run error = %v", runErr)
	}
	var result jsonResult
	if err := json.NewDecoder(&stdout).Decode(&result); err != nil {
		t.Fatalf("decode JSON result: %v; output=%q", err, stdout.String())
	}
	if result.Reply != "partial" || result.Status != agent.RunIncomplete || result.RunID == "" ||
		result.SessionID != "" || result.Model != "deepseek/test-model" || result.ModelAttempts != 1 ||
		!strings.Contains(result.Error, "stop reason length") {
		t.Fatalf("JSON result = %#v", result)
	}
}

func TestRunVersionDoesNotOpenApplication(t *testing.T) {
	t.Setenv("SK_HOME", "")
	t.Setenv("HOME", "")
	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"-version", "-retry-budget", "invalid"}, bytes.NewReader(nil), &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "sk dev\n" {
		t.Fatalf("version output = %q", stdout.String())
	}
}

func TestRunOneShotAndResumeJournal(t *testing.T) {
	t.Setenv("SK_HOME", t.TempDir())
	var requests []chatRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body chatRequestForTest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests = append(requests, body)
		answer := fmt.Sprintf("answer %d", len(requests))
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\n", answer)
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("DEEPSEEK_API_KEY", "secret")
	journal := filepath.Join(t.TempDir(), "session.jsonl")

	var first bytes.Buffer
	err := run(context.Background(), []string{
		"-model", "deepseek/test-model",
		"-base-url", server.URL,
		"-journal", journal,
		"hello",
	}, bytes.NewReader(nil), &first, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if first.String() != "answer 1\n" {
		t.Fatalf("first output = %q", first.String())
	}

	var second bytes.Buffer
	err = run(context.Background(), []string{
		"-model", "deepseek/test-model",
		"-base-url", server.URL,
		"-journal", journal,
		"continue",
	}, bytes.NewReader(nil), &second, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if second.String() != "answer 2\n" {
		t.Fatalf("second output = %q", second.String())
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	got := conversationMessages(requests[1].Messages)
	if len(got) != 3 || got[0].Role != "user" || got[0].Content != "hello" ||
		got[1].Role != "assistant" || got[1].Content != "answer 1" ||
		got[2].Role != "user" || got[2].Content != "continue" {
		t.Fatalf("resumed messages = %#v", got)
	}
}

func TestRunOneShotPrintsPartialAnswerAndReturnsIncomplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("DEEPSEEK_API_KEY", "secret")
	var output bytes.Buffer
	err := run(context.Background(), []string{
		"-model", "deepseek/test-model",
		"-base-url", server.URL,
		"-home", t.TempDir(),
		"-root", t.TempDir(),
		"partial task",
	}, bytes.NewReader(nil), &output, io.Discard)
	if !errors.Is(err, agent.ErrRunIncomplete) {
		t.Fatalf("error = %v", err)
	}
	if output.String() != "partial\n" {
		t.Fatalf("partial output = %q", output.String())
	}
}

func TestRunClassifiesInvalidConfigurationAndProviderFailure(t *testing.T) {
	t.Run("configuration", func(t *testing.T) {
		err := run(context.Background(), []string{
			"-home", t.TempDir(), "-root", t.TempDir(), "-tools", "admin", "task",
		}, bytes.NewReader(nil), io.Discard, io.Discard)
		if !errors.Is(err, agent.ErrInvalidRequest) || exitCodeFor(err) != exitConfig {
			t.Fatalf("configuration error/code = %v/%d", err, exitCodeFor(err))
		}
	})

	t.Run("provider", func(t *testing.T) {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			requests++
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(writer, `{"error":{"message":"try later"}}`)
		}))
		defer server.Close()
		err := run(context.Background(), []string{
			"-model", "deepseek/test-model", "-base-url", server.URL,
			"-home", t.TempDir(), "-root", t.TempDir(), "-retry-budget", "50ms", "task",
		}, bytes.NewReader(nil), io.Discard, io.Discard)
		if !errors.Is(err, agent.ErrProviderFailure) || exitCodeFor(err) != exitProvider {
			t.Fatalf("provider error/code = %v/%d", err, exitCodeFor(err))
		}
		if requests != 1 {
			t.Fatalf("provider attempts = %d, want 1", requests)
		}
	})
}

func TestRunRejectsUnknownModelAPI(t *testing.T) {
	err := run(context.Background(), []string{
		"-model", "deepseek/deepseek-v4-flash", "-model-api", "future",
		"-base-url", "http://127.0.0.1:1", "-home", t.TempDir(), "-root", t.TempDir(), "task",
	}, bytes.NewReader(nil), io.Discard, io.Discard)
	if !errors.Is(err, agent.ErrInvalidRequest) || !strings.Contains(err.Error(), `unsupported model API "future"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestRunUsesAnthropicAdapterWithoutAProactiveCompatibilityWarning(t *testing.T) {
	var received map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/messages" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"-model", "deepseek/deepseek-v4-flash", "-model-api", "anthropic_messages",
		"-base-url", server.URL,
		"-sandbox", "off", "-home", t.TempDir(), "-root", t.TempDir(), "task",
	}, bytes.NewReader(nil), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "done\n" || string(received["max_tokens"]) != "65536" || string(received["stream"]) != "true" {
		t.Fatalf("stdout/request = %q / %#v", stdout.String(), received)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunUsesResponsesAdapterWithoutAProactiveCompatibilityWarning(t *testing.T) {
	var received map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/responses" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}]}}\n\n")
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"-model", "deepseek/deepseek-v4-flash", "-model-api", "responses",
		"-base-url", server.URL, "-sandbox", "off", "-home", t.TempDir(), "-root", t.TempDir(), "task",
	}, bytes.NewReader(nil), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "done\n" || string(received["store"]) != "false" || string(received["stream"]) != "true" {
		t.Fatalf("stdout/request = %q / %#v", stdout.String(), received)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunReadsToolsFileFromEnvironmentBeforeModelRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tools.json")
	if err := os.WriteFile(path, []byte(`{"tools":[],"unexpected":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SK_TOOLS_FILE", path)
	err := run(context.Background(), []string{
		"-home", t.TempDir(), "-root", t.TempDir(), "task",
	}, bytes.NewReader(nil), io.Discard, io.Discard)
	if !errors.Is(err, agent.ErrInvalidRequest) || !strings.Contains(err.Error(), `unknown field "unexpected"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestParsePositiveDuration(t *testing.T) {
	if got, err := parsePositiveDuration("", "test"); err != nil || got != 0 {
		t.Fatalf("empty duration = %s, %v", got, err)
	}
	if got, err := parsePositiveDuration(" 250ms ", "test"); err != nil || got != 250*time.Millisecond {
		t.Fatalf("parsed duration = %s, %v", got, err)
	}
	for _, value := range []string{"0", "-1s", "later"} {
		if _, err := parsePositiveDuration(value, "test"); err == nil {
			t.Fatalf("duration %q accepted", value)
		}
	}
}

func TestParsePositiveIntOrUnlimited(t *testing.T) {
	if got, err := parsePositiveIntOrUnlimited("", "test"); err != nil || got != 0 {
		t.Fatalf("empty limit = %d, %v", got, err)
	}
	if got, err := parsePositiveIntOrUnlimited(" 256 ", "test"); err != nil || got != 256 {
		t.Fatalf("parsed limit = %d, %v", got, err)
	}
	if got, err := parsePositiveIntOrUnlimited("UNLIMITED", "test"); err != nil || got != -1 {
		t.Fatalf("unlimited limit = %d, %v", got, err)
	}
	for _, value := range []string{"0", "-1", "forever"} {
		if _, err := parsePositiveIntOrUnlimited(value, "test"); err == nil {
			t.Fatalf("limit %q accepted", value)
		}
	}
}

func TestRunSavesAndResumesWorkspaceSession(t *testing.T) {
	var requests []chatRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body chatRequestForTest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests = append(requests, body)
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\n", fmt.Sprintf("answer %d", len(requests)))
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("DEEPSEEK_API_KEY", "secret")
	home := t.TempDir()
	root := t.TempDir()

	var firstOutput, firstErrors bytes.Buffer
	err := run(context.Background(), []string{
		"-model", "deepseek/saved-model",
		"-base-url", server.URL,
		"-home", home,
		"-root", root,
		"-save-session",
		"first task",
	}, bytes.NewReader(nil), &firstOutput, &firstErrors)
	if err != nil {
		t.Fatal(err)
	}
	summaries, err := session.List(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Title != "first task" {
		t.Fatalf("summaries = %#v", summaries)
	}
	if !strings.Contains(firstErrors.String(), "sk resume "+session.ShortID(summaries[0].ID)) {
		t.Fatalf("saved-session hint = %q", firstErrors.String())
	}

	var resumedOutput bytes.Buffer
	err = run(context.Background(), []string{
		"-base-url", server.URL,
		"-home", home,
		"-root", root,
		"resume", session.ShortID(summaries[0].ID), "continue",
	}, bytes.NewReader(nil), &resumedOutput, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if resumedOutput.String() != "answer 2\n" || len(requests) != 2 {
		t.Fatalf("resumed output/requests = %q/%d", resumedOutput.String(), len(requests))
	}
	if requests[1].Model != "saved-model" {
		t.Fatalf("resumed model = %q", requests[1].Model)
	}
	if got := conversationMessages(requests[1].Messages); len(got) != 3 || got[0].Content != "first task" || got[2].Content != "continue" {
		t.Fatalf("resumed messages = %#v", got)
	}
}

func TestRunOrdinaryOneShotDoesNotCreateManagedSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("DEEPSEEK_API_KEY", "secret")
	home := t.TempDir()
	root := t.TempDir()
	if err := run(context.Background(), []string{
		"-model", "deepseek/model", "-base-url", server.URL, "-home", home, "-root", root, "task",
	}, bytes.NewReader(nil), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	summaries, err := session.List(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 0 {
		t.Fatalf("temporary one-shot sessions = %#v", summaries)
	}
}

func TestRunKeepsOneShotSessionForChildAgent(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	settings := `{"tool_sets":{"default":["read","grep","glob","edit","write","bash","job","agent"]}}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	var parentRequests int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body chatRequestForTest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		prompt := ""
		for _, message := range body.Messages {
			if message.Role == "user" {
				prompt = message.Content
			}
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		if prompt == "child work" {
			<-request.Context().Done()
			return
		}
		parentRequests++
		if parentRequests == 1 {
			writeSSEChunk(t, writer, map[string]any{"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": "provider_agent", "type": "function",
					"function": map[string]any{"name": "agent", "arguments": `{"action":"start","prompt":"child work"}`},
				}}},
				"finish_reason": "tool_calls",
			}}})
		} else {
			writeSSEChunk(t, writer, map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "delegated"}, "finish_reason": "stop",
			}}})
		}
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()
	clearMainWebCredentials(t)

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{
		"-model", "deepseek/local-model", "-base-url", server.URL,
		"-home", home, "-root", root, "-sandbox", "off", "delegate work",
	}, bytes.NewReader(nil), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "delegated\n" {
		t.Fatalf("output = %q", stdout.String())
	}
	summaries, err := session.List(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Title != "delegate work" {
		t.Fatalf("saved child-agent session = %#v", summaries)
	}
	if !strings.Contains(stderr.String(), "Resume with: sk resume "+session.ShortID(summaries[0].ID)) {
		t.Fatalf("resume hint = %q", stderr.String())
	}
}

func TestRunKeepsOneShotSessionForDetachedJob(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	gate := filepath.Join(root, "release")
	done := filepath.Join(root, "done")
	t.Cleanup(func() { _ = os.WriteFile(gate, nil, 0o600) })
	toolsDocument := fmt.Sprintf(`{"tools":[{
	  "name":"detached_worker","description":"run detached work",
	  "command":["sh","-c",%q,"worker",%q,%q],
	  "background":"always","detach":true,
	  "parameters":{"type":"object","additionalProperties":false}
	}]}`, `while [ ! -f "$1" ]; do sleep 0.05; done; printf done > "$2"`, gate, done)
	if err := os.WriteFile(filepath.Join(home, "tools.json"), []byte(toolsDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := `{"tool_sets":{"default":["read","grep","glob","edit","write","bash","job","detached_worker"]}}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		writer.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			writeSSEChunk(t, writer, map[string]any{"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": "provider_detached", "type": "function",
					"function": map[string]any{"name": "detached_worker", "arguments": `{}`},
				}}},
				"finish_reason": "tool_calls",
			}}})
		} else {
			writeSSEChunk(t, writer, map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "detached started"}, "finish_reason": "stop",
			}}})
		}
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()
	clearMainWebCredentials(t)

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{
		"-model", "deepseek/local-model", "-base-url", server.URL,
		"-home", home, "-root", root, "-sandbox", "off", "-json", "start detached work",
	}, bytes.NewReader(nil), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var result jsonResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v; output=%q", err, stdout.String())
	}
	if result.SessionID == "" || len(result.DetachedJobs) != 1 {
		t.Fatalf("detached result = %#v", result)
	}
	if !strings.Contains(stderr.String(), "Resume with: sk resume "+session.ShortID(result.SessionID)) {
		t.Fatalf("resume hint = %q", stderr.String())
	}
	summaries, err := session.List(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].ID != result.SessionID || summaries[0].Title != "start detached work" {
		t.Fatalf("saved detached session = %#v", summaries)
	}

	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(done); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("detached worker did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunCustomBaseURLDoesNotRequireProviderCredential(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"local\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("DEEPSEEK_API_KEY", "")
	var output bytes.Buffer
	if err := run(context.Background(), []string{
		"-model", "deepseek/local-model",
		"-base-url", server.URL,
		"-home", t.TempDir(),
		"-root", t.TempDir(),
		"-sandbox", "off",
		"task",
	}, bytes.NewReader(nil), &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if output.String() != "local\n" {
		t.Fatalf("output = %q", output.String())
	}
	if authorization != "" {
		t.Fatalf("unexpected authorization = %q", authorization)
	}
}

func TestRunUsesOllamaOpenAICompatibilityWithoutCredential(t *testing.T) {
	var authorization string
	var requestBody chatRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"local\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	clearMainWebCredentials(t)

	var output bytes.Buffer
	if err := run(context.Background(), []string{
		"-model", "ollama/qwen3:8b", "-base-url", server.URL,
		"-home", t.TempDir(), "-root", t.TempDir(), "-sandbox", "off", "task",
	}, bytes.NewReader(nil), &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if output.String() != "local\n" || requestBody.Model != "qwen3:8b" || authorization != "Bearer ollama" {
		t.Fatalf("output/model/authorization = %q/%q/%q", output.String(), requestBody.Model, authorization)
	}
}

func TestRunReadsSystemPromptFileFromFlagAndEnvironment(t *testing.T) {
	var requests []chatRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body chatRequestForTest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests = append(requests, body)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("SK_SYSTEM_PROMPT_FILE", "")
	root := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "system.txt")
	if err := os.WriteFile(promptPath, []byte("  Be exact.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	common := []string{
		"-model", "deepseek/local-model", "-base-url", server.URL,
		"-home", t.TempDir(), "-root", root, "-sandbox", "off",
	}
	flagArgs := append(append([]string{}, common...), "-system-prompt-file", promptPath, "flag task")
	if err := run(context.Background(), flagArgs, bytes.NewReader(nil), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SK_SYSTEM_PROMPT_FILE", promptPath)
	envArgs := append(append([]string{}, common...), "environment task")
	if err := run(context.Background(), envArgs, bytes.NewReader(nil), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	for index, request := range requests {
		if len(request.Messages) == 0 || request.Messages[0].Role != "system" || request.Messages[0].Content != "Be exact." {
			t.Fatalf("request %d messages = %#v", index, request.Messages)
		}
	}
}

func TestRunAddsApplicableAgentsInstructions(t *testing.T) {
	var request chatRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		if err := json.NewDecoder(incoming.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("Always run the focused test.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{
		"-model", "deepseek/local-model", "-base-url", server.URL,
		"-home", t.TempDir(), "-root", root, "-sandbox", "off", "task",
	}, bytes.NewReader(nil), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) == 0 || request.Messages[0].Role != "system" ||
		!strings.Contains(request.Messages[0].Content, "Always run the focused test.") ||
		!strings.Contains(request.Messages[0].Content, "You are a CLI agent") ||
		!strings.Contains(request.Messages[0].Content, "Your workspace root is ") {
		t.Fatalf("system instructions = %#v", request.Messages)
	}
}

func TestSystemPromptFileRejectsMissingEmptyAndConflictingSources(t *testing.T) {
	if prompt, err := loadPromptFile("  ", "system prompt"); err != nil || prompt != "" {
		t.Fatalf("unset prompt file = %q, %v", prompt, err)
	}
	if _, err := loadPromptFile(filepath.Join(t.TempDir(), "missing"), "system prompt"); err == nil || !strings.Contains(err.Error(), "read system prompt") {
		t.Fatalf("missing file error = %v", err)
	}
	emptyPath := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(emptyPath, []byte(" \n\t"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPromptFile(emptyPath, "system prompt"); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("empty file error = %v", err)
	}
	t.Setenv("SK_HOME", t.TempDir())
	err := run(context.Background(), []string{
		"-system-prompt", "inline", "-system-prompt-file", emptyPath, "task",
	}, bytes.NewReader(nil), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("conflicting source error = %v", err)
	}
}

func TestRunOneShotExposesBackgroundBash(t *testing.T) {
	var requests []chatRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body chatRequestForTest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests = append(requests, body)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("DEEPSEEK_API_KEY", "")
	if err := run(context.Background(), []string{
		"-model", "deepseek/local-model", "-base-url", server.URL,
		"-home", t.TempDir(), "-root", t.TempDir(), "-sandbox", "off", "task",
	}, bytes.NewReader(nil), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %d", len(requests))
	}
	if !bashSchemaHasProperty(t, requests[0].Tools, "background") {
		t.Fatal("one-shot Bash schema does not expose background")
	}
}

func TestParseInvocationDistinguishesResumeCommandFromPrompt(t *testing.T) {
	command := parseInvocation([]string{"resume", "session_abc", "continue"}, false)
	if !command.resume || command.sessionPrefix != "session_abc" || strings.Join(command.args, " ") != "continue" {
		t.Fatalf("resume invocation = %#v", command)
	}
	prompt := parseInvocation([]string{"resume", "this", "discussion"}, true)
	if prompt.resume || strings.Join(prompt.args, " ") != "resume this discussion" {
		t.Fatalf("prompt invocation = %#v", prompt)
	}
	update := parseInvocation([]string{"update"}, false)
	if !update.update || len(update.args) != 0 {
		t.Fatalf("update invocation = %#v", update)
	}
	updatePrompt := parseInvocation([]string{"update", "the", "dependencies"}, true)
	if updatePrompt.update || strings.Join(updatePrompt.args, " ") != "update the dependencies" {
		t.Fatalf("update prompt invocation = %#v", updatePrompt)
	}
}

func TestRunFlagTerminatorTreatsResumeAsPrompt(t *testing.T) {
	var request chatRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, raw *http.Request) {
		if err := json.NewDecoder(raw.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	clearMainWebCredentials(t)

	err := run(context.Background(), []string{
		"-model", "deepseek/test-model", "-base-url", server.URL,
		"-home", t.TempDir(), "-root", t.TempDir(), "--", "resume", "this", "discussion",
	}, bytes.NewReader(nil), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	messages := conversationMessages(request.Messages)
	if len(messages) != 1 || messages[0].Role != "user" || messages[0].Content != "resume this discussion" {
		t.Fatalf("messages = %#v", messages)
	}
}

func TestRunExecutesProviderToolCallWithProductOwnedIdentity(t *testing.T) {
	t.Setenv("SK_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hello from file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var requests []chatRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body chatRequestForTest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests = append(requests, body)
		writer.Header().Set("Content-Type", "text/event-stream")
		if len(requests) == 1 {
			writeSSEChunk(t, writer, map[string]any{"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0,
					"id":    "provider_call_read",
					"type":  "function",
					"function": map[string]any{
						"name":      "read",
						"arguments": `{"path":"note.txt"}`,
					},
				}}},
				"finish_reason": "tool_calls",
			}}})
		} else {
			writeSSEChunk(t, writer, map[string]any{"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{"content": "saw the file"},
				"finish_reason": "stop",
			}}})
		}
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("DEEPSEEK_API_KEY", "secret")
	journalPath := filepath.Join(t.TempDir(), "session.jsonl")

	var output bytes.Buffer
	err := run(context.Background(), []string{
		"-model", "deepseek/test-model",
		"-base-url", server.URL,
		"-root", root,
		"-journal", journalPath,
		"read note.txt",
	}, bytes.NewReader(nil), &output, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != "saw the file\n" || len(requests) != 2 {
		t.Fatalf("output/requests = %q/%d", output.String(), len(requests))
	}
	if len(requests[0].Tools) != 8 {
		t.Fatalf("tool catalog size = %d", len(requests[0].Tools))
	}
	messages := conversationMessages(requests[1].Messages)
	if len(messages) != 3 || len(messages[1].ToolCalls) != 1 {
		t.Fatalf("second request messages = %#v", messages)
	}
	if messages[1].ToolCalls[0].ID != "provider_call_read" || messages[2].ToolCallID != "provider_call_read" {
		t.Fatalf("provider tool links = %#v / %#v", messages[1], messages[2])
	}
	if !strings.Contains(messages[2].Content, "hello from file") {
		t.Fatalf("tool result = %q", messages[2].Content)
	}

	journal, err := session.Open(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	records, err := journal.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := agent.Replay(records)
	if err != nil {
		t.Fatal(err)
	}
	var acceptedCall *agent.ToolCall
	for _, item := range state.Items {
		if item.Kind == agent.ItemToolCall {
			acceptedCall = item.ToolCall
			break
		}
	}
	if acceptedCall == nil || !strings.HasPrefix(acceptedCall.ID, "call_") || acceptedCall.ID == "provider_call_read" {
		t.Fatalf("accepted call = %#v", acceptedCall)
	}
}

func TestRunExecutesConfiguredProgramToolEndToEnd(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	toolsDocument := `{"tools":[{
	  "name":"lookup","description":"lookup an entry",
	  "command":["sh","-c","cat; printf '\\nsecret=%s\\n' \"$LOOKUP_TOKEN\"; printf 'diagnostic\\n' >&2"],
	  "background":"auto","parallel_safe":true,
	  "env":{"LOOKUP_TOKEN":"program-secret"},
	  "parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}
	}]}`
	if err := os.WriteFile(filepath.Join(home, "tools.json"), []byte(toolsDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := `{"tool_sets":{"default":["read","grep","glob","edit","write","bash","job","lookup"]}}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests []chatRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body chatRequestForTest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests = append(requests, body)
		writer.Header().Set("Content-Type", "text/event-stream")
		if len(requests) == 1 {
			writeSSEChunk(t, writer, map[string]any{"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": "provider_call_lookup", "type": "function",
					"function": map[string]any{"name": "lookup", "arguments": `{"query":"books","background":false}`},
				}}},
				"finish_reason": "tool_calls",
			}}})
		} else {
			writeSSEChunk(t, writer, map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "lookup completed"}, "finish_reason": "stop",
			}}})
		}
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	journalPath := filepath.Join(t.TempDir(), "session.jsonl")
	var output bytes.Buffer
	err := run(context.Background(), []string{
		"-model", "deepseek/test-model", "-base-url", server.URL,
		"-home", home, "-root", root, "-sandbox", "off", "-journal", journalPath,
		"look it up",
	}, bytes.NewReader(nil), &output, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != "lookup completed\n" || len(requests) != 2 {
		t.Fatalf("output/requests = %q/%d", output.String(), len(requests))
	}
	if !toolSchemaHasProperty(t, requests[0].Tools, "lookup", "background") {
		t.Fatal("configured auto tool schema has no background argument")
	}
	messages := conversationMessages(requests[1].Messages)
	if len(messages) != 3 {
		t.Fatalf("messages = %#v", messages)
	}
	toolResult := messages[2].Content
	for _, want := range []string{`{"query":"books"}`, "stderr:\ndiagnostic", "secret=[REDACTED]", "status: completed"} {
		if !strings.Contains(toolResult, want) {
			t.Fatalf("tool result = %q, want %q", toolResult, want)
		}
	}
	if strings.Contains(toolResult, "program-secret") || strings.Contains(toolResult, "background") {
		t.Fatalf("tool result leaked private or synthetic input: %q", toolResult)
	}
	journal, err := session.Open(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	records, err := journal.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := agent.Replay(records)
	if err != nil || state.Configured == nil || len(state.Configured.Environment.ProgramTools) != 1 {
		t.Fatalf("configured state = %#v, %v", state.Configured, err)
	}
	if encoded, err := json.Marshal(state.Configured); err != nil || strings.Contains(string(encoded), "program-secret") {
		t.Fatalf("snapshot leaked program environment: %s, %v", encoded, err)
	}
}

func TestRunExecutesModelBashAndPersistsProcessDetail(t *testing.T) {
	t.Setenv("SK_HOME", t.TempDir())
	var requests []chatRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body chatRequestForTest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests = append(requests, body)
		writer.Header().Set("Content-Type", "text/event-stream")
		if len(requests) == 1 {
			writeSSEChunk(t, writer, map[string]any{"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0,
					"id":    "provider_call_bash",
					"type":  "function",
					"function": map[string]any{
						"name":      "bash",
						"arguments": `{"command":"printf bash-ok"}`,
					},
				}}},
				"finish_reason": "tool_calls",
			}}})
		} else {
			writeSSEChunk(t, writer, map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "command completed"}, "finish_reason": "stop",
			}}})
		}
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("DEEPSEEK_API_KEY", "secret")
	clearMainWebCredentials(t)
	journalPath := filepath.Join(t.TempDir(), "session.jsonl")
	root := t.TempDir()

	var output bytes.Buffer
	err := run(context.Background(), []string{
		"-model", "deepseek/test-model", "-base-url", server.URL,
		"-sandbox", "off", "-root", root, "-journal", journalPath, "run command",
	}, bytes.NewReader(nil), &output, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != "command completed\n" || len(requests) != 2 {
		t.Fatalf("output/requests = %q/%d", output.String(), len(requests))
	}
	if got := conversationMessages(requests[1].Messages); len(got) != 3 || !strings.Contains(got[2].Content, "status: completed") || !strings.Contains(got[2].Content, "bash-ok") {
		t.Fatalf("bash messages = %#v", got)
	}

	journal, err := session.Open(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	records, err := journal.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := agent.Replay(records)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range state.Items {
		if item.Kind != agent.ItemToolResult || item.ToolResult == nil || len(item.ToolResult.Details) == 0 {
			continue
		}
		process, ok := workspacetools.ProcessResultFromDetail(item.ToolResult.Details[0])
		if !ok || process.Status != workspacetools.ProcessCompleted || process.UserInitiated {
			t.Fatalf("process detail = %#v, ok=%v", process, ok)
		}
		return
	}
	t.Fatal("journal has no process detail")
}

func TestRunDeliversBackgroundCompletionThroughJournal(t *testing.T) {
	var requests []chatRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body chatRequestForTest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests = append(requests, body)
		writer.Header().Set("Content-Type", "text/event-stream")
		completionDelivered := false
		for _, message := range body.Messages {
			if message.Role == "system" && strings.Contains(message.Content, "Background job job-") && strings.Contains(message.Content, "status=completed") {
				completionDelivered = true
				break
			}
		}
		switch {
		case len(requests) == 1:
			writeSSEChunk(t, writer, map[string]any{"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": "provider_background", "type": "function",
					"function": map[string]any{"name": "bash", "arguments": `{"command":"sleep 0.05; printf background-done","background":true}`},
				}}},
				"finish_reason": "tool_calls",
			}}})
		case !completionDelivered:
			writeSSEChunk(t, writer, map[string]any{"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": fmt.Sprintf("provider_pause_%d", len(requests)), "type": "function",
					"function": map[string]any{"name": "bash", "arguments": `{"command":"sleep 0.05"}`},
				}}},
				"finish_reason": "tool_calls",
			}}})
		default:
			writeSSEChunk(t, writer, map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "completion received"}, "finish_reason": "stop",
			}}})
		}
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("DEEPSEEK_API_KEY", "")
	journalPath := filepath.Join(t.TempDir(), "completion.jsonl")
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := run(ctx, []string{
		"-model", "deepseek/local-model", "-base-url", server.URL,
		"-home", t.TempDir(), "-root", t.TempDir(), "-sandbox", "off",
		"-journal", journalPath, "start background work",
	}, bytes.NewReader(nil), &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if output.String() != "completion received\n" {
		t.Fatalf("output/requests = %q/%d", output.String(), len(requests))
	}
	completionMessages := 0
	for _, request := range requests {
		for _, message := range request.Messages {
			if message.Role == "system" && strings.Contains(message.Content, "Background job job-") && strings.Contains(message.Content, "status=completed") {
				completionMessages++
			}
		}
	}
	if completionMessages != 1 {
		t.Fatalf("completion messages = %d; requests = %#v", completionMessages, requests)
	}
	journal, err := session.Open(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	records, err := journal.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	boundaryRecords := 0
	for _, record := range records {
		if record.Kind == agent.RecordBoundaryEvent {
			boundaryRecords++
		}
	}
	if boundaryRecords != 1 {
		t.Fatalf("boundary records = %d; records = %#v", boundaryRecords, records)
	}
}

func TestRunLoadsSavedSettingsUnlessExplicitlyOverridden(t *testing.T) {
	home := t.TempDir()
	settings, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.SetToolSetSelection(toolpolicy.ToolSetEdit); err != nil {
		t.Fatal(err)
	}
	if err := settings.SetDefaultModelSelection("deepseek/saved-model", "high"); err != nil {
		t.Fatal(err)
	}
	var requests []chatRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body chatRequestForTest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests = append(requests, body)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("DEEPSEEK_API_KEY", "secret")
	t.Setenv("SK_HOME", home)

	runOnce := func(extra ...string) {
		args := []string{"-base-url", server.URL, "-root", t.TempDir()}
		args = append(args, extra...)
		args = append(args, "task")
		if err := run(context.Background(), args, bytes.NewReader(nil), io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	runOnce()
	runOnce("-model", "deepseek/explicit-model", "-tools", toolpolicy.ToolSetDefault)
	runOnce("-reasoning-effort", "default")
	t.Setenv("SK_REASONING_EFFORT", "high")
	runOnce("-model", "deepseek/env-effort-model")
	if len(requests) != 4 || len(requests[0].Tools) != 7 || len(requests[1].Tools) != 8 {
		t.Fatalf("request count/tool catalogs = %d/%d/%d", len(requests), len(requests[0].Tools), len(requests[1].Tools))
	}
	if requests[0].Model != "saved-model" || requests[1].Model != "explicit-model" ||
		requests[2].Model != "saved-model" || requests[3].Model != "env-effort-model" {
		t.Fatalf("models = %q/%q/%q/%q", requests[0].Model, requests[1].Model, requests[2].Model, requests[3].Model)
	}
	if requests[0].ReasoningEffort != "high" || requests[1].ReasoningEffort != "" ||
		requests[2].ReasoningEffort != "" || requests[3].ReasoningEffort != "high" {
		t.Fatalf("reasoning efforts = %q/%q/%q/%q", requests[0].ReasoningEffort, requests[1].ReasoningEffort, requests[2].ReasoningEffort, requests[3].ReasoningEffort)
	}
}

func TestRunUsesStoredCredentialWithoutEnvironmentKey(t *testing.T) {
	home := t.TempDir()
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAPIKey("deepseek", "stored-secret"); err != nil {
		t.Fatal(err)
	}
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"stored-secret\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("SK_HOME", home)
	t.Setenv("DEEPSEEK_API_KEY", "")
	var output bytes.Buffer
	if err := run(context.Background(), []string{
		"-model", "deepseek/test-model", "-base-url", server.URL, "-root", t.TempDir(), "task",
	}, bytes.NewReader(nil), &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if output.String() != "[REDACTED]\n" || authorization != "Bearer stored-secret" {
		t.Fatalf("output = %q, authorization = %q", output.String(), authorization)
	}
}

func writeSSEChunk(t *testing.T, writer io.Writer, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(writer, "data: %s\n\n", raw)
}

func clearMainWebCredentials(t *testing.T) {
	t.Helper()
	for _, name := range []string{"TAVILY_API_KEY", "EXA_API_KEY", "FIRECRAWL_API_KEY"} {
		t.Setenv(name, "")
	}
}

func conversationMessages(messages []chatMessageForTest) []chatMessageForTest {
	if len(messages) != 0 && messages[0].Role == "system" {
		return messages[1:]
	}
	return messages
}

type chatRequestForTest struct {
	Model           string               `json:"model"`
	ReasoningEffort string               `json:"reasoning_effort"`
	Messages        []chatMessageForTest `json:"messages"`
	Tools           []json.RawMessage    `json:"tools"`
}

type chatMessageForTest struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func bashSchemaHasProperty(t *testing.T, tools []json.RawMessage, property string) bool {
	return toolSchemaHasProperty(t, tools, "bash", property)
}

func toolSchemaHasProperty(t *testing.T, tools []json.RawMessage, toolName, property string) bool {
	t.Helper()
	for _, raw := range tools {
		var declaration struct {
			Function struct {
				Name       string `json:"name"`
				Parameters struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &declaration); err != nil {
			t.Fatal(err)
		}
		if declaration.Function.Name == toolName {
			_, exists := declaration.Function.Parameters.Properties[property]
			return exists
		}
	}
	t.Fatalf("%s tool is missing", toolName)
	return false
}
