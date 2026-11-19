package agent

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"strings"
	"testing"

	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/anthropic"
	"github.com/levmv/skot/model/chatcompletions"
)

type contextTestBackend struct {
	modelFunc
	projection model.ItemProjection
}

func (backend contextTestBackend) ProjectModelItems(items []model.Item) []model.Item {
	if project := backend.projection.ProjectModelItems; project != nil {
		return project(items)
	}
	return items
}

func (backend contextTestBackend) OpaqueReasoningBytes(items []model.Item, bytes []int) {
	if size := backend.projection.OpaqueReasoningBytes; size != nil {
		size(items, bytes)
	}
}

func TestContextCountsReplayedOpaqueResponseOnce(t *testing.T) {
	signature := strings.Repeat("S", 4000)
	opaque := jsontext.Value(`{"type":"thinking","thinking":"checking","signature":"` + signature + `"}`)
	reasoning := model.Item{
		Kind: model.ItemReasoning, Text: "checking",
		ProviderData: []model.ProviderData{{Kind: "anthropic_messages.thinking_block.v1", Data: opaque}},
	}
	answer := model.Item{Kind: model.ItemAssistantText, Text: "answer"}
	for _, tc := range []struct {
		name       string
		status     model.UsageStatus
		stopReason string
		local      int
	}{
		{"complete group", model.UsageFinal, "stop", 780},
		{"partial usage", model.UsagePartial, "stop", 1000 + perMessageTokens + 2 + perMessageTokens},
		{"truncated response", model.UsageFinal, model.StopReasonOutputLimit, 1000 + perMessageTokens + 2 + perMessageTokens},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := contextTestBackend{
				projection: model.ItemProjection{ProjectModelItems: anthropic.ProjectModelItems, OpaqueReasoningBytes: anthropic.OpaqueReasoningBytes},
				modelFunc: func(context.Context, model.Request, func(model.StreamEvent)) (model.Response, error) {
					return model.Response{
						Items: []model.Item{reasoning, answer}, StopReason: tc.stopReason,
						Usage: model.Usage{Status: tc.status, Tokens: model.ReportedTokens{InputTokens: new(5000), OutputTokens: new(780)}},
					}, nil
				},
			}
			journal := &memoryJournal{}
			runtime := newTestRuntime(t, Config{Backend: backend, Journal: journal})
			_, err := runtime.Run(context.Background(), "ask", nil)
			if (err != nil) != (tc.stopReason == model.StopReasonOutputLimit) {
				t.Fatal(err)
			}
			report := runtime.SessionStatus().ContextReport
			if want := 1 + perMessageTokens + tc.local; report.HistoryTokens != want {
				t.Fatalf("history tokens = %d, want %d", report.HistoryTokens, want)
			}
			if tc.status == model.UsagePartial && report.MeasuredAdjustment != 0 {
				t.Fatalf("partial input adjusted report: %+v", report)
			}
			offline, ok, err := contextReportForJournal(journal.snapshot(), backend.projection)
			if err != nil || !ok || offline != report {
				t.Fatalf("offline = %+v, %v, %v, live = %+v", offline, ok, err, report)
			}
		})
	}
}

