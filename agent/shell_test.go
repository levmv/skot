package agent

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/levmv/skot/model"
)

func TestDurableShellRecordsSyntheticTurn(t *testing.T) {
	journal := &memoryJournal{}
	called := ""
	runtime := newTestRuntime(t, Config{
		Backend: &scriptedModel{},
		Journal: journal,
		UserShell: func(_ context.Context, command string) (ToolOutput, error) {
			called = command
			return ToolOutput{Content: model.TextContent("status: completed\n\nhello\n"), Details: []model.Detail{{
				Kind: "process_result", Data: jsontext.Value(`{"status":"completed"}`),
			}}}, nil
		},
	})

	result, err := runtime.RunShell(context.Background(), "printf hello")
	if err != nil {
		t.Fatal(err)
	}
	if called != "printf hello" || result.CallID == "" || result.Error {
		t.Fatalf("called=%q result=%#v", called, result)
	}
	usage, err := runtime.Usage(t.Context(), 0)
	if err != nil || !usage.Complete || len(usage.Attempts) != 0 || usage.LegacyResponses != 0 || usage.Usage != (model.TokenCounts{}) {
		t.Fatalf("shell command counted as a model request: %#v, %v", usage, err)
	}
	assertSemanticRecordKinds(t, journal.snapshot(),
		RecordSessionStarted, RecordModelSelected, RecordSessionConfigured, RecordRunStarted, RecordRunInputAdded,
		RecordModelResponse, RecordToolResult, RecordRunFinished,
	)
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Items) != 3 || state.Items[0].Kind != model.ItemUserText || state.Items[0].Text != "!printf hello" ||
		state.Items[1].Kind != model.ItemToolCall || state.Items[1].ToolCall.Name != "bash" ||
		state.Items[2].Kind != model.ItemToolResult || state.Items[2].ToolResult.CallID != state.Items[1].ToolCall.ID {
		t.Fatalf("synthetic shell items = %#v", state.Items)
	}
	if len(state.Items[2].ToolResult.Details) != 1 {
		t.Fatalf("replayed details = %#v", state.Items[2].ToolResult.Details)
	}
}

func TestPrivateShellDoesNotTouchJournal(t *testing.T) {
	journal := &memoryJournal{}
	runtime := newTestRuntime(t, Config{
		Backend: &scriptedModel{},
		Journal: journal,
		UserShell: func(_ context.Context, command string) (ToolOutput, error) {
			return ToolOutput{Content: model.TextContent(command)}, nil
		},
	})

	result, err := runtime.RunPrivateShell(context.Background(), "private command")
	if err != nil || result.Content.Text() != "private command" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if records := journal.snapshot(); len(records) != 0 {
		t.Fatalf("private shell journal = %#v", records)
	}
}

func TestShellSanitizesResultAndDurableJournal(t *testing.T) {
	const secret = "shell-secret-token"
	journal := &memoryJournal{}
	runtime := newTestRuntime(t, Config{
		Backend: &scriptedModel{}, Journal: journal,
		Sanitize: func(text string) string { return strings.ReplaceAll(text, secret, "[REDACTED]") },
		UserShell: func(context.Context, string) (ToolOutput, error) {
			return ToolOutput{Content: model.TextContent("value=\xff" + secret)}, nil
		},
	})
	result, err := runtime.RunShell(context.Background(), "printf "+secret)
	if err != nil || result.Content.Text() != "value=�[REDACTED]" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	raw, err := json.Marshal(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("secret reached journal: %s", raw)
	}
}

func TestShellRejectedWhileRunLockIsHeld(t *testing.T) {
	runtime := newTestRuntime(t, Config{
		Backend:   &scriptedModel{},
		Journal:   &memoryJournal{},
		UserShell: func(context.Context, string) (ToolOutput, error) { return ToolOutput{}, nil },
	})
	runtime.runMu.Lock()
	defer runtime.runMu.Unlock()
	if _, err := runtime.RunPrivateShell(context.Background(), "true"); !errors.Is(err, ErrRunActive) {
		t.Fatalf("RunPrivateShell() error = %v", err)
	}
}
