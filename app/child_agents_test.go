package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/session"
)

func TestChildAgentToolIsOptInReadOnlyAndDurable(t *testing.T) {
	requests := make(chan childTestRequest, 8)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		decoded, err := decodeChildTestRequest(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- decoded
		writeChildTestAnswer(writer, "answer: "+decoded.Prompt)
	}))
	t.Cleanup(server.Close)

	home, root := t.TempDir(), t.TempDir()
	config := childTestConfig(home, root, server.URL+"/v1", true)
	application, err := Open(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}

	standard, err := application.config.toolSets.Tools(application.config.tools, ToolSetDefault)
	if err != nil {
		t.Fatal(err)
	}
	if childTestHasTool(standard, "agent") {
		t.Fatal("agent tool leaked into the default tool set")
	}
	if _, err := application.Run(context.Background(), "parent", nil); err != nil {
		t.Fatal(err)
	}
	parentRequest := <-requests
	if !containsChildTestName(parentRequest.Tools, "agent") {
		t.Fatalf("parent tools = %v", parentRequest.Tools)
	}
	parentID := application.SessionID()
	tool := childTestTool(t, application)
	if tool.Spec.ParallelSafe {
		t.Fatal("agent actions must remain serial even though child runs are asynchronous")
	}

	started := runChildTestTool(t, tool, parentID, childToolArgs{Action: "start", Prompt: "first"})
	agentID := strings.Fields(started.Content)[1]
	childRequest := <-requests
	for _, forbidden := range []string{"agent", "edit", "write", "bash", "job"} {
		if containsChildTestName(childRequest.Tools, forbidden) {
			t.Fatalf("read-only child received %q: %v", forbidden, childRequest.Tools)
		}
	}
	for _, required := range []string{"read", "ls", "grep", "glob"} {
		if !containsChildTestName(childRequest.Tools, required) {
			t.Fatalf("read-only child is missing %q: %v", required, childRequest.Tools)
		}
	}

	checked := runChildTestTool(t, tool, parentID, childToolArgs{Action: "check", IDs: []string{agentID}, Wait: "all"})
	if !strings.Contains(checked.Content, "answer: first") || len(checked.Details) != 1 {
		t.Fatalf("check output = %#v", checked)
	}
	if events := application.state.children.PendingEvents(parentID); len(events) != 1 || !strings.Contains(events[0].Content, "answer: first") {
		t.Fatalf("completion events = %#v", events)
	}
	application.state.children.ToolResultCommitted(agent.ToolResult{Details: checked.Details})
	if events := application.state.children.PendingEvents(parentID); len(events) != 0 {
		t.Fatalf("committed completion was offered again: %#v", events)
	}

	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "agents", parentID, ".agent_interrupted.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	application, err = Open(context.Background(), childTestResumeConfig(home, root, server.URL+"/v1", parentID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	tool = childTestTool(t, application)
	if _, err := application.Run(context.Background(), "resumed parent", nil); err != nil {
		t.Fatal(err)
	}
	resumedParentRequest := <-requests
	if !strings.Contains(resumedParentRequest.Boundary, "Child agent "+agentID+" completed") {
		t.Fatalf("resumed parent boundary = %q", resumedParentRequest.Boundary)
	}

	replayed := runChildTestTool(t, tool, parentID, childToolArgs{Action: "check", IDs: []string{agentID}})
	if !strings.Contains(replayed.Content, "answer: first") {
		t.Fatalf("replayed child = %q", replayed.Content)
	}
	runChildTestTool(t, tool, parentID, childToolArgs{Action: "send", ID: agentID, Prompt: "second"})
	if request := <-requests; request.Prompt != "second" {
		t.Fatalf("follow-up prompt = %q", request.Prompt)
	}
	followedUp := runChildTestTool(t, tool, parentID, childToolArgs{Action: "check", IDs: []string{agentID}, Wait: "all"})
	if !strings.Contains(followedUp.Content, "answer: second") || !strings.Contains(followedUp.Content, "· 12 tokens") {
		t.Fatalf("follow-up result = %q", followedUp.Content)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	application, err = Open(context.Background(), childTestResumeConfig(home, root, server.URL+"/v1", parentID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.Run(context.Background(), "parent after second resume", nil); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; !strings.Contains(request.Boundary, "answer: second") || !strings.Contains(request.Boundary, "(12 tokens)") {
		t.Fatalf("second resumed parent boundary = %q", request.Boundary)
	}
	if count := childBoundaryRecordCount(t, application); count != 2 {
		t.Fatalf("parent boundary record count = %d, want 2", count)
	}
}

func TestChildAgentSupervisorRunsInParallelAndAppliesBackpressure(t *testing.T) {
	started := make(chan string, maxActiveChildren)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		decoded, err := decodeChildTestRequest(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.HasPrefix(decoded.Prompt, "parallel-") {
			started <- decoded.Prompt
			select {
			case <-release:
			case <-request.Context().Done():
				return
			}
		}
		writeChildTestAnswer(writer, "done: "+decoded.Prompt)
	}))
	t.Cleanup(server.Close)

	application, err := Open(context.Background(), childTestConfig(t.TempDir(), t.TempDir(), server.URL+"/v1", true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	parentID := application.SessionID()
	tool := childTestTool(t, application)
	ids := make([]string, 0, maxActiveChildren)
	for index := range maxActiveChildren {
		output := runChildTestTool(t, tool, parentID, childToolArgs{Action: "start", Prompt: fmt.Sprintf("parallel-%d", index)})
		ids = append(ids, strings.Fields(output.Content)[1])
	}

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	seen := make(map[string]struct{}, maxActiveChildren)
	for len(seen) != maxActiveChildren {
		select {
		case prompt := <-started:
			seen[prompt] = struct{}{}
		case <-deadline.C:
			t.Fatalf("only %d child requests started in parallel", len(seen))
		}
	}
	if _, err := tool.Run(agent.WithToolSessionID(context.Background(), parentID), `{"action":"start","prompt":"overflow"}`); err == nil || !strings.Contains(err.Error(), "active child agent limit") {
		t.Fatalf("backpressure error = %v", err)
	}
	close(release)
	checked := runChildTestTool(t, tool, parentID, childToolArgs{Action: "check", IDs: ids, Wait: "all"})
	for index := range maxActiveChildren {
		if !strings.Contains(checked.Content, fmt.Sprintf("done: parallel-%d", index)) {
			t.Fatalf("parallel results = %q", checked.Content)
		}
	}
}

func TestChildAgentModelOverrideRequiresAllowlist(t *testing.T) {
	requests := make(chan childTestRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		decoded, err := decodeChildTestRequest(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- decoded
		writeChildTestAnswer(writer, "done")
	}))
	t.Cleanup(server.Close)

	config := childTestConfig(t.TempDir(), t.TempDir(), server.URL+"/v1", true)
	config.AgentModels = []string{"openai/allowed"}
	application, err := Open(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	tool := childTestTool(t, application)
	parentID := application.SessionID()
	if _, err := tool.Run(agent.WithToolSessionID(context.Background(), parentID), `{"action":"start","prompt":"work","model":"openai/blocked"}`); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("model authority error = %v", err)
	}
	started := runChildTestTool(t, tool, parentID, childToolArgs{Action: "start", Prompt: "work", Model: "openai/allowed"})
	id := strings.Fields(started.Content)[1]
	if request := <-requests; request.Model != "allowed" {
		t.Fatalf("child API model = %q", request.Model)
	}
	runChildTestTool(t, tool, parentID, childToolArgs{Action: "check", IDs: []string{id}, Wait: "all"})

	invalid := childTestConfig(t.TempDir(), t.TempDir(), server.URL+"/v1", true)
	invalid.AgentModels = []string{"unknown/value"}
	if opened, err := Open(context.Background(), invalid); err == nil {
		_ = opened.Close()
		t.Fatal("unsupported agent model provider was accepted")
	}
}

func TestExistingChildKeepsItsModelAfterParentSwitch(t *testing.T) {
	requests := make(chan childTestRequest, 3)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		decoded, err := decodeChildTestRequest(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- decoded
		writeChildTestAnswer(writer, "done: "+decoded.Prompt)
	}))
	t.Cleanup(server.Close)

	application, err := Open(context.Background(), childTestConfig(t.TempDir(), t.TempDir(), server.URL+"/v1", true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	tool := childTestTool(t, application)
	parentID := application.SessionID()
	started := runChildTestTool(t, tool, parentID, childToolArgs{Action: "start", Prompt: "before switch"})
	childID := strings.Fields(started.Content)[1]
	if request := <-requests; request.Model != "child-test" {
		t.Fatalf("initial child model = %q", request.Model)
	}
	runChildTestTool(t, tool, parentID, childToolArgs{Action: "check", IDs: []string{childID}, Wait: "all"})

	if err := application.SwitchModel(context.Background(), "deepseek/new-parent", ""); err != nil {
		t.Fatal(err)
	}
	runChildTestTool(t, tool, parentID, childToolArgs{Action: "send", ID: childID, Prompt: "after switch"})
	if request := <-requests; request.Model != "child-test" {
		t.Fatalf("continued child model = %q, want original model", request.Model)
	}
	runChildTestTool(t, tool, parentID, childToolArgs{Action: "check", IDs: []string{childID}, Wait: "all"})

	runChildTestTool(t, tool, parentID, childToolArgs{Action: "start", Prompt: "new child"})
	if request := <-requests; request.Model != "new-parent" {
		t.Fatalf("new child model = %q, want switched parent model", request.Model)
	}
}

func TestChildAgentFollowsParentSessionSwitches(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		decoded, err := decodeChildTestRequest(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		writeChildTestAnswer(writer, "answer: "+decoded.Prompt)
	}))
	t.Cleanup(server.Close)

	application, err := Open(context.Background(), childTestConfig(t.TempDir(), t.TempDir(), server.URL+"/v1", true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	if _, err := application.Run(context.Background(), "first parent", nil); err != nil {
		t.Fatal(err)
	}
	firstParentID := application.SessionID()
	tool := childTestTool(t, application)
	started := runChildTestTool(t, tool, firstParentID, childToolArgs{Action: "start", Prompt: "child work"})
	childID := strings.Fields(started.Content)[1]
	runChildTestTool(t, tool, firstParentID, childToolArgs{Action: "check", IDs: []string{childID}, Wait: "all"})

	secondParentID, err := application.ClearSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if secondParentID == firstParentID {
		t.Fatal("clear reused the parent session ID")
	}
	if _, ok := application.ToolStatus(childID); ok {
		t.Fatal("child from the released parent remained in the active registry")
	}
	if _, err := application.ResumeSession(context.Background(), firstParentID); err != nil {
		t.Fatal(err)
	}
	replayed := runChildTestTool(t, tool, firstParentID, childToolArgs{Action: "check", IDs: []string{childID}})
	if !strings.Contains(replayed.Content, "answer: child work") {
		t.Fatalf("replayed child after parent resume = %q", replayed.Content)
	}
	if _, ok := application.ToolStatus(childID); !ok {
		t.Fatal("resumed child is absent from application tool status")
	}

	sandbox := agent.SandboxSnapshot{RequestedPolicy: "test", EffectivePolicy: "test", Backend: "test"}
	if err := application.state.children.setSandboxSnapshot(context.Background(), sandbox); err != nil {
		t.Fatal(err)
	}
	child := application.state.children.children[firstParentID][childID]
	state, err := child.runtime.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Configured == nil || state.Configured.Environment.Sandbox != sandbox {
		t.Fatalf("child sandbox snapshot = %#v", state.Configured)
	}
}

func TestChildRunsFromJournalUsesTheFinalModelResponse(t *testing.T) {
	journal, err := session.Open(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	model := &childReplayModel{}
	runtime, err := agent.New(agent.Config{
		Model: model, Journal: journal, SessionID: "session_0123456789abcdef0123456789abcdef",
		Tools: []agent.Tool{{
			Spec: agent.ToolSpec{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Run:  func(context.Context, string) (agent.ToolOutput, error) { return agent.ToolOutput{Content: "ok"}, nil },
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "inspect", nil)
	if err != nil || result.Answer != "" {
		t.Fatalf("runtime result = %#v, %v", result, err)
	}
	runs, err := childRunsFromJournal(context.Background(), journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Answer != "" || runs[0].Usage.TotalTokens != 12 || runs[0].Usage.ReasoningTokens != 5 {
		t.Fatalf("replayed child runs = %#v", runs)
	}
	if reply := assistantReply([]agent.Item{{Kind: agent.ItemAssistantText, Text: "first"}, {Kind: agent.ItemAssistantText, Text: "second"}}); reply != "first\nsecond" {
		t.Fatalf("multi-part assistant reply = %q", reply)
	}
}

func TestSubtractModelUsageIncludesReasoningTokens(t *testing.T) {
	got := subtractModelUsage(
		agent.ModelUsage{InputTokens: 20, CachedInputTokens: 5, OutputTokens: 12, ReasoningTokens: 7, TotalTokens: 32},
		agent.ModelUsage{InputTokens: 8, CachedInputTokens: 2, OutputTokens: 5, ReasoningTokens: 3, TotalTokens: 13},
	)
	want := (agent.ModelUsage{InputTokens: 12, CachedInputTokens: 3, OutputTokens: 7, ReasoningTokens: 4, TotalTokens: 19})
	if got != want {
		t.Fatalf("usage delta = %#v, want %#v", got, want)
	}
}

func TestOneShotParentIsRetainedWhenItStartsChildAgent(t *testing.T) {
	childStarted := make(chan struct{}, 1)
	releaseChild := make(chan struct{})
	var mu sync.Mutex
	parentRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		decoded, err := decodeChildTestRequest(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		if decoded.Prompt == "child work" {
			childStarted <- struct{}{}
			select {
			case <-releaseChild:
			case <-request.Context().Done():
				return
			}
			writeChildTestAnswer(writer, "child done")
			return
		}
		mu.Lock()
		parentRequests++
		requestNumber := parentRequests
		mu.Unlock()
		if requestNumber == 1 {
			writeChildTestToolCall(writer, `{"action":"start","prompt":"child work"}`)
			return
		}
		writeChildTestAnswer(writer, "delegated")
	}))
	t.Cleanup(server.Close)

	config := childTestConfig(t.TempDir(), t.TempDir(), server.URL+"/v1", false)
	application, err := Open(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if application.SessionID() != "" {
		t.Fatal("one-shot session was resumable before delegation")
	}
	result, err := application.Run(context.Background(), "delegate", nil)
	if err != nil || result.Answer != "delegated" {
		t.Fatalf("parent result = %#v, %v", result, err)
	}
	if application.SessionID() == "" {
		t.Fatal("one-shot session was not retained after starting a child")
	}
	select {
	case <-childStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("child request did not start")
	}
	close(releaseChild)
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunningChildIsCancelledCleanlyAndCanContinueAfterResume(t *testing.T) {
	childStarted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		decoded, err := decodeChildTestRequest(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		if decoded.Prompt == "cancel child" {
			childStarted <- struct{}{}
			<-request.Context().Done()
			return
		}
		writeChildTestAnswer(writer, "answer: "+decoded.Prompt)
	}))
	t.Cleanup(server.Close)

	home, root := t.TempDir(), t.TempDir()
	config := childTestConfig(home, root, server.URL+"/v1", true)
	application, err := Open(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.Run(context.Background(), "parent", nil); err != nil {
		t.Fatal(err)
	}
	parentID := application.SessionID()
	tool := childTestTool(t, application)
	started := runChildTestTool(t, tool, parentID, childToolArgs{Action: "start", Prompt: "cancel child"})
	agentID := strings.Fields(started.Content)[1]
	select {
	case <-childStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("child request did not start")
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}

	application, err = Open(context.Background(), childTestResumeConfig(home, root, server.URL+"/v1", parentID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	tool = childTestTool(t, application)
	cancelled := runChildTestTool(t, tool, parentID, childToolArgs{Action: "check", IDs: []string{agentID}})
	if !strings.Contains(cancelled.Content, string(agent.RunCancelled)) {
		t.Fatalf("cancelled child = %q", cancelled.Content)
	}
	runChildTestTool(t, tool, parentID, childToolArgs{Action: "send", ID: agentID, Prompt: "continue child"})
	continued := runChildTestTool(t, tool, parentID, childToolArgs{Action: "check", IDs: []string{agentID}, Wait: "all"})
	if !strings.Contains(continued.Content, "answer: continue child") {
		t.Fatalf("continued child = %q", continued.Content)
	}
	stopped := runChildTestTool(t, tool, parentID, childToolArgs{Action: "stop", ID: agentID})
	if !strings.Contains(stopped.Content, "stopped "+agentID) {
		t.Fatalf("stop output = %q", stopped.Content)
	}
	if _, err := tool.Run(agent.WithToolSessionID(context.Background(), parentID), fmt.Sprintf(`{"action":"send","id":%q,"prompt":"too late"}`, agentID)); err == nil || !strings.Contains(err.Error(), "is stopped") {
		t.Fatalf("send after stop error = %v", err)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	application, err = Open(context.Background(), childTestResumeConfig(home, root, server.URL+"/v1", parentID))
	if err != nil {
		t.Fatal(err)
	}
	tool = childTestTool(t, application)
	stoppedAfterResume := runChildTestTool(t, tool, parentID, childToolArgs{Action: "check", IDs: []string{agentID}})
	if !strings.Contains(stoppedAfterResume.Content, agentID+" stopped") {
		t.Fatalf("stopped child after resume = %q", stoppedAfterResume.Content)
	}
	if _, err := tool.Run(agent.WithToolSessionID(context.Background(), parentID), fmt.Sprintf(`{"action":"send","id":%q,"prompt":"still too late"}`, agentID)); err == nil || !strings.Contains(err.Error(), "is stopped") {
		t.Fatalf("send after resumed stop error = %v", err)
	}
}

type childReplayModel struct{ calls int }

func (model *childReplayModel) Info() agent.ModelInfo {
	return agent.ModelInfo{Backend: "test", Provider: "test", Model: "child-replay"}
}

func (model *childReplayModel) Complete(context.Context, agent.ModelRequest, func(agent.ModelStreamEvent)) (agent.ModelResponse, error) {
	model.calls++
	if model.calls == 1 {
		return agent.ModelResponse{
			Items: []agent.Item{
				{Kind: agent.ItemAssistantText, Text: "draft"},
				{Kind: agent.ItemToolCall, ToolCall: &agent.ToolCall{Name: "read", RawArguments: `{}`}},
			},
			Usage: agent.ModelUsage{ReasoningTokens: 2, TotalTokens: 5},
		}, nil
	}
	return agent.ModelResponse{
		Items: []agent.Item{{Kind: agent.ItemAssistantText}},
		Usage: agent.ModelUsage{ReasoningTokens: 3, TotalTokens: 7},
	}, nil
}

func childTestConfig(home, root, baseURL string, interactive bool) Config {
	return Config{
		Home: home, Root: root,
		ModelURI: "deepseek/child-test", ModelExplicit: true,
		BaseURL: baseURL, ContextWindow: 32 * 1024,
		ToolSet: "delegate", ToolSetExplicit: true, ToolSets: childTestToolSets(),
		Sandbox: SandboxOff, SandboxExplicit: true, Interactive: interactive,
	}
}

func childTestResumeConfig(home, root, baseURL, parentID string) Config {
	config := childTestConfig(home, root, baseURL, true)
	config.Resume = true
	config.ResumePrefix = parentID
	return config
}

func childBoundaryRecordCount(t *testing.T, application *Application) int {
	t.Helper()
	application.mu.RLock()
	current := application.state.session
	application.mu.RUnlock()
	records, err := current.journal.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, record := range records {
		if record.Kind == agent.RecordBoundaryEvent {
			count++
		}
	}
	return count
}

func childTestToolSets() map[string][]string {
	return map[string][]string{"delegate": {"read", "grep", "glob", "agent"}}
}

func childTestTool(t *testing.T, application *Application) agent.Tool {
	t.Helper()
	for _, tool := range application.config.tools {
		if tool.Spec.Name == "agent" {
			return tool
		}
	}
	t.Fatal("agent tool is absent")
	return agent.Tool{}
}

func runChildTestTool(t *testing.T, tool agent.Tool, parentID string, args childToolArgs) agent.ToolOutput {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	output, err := tool.Run(agent.WithToolSessionID(context.Background(), parentID), string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return output
}

type childTestRequest struct {
	Prompt   string
	Tools    []string
	Model    string
	Boundary string
}

func decodeChildTestRequest(body io.Reader) (childTestRequest, error) {
	var request struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(body).Decode(&request); err != nil {
		return childTestRequest{}, err
	}
	decoded := childTestRequest{Model: request.Model}
	for _, message := range request.Messages {
		if message.Role == "user" {
			decoded.Prompt = message.Content
		} else if message.Role == "system" && strings.Contains(message.Content, "Child agent ") {
			decoded.Boundary += message.Content
		}
	}
	for _, tool := range request.Tools {
		decoded.Tools = append(decoded.Tools, tool.Function.Name)
	}
	sort.Strings(decoded.Tools)
	if decoded.Prompt == "" {
		return childTestRequest{}, fmt.Errorf("request has no user prompt")
	}
	return decoded, nil
}

func writeChildTestAnswer(writer http.ResponseWriter, answer string) {
	payload, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]string{"content": answer}, "finish_reason": "stop",
		}},
	})
	writer.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(writer, "data: %s\n\n", payload)
	_, _ = io.WriteString(writer, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n")
	_, _ = io.WriteString(writer, "data: [DONE]\n\n")
}

func writeChildTestToolCall(writer http.ResponseWriter, arguments string) {
	payload, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": "provider_agent_call", "type": "function",
				"function": map[string]string{"name": "agent", "arguments": arguments},
			}}},
			"finish_reason": "tool_calls",
		}},
	})
	writer.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(writer, "data: %s\n\n", payload)
	_, _ = io.WriteString(writer, "data: [DONE]\n\n")
}

func childTestHasTool(tools []agent.Tool, name string) bool {
	for _, tool := range tools {
		if tool.Spec.Name == name {
			return true
		}
	}
	return false
}

func containsChildTestName(values []string, name string) bool {
	for _, value := range values {
		if value == name {
			return true
		}
	}
	return false
}

func (model *childReplayModel) ProjectModelItems(items []agent.Item) []agent.Item { return items }