func TestContextMeasurementFollowsQueuedTurnProjection(t *testing.T) {
	reasoning := strings.Repeat("abcd", 1000)
	backend := contextTestBackend{
		projection: model.ItemProjection{ProjectModelItems: chatcompletions.ReasoningReplayCurrentTurn.ProjectModelItems},
		modelFunc: func(context.Context, model.Request, func(model.StreamEvent)) (model.Response, error) {
			return model.Response{
				Items: []model.Item{{Kind: model.ItemReasoning, Text: reasoning}, {Kind: model.ItemAssistantText, Text: "done"}},
				Usage: model.Usage{Status: model.UsageFinal, Tokens: model.ReportedTokens{InputTokens: new(30000)}},
			}, nil
		},
	}
	journal := &memoryJournal{}
	runtime := newTestRuntime(t, Config{Backend: backend, Journal: journal})
	ctx := context.Background()
	if _, err := runtime.Run(ctx, "first", nil); err != nil {
		t.Fatal(err)
	}
	before := runtime.SessionStatus().ContextReport
	if before.TotalInputTokens != 30000+1000+perMessageTokens+1+perMessageTokens {
		t.Fatalf("before queue = %+v", before)
	}
	if err := runtime.QueueInput("next"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.State(ctx); err != nil {
		t.Fatal(err)
	}
	after := runtime.SessionStatus().ContextReport
	if want := before.TotalInputTokens - 1000 - perMessageTokens + 1 + perMessageTokens; after.TotalInputTokens != want {
		t.Fatalf("queued report = %+v, want total %d", after, want)
	}
	if after.PendingTokens != 1+perMessageTokens || after.MeasuredAdjustment != before.MeasuredAdjustment {
		t.Fatalf("queued breakdown = %+v", after)
	}
	offline, ok, err := contextReportForJournal(journal.snapshot(), backend.projection)
	if err != nil || !ok || offline != before {
		t.Fatalf("offline = %+v, %v, %v, live before queue = %+v", offline, ok, err, before)
	}
}

func TestContextMeasurementRecoversAfterUnstableInput(t *testing.T) {
	journal := &memoryJournal{}
	counts := []int{643000, 127000, 619000, 620000}
	calls := 0
	backend := modelFunc(func(context.Context, model.Request, func(model.StreamEvent)) (model.Response, error) {
		input := counts[calls]
		calls++
		return model.Response{
			Items: []model.Item{{Kind: model.ItemAssistantText, Text: "done"}},
			Usage: model.Usage{Status: model.UsageFinal, Tokens: model.ReportedTokens{InputTokens: &input}},
		}, nil
	})
	runtime := newTestRuntime(t, Config{Backend: backend, Journal: journal})
	for index, want := range []int{643007, 28, 42, 620007} {
		if _, err := runtime.Run(context.Background(), "next", nil); err != nil {
			t.Fatal(err)
		}
		live := runtime.SessionStatus().ContextReport
		offline, ok, err := contextReportForJournal(journal.snapshot(), model.ItemProjection{})
		if err != nil || !ok || live.TotalInputTokens != want || offline != live {
			t.Fatalf("call %d: offline = %+v, %v, %v, live = %+v, want total %d", index+1, offline, ok, err, live, want)
		}
	}
}

func TestMeasuredInputTriggersCompactionWithSmallLocalHistory(t *testing.T) {
	journal := &memoryJournal{}
	calls := 0
	backend := modelFunc(func(_ context.Context, request model.Request, _ func(model.StreamEvent)) (model.Response, error) {
		if isCompactionRequest(request) {
			if calls != 2 || itemsContainText(request.Items, "recent") {
				t.Fatalf("unexpected summarization request: %+v", request.Items)
			}
			return model.Response{Items: []model.Item{{Kind: model.ItemAssistantText, Text: "old task summarized"}}}, nil
		}
		calls++
		response := model.Response{Items: []model.Item{{Kind: model.ItemAssistantText, Text: "done"}}}
		switch calls {
		case 1:
			response.Usage = model.Usage{Status: model.UsageFinal, Tokens: model.ReportedTokens{InputTokens: new(40000)}}
		case 2:
			response.Usage = model.Usage{Status: model.UsageFinal, Tokens: model.ReportedTokens{InputTokens: new(52000)}}
		case 3:
			if request.Summary != "old task summarized" {
				t.Fatal("next request lacked the summary after exceeding the measured budget")
			}
		}
		return response, nil
	})
	runtime := newTestRuntime(t, Config{
		Model: model.Info{BackendID: "test", Provider: "test", Model: "test", ContextWindow: 65536}, Backend: backend, Journal: journal,
	})
	for _, input := range []string{"old", "recent", strings.Repeat("a", 2400)} {
		if _, err := runtime.Run(context.Background(), input, nil); err != nil {
			t.Fatal(err)
		}
	}
	if report := runtime.SessionStatus().ContextReport; report.CompactionCount != 1 || report.MeasuredAdjustment != 0 {
		t.Fatalf("report after compaction = %+v", report)
	}
}

func TestContextRestoresLegacyInputMeasurement(t *testing.T) {
	journal := &memoryJournal{}
	backend := modelFunc(func(context.Context, model.Request, func(model.StreamEvent)) (model.Response, error) {
		return model.Response{
			Items: []model.Item{{Kind: model.ItemAssistantText, Text: "done"}},
			Usage: model.Usage{Status: model.UsageFinal, Tokens: model.ReportedTokens{InputTokens: new(1000)}},
		}, nil
	})
	runtime := newTestRuntime(t, Config{Backend: backend, Journal: journal})
	if _, err := runtime.Run(context.Background(), "ask", nil); err != nil {
		t.Fatal(err)
	}
	var legacy []Record
	for _, record := range journal.snapshot() {
		if isAuxiliaryRecordKind(record.Kind) {
			continue
		}
		if record.Kind == RecordModelResponse {
			response, err := record.decode[ModelResponseRecord]()
			if err != nil {
				t.Fatal(err)
			}
			response.AttemptID = ""
			record = recordForTest(t, record.Sequence, record.Kind, response)
		}
		legacy = append(legacy, record)
	}
	report, ok, err := contextReportForJournal(legacy, model.ItemProjection{})
	if err != nil || !ok || report.TotalInputTokens != 1000+1+perMessageTokens {
		t.Fatalf("legacy report = %+v, %v, %v", report, ok, err)
	}
}

func TestContextMeasuresOrdinaryRequestAfterToolError(t *testing.T) {
	journal := &memoryJournal{}
	backend := modelFunc(func(_ context.Context, request model.Request, _ func(model.StreamEvent)) (model.Response, error) {
		if request.Items[len(request.Items)-1].Kind == model.ItemToolResult {
			return model.Response{
				Items: []model.Item{{Kind: model.ItemAssistantText, Text: "done"}},
				Usage: model.Usage{Status: model.UsageFinal, Tokens: model.ReportedTokens{InputTokens: new(12000)}},
			}, nil
		}
		return model.Response{
			Items: []model.Item{{Kind: model.ItemToolCall, ToolCall: &model.ToolCall{Name: "inspect", RawArguments: "{}"}}},
			Usage: model.Usage{Status: model.UsageFinal, Tokens: model.ReportedTokens{InputTokens: new(10000)}},
		}, nil
	})
	runtime := newTestRuntime(t, Config{Backend: backend, Journal: journal, Tools: []Tool{{
		Spec: model.ToolSpec{Name: "inspect", InputSchema: jsontext.Value(`{"type":"object"}`)},
		Run: func(context.Context, string) (ToolOutput, error) {
			return ToolOutput{}, errors.New("tool inspect error: tool iteration limit reached after 2 iterations")
		},
	}}})
	if _, err := runtime.Run(t.Context(), "inspect", nil); err != nil {
		t.Fatal(err)
	}
	live := runtime.SessionStatus().ContextReport
	offline, ok, err := contextReportForJournal(journal.snapshot(), model.ItemProjection{})
	if err != nil || !ok || live.TotalInputTokens != 12000+1+perMessageTokens || offline != live {
		t.Fatalf("measurement after tool error: live = %+v, offline = %+v, %v, %v", live, offline, ok, err)
	}
}

func contextReportForJournal(records []Record, projection model.ItemProjection) (ContextReport, bool, error) {
	state, err := ReplayWithContext(records)
	if err != nil {
		return ContextReport{}, false, err
	}
	report, ok := state.ContextReport(projection)
	return report, ok, nil
}
