package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/app"
)

// TestCompositionPrototypeRunsIndependentApplications preserves the original
// public composition probe behind the built-in child supervisor: independent
// Applications must still run, cancel, close, and replay without sharing
// mutable session state.
func TestCompositionPrototypeRunsIndependentApplications(t *testing.T) {
	const childCount = 3

	parallelStarted := make(chan string, childCount)
	parallelRelease := make(chan struct{})
	cancelStarted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		prompt, toolNames, err := compositionRequest(request.Body)
		if err != nil {
			t.Errorf("decode composition request: %v", err)
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		if !containsString(toolNames, "read") {
			t.Errorf("read-only child did not receive the read tool: %v", toolNames)
		}
		for _, forbidden := range []string{"edit", "write", "bash", "job"} {
			if containsString(toolNames, forbidden) {
				t.Errorf("read-only child received %q tool: %v", forbidden, toolNames)
			}
		}

		switch {
		case strings.HasPrefix(prompt, "parallel-"):
			parallelStarted <- prompt
			select {
			case <-parallelRelease:
			case <-request.Context().Done():
				return
			}
		case prompt == "cancel-me":
			cancelStarted <- struct{}{}
			<-request.Context().Done()
			return
		}
		writeCompositionResponse(t, writer, "answer: "+prompt)
	}))
	t.Cleanup(server.Close)

	home := t.TempDir()
	root := t.TempDir()
	journalDir := t.TempDir()
	children := make([]*app.Application, childCount)
	journalPaths := make([]string, childCount)
	for index := range children {
		journalPaths[index] = filepath.Join(journalDir, fmt.Sprintf("child-%d.jsonl", index))
		child, err := openCompositionChild(home, root, server.URL+"/v1", journalPaths[index])
		if err != nil {
			t.Fatalf("open child %d: %v", index, err)
		}
		children[index] = child
	}
	t.Cleanup(func() {
		for _, child := range children {
			_ = child.Close()
		}
	})

	type outcome struct {
		index  int
		result agent.RunResult
		err    error
	}
	outcomes := make(chan outcome, childCount)
	for index, child := range children {
		go func() {
			result, err := child.Run(context.Background(), fmt.Sprintf("parallel-%d", index), nil)
			outcomes <- outcome{index: index, result: result, err: err}
		}()
	}

	started := make(map[string]struct{}, childCount)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for len(started) < childCount {
		select {
		case prompt := <-parallelStarted:
			started[prompt] = struct{}{}
		case <-deadline.C:
			t.Fatalf("only %d/%d child requests ran concurrently", len(started), childCount)
		}
	}
	close(parallelRelease)

	runIDs := make(map[string]struct{}, childCount)
	for range childCount {
		select {
		case got := <-outcomes:
			if got.err != nil {
				t.Fatalf("child %d run: %v", got.index, got.err)
			}
			wantAnswer := fmt.Sprintf("answer: parallel-%d", got.index)
			if got.result.Status != agent.RunCompleted || got.result.Answer != wantAnswer || got.result.RunID == "" {
				t.Fatalf("child %d result = %#v", got.index, got.result)
			}
			if _, duplicate := runIDs[got.result.RunID]; duplicate {
				t.Fatalf("duplicate run ID %q", got.result.RunID)
			}
			runIDs[got.result.RunID] = struct{}{}
		case <-deadline.C:
			t.Fatal("parallel child runs did not finish")
		}
	}

	states := make([]agent.State, childCount)
	sessionIDs := make(map[string]struct{}, childCount)
	exposedSessionIDs := 0
	for index, child := range children {
		state, err := child.State(context.Background())
		if err != nil {
			t.Fatalf("state child %d: %v", index, err)
		}
		states[index] = state
		if state.SessionID == "" || len(state.Blocks) != 1 || state.Blocks[0].Status != agent.RunCompleted {
			t.Fatalf("child %d state = %#v", index, state)
		}
		if _, duplicate := sessionIDs[state.SessionID]; duplicate {
			t.Fatalf("duplicate journal session ID %q", state.SessionID)
		}
		sessionIDs[state.SessionID] = struct{}{}
		if state.Configured == nil || state.Configured.ModelContext.ToolSet != app.ToolSetReadOnly {
			t.Fatalf("child %d effective configuration = %#v", index, state.Configured)
		}
		if exposed := child.SessionID(); exposed != "" {
			exposedSessionIDs++
			if exposed != state.SessionID {
				t.Fatalf("child %d exposed session ID %q, journal has %q", index, exposed, state.SessionID)
			}
		}
	}
	if exposedSessionIDs == 0 {
		t.Log("explicit-journal Applications have runtime session IDs in State, but Application.SessionID does not expose them")
	}
	listed, err := children[1].ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("explicit child journals polluted managed session picker: %#v", listed)
	}

	if err := children[0].Close(); err != nil {
		t.Fatalf("close first child: %v", err)
	}
	if _, err := children[0].State(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed child remained usable: %v", err)
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan outcome, 1)
	go func() {
		result, err := children[1].Run(cancelCtx, "cancel-me", nil)
		cancelled <- outcome{index: 1, result: result, err: err}
	}()
	select {
	case <-cancelStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled child did not reach the model")
	}

	survivor, err := children[2].Run(context.Background(), "still-alive", nil)
	if err != nil || survivor.Status != agent.RunCompleted || survivor.Answer != "answer: still-alive" {
		t.Fatalf("sibling run while another child was blocked = %#v, %v", survivor, err)
	}
	cancel()
	select {
	case got := <-cancelled:
		if !errors.Is(got.err, context.Canceled) || got.result.Status != agent.RunCancelled || got.result.RunID == "" {
			t.Fatalf("cancelled child result = %#v, %v", got.result, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child cancellation did not finish")
	}

	for index := 1; index < childCount; index++ {
		if err := children[index].Close(); err != nil {
			t.Fatalf("close child %d: %v", index, err)
		}
	}
	for index, path := range journalPaths {
		reopened, err := openCompositionChild(home, root, server.URL+"/v1", path)
		if err != nil {
			t.Fatalf("reopen child %d: %v", index, err)
		}
		replayed, stateErr := reopened.State(context.Background())
		closeErr := reopened.Close()
		if stateErr != nil || closeErr != nil {
			t.Fatalf("replay child %d through Application: %v", index, errors.Join(stateErr, closeErr))
		}
		wantBlocks := 1
		if index > 0 {
			wantBlocks = 2
		}
		if replayed.SessionID != states[index].SessionID || len(replayed.Blocks) != wantBlocks || len(replayed.ActiveRuns) != 0 || len(replayed.PendingTools) != 0 {
			t.Fatalf("replayed child %d state = %#v", index, replayed)
		}
		wantUsage := agent.ModelUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}
		wantStatuses := []agent.RunStatus{agent.RunCompleted}
		switch index {
		case 1:
			wantStatuses = append(wantStatuses, agent.RunCancelled)
		case 2:
			wantUsage = agent.ModelUsage{InputTokens: 20, OutputTokens: 4, TotalTokens: 24}
			wantStatuses = append(wantStatuses, agent.RunCompleted)
		}
		if replayed.Usage != wantUsage {
			t.Fatalf("replayed child %d usage = %#v, want %#v", index, replayed.Usage, wantUsage)
		}
		for blockIndex, wantStatus := range wantStatuses {
			if replayed.Blocks[blockIndex].Status != wantStatus {
				t.Fatalf("replayed child %d block %d status = %q, want %q", index, blockIndex, replayed.Blocks[blockIndex].Status, wantStatus)
			}
		}
	}
}

