package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/levmv/skot/model"
)

func TestRuntimeAppliesSelectionsAfterTheCurrentResponseAndTools(t *testing.T) {
	for _, ending := range []string{"tools", "answer", "cancel"} {
		t.Run(ending, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			journal := &memoryJournal{}
			started := make(chan model.Request, 1)
			release := make(chan struct{})
			oldToolCalls := 0
			oldTool := testRuntimeTool("edit")
			oldTool.Run = func(context.Context, string) (ToolOutput, error) {
				oldToolCalls++
				return ToolOutput{Content: model.TextContent("edited")}, nil
			}
			first := &scriptedModel{
				info: model.Info{BackendID: "backend.a", Provider: "provider-a", Model: "alpha"},
				steps: []modelStep{func(ctx context.Context, request model.Request, _ func(model.StreamEvent)) (model.Response, error) {
					started <- request
					select {
					case <-ctx.Done():
						return model.Response{}, ctx.Err()
					case <-release:
					}
					items := []model.Item{{Kind: model.ItemReasoning, Text: "old model reasoning"}}
					if ending == "tools" {
						items = append(items, model.Item{Kind: model.ItemToolCall, ToolCall: &model.ToolCall{ID: "edit-1", Name: "edit", RawArguments: `{}`}})
					} else {
						items = append(items, model.Item{Kind: model.ItemAssistantText, Text: "first answer"})
					}
					return model.Response{Items: items}, nil
				}},
			}
			var nextRequest model.Request
			second := &scriptedModel{
				info: model.Info{BackendID: "backend.b", Provider: "provider-b", Model: "beta"},
				steps: []modelStep{func(_ context.Context, request model.Request, _ func(model.StreamEvent)) (model.Response, error) {
					nextRequest = request
					return model.Response{Items: []model.Item{{Kind: model.ItemAssistantText, Text: "second answer"}}}, nil
				}},
			}
			runtime := newTestRuntime(t, Config{Backend: first, Journal: journal, Tools: []Tool{oldTool}})
			done := make(chan error, 1)
			go func() {
				_, err := runtime.Run(ctx, "work", nil)
				done <- err
			}()
			var firstRequest model.Request
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
				if item.Kind == model.ItemReasoning {
					t.Fatal("previous model's reasoning leaked across the selection")
				}
			}
		})
	}
}
