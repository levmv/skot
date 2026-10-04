package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	modelapi "github.com/levmv/skot/model"
)

func finalUsageForTest(input, output int) modelapi.Usage {
	total := input + output
	return modelapi.Usage{Status: modelapi.UsageFinal, Tokens: modelapi.ReportedTokens{
		InputTokens: &input, OutputTokens: &output, TotalTokens: &total,
	}}
}

func TestRuntimeUsageIncludesFailedAttemptsWithoutDoubleCountingResponses(t *testing.T) {
	journal := &memoryJournal{}
	calls := 0
	model := modelFunc(func(context.Context, modelapi.Request, func(modelapi.StreamEvent)) (modelapi.Response, error) {
		calls++
		records := journal.snapshot()
		if records[len(records)-1].Kind != RecordModelAttemptStarted {
			t.Fatal("model was called before its attempt was recorded")
		}
		details := finalUsageForTest(10, calls)
		details.RequestID = "req-1"
		details.ResponseID = "gen-1"
		details.Costs = []modelapi.ReportedCost{{Kind: "account_charge", Amount: "0.000000000000012345678900", Currency: "USD"}}
		if calls == 1 {
			return modelapi.Response{Usage: details}, modelapi.MarkProviderFailure(errors.New("connection lost"))
		}
		return modelapi.Response{Items: []modelapi.Item{{Kind: modelapi.ItemAssistantText, Text: "done"}}, Usage: details, StopReason: "stop"}, nil
	})
	runtime := newTestRuntime(t, Config{
		Backend: model, Journal: journal,
		RequestPolicy: ModelRequestPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond},
	})
	result, err := runtime.Run(t.Context(), "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := runtime.Usage(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.Usage != (modelapi.TokenCounts{InputTokens: 20, OutputTokens: 3, TotalTokens: 23}) || len(report.Attempts) != 2 || report.LegacyResponses != 0 {
		t.Fatalf("report = %#v", report)
	}
	first, second := report.Attempts[0], report.Attempts[1]
	if first.AttemptID == "" || second.AttemptID == "" || first.AttemptID == second.AttemptID || first.RequestID == "" || first.RequestID != second.RequestID ||
		first.RunID != result.RunID || second.RunID != result.RunID || first.Attempt != 1 || second.Attempt != 2 ||
		first.Outcome != ModelAttemptFailed || second.Outcome != ModelAttemptCompleted {
		t.Fatalf("attempt identities = %#v", report.Attempts)
	}
	for _, attempt := range report.Attempts {
		if attempt.StartedSequence == 0 || attempt.FinishedSequence <= attempt.StartedSequence || attempt.StartedAt.IsZero() || attempt.FinishedAt.Before(attempt.StartedAt) {
			t.Fatalf("attempt lifecycle = %#v", attempt)
		}
		if attempt.Usage.ResponseID != "gen-1" || attempt.Usage.RequestID != "req-1" ||
			len(attempt.Usage.Costs) != 1 || attempt.Usage.Costs[0].Amount != "0.000000000000012345678900" {
			t.Fatalf("receipt = %#v", attempt.Usage)
		}
	}
	state, err := Replay(journal.snapshot())
	if err != nil || state.Usage.TotalTokens != 12 {
		t.Fatalf("accepted response usage = %#v, %v", state.Usage, err)
	}
	var semantic []Record
	for _, record := range journal.snapshot() {
		if !isAuxiliaryRecordKind(record.Kind) {
			semantic = append(semantic, record)
		}
	}
	withoutAuxiliary, err := Replay(semantic)
	if err != nil || !reflect.DeepEqual(withoutAuxiliary, state) {
		t.Fatalf("accounting changed conversation replay: %v", err)
	}
	legacy, err := ReplayUsage(semantic, 0)
	if err != nil || legacy.Complete || legacy.LegacyResponses != 1 || legacy.Usage != state.Usage {
		t.Fatalf("usage without auxiliary history = %#v, %v", legacy, err)
	}
}

