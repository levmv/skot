package agent

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/levmv/skot/model"
)

func TestReplayedResourceRequestsRequireCurrentHostAuthorization(t *testing.T) {
	allowed, other := t.TempDir(), t.TempDir()
	journal := &memoryJournal{}
	runtime := newTestRuntime(t, Config{
		Backend: &scriptedModel{steps: []modelStep{directModelResponse("done")}}, Journal: journal,
	})
	if _, err := runtime.RunInput(t.Context(), Input{Text: "saved input", ReadOnlyPaths: []string{allowed, other}}, nil); err != nil {
		t.Fatal(err)
	}
	for _, authorize := range []bool{false, true} {
		t.Run(fmt.Sprintf("host authorizer %t", authorize), func(t *testing.T) {
			var got []string
			calls := 0
			tool := Tool{
				Spec: model.ToolSpec{Name: "inspect", InputSchema: jsontext.Value(`{"type":"object"}`)},
				Run: func(ctx context.Context, _ string) (ToolOutput, error) {
					calls++
					got = ToolReadOnlyPaths(ctx)
					return ToolOutput{Content: model.TextContent("done")}, nil
				},
			}
			backend := modelFunc(func(_ context.Context, request model.Request, _ func(model.StreamEvent)) (model.Response, error) {
				if request.Items[len(request.Items)-1].Kind == model.ItemUserText {
					return model.Response{Items: []model.Item{{Kind: model.ItemToolCall, ToolCall: &model.ToolCall{Name: "inspect", RawArguments: `{}`}}}}, nil
				}
				return model.Response{Items: []model.Item{{Kind: model.ItemAssistantText, Text: "done"}}}, nil
			})
			config := Config{Backend: backend, Journal: &memoryJournal{records: journal.snapshot()}, Tools: []Tool{tool}}
			revoked := false
			if authorize {
				config.AuthorizeReadOnlyPaths = func(requested []string) []string {
					if !revoked && slices.Contains(requested, allowed) {
						return []string{allowed}
					}
					return nil
				}
			}
			runtime := newTestRuntime(t, config)
			for turn := range 2 {
				if _, err := runtime.Run(t.Context(), "continue", nil); err != nil {
					t.Fatal(err)
				}
				var want []string
				if authorize && !revoked {
					want = []string{allowed}
				}
				if calls != turn+1 || !slices.Equal(got, want) {
					t.Fatalf("tool received %q, want %q (calls %d)", got, want, calls)
				}
				revoked = true
			}
		})
	}
}

func TestCapturedInputSurvivesQueueRecallAndJournalReplay(t *testing.T) {
	journal := &memoryJournal{}
	runtime := newTestRuntime(t, Config{
		Backend:  &scriptedModel{steps: []modelStep{directModelResponse("done")}},
		Journal:  journal,
		Sanitize: func(text string) string { return strings.ReplaceAll(text, "secret", "[redacted]") },
	})
	original := Input{Text: "$review check changes", Instructions: []Instruction{{Key: "review", Text: "Review secret changes carefully.\n"}}}
	if err := runtime.QueueUserInput(original); err != nil {
		t.Fatal(err)
	}
	original.Instructions[0].Text = "changed after queuing"
	claimed, ok := runtime.ClaimQueuedInput()
	if !ok || len(claimed.Instructions) != 1 || claimed.Instructions[0].Text != "Review [redacted] changes carefully.\n" {
		t.Fatalf("claimed input = %#v, %v", claimed, ok)
	}
	if err := runtime.QueueUserInput(claimed); err != nil {
		t.Fatal(err)
	}
	recalled, ok := runtime.PopQueuedInput()
	if !ok || recalled.Text != claimed.Text || !slices.Equal(recalled.Instructions, claimed.Instructions) {
		t.Fatalf("recalled input = %#v, %v", recalled, ok)
	}
	if _, err := runtime.RunInput(context.Background(), recalled, nil); err != nil {
		t.Fatal(err)
	}
	state, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	item := state.Items[0]
	if !strings.Contains(item.Text, "Review [redacted] changes carefully.") || InputText(item) != claimed.Text {
		t.Fatalf("replayed input = %#v", item)
	}
	for _, record := range journal.snapshot() {
		if strings.Contains(string(record.Data), "secret") {
			t.Fatal("unredacted supplementary context reached the journal")
		}
	}
}