func openCompositionChild(home, root, baseURL, journalPath string) (*app.Application, error) {
	return app.Open(context.Background(), app.Config{
		Home: home, Root: root,
		ModelURI: "deepseek/composition-probe", ModelExplicit: true,
		BaseURL: baseURL, ContextWindow: 32 * 1024,
		ToolSet: app.ToolSetReadOnly, ToolSetExplicit: true,
		Scope: app.ScopeMachine, ScopeExplicit: true,
		JournalPath: journalPath,
	})
}

func compositionRequest(body io.Reader) (string, []string, error) {
	var request struct {
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
		return "", nil, err
	}
	prompt := ""
	for _, message := range request.Messages {
		if message.Role == "user" {
			prompt = message.Content
		}
	}
	toolNames := make([]string, 0, len(request.Tools))
	for _, tool := range request.Tools {
		toolNames = append(toolNames, tool.Function.Name)
	}
	sort.Strings(toolNames)
	if prompt == "" {
		return "", nil, errors.New("request has no user prompt")
	}
	return prompt, toolNames, nil
}

func writeCompositionResponse(t *testing.T, writer http.ResponseWriter, answer string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]string{"content": answer},
			"finish_reason": "stop",
		}},
	})
	if err != nil {
		t.Errorf("encode composition response: %v", err)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(writer, "data: %s\n\n", payload)
	_, _ = io.WriteString(writer, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n")
	_, _ = io.WriteString(writer, "data: [DONE]\n\n")
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
