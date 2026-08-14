package agent

import (
	"context"
	"strings"
	"testing"
)

type reasoningReplay int

const (
	replayNone reasoningReplay = iota
	replayCurrentTurn
	replayAll
)

// reasoningModel answers with a long reasoning item and records the requests it
// received. Its projection policies mirror the real Chat Completions ones.
type reasoningModel struct {
	replay   reasoningReplay
	requests []ModelRequest
}

func (model *reasoningModel) Info() ModelInfo {
	return ModelInfo{Backend: "test", Provider: "test", Model: "test", ProviderStateContract: "test.reasoning.v1"}
}

func (model *reasoningModel) Complete(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
	model.requests = append(model.requests, request)
	return ModelResponse{Items: []Item{
		{Kind: ItemReasoning, Text: strings.Repeat("weighing the options ", 256)},
		{Kind: ItemAssistantText, Text: "answer"},
	}}, nil
}

func (model *reasoningModel) ProjectModelItems(items []Item) []Item {
	if model.replay == replayAll {
		return items
	}
	lastUser := -1
	for index, item := range items {
		if item.Kind == ItemUserText {
			lastUser = index
		}
	}
	kept := items[:0]
	for index, item := range items {
		if item.Kind == ItemReasoning && (model.replay == replayNone || index <= lastUser) {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

func replayName(replay reasoningReplay) string {
	switch replay {
	case replayNone:
		return "route drops reasoning"
	case replayCurrentTurn:
		return "route replays the current turn"
	default:
		return "route replays every turn"
	}
}

func TestContextEstimateMatchesTheProjectedRequest(t *testing.T) {
	for _, replay := range []reasoningReplay{replayNone, replayCurrentTurn, replayAll} {
		t.Run(replayName(replay), func(t *testing.T) {
			journal := &memoryJournal{}
			model := &reasoningModel{replay: replay}
			runtime := newTestRuntime(t, Config{Model: model, Journal: journal})
			if _, err := runtime.Run(context.Background(), "question", nil); err != nil {
				t.Fatal(err)
			}
			state, err := Replay(journal.snapshot())
			if err != nil {
				t.Fatal(err)
			}
			request, err := runtime.modelRequest(state)
			if err != nil {
				t.Fatal(err)
			}

			replayed := false
			for _, item := range request.Items {
				if item.Kind == ItemReasoning {
					replayed = true
				}
			}
			// The answer's reasoning follows the last user message, so only a
			// route without any replay policy drops it here.
			if replayed != (replay != replayNone) {
				t.Fatalf("request replayed reasoning = %v: %#v", replayed, request.Items)
			}
			report := runtime.contextReport(state)
			if report.HistoryTokens != estimateItemsTokens(request.Items) {
				t.Fatalf("history estimate = %d, request items estimate = %d",
					report.HistoryTokens, estimateItemsTokens(request.Items))
			}
		})
	}
}

// A run is estimated before its input is journaled, so pending input has to be
// projected as the user message it is about to become. Otherwise a route that
// replays only the turn in progress is charged for reasoning that the very next
// request drops, and the run can compact or refuse while it still fits.
func TestContextEstimateProjectsPendingInputAsTheNextUserMessage(t *testing.T) {
	for _, replay := range []reasoningReplay{replayNone, replayCurrentTurn, replayAll} {
		t.Run(replayName(replay), func(t *testing.T) {
			journal := &memoryJournal{}
			model := &reasoningModel{replay: replay}
			runtime := newTestRuntime(t, Config{Model: model, Journal: journal})
			if _, err := runtime.Run(context.Background(), "question", nil); err != nil {
				t.Fatal(err)
			}
			state, err := Replay(journal.snapshot())
			if err != nil {
				t.Fatal(err)
			}
			estimate := runtime.contextReport(state, "next question")

			if _, err := runtime.Run(context.Background(), "next question", nil); err != nil {
				t.Fatal(err)
			}
			if len(model.requests) != 2 {
				t.Fatalf("model requests = %d", len(model.requests))
			}
			sent := estimateItemsTokens(model.requests[1].Items)
			if estimate.HistoryTokens+estimate.PendingTokens != sent {
				t.Fatalf("estimate before the run = %d (history %d + pending %d), request = %d",
					estimate.HistoryTokens+estimate.PendingTokens,
					estimate.HistoryTokens, estimate.PendingTokens, sent)
			}
		})
	}
}