func TestInstructionRevisionsDoNotStrandSessionsAfterCompaction(t *testing.T) {
	var lastRequest model.Request
	backend := modelFunc(func(_ context.Context, request model.Request, _ func(model.StreamEvent)) (model.Response, error) {
		lastRequest = request
		return model.Response{Items: []model.Item{{Kind: model.ItemAssistantText, Text: "done"}}, StopReason: "stop"}, nil
	})
	version := func(index int) Input {
		return Input{Text: "$review", Instructions: []Instruction{{Key: "review", Text: fmt.Sprintf("Revision %d\n", index) + strings.Repeat("Do the documented checks.\n", 400)}}}
	}
	info := model.Info{BackendID: "test", Provider: "test", Model: "test", ContextWindow: 32 * 1024}
	t.Run("resumed with a smaller window", func(t *testing.T) {
		journal := &memoryJournal{}
		large := info
		large.ContextWindow = 256 * 1024
		runtime := newTestRuntime(t, Config{Model: large, Backend: backend, Journal: journal})
		for index := range 30 {
			if _, err := runtime.RunInput(t.Context(), version(index), nil); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := runtime.Compact(t.Context()); err != nil {
			t.Fatal(err)
		}
		runtime = newTestRuntime(t, Config{Model: info, Backend: backend, Journal: journal})
		if _, err := runtime.Run(t.Context(), "continue", nil); err != nil {
			t.Fatal(err)
		}
		state, err := runtime.State(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(state.Blocks) != 31 || !itemsContainText(lastRequest.Items, "Revision 29\n") || itemsContainText(lastRequest.Items, "Revision 0\n") {
			t.Fatal("compaction lost the latest version, retained obsolete instructions, or rewrote history")
		}
	})
	t.Run("queued revisions", func(t *testing.T) {
		runtime := newTestRuntime(t, Config{Model: info, Backend: backend, Journal: &memoryJournal{}})
		for index := range 12 {
			if err := runtime.QueueUserInput(version(index)); err != nil {
				t.Fatal(err)
			}
		}
		// An oversized active turn may fail, but must leave a session whose old
		// revisions can be compacted before the next ordinary request.
		_, _ = runtime.Run(t.Context(), "start", nil)
		if _, err := runtime.Run(t.Context(), "continue", nil); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRepeatedInstructionsFitTheExistingContextBudget(t *testing.T) {
	backend := &scriptedModel{
		info:  model.Info{BackendID: "test", Provider: "test", Model: "test", ContextWindow: 32 * 1024},
		steps: []modelStep{directModelResponse("done"), directModelResponse("done")},
	}
	runtime := newTestRuntime(t, Config{Backend: backend, Journal: &memoryJournal{}})
	input := Input{Text: "apply the procedure", Instructions: []Instruction{{Key: "procedure", Text: strings.Repeat("Check the result.\n", 3500)}}}
	// One copy fits this model; a second copy would require compaction even
	// though no old conversation can be removed yet.
	for range 2 {
		if _, err := runtime.RunInput(t.Context(), input, nil); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.SessionStatus().ContextReport.CompactionCount != 0 {
		t.Fatal("unchanged instructions forced compaction")
	}
}

func TestCapturedInstructionsSurviveRepeatedCompaction(t *testing.T) {
	journal := &memoryJournal{}
	var lastRequest model.Request
	backend := modelFunc(func(_ context.Context, request model.Request, _ func(model.StreamEvent)) (model.Response, error) {
		lastRequest = request
		// Deliberately omit the instructions from the model-generated summary.
		return model.Response{Items: []model.Item{{Kind: model.ItemAssistantText, Text: "done"}}, StopReason: "stop"}, nil
	})
	runtime := newTestRuntime(t, Config{Backend: backend, Journal: journal})
	first := Input{Text: compactionTestText("first question", 132*1024), Instructions: []Instruction{{Key: "first", Text: "Первое правило: объясняй причины решений.\n"}}}
	second := Input{Text: "second question", Instructions: []Instruction{{Key: "second", Text: "Second rule: include a concrete example.\n"}}}
	firstRule, secondRule := first.Instructions[0].Text, second.Instructions[0].Text
	for _, input := range []Input{first, second} {
		if _, err := runtime.RunInput(t.Context(), input, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runtime.Compact(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !itemsContainText(lastRequest.Items, firstRule) || itemsContainText(lastRequest.Items, secondRule) {
		t.Fatal("compaction did not respect the selected history boundary")
	}

	runtime = newTestRuntime(t, Config{Backend: backend, Journal: journal})
	if _, err := runtime.Run(t.Context(), compactionTestText("third question", 132*1024), nil); err != nil {
		t.Fatal(err)
	}
	if lastRequest.Items[0].Kind != model.ItemUserText || !strings.Contains(lastRequest.Items[0].Text, firstRule) ||
		!itemsContainText(lastRequest.Items, secondRule) || !itemsContainText(lastRequest.Items, second.Text) || itemsContainText(lastRequest.Items, "first question") {
		t.Fatal("compaction lost instructions, repeated tail context, or retained the old question")
	}
	const latest = "From now on, give examples only when I ask for one."
	if _, err := runtime.Run(t.Context(), latest, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Compact(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !itemsContainText(lastRequest.Items, firstRule) || !itemsContainText(lastRequest.Items, secondRule) || itemsContainText(lastRequest.Items, latest) {
		t.Fatal("second compaction lost earlier instructions or included the retained tail")
	}

	runtime = newTestRuntime(t, Config{Backend: backend, Journal: journal})
	repeated := Input{Text: "continue", Instructions: append(slices.Clone(first.Instructions), second.Instructions...)}
	if _, err := runtime.RunInput(t.Context(), repeated, nil); err != nil {
		t.Fatal(err)
	}
	var userMessages []string
	for _, item := range lastRequest.Items {
		if item.Kind == model.ItemUserText {
			userMessages = append(userMessages, item.Text)
		}
	}
	userText := strings.Join(userMessages, "\n")
	previous := -1
	for _, instructions := range []string{firstRule, secondRule, latest} {
		position := strings.Index(userText, instructions)
		if strings.Count(userText, instructions) != 1 || position <= previous {
			t.Fatal("instructions or later user directions changed text, order, or authority")
		}
		previous = position
	}
	if lastRequest.Instructions != "" || lastRequest.Summary != "done" ||
		itemsContainText(lastRequest.Items, "second question") || itemsContainText(lastRequest.Items, "third question") {
		t.Fatal("compacted instructions displaced later user directions or retained summarized history")
	}
	state, err := runtime.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.CompactionCount != 2 || InputText(state.Items[0]) != first.Text || state.VerbatimItems()[0].Text != latest {
		t.Fatal("retaining instructions changed the journal or the verbatim tail")
	}
}
