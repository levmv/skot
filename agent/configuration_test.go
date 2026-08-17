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
	modified := false
	runtime := newTestRuntime(t, Config{
		Model: model, Journal: journal, Tools: []Tool{read}, Instructions: "follow secret",
		RequestPolicy: ModelRequestPolicy{MaxAttempts: 3}, MaxToolIterations: 7,
		Sanitize: func(text string) string { return strings.ReplaceAll(text, "secret", "[redacted]") },
		Metadata: ConfigurationMetadata{
			ToolSet: "read-only", AwaitRequiredJobs: true,
			Build: BuildSnapshot{Version: "v1.2.3", Revision: "abc123", Modified: &modified},
			Scope: ScopeSnapshot{
				RequestedScope: "auto", EffectiveScope: "machine", ProtectedPathCount: 2,
				Container: "secret container", Network: "inherited",
			},
		},
	})
	modified = true

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
	if configured.ModelContext.Instructions != "follow [redacted]" || configured.ModelContext.ToolSet != "read-only" || len(configured.ModelContext.Tools) != 1 {
		t.Fatalf("model context snapshot = %#v", configured.ModelContext)
	}
	if configured.RuntimePolicy.ContextWindow != 64_000 || !configured.RuntimePolicy.ContextWindowEstimated || configured.RuntimePolicy.MaxModelAttempts != 3 || configured.RuntimePolicy.MaxToolIterations != 7 || configured.RuntimePolicy.MaxRequestBytes != 1_000_000 || configured.RuntimePolicy.MaxCompletionBytes != 100_000 || !configured.RuntimePolicy.AwaitRequiredJobs {
		t.Fatalf("runtime policy snapshot = %#v", configured.RuntimePolicy)
	}
	if configured.Environment.Endpoint != "https://[redacted]@example.test/v1?token=[redacted]" || configured.Environment.Scope.Container != "[redacted] container" {
		t.Fatalf("execution environment snapshot = %#v", configured.Environment)
	}
	if configured.Environment.Build.Version != "v1.2.3" || configured.Environment.Build.Revision != "abc123" || configured.Environment.Build.Modified == nil || *configured.Environment.Build.Modified {
		t.Fatalf("build snapshot = %#v", configured.Environment.Build)
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
	scope := ScopeSnapshot{RequestedScope: "workspace", EffectiveScope: "workspace", Backend: "landlock", Network: "inherited"}
	if err := runtime.SetScopeSnapshot(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetScopeSnapshot(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	if got := countRecordKind(journal.snapshot(), RecordSessionConfigured); got != 3 {
		t.Fatalf("configuration records after scope = %d, want 3", got)
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
	if state.Configured == nil || state.Configured.RuntimePolicy.ContextWindow != 128_000 || state.Configured.Environment.Endpoint != "https://next.example/v1" || state.Configured.ModelContext.ToolSet != "edit" || len(state.Configured.ModelContext.Tools) != 2 {
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

func TestRuntimeReplacesToolsAndProgramMetadataAtomically(t *testing.T) {
	program := Tool{
		Spec: ToolSpec{Name: "lookup", Description: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run:  func(context.Context, string) (ToolOutput, error) { return ToolOutput{}, nil },
	}
	runtime := newTestRuntime(t, Config{
		Model: &scriptedModel{}, Journal: &memoryJournal{}, Tools: []Tool{program},
	})
	resolved := ProgramToolSnapshot{
		Name: "lookup", Program: "/bin/lookup", Command: []string{"lookup"},
		Timeout: "1s", Background: "never",
	}
	if err := runtime.SetToolsWithProgramTools(context.Background(), []Tool{program}, "programs", []ProgramToolSnapshot{resolved}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.programTools) != 1 || runtime.programTools[0].Program != "/bin/lookup" || runtime.toolSet != "programs" {
		t.Fatalf("runtime configuration = %#v, %q", runtime.programTools, runtime.toolSet)
	}
	invalid := resolved
	invalid.Name = "inactive"
	invalid.Program = ""
	if err := runtime.SetToolsWithProgramTools(context.Background(), []Tool{program}, "broken", []ProgramToolSnapshot{invalid}); err == nil {
		t.Fatal("invalid inactive program metadata was accepted")
	}
	if len(runtime.programTools) != 1 || runtime.programTools[0].Name != "lookup" || runtime.toolSet != "programs" {
		t.Fatalf("failed update mutated runtime configuration = %#v, %q", runtime.programTools, runtime.toolSet)
	}
}

func TestCloneEffectiveConfigSnapshotDoesNotAliasBuildStatus(t *testing.T) {
	modified := false
	snapshot := EffectiveConfigSnapshot{
		Environment: ExecutionEnvironmentSnapshot{
			Build: BuildSnapshot{Modified: &modified},
		},
	}
	cloned := cloneEffectiveConfigSnapshot(snapshot)
	if cloned.Environment.Build.Modified == nil {
		t.Fatal("cloned build status is nil")
	}
	*cloned.Environment.Build.Modified = true
	if *snapshot.Environment.Build.Modified {
		t.Fatal("cloned build status aliases the source snapshot")
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
