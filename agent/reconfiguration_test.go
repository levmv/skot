package agent

import (
	"context"
	"errors"
	"testing"
)

func TestRuntimeAppliesSelectionsAfterTheCurrentResponseAndTools(t *testing.T) {
	for _, ending := range []string{"tools", "answer", "cancel"} {
		t.Run(ending, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			journal := &memoryJournal{}
			started := make(chan ModelRequest, 1)
			release := make(chan struct{})
			oldToolCalls := 0
			oldTool := testRuntimeTool("edit")
			oldTool.Run = func(context.Context, string) (ToolOutput, error) {
				oldToolCalls++
				return ToolOutput{Content: TextContent("edited")}, nil
			}
			first := &scriptedModel{
				info: ModelInfo{BackendID: "backend.a", Provider: "provider-a", Model: "alpha"},
				steps: []modelStep{func(ctx context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
					started <- request
					select {
					case <-ctx.Done():
						return ModelResponse{}, ctx.Err()
					case <-release:
					}
					items := []Item{{Kind: ItemReasoning, Text: "old model reasoning"}}
					if ending == "tools" {
						items = append(items, Item{Kind: ItemToolCall, ToolCall: &ToolCall{ID: "edit-1", Name: "edit", RawArguments: `{}`}})
					} else {
						items = append(items, Item{Kind: ItemAssistantText, Text: "first answer"})
					}
					return ModelResponse{Items: items}, nil
				}},
			}
			var nextRequest ModelRequest
			second := &scriptedModel{
				info: ModelInfo{BackendID: "backend.b", Provider: "provider-b", Model: "beta"},
				steps: []modelStep{func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
					nextRequest = request
					return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "second answer"}}}, nil
				}},
			}
			runtime := newTestRuntime(t, Config{Backend: first, Journal: journal, Tools: []Tool{oldTool}})
			done := make(chan error, 1)
			go func() {
				_, err := runtime.Run(ctx, "work", nil)
				done <- err
			}()
			var firstRequest ModelRequest
			select {
			case firstRequest = <-started:
			case err := <-done:
				t.Fatalf("run ended before its first request: %v", err)
			}
			if err := runtime.SwitchModel(t.Context(), second.testModelInfo(), second); err != nil {
				t.Fatal(err)
			}
			if err := runtime.SetTools(t.Context(), []Tool{testRuntimeTool("read")}, "read-only"); err != nil {
				t.Fatal(err)
			}
			state, err := Replay(journal.snapshot())
			if err != nil {
				t.Fatal(err)
			}
			if state.Selection.Model != "alpha" || toolNames(state.Configured.ModelContext.Tools) != "edit" {
				t.Fatal("selection changed before the outstanding response finished")
			}
			if ending == "cancel" {
				cancel()
			} else {
				close(release)
			}
			if err := <-done; err != nil && !(ending == "cancel" && errors.Is(err, context.Canceled)) {
				t.Fatal(err)
			}
			state, err = Replay(journal.snapshot())
			if err != nil {
				t.Fatal(err)
			}
			if state.Selection.Model != "beta" || toolNames(state.Configured.ModelContext.Tools) != "read" || state.hasUnfinishedWork() {
				t.Fatalf("completed selection = %#v, tools = %#v", state.Selection, state.Configured)
			}
			if ending == "tools" {
				if oldToolCalls != 1 {
					t.Fatalf("old response's tool ran %d times", oldToolCalls)
				}
			} else if _, err := runtime.Run(t.Context(), "continue", nil); err != nil {
				t.Fatal(err)
			}
			if nextRequest.ProviderEpoch == "" || nextRequest.ProviderEpoch == firstRequest.ProviderEpoch || toolNames(nextRequest.Tools) != "read" {
				t.Fatalf("next request = %#v", nextRequest)
			}
			for _, item := range nextRequest.Items {
				if item.Kind == ItemReasoning {
					t.Fatal("previous model's reasoning leaked across the selection")
				}
			}
		})
	}
}
