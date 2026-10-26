package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/levmv/skot/model"
)

func TestClearingInputContextRecoversAFullSession(t *testing.T) {
	journal := &memoryJournal{}
	var requests []model.Request
	backend := modelFunc(func(_ context.Context, request model.Request, _ func(model.StreamEvent)) (model.Response, error) {
		requests = append(requests, request)
		return model.Response{Items: []model.Item{{Kind: model.ItemAssistantText, Text: "done"}}, StopReason: "stop"}, nil
	})
	config := Config{Model: model.Info{BackendID: "test", Provider: "test", Model: "test", ContextWindow: 32 * 1024}, Backend: backend, Journal: journal}
	runtime := newTestRuntime(t, config)
	resource := t.TempDir()
	input := func(index int) Input {
		return Input{
			Text: fmt.Sprintf("question %d", index), ReadOnlyPaths: []string{resource},
			Instructions: []Instruction{{Key: fmt.Sprint(index), Text: fmt.Sprintf("Attached rule %d\n", index) + strings.Repeat("x", 20*1024)}},
		}
	}
	for index := range 4 {
		if _, err := runtime.RunInput(t.Context(), input(index), nil); err != nil {
			t.Fatal(err)
		}
	}
	_, err := runtime.RunInput(t.Context(), input(4), nil)
	var limit *ContextLimitError
	if !errors.As(err, &limit) || limit.AttachedTokens <= 0 {
		t.Fatalf("missing retained-instruction budget in error: %v", err)
	}
	before, err := runtime.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.QueueUserInput(input(5)); err != nil {
		t.Fatal(err)
	}
	requestCount := len(requests)
	if err := runtime.ClearInputContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != requestCount || !reflect.DeepEqual(before.Items, after.Items[:len(before.Items)]) {
		t.Fatal("clearing called the model or rewrote the transcript")
	}
	queued, ok := runtime.PopQueuedInput()
	if !ok || queued.Text != "question 5" || len(queued.Instructions) != 0 || len(queued.ReadOnlyPaths) != 0 {
		t.Fatal("clearing discarded queued text or retained its attachments")
	}
	runtime = newTestRuntime(t, config)
	for _, text := range []string{compactionTestText("later conversation", 40*1024), "recent question"} {
		if _, err := runtime.Run(t.Context(), text, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runtime.Compact(t.Context()); err != nil {
		t.Fatal(err)
	}
	runtime = newTestRuntime(t, config)
	if _, err := runtime.Run(t.Context(), "continue after resume", nil); err != nil {
		t.Fatal(err)
	}
	last := requests[len(requests)-1]
	if itemsContainText(last.Items, "Attached rule") || !itemsContainText(last.Items, inputContextClearedText) {
		t.Fatal("compaction or resume restored removed instructions or lost the removal notice")
	}
	if _, err := runtime.RunInput(t.Context(), input(0), nil); err != nil {
		t.Fatal(err)
	}
	if !itemsContainText(requests[len(requests)-1].Items, input(0).Instructions[0].Text) {
		t.Fatal("reattaching a cleared source sent a dangling reference")
	}
	if err := runtime.ClearInputContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(t.Context(), "continue after clearing again", nil); err != nil {
		t.Fatal(err)
	}
	if itemsContainText(requests[len(requests)-1].Items, "Attached rule") {
		t.Fatal("clearing after reattachment did not release the new context")
	}
}
