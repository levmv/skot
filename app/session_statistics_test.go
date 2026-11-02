package app_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/app"
	"github.com/levmv/skot/internal/modelconfig"
	"github.com/levmv/skot/internal/session"
	"github.com/levmv/skot/model"
)

type statisticsBackend struct{ model.Backend }

func (statisticsBackend) Complete(context.Context, model.Request, func(model.StreamEvent)) (model.Response, error) {
	return model.Response{Items: []model.Item{
		{Kind: model.ItemReasoning, Text: "Reasoning retained according to the saved route."},
		{Kind: model.ItemAssistantText, Text: "Done"},
	}, Usage: model.Usage{Status: model.UsageFinal, Tokens: model.ReportedTokens{
		InputTokens: new(1200), OutputTokens: new(80),
	}}}, nil
}

func TestReadSessionStatisticsMatchesSavedRoutes(t *testing.T) {
	for _, test := range []struct {
		name, uri, baseURL string
		archived           bool
		unknownContract    bool
		unknownBackend     bool
	}{
		{name: "all reasoning", uri: "deepseek/deepseek-v4-flash"},
		{name: "current turn reasoning", uri: "openai/unknown-model"},
		{name: "custom endpoint", uri: "deepseek/deepseek-v4-flash", baseURL: "https://custom.invalid/v1"},
		{name: "explicit default endpoint", uri: "deepseek/deepseek-v4-flash", baseURL: "https://api.deepseek.com/v1"},
		{name: "codex", uri: "openai-codex/gpt-6-astra"},
		{name: "anthropic", uri: "anthropic/claude-sonnet-4-6"},
		{name: "google", uri: "google/gemini-2.5-pro"},
		{name: "archived configuration", uri: "deepseek/deepseek-v4-flash", archived: true},
		{name: "unknown replay contract", uri: "deepseek/deepseek-v4-flash", unknownContract: true},
		{name: "unknown backend", uri: "deepseek/deepseek-v4-flash", unknownBackend: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			route, err := modelconfig.Resolve(test.uri, "", modelconfig.Overrides{BaseURL: test.baseURL}, modelconfig.Enrichment{})
			if err != nil {
				t.Fatal(err)
			}
			info, err := modelconfig.Info(route)
			if err != nil {
				t.Fatal(err)
			}
			backend, _, err := modelconfig.BuildConfiguredBackend(route, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if test.archived {
				// A saved configuration may no longer be usable for generation;
				// the versioned replay policy still describes its history.
				info.Model, info.ReasoningEffort, info.Endpoint = "former-model", "former-effort", "[redacted]"
			}
			if test.unknownContract {
				info.ProviderStateContract = "future-replay-contract"
			}
			if test.unknownBackend {
				info.BackendID = "external.deepseek"
			}
			home := t.TempDir()
			journal, id, err := session.Create(home)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = journal.Close() })
			runtime, err := agent.New(agent.Config{Model: info, Backend: statisticsBackend{backend}, Journal: journal, SessionID: id})
			if err != nil {
				t.Fatal(err)
			}
			for _, prompt := range []string{"First request", "Second request"} {
				if _, err := runtime.Run(t.Context(), prompt, nil); err != nil {
					t.Fatal(err)
				}
			}
			stats, err := app.ReadSessionStatistics(t.Context(), home, id)
			if err != nil {
				t.Fatal(err)
			}
			if !stats.Accounting.Complete || stats.Accounting.Usage.InputTokens != 2400 || stats.Accounting.Usage.OutputTokens != 160 ||
				len(stats.Accounting.Attempts) != 2 || stats.LastSequence != stats.Accounting.LastSequence {
				t.Fatalf("saved accounting = %+v", stats)
			}
			if test.unknownContract || test.unknownBackend {
				if stats.ContextReport != nil || stats.ContextError == "" {
					t.Fatalf("unsupported replay policy produced an estimate: %+v", stats)
				}
				return
			}
			want := runtime.SessionStatus().ContextReport
			if stats.ContextReport == nil || *stats.ContextReport != want || stats.ContextError != "" {
				t.Fatalf("saved context = %+v (%s), want %+v", stats.ContextReport, stats.ContextError, want)
			}
			if stats.Model != info.Provider+"/"+info.Model || stats.ContextWindowEstimated != info.ContextWindowEstimated {
				t.Fatalf("saved model metadata = %+v", stats)
			}
		})
	}
}

