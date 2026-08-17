package ui

import (
	"bytes"
	"context"
	"testing"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/toolpolicy"
)

func TestFooterShowsModelToolsRootContextAndUsage(t *testing.T) {
	t.Setenv("SK_COLOR", "never")
	fake := &fakeAgent{
		model:   "deepseek/deepseek-v4-flash",
		toolSet: "edit",
		theme:   ThemeLight,
		state: agent.State{Usage: agent.ModelUsage{
			InputTokens: 1_200, CachedInputTokens: 340, OutputTokens: 890, TotalTokens: 2_090,
		}},
		contextReport: agent.ContextReport{
			Window: 100_000, InputLimit: 80_000, TotalInputTokens: 68_000,
		},
	}
	model, err := newScreenModel(context.Background(), fake, Config{Root: "/home/me/work"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	model.resize(160, 24)

	want := "deepseek/deepseek-v4-flash · ctx ~85% · ↑1.2k(340) ↓890 · edit · /home/me/work"
	if got := model.footerLine(); got != want {
		t.Fatalf("footer = %q, want %q", got, want)
	}
	frame := model.inlineFrame()
	if len(frame.dynamic) < 2 || frame.dynamic[len(frame.dynamic)-2] != "" || frame.dynamic[len(frame.dynamic)-1] != "  "+want {
		t.Fatalf("footer frame tail = %#v", frame.dynamic)
	}
}

func TestFooterShowsReasoningEffortOnlyWhenChosen(t *testing.T) {
	t.Setenv("SK_COLOR", "never")
	newFooter := func(t *testing.T, effort string) string {
		t.Helper()
		fake := &fakeAgent{model: "openai/gpt-5.2", toolSet: toolpolicy.ToolSetDefault, theme: ThemeLight, reasoningEffort: effort}
		model, err := newScreenModel(context.Background(), fake, Config{}, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		model.resize(160, 24)
		return model.footerLine()
	}
	if got := newFooter(t, "high"); got != "openai/gpt-5.2 high" {
		t.Fatalf("chosen effort footer = %q", got)
	}
	// The default effort is the empty string; naming it would assert a choice
	// the user never made.
	if got := newFooter(t, ""); got != "openai/gpt-5.2" {
		t.Fatalf("default effort footer = %q", got)
	}
}

func TestFooterNamesTheToolSetOnlyWhenItIsNotTheDefault(t *testing.T) {
	t.Setenv("SK_COLOR", "never")
	newFooter := func(t *testing.T, toolSet string) string {
		t.Helper()
		fake := &fakeAgent{model: "openai/gpt-5.2", toolSet: toolSet, theme: ThemeLight}
		model, err := newScreenModel(context.Background(), fake, Config{}, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		model.resize(160, 24)
		return model.footerLine()
	}
	if got := newFooter(t, toolpolicy.ToolSetReadOnly); got != "openai/gpt-5.2 · read-only" {
		t.Fatalf("restricted tool set footer = %q", got)
	}
	if got := newFooter(t, toolpolicy.ToolSetDefault); got != "openai/gpt-5.2" {
		t.Fatalf("default tool set footer = %q", got)
	}
}

func TestFooterShowsLiveEffectiveMachineScopeBeforeRoot(t *testing.T) {
	t.Setenv("SK_COLOR", "never")
	fake := &fakeAgent{
		model: "openai/gpt-5.2", toolSet: toolpolicy.ToolSetDefault, theme: ThemeLight,
		scope: "auto", effectiveScope: "machine",
	}
	model, err := newScreenModel(context.Background(), fake, Config{Root: "/work"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	model.resize(160, 24)
	if got := model.footerLine(); got != "openai/gpt-5.2 · scope: machine · /work" {
		t.Fatalf("machine footer = %q", got)
	}
	fake.effectiveScope = "workspace"
	if got := model.footerLine(); got != "openai/gpt-5.2 · /work" {
		t.Fatalf("updated workspace footer = %q", got)
	}
}

func TestFooterOmitsUnavailableContextAndUsage(t *testing.T) {
	t.Setenv("SK_COLOR", "never")
	fake := &fakeAgent{model: "openai/gpt", toolSet: toolpolicy.ToolSetReadOnly, theme: ThemeLight}
	model, err := newScreenModel(context.Background(), fake, Config{Root: "/work"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	model.resize(80, 24)
	if got := model.footerLine(); got != "openai/gpt · read-only · /work" {
		t.Fatalf("footer = %q", got)
	}
}

func TestFooterTruncatesRootInTheMiddle(t *testing.T) {
	t.Setenv("SK_COLOR", "never")
	fake := &fakeAgent{model: "m", toolSet: "edit", theme: ThemeLight}
	model, err := newScreenModel(context.Background(), fake, Config{Root: "/very/long/path/to/a/workspace"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	model.resize(23, 24)
	got := model.footerLine()
	if got != "m · edit · /ver…pace" {
		t.Fatalf("footer = %q", got)
	}
	if visibleLen(got) > model.contentWidth() {
		t.Fatalf("footer width = %d, content width = %d", visibleLen(got), model.contentWidth())
	}
}

func TestTurnTickRefreshesPublishedSessionStatus(t *testing.T) {
	fake := &fakeAgent{model: "m", theme: ThemeLight}
	model, err := newScreenModel(context.Background(), fake, Config{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	fake.contextReport = agent.ContextReport{Window: 100_000, InputLimit: 80_000, TotalInputTokens: 40_000}
	fake.state.Usage = agent.ModelUsage{InputTokens: 1_200, OutputTokens: 40, TotalTokens: 1_240}
	model.operation.kind = operationTurn
	model, cmd := model.update(turnTickMsg{})
	if cmd == nil {
		t.Fatal("active turn did not schedule the next tick")
	}
	if model.sessionStatus != fake.SessionStatus() {
		t.Fatalf("status after tick = %#v", model.sessionStatus)
	}
}

func TestCompactUsage(t *testing.T) {
	// Cached input is a subset of the input, so it nests inside it.
	if got := compactUsage(agent.ModelUsage{InputTokens: 1_200, CachedInputTokens: 340, OutputTokens: 890}); got != "↑1.2k(340) ↓890" {
		t.Fatalf("compact usage = %q", got)
	}
	if got := compactUsage(agent.ModelUsage{InputTokens: 12_300, OutputTokens: 1_250_000}); got != "↑12.3k ↓1.2m" {
		t.Fatalf("compact usage without cache = %q", got)
	}
	if got := compactUsage(agent.ModelUsage{}); got != "" {
		t.Fatalf("zero usage = %q", got)
	}
	if got := compactUsage(agent.ModelUsage{ReasoningTokens: 4, TotalTokens: 4}); got != "" {
		t.Fatalf("usage without displayed counters = %q", got)
	}
	if got := formatTokenCount(999_999); got != "1m" {
		t.Fatalf("promoted token count = %q", got)
	}
}
