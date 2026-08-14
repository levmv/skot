package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRuntimeRecordsOnlyEffectiveConfigurationChanges(t *testing.T) {
	journal := &memoryJournal{}
	read := Tool{Spec: ToolSpec{
		Name: "read", Description: "read files", InputSchema: json.RawMessage(`{"type":"object"}`),
	}, Run: func(context.Context, string) (ToolOutput, error) { return ToolOutput{}, nil }}
	edit := Tool{Spec: ToolSpec{
		Name: "edit", Description: "edit files", InputSchema: json.RawMessage(`{"type":"object"}`),
	}, Run: func(context.Context, string) (ToolOutput, error) { return ToolOutput{}, nil }}
	var requests []ModelRequest
	model := configurationModel{
		info: ModelInfo{
			Backend: "test", Provider: "provider", Model: "alpha", ContextWindow: 64_000,
			ContextWindowEstimated: true, Endpoint: "https://secret@example.test/v1?token=secret",
			MaxRequestBytes: 1_000_000, MaxCompletionBytes: 100_000,
		},
		complete: func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			requests = append(requests, request)
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "done"}}, StopReason: "stop"}, nil
		},
	}
	runtime := newTestRuntime(t, Config{
		Model: model, Journal: journal, Tools: []Tool{read}, Instructions: "follow secret",
		RequestPolicy: ModelRequestPolicy{MaxAttempts: 3}, MaxToolIterations: 7,
		Sanitize: func(text string) string { return strings.ReplaceAll(text, "secret", "[redacted]") },
		Metadata: ConfigurationMetadata{
			ToolProfile: "read-only", AwaitRequiredJobs: true,
			Sandbox: SandboxSnapshot{
				RequestedPolicy: "auto", EffectivePolicy: "masked", Container: "secret container", Network: "inherited",
			},
		},
	})

	if _, err := runtime.Run(context.Background(), "first", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "second", nil); err != nil {
		t.Fatal(err)
	}
	if got := countRecordKind(journal.snapshot(), RecordSessionConfigured); got != 1 {
		t.Fatalf("initial configuration records = %d, want 1", got)
	}
	state, err := runtime.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	configured := state.Configured
	if configured == nil {
		t.Fatal("replay lost effective configuration")
	}
	if configured.ModelContext.Instructions != "follow [redacted]" || configured.ModelContext.ToolProfile != "read-only" || len(configured.ModelContext.Tools) != 1 {
		t.Fatalf("model context snapshot = %#v", configured.ModelContext)
	}
	if configured.RuntimePolicy.ContextWindow != 64_000 || !configured.RuntimePolicy.ContextWindowEstimated || configured.RuntimePolicy.MaxModelAttempts != 3 || configured.RuntimePolicy.MaxToolIterations != 7 || configured.RuntimePolicy.MaxRequestBytes != 1_000_000 || configured.RuntimePolicy.MaxCompletionBytes != 100_000 || !configured.RuntimePolicy.AwaitRequiredJobs {
		t.Fatalf("runtime policy snapshot = %#v", configured.RuntimePolicy)
	}
	if configured.Environment.Endpoint != "https://[redacted]@example.test/v1?token=[redacted]" || configured.Environment.Sandbox.Container != "[redacted] container" {
		t.Fatalf("execution environment snapshot = %#v", configured.Environment)
	}
	if len(requests) != 2 || requests[0].Instructions != configured.ModelContext.Instructions || len(requests[0].Tools) != len(configured.ModelContext.Tools) {
		t.Fatalf("requests = %#v, configured = %#v", requests, configured.ModelContext)
	}

	if err := runtime.SetTools(context.Background(), []Tool{read, edit}, "edit"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetTools(context.Background(), []Tool{read, edit}, "edit"); err != nil {
		t.Fatal(err)
	}
	if got := countRecordKind(journal.snapshot(), RecordSessionConfigured); got != 2 {
		t.Fatalf("configuration records after tools = %d, want 2", got)
	}
	sandbox := SandboxSnapshot{RequestedPolicy: "workspace", EffectivePolicy: "workspace", Backend: "landlock", Network: "inherited"}
	if err := runtime.SetSandboxSnapshot(context.Background(), sandbox); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetSandboxSnapshot(context.Background(), sandbox); err != nil {
		t.Fatal(err)
	}
	if got := countRecordKind(journal.snapshot(), RecordSessionConfigured); got != 3 {
		t.Fatalf("configuration records after sandbox = %d, want 3", got)
	}

	next := configurationModel{
		info:     ModelInfo{Backend: "test", Provider: "provider", Model: "beta", ContextWindow: 128_000, Endpoint: "https://next.example/v1"},
		complete: model.complete,
	}
	if err := runtime.SwitchModel(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if got := countRecordKind(journal.snapshot(), RecordSessionConfigured); got != 4 {
		t.Fatalf("configuration records after model = %d, want 4", got)
	}
	state, err = runtime.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Configured == nil || state.Configured.RuntimePolicy.ContextWindow != 128_000 || state.Configured.Environment.Endpoint != "https://next.example/v1" || state.Configured.ModelContext.ToolProfile != "edit" || len(state.Configured.ModelContext.Tools) != 2 {
		t.Fatalf("latest configuration = %#v", state.Configured)
	}
	firstConfigured, firstRun := -1, -1
	for index, record := range journal.snapshot() {
		switch record.Kind {
		case RecordSessionConfigured:
			if firstConfigured < 0 {
				firstConfigured = index
			}
		case RecordRunStarted:
			if firstRun < 0 {
				firstRun = index
			}
		}
	}
	if firstConfigured < 0 || firstRun < 0 || firstConfigured >= firstRun {
		t.Fatalf("first configured/run indexes = %d/%d", firstConfigured, firstRun)
	}
}

type configurationModel struct {
	info     ModelInfo
	complete func(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error)
}

func (model configurationModel) Info() ModelInfo { return model.info }

func (model configurationModel) Complete(ctx context.Context, request ModelRequest, emit func(ModelStreamEvent)) (ModelResponse, error) {
	return model.complete(ctx, request, emit)
}

func (model configurationModel) ProjectModelItems(items []Item) []Item { return items }
