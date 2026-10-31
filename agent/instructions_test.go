package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/levmv/skot/model"
)

func TestInstructionsReloadAtCompactionAndResume(t *testing.T) {
	journal := &memoryJournal{}
	source, loads := "initial secret", 0
	var requests []model.Request
	config := Config{
		Journal: journal, Instructions: "base secret",
		Sanitize: func(text string) string { return strings.ReplaceAll(text, "secret", "[redacted]") },
		LoadInstructions: func(_ context.Context, base string) (string, error) {
			loads++
			if base != "base [redacted]" {
				t.Fatalf("callback base = %q", base)
			}
			if source == "" {
				return "", nil
			}
			return base + "\n\n" + source, nil
		},
		Backend: modelFunc(func(_ context.Context, request model.Request, _ func(model.StreamEvent)) (model.Response, error) {
			state, err := Replay(journal.snapshot())
			if err != nil || state.Configured == nil || state.Configured.ModelContext.Instructions != request.Instructions {
				t.Fatalf("request instructions were not recorded: %q, %v", request.Instructions, err)
			}
			requests = append(requests, request)
			return model.Response{Items: []model.Item{{Kind: model.ItemAssistantText, Text: "done"}}}, nil
		}),
	}
	runtime := newTestRuntime(t, config)
	if _, err := runtime.State(t.Context()); err != nil || loads != 0 {
		t.Fatalf("reading state loaded instructions: loads=%d error=%v", loads, err)
	}
	if _, err := runtime.Run(t.Context(), compactionTestText("old question", 132*1024), nil); err != nil {
		t.Fatal(err)
	}
	source = "updated instructions"
	if _, err := runtime.Run(t.Context(), "recent question", nil); err != nil {
		t.Fatal(err)
	}
	if loads != 1 || requests[0].Instructions != "base [redacted]\n\ninitial [redacted]" || requests[1].Instructions != requests[0].Instructions {
		t.Fatal("ordinary turns did not keep the loaded instructions")
	}
	if _, err := runtime.Compact(t.Context()); err != nil {
		t.Fatal(err)
	}
	if loads != 2 || requests[2].Instructions != requests[0].Instructions {
		t.Fatal("compaction did not use the previous instructions before reloading")
	}
	if _, err := runtime.Run(t.Context(), "continue", nil); err != nil {
		t.Fatal(err)
	}
	want := "base [redacted]\n\n" + source
	if loads != 2 || requests[3].Instructions != want {
		t.Fatal("continuation did not use the refreshed instructions")
	}
	if got := runtime.SessionStatus().ContextReport.InstructionTokens; got != estimateTextTokens(want)+perMessageTokens {
		t.Fatalf("instruction token estimate = %d", got)
	}
	// An empty source replaces the old prompt too, including on resume.
	source = ""
	runtime = newTestRuntime(t, config)
	if _, err := runtime.State(t.Context()); err != nil || loads != 2 {
		t.Fatalf("inspecting a resumed session loaded instructions: %d, %v", loads, err)
	}
	if _, err := runtime.Run(t.Context(), "resume", nil); err != nil {
		t.Fatal(err)
	}
	if loads != 3 || requests[4].Instructions != "" || runtime.SessionStatus().ContextReport.InstructionTokens != 0 {
		t.Fatal("resume did not load the current, empty instructions")
	}
	for _, record := range journal.snapshot() {
		if strings.Contains(string(record.Data), "secret") {
			t.Fatal("unredacted instructions reached the journal")
		}
	}
}

func TestInstructionRefreshFailurePreservesCompactionAndRetries(t *testing.T) {
	for _, failure := range []string{"load", "journal"} {
		t.Run(failure, func(t *testing.T) {
			journal := &failingJournal{memoryJournal: &memoryJournal{}}
			source, failLoad, calls := "initial instructions", true, 0
			runtime := newTestRuntime(t, Config{
				Journal: journal,
				LoadInstructions: func(context.Context, string) (string, error) {
					if failLoad {
						return "", errors.New("source unavailable")
					}
					return source, nil
				},
				Backend: modelFunc(func(_ context.Context, request model.Request, _ func(model.StreamEvent)) (model.Response, error) {
					calls++
					return model.Response{Items: []model.Item{{Kind: model.ItemAssistantText, Text: "done"}}}, nil
				}),
			})
			if _, err := runtime.Run(t.Context(), "first attempt", nil); err == nil || !strings.Contains(err.Error(), "source unavailable") || calls != 0 {
				t.Fatalf("initial load failure = %v, model calls = %d", err, calls)
			}
			failLoad = false
			for _, input := range []string{compactionTestText("old question", 132*1024), "recent question"} {
				if _, err := runtime.Run(t.Context(), input, nil); err != nil {
					t.Fatal(err)
				}
			}
			source = "updated instructions"
			wantError := "source unavailable"
			if failure == "load" {
				failLoad = true
			} else {
				journal.failKind, journal.remaining = RecordSessionConfigured, 1
				wantError = "injected journal failure"
			}
			if _, err := runtime.Compact(t.Context()); err == nil || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("refresh failure = %v", err)
			}
			state, err := runtime.State(t.Context())
			if err != nil || state.CompactionCount != 1 || state.Configured.ModelContext.Instructions != "initial instructions" {
				t.Fatalf("refresh failure lost the compaction or changed instructions: %#v, %v", state.Configured, err)
			}
			failLoad = false
			if _, err := runtime.Run(t.Context(), "retry", nil); err != nil {
				t.Fatal(err)
			}
			state, err = runtime.State(t.Context())
			if err != nil || calls != 4 || state.CompactionCount != 1 || state.Configured.ModelContext.Instructions != source {
				t.Fatalf("retry did not reuse the summary and refresh instructions: calls=%d, state=%#v, error=%v", calls, state.Configured, err)
			}
		})
	}
}

func TestOversizedInstructionsCanBeRefreshedOnResume(t *testing.T) {
	source := strings.Repeat("x", 100_000)
	calls := 0
	config := Config{
		Journal: &memoryJournal{},
		Model:   model.Info{BackendID: "test", Provider: "test", Model: "test", ContextWindow: 16 * 1024},
		LoadInstructions: func(context.Context, string) (string, error) {
			return source, nil
		},
		Backend: modelFunc(func(context.Context, model.Request, func(model.StreamEvent)) (model.Response, error) {
			calls++
			return model.Response{Items: []model.Item{{Kind: model.ItemAssistantText, Text: "done"}}}, nil
		}),
	}
	runtime := newTestRuntime(t, config)
	_, err := runtime.Run(t.Context(), "first attempt", nil)
	var limit *ContextLimitError
	if !errors.As(err, &limit) || calls != 0 {
		t.Fatalf("oversized instructions: error=%v, model calls=%d", err, calls)
	}
	if got := runtime.SessionStatus().ContextReport.InstructionTokens; got != limit.Report.InstructionTokens {
		t.Fatalf("status instruction tokens=%d, rejected request=%d", got, limit.Report.InstructionTokens)
	}
	source = "short instructions"
	runtime = newTestRuntime(t, config)
	if _, err := runtime.Run(t.Context(), "retry", nil); err != nil || calls != 1 {
		t.Fatalf("resume after correcting instructions: error=%v, model calls=%d", err, calls)
	}
}