func TestRuntimeUsagePreservesCancelledReceiptsAndCheckpoint(t *testing.T) {
	for _, status := range []modelapi.UsageStatus{modelapi.UsagePartial, modelapi.UsageFinal} {
		t.Run(string(status), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			journal := &memoryJournal{}
			var checkpoint uint64
			var runtime *Runtime
			model := modelFunc(func(context.Context, modelapi.Request, func(modelapi.StreamEvent)) (modelapi.Response, error) {
				started, err := runtime.Usage(t.Context(), 0)
				if err != nil || started.Complete || len(started.Attempts) != 1 || started.Attempts[0].Outcome != ModelAttemptStarted {
					t.Fatalf("in-flight usage = %#v, %v", started, err)
				}
				checkpoint = started.LastSequence
				cancel()
				details := finalUsageForTest(10, 0)
				details.Status = status
				return modelapi.Response{Usage: details}, context.Canceled
			})
			runtime = newTestRuntime(t, Config{Backend: model, Journal: journal})
			if _, err := runtime.Run(ctx, "task", nil); !errors.Is(err, context.Canceled) {
				t.Fatalf("run error = %v", err)
			}
			report, err := runtime.Usage(t.Context(), checkpoint)
			if err != nil || len(report.Attempts) != 1 || report.Usage.InputTokens != 10 || report.Complete != (status == modelapi.UsageFinal) {
				t.Fatalf("cancelled usage = %#v, %v", report, err)
			}
			attempt := report.Attempts[0]
			if attempt.Outcome != ModelAttemptCancelled || attempt.Usage.Status != status || attempt.Usage.Tokens.OutputTokens == nil || *attempt.Usage.Tokens.OutputTokens != 0 {
				t.Fatalf("cancelled attempt = %#v", attempt)
			}
			next, err := runtime.Usage(t.Context(), report.LastSequence)
			if err != nil || len(next.Attempts) != 0 || next.Usage != (modelapi.TokenCounts{}) {
				t.Fatalf("unchanged checkpoint = %#v, %v", next, err)
			}
		})
	}
}

func TestRuntimeStopsRequestsWhenAttemptCannotBeJournaled(t *testing.T) {
	for _, kind := range []RecordKind{RecordModelAttemptStarted, RecordModelAttemptFinished, RecordModelAttemptFailed} {
		t.Run(string(kind), func(t *testing.T) {
			journal := &failingJournal{memoryJournal: &memoryJournal{}, failKind: kind, remaining: 1}
			calls := 0
			model := modelFunc(func(context.Context, modelapi.Request, func(modelapi.StreamEvent)) (modelapi.Response, error) {
				calls++
				if kind == RecordModelAttemptFailed {
					return modelapi.Response{Usage: finalUsageForTest(10, 1)}, modelapi.MarkProviderFailure(errors.New("connection lost"))
				}
				return modelapi.Response{Usage: finalUsageForTest(10, 1), Items: []modelapi.Item{{Kind: modelapi.ItemAssistantText, Text: "done"}}}, nil
			})
			runtime := newTestRuntime(t, Config{Backend: model, Journal: journal})
			if _, err := runtime.Run(t.Context(), "task", nil); err == nil || !strings.Contains(err.Error(), "injected journal failure") {
				t.Fatalf("run error = %v", err)
			}
			wantCalls := 1
			if kind == RecordModelAttemptStarted {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("model calls = %d, want %d", calls, wantCalls)
			}
			if wantCalls != 0 {
				report, err := runtime.Usage(t.Context(), 0)
				if err != nil || report.Complete || len(report.Attempts) != 1 || report.Attempts[0].FinishedSequence != 0 || report.Attempts[0].Usage.Status != modelapi.UsageUnavailable {
					t.Fatalf("unrecorded finish = %#v, %v", report, err)
				}
			}
		})
	}
}

func TestReplayUsageMarksLegacyHistoryIncomplete(t *testing.T) {
	records := []Record{
		recordForTest(t, 1, RecordModelResponse, ModelResponseRecord{Usage: modelapi.TokenCounts{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}}),
		recordForTest(t, 2, RecordContextCompacted, ContextCompactedRecord{Usage: modelapi.TokenCounts{InputTokens: 20, OutputTokens: 3, TotalTokens: 23}}),
		recordForTest(t, 3, RecordModelAttemptFailed, ModelAttemptFailedRecord{RequestID: "old", Error: "connection lost"}),
	}
	report, err := ReplayUsage(records, 0)
	if err != nil || report.Complete || len(report.Attempts) != 0 || report.LegacyResponses != 2 || report.UntrackedAttempts != 1 ||
		report.Usage != (modelapi.TokenCounts{InputTokens: 30, OutputTokens: 5, TotalTokens: 35}) || report.LegacyUsage != report.Usage {
		t.Fatalf("legacy report = %#v, %v", report, err)
	}
}