func TestReadSessionStatisticsBeforeConfiguration(t *testing.T) {
	home := t.TempDir()
	journal, id, err := session.Create(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	data, err := json.Marshal(agent.SessionStartedRecord{SessionID: id, SchemaVersion: agent.JournalSchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	last, err := journal.Append(t.Context(), agent.PendingRecord{Kind: agent.RecordSessionStarted, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := app.ReadSessionStatistics(t.Context(), home, id)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ContextReport != nil || stats.ContextError != "" || stats.Model != "" || !stats.Accounting.Complete ||
		len(stats.Accounting.Attempts) != 0 || stats.LastSequence != last.Sequence || !stats.UpdatedAt.Equal(last.Time) {
		t.Fatalf("unconfigured statistics = %+v", stats)
	}
}

func TestReadSessionStatisticsPreservesOwnershipAndIncompleteTail(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "statistics-test-key")
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		cache := `,"prompt_tokens_details":{"cached_tokens":900}`
		if calls.Add(1) > 1 {
			cache = ""
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Done\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1200,\"completion_tokens\":80%s}}\n\ndata: [DONE]\n\n", cache)
	}))
	t.Cleanup(provider.Close)
	home := t.TempDir()
	a, err := app.Open(t.Context(), app.Config{Home: home, Root: t.TempDir(), Interactive: true,
		ModelURI: "deepseek/deepseek-flash", ModelExplicit: true, BaseURL: provider.URL,
		ContextWindow: 32 * 1024, ToolSet: "none", ToolSetExplicit: true, SystemPrompt: "Saved instructions"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	for _, prompt := range []string{"First request", "Second request"} {
		if _, err := a.Run(t.Context(), prompt, nil); err != nil {
			t.Fatal(err)
		}
	}
	live := a.SessionStatus().ContextReport
	id := a.SessionID()
	provider.Close()
	t.Setenv("DEEPSEEK_API_KEY", "")
	journal := filepath.Join(home, "sessions", a.SessionID(), "events.jsonl")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := app.ReadSessionStatistics(t.Context(), home, a.SessionID())
	if err != nil || stats.ContextReport == nil || *stats.ContextReport != live {
		t.Fatalf("saved/live context: %+v, want %+v, %v", stats, live, err)
	}
	if !stats.Accounting.Complete || stats.Accounting.Usage.InputTokens != 2400 || stats.Accounting.Usage.OutputTokens != 160 || len(stats.Accounting.Attempts) != 2 {
		t.Fatalf("accounting: %+v", stats.Accounting)
	}
	attempts := stats.Accounting.Attempts
	if cached := attempts[0].Usage.Tokens.CachedInputTokens; cached == nil || *cached != 900 || attempts[1].Usage.Tokens.CachedInputTokens != nil {
		t.Fatalf("cache presence lost: %+v", attempts)
	}
	view, err := app.ReadSession(t.Context(), home, a.SessionID())
	if err != nil || !view.Summary.Locked || calls.Load() != 2 {
		t.Fatalf("reader changed ownership or called provider: %+v, %v", view.Summary, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	withTail := append(bytes.Clone(before), []byte(`{"sequence":`)...)
	if err := os.WriteFile(journal, withTail, 0600); err != nil {
		t.Fatal(err)
	}
	modified := stats.UpdatedAt.Add(time.Hour)
	if err := os.Chtimes(journal, modified, modified); err != nil {
		t.Fatal(err)
	}
	after, err := app.ReadSessionStatistics(t.Context(), home, id)
	if err != nil || after.LastSequence != stats.LastSequence || after.ContextReport == nil || *after.ContextReport != live || after.Accounting.Usage != stats.Accounting.Usage {
		t.Fatalf("incomplete tail changed statistics: %+v, %v", after, err)
	}
	if !after.UpdatedAt.Equal(stats.UpdatedAt) {
		t.Fatalf("incomplete tail changed snapshot time: %v, want %v", after.UpdatedAt, stats.UpdatedAt)
	}
	data, err := os.ReadFile(journal)
	if err != nil || !bytes.Equal(data, withTail) {
		t.Fatalf("reader repaired or wrote journal: %v", err)
	}
}
