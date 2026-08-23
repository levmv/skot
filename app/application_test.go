package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/canonicalpath"
	"github.com/levmv/skot/internal/session"
	"github.com/levmv/skot/internal/state"
	"github.com/levmv/skot/internal/toolpolicy"
	workspacetools "github.com/levmv/skot/tools"
)

func canonicalApplicationTestRoot(t *testing.T, root string) string {
	t.Helper()
	canonical, err := workspacetools.ResolveWorkspaceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func emptyApplicationTestFilesystemPolicies(t *testing.T, root string) (*workspacetools.AddedDirectoryPolicy, *workspacetools.ProtectedPathPolicy) {
	t.Helper()
	additions, err := workspacetools.NewAddedDirectoryPolicy(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	protection, err := workspacetools.NewProtectedPathPolicy(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	return additions, protection
}

func TestApplicationSwitchesAndPersistsScope(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	settings, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	preferences, err := state.OpenInteractive(home, root)
	if err != nil {
		t.Fatal(err)
	}
	processes, err := workspacetools.NewProcessManager(root, home, t.TempDir(), workspacetools.ScopeWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = processes.Close() })
	journal, err := session.Open(filepath.Join(t.TempDir(), "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	runtime, err := newApplicationTestRuntime(agent.Config{Backend: applicationTestModel{}, Journal: journal})
	if err != nil {
		t.Fatal(err)
	}
	additions, protection := emptyApplicationTestFilesystemPolicies(t, root)
	application := &Application{
		config: applicationConfig{settings: settings, interactive: preferences, root: root, home: home},
		state: applicationState{
			session:   newLiveSession("", runtime, nil, false),
			processes: processes,
			security:  securityState{Scope: workspacetools.ScopeWorkspace, Backend: "landlock", BackendRequired: true},
			additions: additions, protection: protection,
		},
	}

	if err := application.SwitchScope(context.Background(), workspacetools.ScopeMachine); err != nil {
		t.Fatal(err)
	}
	if application.CurrentScope() != workspacetools.ScopeMachine || application.ScopeSummary() != "scope: machine" {
		t.Fatalf("scope = %q, summary = %q", application.CurrentScope(), application.ScopeSummary())
	}
	stored, err := preferences.Settings()
	if err != nil || stored.Workspace.Scope != workspacetools.ScopeMachine {
		t.Fatalf("stored scope = %q, %v", stored.Workspace.Scope, err)
	}
	if _, err := processes.RunShell(context.Background(), "true"); err != nil {
		t.Fatalf("process manager unusable after switch: %v", err)
	}
}

func TestApplicationKeepsSwitchedScopeWhenPreferenceIsNotPersisted(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	settings, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	preferences, err := state.OpenInteractive(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "interactive.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	processes, err := workspacetools.NewProcessManager(root, home, t.TempDir(), workspacetools.ScopeWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = processes.Close() })
	journal, err := session.Open(filepath.Join(t.TempDir(), "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	runtime, err := newApplicationTestRuntime(agent.Config{Backend: applicationTestModel{}, Journal: journal})
	if err != nil {
		t.Fatal(err)
	}
	additions, protection := emptyApplicationTestFilesystemPolicies(t, root)
	application := &Application{
		config: applicationConfig{settings: settings, interactive: preferences, root: root, home: home},
		state: applicationState{
			session: newLiveSession("", runtime, nil, false), processes: processes,
			security:  securityState{Scope: ScopeWorkspace, Backend: "landlock", BackendRequired: true},
			additions: additions, protection: protection,
		},
	}
	err = application.SwitchScope(context.Background(), ScopeMachine)
	if err == nil || !IsPreferenceNotPersisted(err) {
		t.Fatalf("scope persistence error = %v", err)
	}
	if application.CurrentScope() != ScopeMachine {
		t.Fatalf("live scope = %q", application.CurrentScope())
	}
	stored, readErr := preferences.Settings()
	if readErr != nil || stored.Workspace.Scope != "" {
		t.Fatalf("unexpected stored scope = %#v, %v", stored.Workspace, readErr)
	}
	if err := os.Remove(filepath.Join(home, "interactive.lock")); err != nil {
		t.Fatal(err)
	}
	if err := application.SwitchScope(context.Background(), ScopeMachine); err != nil {
		t.Fatalf("retry current scope: %v", err)
	}
	stored, readErr = preferences.Settings()
	if readErr != nil || stored.Workspace.Scope != ScopeMachine {
		t.Fatalf("retried stored scope = %#v, %v", stored.Workspace, readErr)
	}
}

func TestApplicationGivesNestedHomeNoSpecialStatus(t *testing.T) {
	if workspacetools.BoundaryBackend() != "landlock" {
		t.Skip("nested-home process limitation is specific to Landlock")
	}
	root := t.TempDir()
	home := filepath.Join(root, ".skot")
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, ".cache"))
	application, err := Open(context.Background(), Config{
		Home: home, Root: root, Interactive: true,
		Scope: ScopeMachine, ScopeExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	if slices.Contains(application.state.security.ProtectedPaths, canonicalpath.Resolve(home)) {
		t.Fatalf("Skot home became protected: %#v", application.state.security.ProtectedPaths)
	}
	if tools := application.ToolSetTools(toolpolicy.ToolSetDefault); slices.Contains(tools, "ls") {
		t.Fatalf("nested home changed default tools = %#v", tools)
	}
	if err := application.SwitchScope(context.Background(), ScopeWorkspace); err != nil {
		t.Fatal(err)
	}
	if notice := application.ScopeNotice(); notice != "" {
		t.Fatalf("nested home produced a scope notice = %q", notice)
	}
}

func TestApplicationSwitchesAndPersistsTheme(t *testing.T) {
	home := t.TempDir()
	application, err := Open(context.Background(), Config{
		Home: home, Root: t.TempDir(), Interactive: true,
		Scope: ScopeMachine, ScopeExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := application.SwitchTheme(" DARK "); err != nil {
		t.Fatal(err)
	}
	stored, err := application.config.interactive.Settings()
	if err != nil || application.CurrentTheme() != state.ThemeDark || stored.Theme != state.ThemeDark {
		t.Fatalf("theme = %q, stored = %q, err = %v", application.CurrentTheme(), stored.Theme, err)
	}
	if err := application.SwitchTheme("sepia"); err == nil {
		t.Fatal("invalid theme accepted")
	}
	if application.CurrentTheme() != state.ThemeDark {
		t.Fatalf("theme changed after invalid selection: %q", application.CurrentTheme())
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	if err := application.SwitchTheme(state.ThemeLight); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("switch theme after close error = %v", err)
	}
	reopened, err := state.OpenInteractive(home, application.config.root)
	if err != nil {
		t.Fatal(err)
	}
	stored, err = reopened.Settings()
	if err != nil || stored.Theme != state.ThemeDark {
		t.Fatalf("theme after closed switch = %q, err = %v", stored.Theme, err)
	}
}

func TestApplicationKeepsLiveThemeAndToolsWhenPreferenceIsNotPersisted(t *testing.T) {
	home := t.TempDir()
	application, err := Open(context.Background(), Config{
		Home: home, Root: t.TempDir(), Interactive: true,
		Scope: ScopeMachine, ScopeExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if err := os.Mkdir(filepath.Join(home, "interactive.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := application.SwitchTheme(state.ThemeDark); err == nil || !IsPreferenceNotPersisted(err) {
		t.Fatalf("theme persistence error = %v", err)
	}
	if application.CurrentTheme() != state.ThemeDark {
		t.Fatalf("live theme = %q", application.CurrentTheme())
	}
	if err := application.SwitchToolSet(context.Background(), toolpolicy.ToolSetReadOnly); err == nil || !IsPreferenceNotPersisted(err) {
		t.Fatalf("tool persistence error = %v", err)
	}
	if application.CurrentToolSet() != toolpolicy.ToolSetReadOnly {
		t.Fatalf("live tool set = %q", application.CurrentToolSet())
	}
	if err := os.Remove(filepath.Join(home, "interactive.lock")); err != nil {
		t.Fatal(err)
	}
	if err := application.SwitchTheme(state.ThemeDark); err != nil {
		t.Fatalf("retry current theme: %v", err)
	}
	if err := application.SwitchToolSet(context.Background(), toolpolicy.ToolSetReadOnly); err != nil {
		t.Fatalf("retry current tool set: %v", err)
	}
	stored, err := application.config.interactive.Settings()
	if err != nil || stored.Theme != state.ThemeDark || stored.Workspace.ToolSet != toolpolicy.ToolSetReadOnly {
		t.Fatalf("retried preferences = %#v, %v", stored, err)
	}
}

func TestOpenIgnoresInvalidInteractiveThemeWithoutRewritingIt(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "interactive.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"ui":{"theme":"sepia"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	application, err := Open(context.Background(), Config{
		Home: home, Root: t.TempDir(), Interactive: true,
		Scope: ScopeMachine, ScopeExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	if got := application.CurrentTheme(); got != state.ThemeAuto {
		t.Fatalf("theme = %q, want auto", got)
	}
	settings, err := application.config.interactive.Settings()
	if err != nil || settings.Theme != "" {
		t.Fatalf("stored theme view = %q, err = %v", settings.Theme, err)
	}
	if notices := strings.Join(application.StartupNotices(), "\n"); !strings.Contains(notices, "ignored") {
		t.Fatalf("startup notices = %q", notices)
	}
	if raw, err := os.ReadFile(path); err != nil || !strings.Contains(string(raw), `"theme":"sepia"`) {
		t.Fatalf("invalid state was rewritten = %q, %v", raw, err)
	}
}

func TestOpenAppliesWorkspaceScopedAndSharedInteractivePreferences(t *testing.T) {
	home, firstRoot, secondRoot := t.TempDir(), t.TempDir(), t.TempDir()
	if _, err := state.Open(home); err != nil {
		t.Fatal(err)
	}
	preferences, err := state.OpenInteractive(home, canonicalApplicationTestRoot(t, firstRoot))
	if err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetModelSelection("ollama/saved-model", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetToolSetSelection(toolpolicy.ToolSetReadOnly); err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetScopeSelection(string(ScopeMachine)); err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetThemeSelection(state.ThemeDark); err != nil {
		t.Fatal(err)
	}

	first, err := Open(context.Background(), Config{Home: home, Root: firstRoot, Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.CurrentModel() != "ollama/saved-model" || first.CurrentReasoningEffort() != "" ||
		first.CurrentToolSet() != toolpolicy.ToolSetReadOnly || first.CurrentScope() != ScopeMachine ||
		first.CurrentTheme() != state.ThemeDark {
		t.Fatalf("first workspace: model=%q effort=%q tools=%q scope=%q theme=%q",
			first.CurrentModel(), first.CurrentReasoningEffort(), first.CurrentToolSet(), first.CurrentScope(), first.CurrentTheme())
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(context.Background(), Config{Home: home, Root: secondRoot, Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	// Model, effort, and theme are shared; tool set and scope stay workspace-local.
	if second.CurrentModel() != "ollama/saved-model" || second.CurrentReasoningEffort() != "" ||
		second.CurrentToolSet() != toolpolicy.ToolSetDefault ||
		second.CurrentScope() != ScopeWorkspace || second.CurrentTheme() != state.ThemeDark {
		t.Fatalf("second workspace: model=%q effort=%q tools=%q scope=%q theme=%q",
			second.CurrentModel(), second.CurrentReasoningEffort(), second.CurrentToolSet(), second.CurrentScope(), second.CurrentTheme())
	}
}

func TestOpenPrefersWorkspaceModelOverNewerSelectionElsewhere(t *testing.T) {
	home, pinnedRoot, otherRoot := t.TempDir(), t.TempDir(), t.TempDir()
	if _, err := state.Open(home); err != nil {
		t.Fatal(err)
	}
	pinned, err := state.OpenInteractive(home, canonicalApplicationTestRoot(t, pinnedRoot))
	if err != nil {
		t.Fatal(err)
	}
	if err := pinned.SetModelSelection("ollama/pinned-model", "", ""); err != nil {
		t.Fatal(err)
	}
	other, err := state.OpenInteractive(home, canonicalApplicationTestRoot(t, otherRoot))
	if err != nil {
		t.Fatal(err)
	}
	if err := other.SetModelSelection("ollama/latest-model", "", ""); err != nil {
		t.Fatal(err)
	}
	application, err := Open(context.Background(), Config{Home: home, Root: pinnedRoot, Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if got := application.CurrentModel(); got != "ollama/pinned-model" {
		t.Fatalf("workspace model = %q; a newer selection elsewhere must not replace it", got)
	}
}

func TestOpenLegacyInteractiveConfigIsIgnoredAndNoticedOnlyInteractively(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	legacy := `{"model":"ollama/legacy","reasoning_effort":"high","recent_models":["ollama/older"],"tool_set":"read-only","scope":"machine","theme":"dark"}`
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	interactive, err := Open(context.Background(), Config{Home: home, Root: root, Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if interactive.CurrentModel() != DefaultModelURI || interactive.CurrentToolSet() != toolpolicy.ToolSetDefault ||
		interactive.CurrentScope() != ScopeWorkspace || interactive.CurrentTheme() != state.ThemeAuto {
		t.Fatalf("legacy values leaked: model=%q tools=%q scope=%q theme=%q",
			interactive.CurrentModel(), interactive.CurrentToolSet(), interactive.CurrentScope(), interactive.CurrentTheme())
	}
	notices := strings.Join(interactive.StartupNotices(), "\n")
	for _, key := range []string{"model", "reasoning_effort", "recent_models", "tool_set", "scope", "theme"} {
		if !strings.Contains(notices, key) {
			t.Fatalf("legacy notice %q does not name %q", notices, key)
		}
	}
	if err := interactive.Close(); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != legacy {
		t.Fatalf("legacy config changed = %q, %v", raw, err)
	}
	for _, name := range []string{"interactive.json", "interactive.lock"} {
		if _, err := os.Stat(filepath.Join(home, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read-only legacy startup created %s: %v", name, err)
		}
	}

	headless, err := Open(context.Background(), Config{
		Home: home, Root: root, ModelURI: "ollama/headless", ModelExplicit: true,
		Scope: ScopeMachine, ScopeExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer headless.Close()
	if notices := headless.StartupNotices(); len(notices) != 0 {
		t.Fatalf("headless legacy notices = %#v", notices)
	}
	if headless.CurrentTheme() != state.ThemeAuto {
		t.Fatalf("headless legacy theme = %q", headless.CurrentTheme())
	}
}

func TestOpenHeadlessDoesNotInspectInteractiveStateOrLock(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "interactive.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "interactive.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	application, err := Open(context.Background(), Config{
		Home: home, Root: root, ModelURI: "ollama/headless", ModelExplicit: true,
		Scope: ScopeMachine, ScopeExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if application.CurrentModel() != "ollama/headless" {
		t.Fatalf("headless model = %q", application.CurrentModel())
	}
}

func TestOpenHeadlessDoesNotInheritStoredMachineScope(t *testing.T) {
	home, root, added := t.TempDir(), t.TempDir(), t.TempDir()
	if _, err := state.Open(home); err != nil {
		t.Fatal(err)
	}
	preferences, err := state.OpenInteractive(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetScopeSelection(string(ScopeMachine)); err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetFilesystemPaths([]string{added}, []string{filepath.Join(root, ".env")}); err != nil {
		t.Fatal(err)
	}
	application, err := Open(context.Background(), Config{
		Home: home, Root: root, ModelURI: "ollama/headless", ModelExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if application.CurrentScope() != ScopeWorkspace {
		t.Fatalf("headless inherited scope %q", application.CurrentScope())
	}
	if len(application.state.security.AddedPaths) != 0 || len(application.state.security.ProtectedPaths) != 0 {
		t.Fatalf("headless inherited workspace filesystem paths: %#v", application.state.security)
	}
}

func TestOpenCanonicalWorkspaceAliasSharesPreferences(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	canonicalRoot := canonicalApplicationTestRoot(t, root)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := state.Open(home); err != nil {
		t.Fatal(err)
	}
	preferences, err := state.OpenInteractive(home, canonicalRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetModelSelection("ollama/aliased", "", ""); err != nil {
		t.Fatal(err)
	}
	application, err := Open(context.Background(), Config{
		Home: home, Root: alias, Interactive: true, Scope: ScopeMachine, ScopeExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if application.Root() != canonicalRoot || application.CurrentModel() != "ollama/aliased" {
		t.Fatalf("alias root/model = %q/%q", application.Root(), application.CurrentModel())
	}
}

func TestOpenInvalidWorkspaceToolSetFallsBackWithoutRewritingPreference(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	if _, err := state.Open(home); err != nil {
		t.Fatal(err)
	}
	preferences, err := state.OpenInteractive(home, canonicalApplicationTestRoot(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetToolSetSelection("missing"); err != nil {
		t.Fatal(err)
	}
	application, err := Open(context.Background(), Config{
		Home: home, Root: root, Interactive: true, Scope: ScopeMachine, ScopeExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if application.CurrentToolSet() != toolpolicy.ToolSetDefault || !strings.Contains(strings.Join(application.StartupNotices(), "\n"), "invalid workspace tool_set") {
		t.Fatalf("tool set/notices = %q/%#v", application.CurrentToolSet(), application.StartupNotices())
	}
	stored, err := preferences.Settings()
	if err != nil || stored.Workspace.ToolSet != "missing" {
		t.Fatalf("invalid preference was rewritten = %#v, %v", stored.Workspace, err)
	}
	if err := application.SwitchToolSet(context.Background(), toolpolicy.ToolSetDefault); err != nil {
		t.Fatalf("select current fallback: %v", err)
	}
	stored, err = preferences.Settings()
	if err != nil || stored.Workspace.ToolSet != toolpolicy.ToolSetDefault {
		t.Fatalf("invalid preference was not replaced = %#v, %v", stored.Workspace, err)
	}
}

func TestOpenExistingJournalRestoresModelAndJobsButEmptyJournalUsesFreshDefaults(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	sessionID := "session_0123456789abcdef0123456789abcdef"
	existingPath := filepath.Join(t.TempDir(), "existing.jsonl")
	existing, err := session.Open(existingPath)
	if err != nil {
		t.Fatal(err)
	}
	appendApplicationRecord(t, existing, agent.RecordSessionStarted, agent.SessionStartedRecord{
		SchemaVersion: agent.JournalSchemaVersion, SessionID: sessionID, Workspace: root,
	})
	appendApplicationRecord(t, existing, agent.RecordModelSelected, agent.ModelSelectedRecord{
		Backend: "chat_completions", Provider: "ollama", Model: "saved", Epoch: "saved-epoch",
	})
	if err := existing.Close(); err != nil {
		t.Fatal(err)
	}
	continued, err := Open(context.Background(), Config{
		Home: home, Root: root, JournalPath: existingPath, ModelURI: "ollama/fresh",
		Scope: ScopeMachine, ScopeExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if continued.CurrentModel() != "ollama/saved" {
		t.Fatalf("continued journal model = %q", continued.CurrentModel())
	}
	ctx := agent.WithToolSessionID(context.Background(), sessionID)
	started, err := continued.state.processes.Tools()[0].Run(ctx, `{"command":"printf durable","background":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(started.Details) != 1 {
		t.Fatalf("background process details = %#v", started.Details)
	}
	process, ok := agent.ProcessResultFromDetail(started.Details[0])
	if !ok || process.JobID == "" {
		t.Fatalf("background process = %#v, %t", process, ok)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		status, found := continued.state.processes.Status(process.JobID)
		if found && status.Status != agent.ProcessRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background process did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := continued.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := Open(context.Background(), Config{
		Home: home, Root: root, JournalPath: existingPath, ModelURI: "ollama/fresh",
		Scope: ScopeMachine, ScopeExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	events := resumed.state.processes.PendingCompletionEvents(sessionID)
	if len(events) != 1 || events[0].JobID != process.JobID {
		t.Fatalf("continued journal completion events = %#v", events)
	}
	if _, err := resumed.ClearSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, found := resumed.state.processes.Status(process.JobID); found {
		t.Fatal("cleared explicit journal retained its previous background job")
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}

	emptyPath := filepath.Join(t.TempDir(), "empty.jsonl")
	fresh, err := Open(context.Background(), Config{
		Home: home, Root: root, JournalPath: emptyPath, ModelURI: "ollama/fresh",
		Scope: ScopeMachine, ScopeExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if fresh.CurrentModel() != "ollama/fresh" {
		t.Fatalf("empty journal model = %q", fresh.CurrentModel())
	}
}

func TestApplicationRecordsResolvedProductConfiguration(t *testing.T) {
	application, err := Open(context.Background(), Config{
		Home: t.TempDir(), Root: t.TempDir(), ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		ToolSet: toolpolicy.ToolSetReadOnly, ToolSetExplicit: true,
		Scope: workspacetools.ScopeMachine, ScopeExplicit: true, Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	if _, err := application.RunShell(context.Background(), "printf configured"); err != nil {
		t.Fatal(err)
	}
	configured, err := application.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if configured.Configured == nil {
		t.Fatal("application session has no effective configuration")
	}
	snapshot := configured.Configured
	if snapshot.ModelContext.ToolSet != toolpolicy.ToolSetReadOnly || len(snapshot.ModelContext.Tools) == 0 || snapshot.ModelContext.CompactionInstructions == "" {
		t.Fatalf("model context = %#v", snapshot.ModelContext)
	}
	if snapshot.Environment.Endpoint != "https://api.deepseek.com/v1" || snapshot.Environment.Scope.Scope != workspacetools.ScopeMachine || snapshot.Environment.Scope.Network != "inherited" {
		t.Fatalf("environment = %#v", snapshot.Environment)
	}
	if snapshot.RuntimePolicy.AwaitRequiredJobs || snapshot.RuntimePolicy.ContextWindow != 1_000_000 || snapshot.RuntimePolicy.MaxRequestBytes == 0 || snapshot.RuntimePolicy.MaxCompletionBytes == 0 || snapshot.RuntimePolicy.MaxModelAttempts != -1 || snapshot.RuntimePolicy.RetryBudget != DefaultRetryBudget.String() || snapshot.RuntimePolicy.StreamIdleTimeout != DefaultStreamIdleTimeout.String() || snapshot.RuntimePolicy.MaxToolIterations != agent.DefaultMaxToolIterations {
		t.Fatalf("runtime policy = %#v", snapshot.RuntimePolicy)
	}
	if snapshot.ModelContext.ToolLimitInstructions == "" {
		t.Fatalf("model context = %#v", snapshot.ModelContext)
	}
}

func TestOpenMergesFilesystemPathsAndScopeSwitchPreservesThem(t *testing.T) {
	if workspacetools.BoundaryBackend() == "" {
		t.Skip("platform sandbox is unavailable")
	}
	home, root, added := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"protected_paths":["settings-secret"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "settings-secret"), []byte("settings\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	addedFile := filepath.Join(added, "shared.txt")
	if err := os.WriteFile(addedFile, []byte("shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	preferences, err := state.OpenInteractive(home, canonicalApplicationTestRoot(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetFilesystemPaths(nil, []string{"workspace-secret"}); err != nil {
		t.Fatal(err)
	}
	application, err := Open(context.Background(), Config{
		Home: home, Root: root, ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		Scope: ScopeMachine, ScopeExplicit: true, Interactive: true,
		AddedPaths:     []string{added},
		ProtectedPaths: []string{"api-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	wantPaths := []string{
		canonicalpath.Resolve(filepath.Join(root, "settings-secret")),
		canonicalpath.Resolve(filepath.Join(root, "api-secret")),
		canonicalpath.Resolve(filepath.Join(root, "workspace-secret")),
	}
	assertJournalPaths := func() {
		t.Helper()
		state, stateErr := application.State(context.Background())
		if stateErr != nil {
			t.Fatal(stateErr)
		}
		if state.Configured == nil {
			t.Fatal("application session has no effective configuration")
		}
		if !slices.Equal(state.Configured.Environment.Scope.AddedPaths, []string{canonicalpath.Resolve(added)}) {
			t.Fatalf("journaled added paths = %#v", state.Configured.Environment.Scope.AddedPaths)
		}
		for _, want := range wantPaths {
			if !slices.Contains(state.Configured.Environment.Scope.ProtectedPaths, want) {
				t.Fatalf("journaled protected paths = %#v; missing %q", state.Configured.Environment.Scope.ProtectedPaths, want)
			}
		}
	}
	if _, err := application.RunShell(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	assertJournalPaths()
	read := func(path string) error {
		for _, tool := range application.config.tools {
			if tool.Spec.Name == "read" {
				arguments, encodeErr := json.Marshal(map[string]string{"path": path})
				if encodeErr != nil {
					return encodeErr
				}
				_, err := tool.Run(context.Background(), string(arguments))
				return err
			}
		}
		return errors.New("read tool not found")
	}
	if err := read("settings-secret"); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("machine read error = %v", err)
	}
	if err := application.SwitchScope(context.Background(), ScopeWorkspace); err != nil {
		t.Fatal(err)
	}
	assertJournalPaths()
	if err := read("settings-secret"); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("workspace read error = %v", err)
	}
	if err := read(addedFile); err != nil {
		t.Fatalf("workspace could not read added directory: %v", err)
	}
	if err := read(filepath.Join(t.TempDir(), "outside.txt")); err == nil || !strings.Contains(err.Error(), "outside workspace scope") {
		t.Fatalf("workspace reached a directory that was not added: %v", err)
	}
}

func TestInteractiveScopeSwitchKeepsProcessBoundaryReadyAcrossToolSets(t *testing.T) {
	if workspacetools.BoundaryBackend() == "" {
		t.Skip("platform filesystem boundary is unavailable")
	}
	application, err := Open(context.Background(), Config{
		Home: t.TempDir(), Root: t.TempDir(), Interactive: true,
		ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		ToolSet: toolpolicy.ToolSetReadOnly, ToolSetExplicit: true,
		Scope: ScopeMachine, ScopeExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })

	if err := application.SwitchScope(context.Background(), ScopeWorkspace); err != nil {
		t.Fatal(err)
	}
	if _, err := application.RunShell(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	state, err := application.State(context.Background())
	if err != nil || state.Configured == nil || state.Configured.Environment.Scope.Backend == "" {
		t.Fatalf("workspace boundary was not kept ready: %#v, %v", state.Configured, err)
	}
	if err := application.SwitchToolSet(context.Background(), toolpolicy.ToolSetDefault); err != nil {
		t.Fatalf("enable process tools: %v", err)
	}
}

func TestOpenSharesScopeSwitchWithBuiltInFileTools(t *testing.T) {
	if workspacetools.BoundaryBackend() == "" {
		t.Skip("platform filesystem boundary is unavailable")
	}
	home, root, outside := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	external := filepath.Join(outside, "external.txt")
	if err := os.WriteFile(external, []byte("external\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	application, err := Open(context.Background(), Config{
		Home: home, Root: root, Interactive: true,
		Scope: ScopeMachine, ScopeExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	var read agent.Tool
	for _, tool := range application.config.tools {
		if tool.Spec.Name == "read" {
			read = tool
			break
		}
	}
	if read.Run == nil {
		t.Fatal("read tool not found")
	}
	application.state.children.mu.Lock()
	childCatalog := append([]agent.Tool(nil), application.state.children.builder.tools...)
	application.state.children.mu.Unlock()
	var childRead agent.Tool
	for _, tool := range childCatalog {
		if tool.Spec.Name == "read" {
			childRead = tool
			break
		}
	}
	if childRead.Run == nil {
		t.Fatal("child read tool not found")
	}
	arguments, err := json.Marshal(map[string]string{"path": external})
	if err != nil {
		t.Fatal(err)
	}
	if output, err := read.Run(context.Background(), string(arguments)); err != nil || !strings.Contains(output.Content, "external") {
		t.Fatalf("machine read = %q, %v", output.Content, err)
	}
	if _, err := childRead.Run(context.Background(), string(arguments)); err != nil {
		t.Fatalf("child machine read: %v", err)
	}
	if err := application.SwitchScope(context.Background(), ScopeWorkspace); err != nil {
		t.Fatal(err)
	}
	if _, err := read.Run(context.Background(), string(arguments)); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("workspace read error = %v", err)
	}
	if _, err := childRead.Run(context.Background(), string(arguments)); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("child workspace read error = %v", err)
	}
	if err := application.SwitchScope(context.Background(), ScopeMachine); err != nil {
		t.Fatal(err)
	}
	if _, err := read.Run(context.Background(), string(arguments)); err != nil {
		t.Fatalf("restored machine read: %v", err)
	}
}

func TestOpenConfiguresAndOwnsCompleteToolCatalog(t *testing.T) {
	var borrowed []agent.Tool
	seenDefaults := make(map[string]bool)
	application, err := Open(context.Background(), Config{
		Home: t.TempDir(), Root: t.TempDir(), ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		ToolSet: toolpolicy.ToolSetDefault, ToolSetExplicit: true,
		Scope: workspacetools.ScopeMachine, ScopeExplicit: true, Interactive: true,
		ToolSets: map[string][]string{
			toolpolicy.ToolSetReadOnly: {"ls", "grep", "glob"},
			toolpolicy.ToolSetEdit:     {"ls", "grep", "glob", "edit", "write"},
			toolpolicy.ToolSetDefault:  {"grep", "glob", "edit", "write", "bash", "job", "custom"},
		},
		ConfigureTools: func(catalog []agent.Tool) ([]agent.Tool, error) {
			borrowed = catalog
			configured := catalog[:0]
			for _, tool := range catalog {
				seenDefaults[tool.Spec.Name] = true
				switch tool.Spec.Name {
				case "read":
					continue
				case "bash":
					tool = applicationTool("bash")
					tool.Spec.Description = "replacement bash"
				}
				configured = append(configured, tool)
			}
			custom := applicationTool("custom")
			custom.Spec.Description = "custom tool"
			return append(configured, custom), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	for _, name := range []string{"read", "write", "ls", "bash", "job"} {
		if !seenDefaults[name] {
			t.Fatalf("ConfigureTools did not receive default %q: %#v", name, seenDefaults)
		}
	}

	byName := make(map[string]agent.Tool, len(application.config.tools))
	for _, tool := range application.config.tools {
		byName[tool.Spec.Name] = tool
	}
	if _, ok := byName["read"]; ok {
		t.Fatal("removed read tool remains in application catalog")
	}
	if byName["bash"].Spec.Description != "replacement bash" || byName["custom"].Spec.Description != "custom tool" {
		t.Fatalf("configured catalog = %#v", byName)
	}

	borrowed[0].Spec.Name = "mutated-after-open"
	borrowed[0].Spec.InputSchema[0] = '['
	for _, tool := range application.config.tools {
		if tool.Spec.Name == "mutated-after-open" || !json.Valid(tool.Spec.InputSchema) {
			t.Fatalf("application catalog aliases callback storage: %#v", application.config.tools)
		}
	}

	if _, err := application.RunShell(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	state, err := application.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Configured == nil {
		t.Fatal("application session has no effective configuration")
	}
	visible := make(map[string]string, len(state.Configured.ModelContext.Tools))
	for _, tool := range state.Configured.ModelContext.Tools {
		visible[tool.Name] = tool.Description
	}
	if _, ok := visible["read"]; ok || visible["bash"] != "replacement bash" || visible["custom"] != "custom tool" {
		t.Fatalf("visible tools = %#v", visible)
	}
}

func TestOpenLoadsProgramToolsIntoExactToolSetsAndRecordsResolvedRuntime(t *testing.T) {
	home := t.TempDir()
	toolsDocument := `{"tools":[{
	  "name":"lookup","description":"lookup configured data",
	  "command":["sh","-c","cat"],"background":"auto","yield":2,
	  "parallel_safe":true,"env":{"LOOKUP_TOKEN":"journal-secret"},
	  "parameters":{"type":"object","properties":{"query":{"type":"string"}}}
	}]}`
	if err := os.WriteFile(filepath.Join(home, "tools.json"), []byte(toolsDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := `{"tool_sets":{"default":["read","grep","glob","edit","write","bash","job","lookup"]}}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	application, err := Open(context.Background(), Config{
		Home: home, Root: t.TempDir(), ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		ToolSet: toolpolicy.ToolSetReadOnly, ToolSetExplicit: true,
		Scope: workspacetools.ScopeMachine, ScopeExplicit: true, Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	if _, err := application.RunShell(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	state, err := application.State(context.Background())
	if err != nil || state.Configured == nil {
		t.Fatalf("state = %#v, %v", state, err)
	}
	if len(state.Configured.Environment.ProgramTools) != 0 {
		t.Fatalf("unselected program was resolved into snapshot: %#v", state.Configured.Environment.ProgramTools)
	}
	if err := application.SwitchToolSet(context.Background(), toolpolicy.ToolSetDefault); err != nil {
		t.Fatal(err)
	}
	state, err = application.State(context.Background())
	if err != nil || state.Configured == nil {
		t.Fatalf("state after program switch = %#v, %v", state, err)
	}
	programs := state.Configured.Environment.ProgramTools
	if len(programs) != 1 || programs[0].Name != "lookup" || programs[0].Program == "" || programs[0].Timeout != "10m0s" ||
		programs[0].Background != "auto" || programs[0].Yield != "2s" || !programs[0].ParallelSafe ||
		strings.Join(programs[0].EnvironmentNames, ",") != "LOOKUP_TOKEN" {
		t.Fatalf("program snapshots = %#v", programs)
	}
	encoded, err := json.Marshal(state.Configured)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "journal-secret") {
		t.Fatalf("environment value reached snapshot: %s", encoded)
	}
	if got := application.config.masker.Redact("token=journal-secret"); got != "token=[REDACTED]" {
		t.Fatalf("program environment was not registered for redaction: %q", got)
	}
	if err := application.SwitchToolSet(context.Background(), toolpolicy.ToolSetReadOnly); err != nil {
		t.Fatal(err)
	}
	state, err = application.State(context.Background())
	if err != nil || state.Configured == nil || len(state.Configured.Environment.ProgramTools) != 0 {
		t.Fatalf("inactive program remained in snapshot: %#v, %v", state.Configured, err)
	}
}

func TestOpenDoesNotBindUnselectedProgramExecutable(t *testing.T) {
	home := t.TempDir()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	toolsDocument := fmt.Sprintf(`{"tools":[{"name":"missing","description":"missing program","command":[%q]}]}`, missing)
	if err := os.WriteFile(filepath.Join(home, "tools.json"), []byte(toolsDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	application, err := Open(context.Background(), Config{
		Home: home, Root: t.TempDir(), ModelURI: "ollama/test", ModelExplicit: true,
		ToolSet: toolpolicy.ToolSetReadOnly, ToolSetExplicit: true,
		Scope: workspacetools.ScopeMachine, ScopeExplicit: true,
	})
	if err != nil {
		t.Fatalf("unselected executable blocked process-free startup: %v", err)
	}
	if !application.state.session.memory {
		t.Fatal("unselected program disabled in-memory one-shot")
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSwitchToolSetBindsProgramBeforePublishingIt(t *testing.T) {
	home := t.TempDir()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	toolsDocument := fmt.Sprintf(`{"tools":[{"name":"missing","description":"missing program","command":[%q]}]}`, missing)
	if err := os.WriteFile(filepath.Join(home, "tools.json"), []byte(toolsDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	application, err := Open(context.Background(), Config{
		Home: home, Root: t.TempDir(), ModelURI: "ollama/test", ModelExplicit: true,
		ToolSet: toolpolicy.ToolSetReadOnly, ToolSetExplicit: true,
		ToolSets: map[string][]string{"program-only": {"missing"}},
		Scope:    workspacetools.ScopeMachine, ScopeExplicit: true, Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	err = application.SwitchToolSet(context.Background(), "program-only")
	if err == nil || !strings.Contains(err.Error(), `tool missing`) || !strings.Contains(err.Error(), "does-not-exist") {
		t.Fatalf("program binding error = %v", err)
	}
	if got := application.CurrentToolSet(); got != toolpolicy.ToolSetReadOnly {
		t.Fatalf("failed switch published tool set %q", got)
	}
}

func TestOpenRejectsBackgroundProgramToolSetWithoutJob(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "tools.json"), []byte(`{"tools":[{"name":"worker","description":"work","command":["true"],"background":"always"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), Config{
		Home: home, Root: t.TempDir(), ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		ToolSet: "worker-only", ToolSetExplicit: true,
		ToolSets: map[string][]string{"worker-only": {"worker"}},
		Scope:    workspacetools.ScopeMachine, ScopeExplicit: true, Interactive: true,
	})
	if !errors.Is(err, agent.ErrInvalidRequest) || !strings.Contains(err.Error(), `not required tool "job"`) {
		t.Fatalf("error = %v", err)
	}
}

// Bash needs the job tool for the same reason a background-capable program
// does, and needs it without anyone having asked for background: a foreground
// command still running after the yield hands the model a job id.
func TestOpenRejectsBashToolSetWithoutJob(t *testing.T) {
	_, err := Open(context.Background(), Config{
		Home: t.TempDir(), Root: t.TempDir(), ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		ToolSet: "shell-only", ToolSetExplicit: true,
		ToolSets: map[string][]string{"shell-only": {"read", "bash"}},
		Scope:    workspacetools.ScopeMachine, ScopeExplicit: true, Interactive: true,
	})
	if !errors.Is(err, agent.ErrInvalidRequest) || !strings.Contains(err.Error(), `not required tool "job"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenValidatesCompleteCatalogBeforeToolSetFiltering(t *testing.T) {
	_, err := Open(context.Background(), Config{
		Home: t.TempDir(), Root: t.TempDir(), ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		ToolSet: toolpolicy.ToolSetReadOnly, ToolSetExplicit: true,
		Scope: workspacetools.ScopeMachine, ScopeExplicit: true, Interactive: true,
		ConfigureTools: func(catalog []agent.Tool) ([]agent.Tool, error) {
			return append(catalog, applicationTool("bash")), nil
		},
	})
	if !errors.Is(err, agent.ErrInvalidRequest) || !strings.Contains(err.Error(), `duplicate tool "bash"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenAllowsDeliberatelyEmptyToolCatalog(t *testing.T) {
	application, err := Open(context.Background(), Config{
		Home: t.TempDir(), Root: t.TempDir(), ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		Scope: workspacetools.ScopeMachine, ScopeExplicit: true, Interactive: true,
		ToolSets: map[string][]string{
			toolpolicy.ToolSetReadOnly: nil,
			toolpolicy.ToolSetEdit:     nil,
			toolpolicy.ToolSetDefault:  nil,
		},
		ConfigureTools: func([]agent.Tool) ([]agent.Tool, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	if _, err := application.RunShell(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	state, err := application.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(application.config.tools) != 0 || state.Configured == nil || len(state.Configured.ModelContext.Tools) != 0 {
		t.Fatalf("catalog = %#v, configured = %#v", application.config.tools, state.Configured)
	}
}

func TestOpenLoadsCustomToolSetsAndLetsConfigReplaceBuiltIns(t *testing.T) {
	home := t.TempDir()
	settings := `{"tool_sets":{"default":["read"],"review":["read","custom"]}}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	configuredToolSets := map[string][]string{toolpolicy.ToolSetDefault: {"custom"}}
	application, err := Open(context.Background(), Config{
		Home: home, Root: t.TempDir(), ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		ToolSet: toolpolicy.ToolSetDefault, ToolSetExplicit: true,
		ToolSets: configuredToolSets,
		Scope:    workspacetools.ScopeMachine, ScopeExplicit: true, Interactive: true,
		ConfigureTools: func(catalog []agent.Tool) ([]agent.Tool, error) {
			return append(catalog, applicationTool("custom")), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	configuredToolSets[toolpolicy.ToolSetDefault][0] = "read"
	if got := strings.Join(application.ToolSets(), ","); got != "default,edit,read-only,review" {
		t.Fatalf("tool sets = %q", got)
	}
	if got := strings.Join(application.ToolSetTools(toolpolicy.ToolSetDefault), ","); got != "custom" {
		t.Fatalf("tools in default set = %q", got)
	}

	if _, err := application.RunShell(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	state, err := application.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := configuredToolNames(state); got != "custom" {
		t.Fatalf("overridden default tools = %q", got)
	}

	if err := application.SwitchToolSet(context.Background(), " REVIEW "); err != nil {
		t.Fatal(err)
	}
	state, err = application.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := configuredToolNames(state); got != "read,custom" || application.CurrentToolSet() != "review" {
		t.Fatalf("review tools = %q, current = %q", got, application.CurrentToolSet())
	}
	stored, err := application.config.settings.Settings()
	if err != nil || strings.Join(stored.ToolSets["review"], ",") != "read,custom" {
		t.Fatalf("stored tool sets = %#v, %v", stored.ToolSets, err)
	}
}

func TestOpenRejectsNegativeModelRequestDurations(t *testing.T) {
	for name, config := range map[string]Config{
		"retry budget":        {RetryBudget: -time.Second},
		"stream idle timeout": {StreamIdleTimeout: -time.Second},
		"tool iterations":     {MaxToolIterations: -2},
	} {
		t.Run(name, func(t *testing.T) {
			config.Home = t.TempDir()
			config.Root = t.TempDir()
			if _, err := Open(context.Background(), config); !errors.Is(err, agent.ErrInvalidRequest) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestOpenHeadlessReadOnlyDoesNotProvisionProcessCapabilities(t *testing.T) {
	for _, scope := range []string{string(workspacetools.ScopeMachine), string(workspacetools.ScopeWorkspace)} {
		t.Run(scope, func(t *testing.T) {
			homeParent := t.TempDir()
			home := filepath.Join(homeParent, "home")
			t.Setenv("PATH", "/definitely-missing")
			t.Setenv("HOME", "")
			t.Setenv("XDG_CACHE_HOME", "")
			application, err := Open(context.Background(), Config{
				Home: home, Root: t.TempDir(), ModelURI: "ollama/test", ModelExplicit: true,
				ToolSet: toolpolicy.ToolSetReadOnly, ToolSetExplicit: true, Scope: scope, ScopeExplicit: true,
			})
			if err != nil {
				t.Fatalf("read-only startup depends on process environment: %v", err)
			}
			if !application.state.session.memory {
				t.Fatal("process-free one-shot did not use an in-memory journal")
			}
			if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("in-memory startup created home: %v", err)
			}
			if err := application.SwitchToolSet(context.Background(), toolpolicy.ToolSetEdit); err != nil {
				t.Fatalf("switch between memory-safe tool sets: %v", err)
			}
			if err := application.SwitchToolSet(context.Background(), toolpolicy.ToolSetDefault); err == nil || !strings.Contains(err.Error(), "in-memory one-shot") {
				t.Fatalf("unsafe memory-session switch error = %v", err)
			}
			if err := application.Close(); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"jobs"} {
				if _, err := os.Stat(filepath.Join(home, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("read-only startup created %s: %v", name, err)
				}
			}
		})
	}
}

func TestOpenInvalidToolSetLeavesMissingHomeUntouched(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	_, err := Open(context.Background(), Config{
		Home: home, Root: t.TempDir(), ModelURI: "ollama/test", ModelExplicit: true,
		ToolSet: "invalid", ToolSetExplicit: true, Scope: string(workspacetools.ScopeMachine), ScopeExplicit: true,
	})
	if err == nil || !strings.Contains(err.Error(), "unknown tool set") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid tool set created home: %v", err)
	}
}

func TestOpenReadOnlyValidationPreservesExistingModes(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
	_, err := Open(context.Background(), Config{
		Home: home, Root: t.TempDir(), ModelURI: "ollama/test", ModelExplicit: true,
		ToolSet: "invalid", ToolSetExplicit: true, Scope: string(workspacetools.ScopeMachine), ScopeExplicit: true,
	})
	if err == nil || !strings.Contains(err.Error(), "unknown tool set") {
		t.Fatalf("error = %v", err)
	}
	for path, want := range map[string]os.FileMode{home: 0o500, configPath: 0o400} {
		info, statErr := os.Stat(path)
		if statErr != nil || info.Mode().Perm() != want {
			t.Fatalf("mode %s = %v, %v; want %04o", path, info, statErr, want)
		}
	}
}

func TestApplicationRejectsUnknownScopeWithoutChangingState(t *testing.T) {
	application := &Application{state: applicationState{security: securityState{Scope: workspacetools.ScopeWorkspace}}}
	if err := application.SwitchScope(context.Background(), "magic"); err == nil {
		t.Fatal("unknown scope accepted")
	}
	if application.CurrentScope() != workspacetools.ScopeWorkspace {
		t.Fatalf("scope changed to %q", application.CurrentScope())
	}
}

func TestScopeSummaryReportsRunningProcessesWithEarlierScope(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	processes, err := workspacetools.NewProcessManager(root, home, t.TempDir(), workspacetools.ScopeMachine)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = processes.Close() })
	tool := processes.Tools()[0]
	ctx := agent.WithToolSessionID(context.Background(), "session-test")
	if _, err := tool.Run(ctx, `{"command":"sleep 30","background":true}`); err != nil {
		t.Fatal(err)
	}
	additions, protection := emptyApplicationTestFilesystemPolicies(t, root)
	if err := processes.SetFilesystemPolicyAfter(workspacetools.ScopeWorkspace, additions, protection, nil); err != nil {
		t.Fatal(err)
	}
	application := &Application{
		state: applicationState{
			processes: processes,
			security: securityState{
				Scope: workspacetools.ScopeWorkspace,
			},
		},
	}
	if summary := application.ScopeSummary(); !strings.Contains(summary, "running processes retain launch scope: machine (1)") {
		t.Fatalf("security summary = %q", summary)
	}
}

func TestApplicationBuildsAndPersistsSelectedModel(t *testing.T) {
	application, preferences, _ := newModelSwitchApplication(t)
	if err := application.SwitchModel(context.Background(), "deepseek/new-model", "high", ""); err != nil {
		t.Fatal(err)
	}
	stored, err := preferences.Settings()
	if err != nil || stored.Workspace.Model != "deepseek/new-model" || stored.Workspace.ReasoningEffort == nil || *stored.Workspace.ReasoningEffort != "high" || application.CurrentModel() != "deepseek/new-model" || application.CurrentReasoningEffort() != "high" {
		t.Fatalf("stored=%#v current=%q err=%v", stored, application.CurrentModel(), err)
	}
}

func TestApplicationKeepsSwitchedModelWhenSavingSelectionFails(t *testing.T) {
	application, preferences, home := newModelSwitchApplication(t)
	if err := os.Mkdir(filepath.Join(home, "interactive.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := application.SwitchModel(context.Background(), "deepseek/new-model", "high", ""); err == nil || !IsPreferenceNotPersisted(err) {
		t.Fatalf("switch error = %v", err)
	}
	if got := application.CurrentModel(); got != "deepseek/new-model" {
		t.Fatalf("model did not remain switched after persistence failure: %q", got)
	}
	if err := os.Remove(filepath.Join(home, "interactive.lock")); err != nil {
		t.Fatal(err)
	}
	if err := application.SwitchModel(context.Background(), "deepseek/new-model", "high", ""); err != nil {
		t.Fatalf("retry current model: %v", err)
	}
	stored, err := preferences.Settings()
	if err != nil || stored.Workspace.Model != "deepseek/new-model" || stored.Workspace.ReasoningEffort == nil || *stored.Workspace.ReasoningEffort != "high" {
		t.Fatalf("retried model preference = %#v, %v", stored.Workspace, err)
	}
}

func TestApplicationRestoresSavedModelWhenRuntimeRejectsSwitch(t *testing.T) {
	application, preferences, _ := newModelSwitchApplication(t)
	if err := preferences.SetModelSelection("deepseek/old-model", "high", ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := application.SwitchModel(ctx, "deepseek/new-model", "high", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("switch error = %v", err)
	}
	stored, err := preferences.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if stored.Workspace.Model != "deepseek/old-model" || stored.Workspace.ReasoningEffort == nil || *stored.Workspace.ReasoningEffort != "high" {
		t.Fatalf("saved model after rejected switch = %#v", stored)
	}
	if got := application.CurrentModel(); got != "test/initial" {
		t.Fatalf("runtime model changed after rejected switch: %q", got)
	}
}

func newModelSwitchApplication(t *testing.T) (*Application, *state.InteractiveStore, string) {
	t.Helper()
	home := t.TempDir()
	settings, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	preferences, err := state.OpenInteractive(home, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	journal, err := session.Open(filepath.Join(t.TempDir(), "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	runtime, err := newApplicationTestRuntime(agent.Config{Backend: applicationTestModel{}, Journal: journal})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "secret")
	return &Application{
		config: applicationConfig{settings: settings, interactive: preferences},
		state:  applicationState{session: newLiveSession("", runtime, nil, false)},
	}, preferences, home
}

func TestOpenAppliesModelAPIOverrideBeforeReasoningEffortValidation(t *testing.T) {
	application, err := Open(context.Background(), Config{
		Home: t.TempDir(), Root: t.TempDir(), ModelURI: "opencode-go/future-model", ModelExplicit: true,
		ModelAPI: "responses", ReasoningEffort: "high", ReasoningEffortExplicit: true,
		Scope: workspacetools.ScopeMachine, ScopeExplicit: true, Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	if application.CurrentModel() != "opencode-go/future-model" || application.CurrentReasoningEffort() != "high" {
		t.Fatalf("model=%q effort=%q", application.CurrentModel(), application.CurrentReasoningEffort())
	}
}

func TestApplicationOwnsAndPersistsToolSet(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	settings, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	preferences, err := state.OpenInteractive(home, root)
	if err != nil {
		t.Fatal(err)
	}
	catalog := []agent.Tool{
		applicationTool("read"),
		applicationTool("ls"),
		applicationTool("grep"),
		applicationTool("glob"),
		applicationTool("edit"),
		applicationTool("write"),
		applicationTool("bash"),
		applicationTool("job"),
	}
	toolSets, err := toolpolicy.NewToolSets(catalog)
	if err != nil {
		t.Fatal(err)
	}
	selectedTools, err := toolSets.Tools(catalog, toolpolicy.ToolSetDefault)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := newApplicationTestRuntime(agent.Config{
		Backend: toolSetCaptureModel{t: t, want: "read,ls,grep,glob"},
		Journal: &applicationMemoryJournal{},
		Tools:   selectedTools,
	})
	if err != nil {
		t.Fatal(err)
	}
	application := &Application{
		config: applicationConfig{settings: settings, interactive: preferences, tools: catalog, toolSets: toolSets},
		state: applicationState{
			session: newLiveSession("", runtime, nil, false),
			toolSet: toolpolicy.ToolSetDefault,
		},
	}
	if err := application.SwitchToolSet(context.Background(), toolpolicy.ToolSetReadOnly); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "check tools", nil); err != nil {
		t.Fatal(err)
	}
	stored, err := preferences.Settings()
	if err != nil || stored.Workspace.ToolSet != toolpolicy.ToolSetReadOnly || application.CurrentToolSet() != toolpolicy.ToolSetReadOnly {
		t.Fatalf("stored=%q current=%q err=%v", stored.Workspace.ToolSet, application.CurrentToolSet(), err)
	}
}

func TestApplicationClearSessionSwapsJournalAndPrunesEmptySession(t *testing.T) {
	application, oldJournal := newSessionApplication(t)
	if application.HasUserTurn() {
		t.Fatal("new session has a user turn")
	}
	oldID := application.SessionID()
	appendApplicationRecord(t, oldJournal, agent.RecordRunStarted, agent.RunStartedRecord{RunID: "run"})
	appendApplicationRecord(t, oldJournal, agent.RecordRunInputAdded, agent.RunInputAddedRecord{RunID: "run", Text: "hello"})
	if !application.HasUserTurn() {
		t.Fatal("application missed its current user turn")
	}
	id, err := application.ClearSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || id == oldID || application.SessionID() != id {
		t.Fatalf("session IDs: old=%q new=%q current=%q", oldID, id, application.SessionID())
	}
	if _, err := oldJournal.Records(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("old journal remained open: %v", err)
	}
	state, err := application.State(context.Background())
	if err != nil || len(state.Items) != 0 || state.SessionID != "" {
		t.Fatalf("new state = %#v, %v", state, err)
	}
	if application.HasUserTurn() {
		t.Fatal("cleared session retained the previous user turn")
	}
	dir := filepath.Join(application.config.home, "sessions", id)
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty session was not pruned: %v", err)
	}
}

func TestApplicationClearSessionStopsOldSessionJobs(t *testing.T) {
	application, _ := newSessionApplication(t)
	oldID := application.SessionID()
	tool := application.state.processes.Tools()[0]
	ctx := agent.WithToolSessionID(context.Background(), oldID)
	started, err := tool.Run(ctx, `{"command":"sleep 30","background":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(started.Details) != 1 {
		t.Fatalf("process details = %#v", started.Details)
	}
	process, ok := agent.ProcessResultFromDetail(started.Details[0])
	if !ok || process.JobID == "" {
		t.Fatalf("process result = %#v, %t", process, ok)
	}

	if _, err := application.ClearSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := application.state.processes.Status(process.JobID); ok {
		t.Fatal("old session job remained registered after clear")
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestApplicationListsAndResumesSessionWithRecordedModel(t *testing.T) {
	application, oldJournal := newSessionApplication(t)
	currentID := application.SessionID()
	appendApplicationRecord(t, oldJournal, agent.RecordRunStarted, agent.RunStartedRecord{RunID: "run-current"})
	appendApplicationRecord(t, oldJournal, agent.RecordRunInputAdded, agent.RunInputAddedRecord{RunID: "run-current", Text: "current task"})
	appendApplicationRecord(t, oldJournal, agent.RecordRunFinished, agent.RunFinishedRecord{RunID: "run-current", Status: agent.RunCompleted})
	source, sourceID, err := session.Create(application.config.home)
	if err != nil {
		t.Fatal(err)
	}
	appendApplicationRecord(t, source, agent.RecordSessionStarted, agent.SessionStartedRecord{SchemaVersion: agent.JournalSchemaVersion, SessionID: sourceID, Workspace: application.config.root})
	appendApplicationRecord(t, source, agent.RecordModelSelected, agent.ModelSelectedRecord{
		Backend: "chat_completions", Provider: "deepseek", Model: "resumed-model", ReasoningEffort: " HIGH ", Epoch: "epoch-resumed",
	})
	appendApplicationRecord(t, source, agent.RecordRunStarted, agent.RunStartedRecord{RunID: "run-resumed"})
	appendApplicationRecord(t, source, agent.RecordRunInputAdded, agent.RunInputAddedRecord{RunID: "run-resumed", Text: "resume this task"})
	appendApplicationRecord(t, source, agent.RecordModelResponse, agent.ModelResponseRecord{
		RunID: "run-resumed", Backend: "chat_completions", Model: "resumed-model", Epoch: "epoch-resumed",
		Items: []agent.Item{{Kind: agent.ItemAssistantText, ResponseID: "response-resumed", Text: "saved answer"}},
	})
	appendApplicationRecord(t, source, agent.RecordRunFinished, agent.RunFinishedRecord{RunID: "run-resumed", Status: agent.RunCompleted})
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	summaries, err := application.ListSessions()
	if err != nil || len(summaries) != 2 {
		t.Fatalf("sessions = %#v, %v", summaries, err)
	}
	wantTitles := map[string]string{currentID: "current task", sourceID: "resume this task"}
	for _, summary := range summaries {
		if want, ok := wantTitles[summary.ID]; !ok || summary.Title != want {
			t.Fatalf("session summary = %#v, want titles %#v", summary, wantTitles)
		}
		delete(wantTitles, summary.ID)
	}
	if len(wantTitles) != 0 {
		t.Fatalf("missing session summaries: %#v", wantTitles)
	}
	resumedID, err := application.ResumeSession(context.Background(), session.ShortID(sourceID))
	if err != nil {
		t.Fatal(err)
	}
	if resumedID != sourceID || application.SessionID() != sourceID || application.CurrentModel() != "deepseek/resumed-model" || application.CurrentReasoningEffort() != "high" {
		t.Fatalf("resumed=%q current=%q model=%q effort=%q", resumedID, application.SessionID(), application.CurrentModel(), application.CurrentReasoningEffort())
	}
	if _, err := oldJournal.Records(context.Background()); err == nil {
		t.Fatal("previous journal remained open")
	}
	state, err := application.State(context.Background())
	if err != nil || len(state.Items) != 2 || state.Items[0].Text != "resume this task" || state.Items[1].Text != "saved answer" {
		t.Fatalf("resumed state = %#v, %v", state, err)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestApplicationResumeOpensHistoryWhenSavedModelIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/chat/completions" {
			t.Errorf("request path = %q", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"continued\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	application, _ := newSessionApplication(t)
	application.config.baseURL = server.URL
	sourceID := createUnavailableModelSession(t, application.config.home, application.config.root)

	if _, err := application.ResumeSession(context.Background(), session.ShortID(sourceID)); err != nil {
		t.Fatal(err)
	}
	if application.CurrentModel() != "opencode-go/minimax-m2.5" {
		t.Fatalf("restored model = %q", application.CurrentModel())
	}
	choices := application.ModelChoices()
	currentChoice := slices.IndexFunc(choices, func(choice ModelChoice) bool {
		return strings.EqualFold(choice.URI, application.CurrentModel())
	})
	if currentChoice < 0 || !choices[currentChoice].Unavailable || choices[currentChoice].UnavailableReason == "" {
		t.Fatalf("restored model choice = %#v", choices)
	}
	opened, err := application.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(opened.Items) != 1 || opened.Items[0].Text != "saved question" || opened.Selection.Model != "minimax-m2.5" {
		t.Fatalf("opened history = %#v", opened)
	}

	if _, err := application.Run(context.Background(), "continue it", nil); !errors.Is(err, agent.ErrModelUnavailable) {
		t.Fatalf("run unavailable model error = %v", err)
	}
	unchanged, err := application.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(unchanged.Items) != 1 || unchanged.Selection.Model != "minimax-m2.5" {
		t.Fatalf("failed continuation changed history = %#v", unchanged)
	}
	if err := application.SwitchModel(context.Background(), "deepseek/initial-model", "", ""); err != nil {
		t.Fatal(err)
	}
	result, err := application.Run(context.Background(), "continue it", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "continued" {
		t.Fatalf("continued result = %#v", result)
	}
	continued, err := application.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if continued.Selection.Provider != "deepseek" || continued.Selection.Model != "initial-model" {
		t.Fatalf("continued selection = %#v", continued.Selection)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitResumeKeepsUnavailableSavedModelAndLetsClearStartOver(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	root = canonicalApplicationTestRoot(t, root)
	sourceID := createUnavailableModelSession(t, home, root)

	application, err := Open(context.Background(), Config{
		Home: home, Root: root, ModelURI: "opencode-go/minimax-m2.5", ModelExplicit: true,
		ReasoningEffort: "high", ReasoningEffortExplicit: true,
		Resume: true, ResumePrefix: session.ShortID(sourceID),
		Scope: ScopeMachine, ScopeExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = application.Close() }()
	if _, err := application.Run(context.Background(), "continue", nil); !errors.Is(err, agent.ErrModelUnavailable) ||
		!strings.Contains(err.Error(), `model "opencode-go/minimax-m2.5" is unavailable`) {
		t.Fatalf("headless run error = %v", err)
	}
	if application.CurrentReasoningEffort() != "" {
		t.Fatalf("unavailable model used unsaved reasoning effort %q", application.CurrentReasoningEffort())
	}
	if _, err := application.ClearSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := application.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if application.CurrentModel() != "opencode-go/minimax-m2.5" || len(state.Items) != 0 || application.HasUserTurn() {
		t.Fatalf("cleared unavailable session = model %q, state %#v, user turn %t", application.CurrentModel(), state, application.HasUserTurn())
	}
	if _, err := application.Run(context.Background(), "new task", nil); !errors.Is(err, agent.ErrModelUnavailable) || application.HasUserTurn() {
		t.Fatalf("cleared unavailable run = %v, user turn %t", err, application.HasUserTurn())
	}
}

func TestResumeDoesNotTreatDifferentUnknownModelAsSavedSelection(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	root = canonicalApplicationTestRoot(t, root)
	sourceID := createUnavailableModelSession(t, home, root)

	_, err := Open(context.Background(), Config{
		Home: home, Root: root, ModelURI: "opencode-go/not-the-saved-model", ModelExplicit: true,
		Resume: true, ResumePrefix: session.ShortID(sourceID),
		Scope: ScopeMachine, ScopeExplicit: true,
	})
	if !errors.Is(err, agent.ErrInvalidRequest) || !strings.Contains(err.Error(), "not available in Skot's current model list") {
		t.Fatalf("different unknown model error = %v", err)
	}
}

func TestResumeDoesNotTreatInvalidEffortAsAnUnavailableModel(t *testing.T) {
	home, root := t.TempDir(), canonicalApplicationTestRoot(t, t.TempDir())
	source, sourceID, err := session.Create(home)
	if err != nil {
		t.Fatal(err)
	}
	appendApplicationRecord(t, source, agent.RecordSessionStarted, agent.SessionStartedRecord{
		SchemaVersion: agent.JournalSchemaVersion, SessionID: sourceID, Workspace: root,
	})
	appendApplicationRecord(t, source, agent.RecordModelSelected, agent.ModelSelectedRecord{
		Backend: "chat_completions.deepseek", Provider: "deepseek", Model: "deepseek-v4-flash", Epoch: "epoch-saved",
	})
	appendApplicationRecord(t, source, agent.RecordRunStarted, agent.RunStartedRecord{RunID: "saved-run"})
	appendApplicationRecord(t, source, agent.RecordRunInputAdded, agent.RunInputAddedRecord{RunID: "saved-run", Text: "saved question"})
	appendApplicationRecord(t, source, agent.RecordRunFinished, agent.RunFinishedRecord{RunID: "saved-run", Status: agent.RunCompleted})
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Open(context.Background(), Config{
		Home: home, Root: root, ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		ReasoningEffort: "invalid", ReasoningEffortExplicit: true,
		Resume: true, ResumePrefix: session.ShortID(sourceID), Interactive: true,
		Scope: ScopeMachine, ScopeExplicit: true,
	})
	if !errors.Is(err, agent.ErrInvalidRequest) || !strings.Contains(err.Error(), "reasoning effort") {
		t.Fatalf("invalid effort error = %v", err)
	}
}

func TestSwitchModelRejectsAMistypedProtocol(t *testing.T) {
	application, _ := newSessionApplication(t)
	err := application.SwitchModel(context.Background(), "opencode-go/ox-alpha-free", "", "chat-completions")
	if !errors.Is(err, agent.ErrInvalidRequest) || !strings.Contains(err.Error(), `unsupported model API "chat-completions"`) {
		t.Fatalf("mistyped protocol error = %v", err)
	}
	if application.CurrentModel() == "opencode-go/ox-alpha-free" {
		t.Fatal("mistyped protocol switched the model")
	}
}

func TestApplicationResumeRebuildsUndeclaredRouteFromItsRecordedProtocol(t *testing.T) {
	application, _ := newSessionApplication(t)
	source, sourceID, err := session.Create(application.config.home)
	if err != nil {
		t.Fatal(err)
	}
	appendApplicationRecord(t, source, agent.RecordSessionStarted, agent.SessionStartedRecord{
		SchemaVersion: agent.JournalSchemaVersion, SessionID: sourceID, Workspace: application.config.root,
	})
	appendApplicationRecord(t, source, agent.RecordModelSelected, agent.ModelSelectedRecord{
		Backend: "anthropic_messages.opencode-go", Provider: "opencode-go", Model: "ox-alpha-free", Epoch: "epoch-typed",
	})
	appendApplicationRecord(t, source, agent.RecordRunStarted, agent.RunStartedRecord{RunID: "typed-run"})
	appendApplicationRecord(t, source, agent.RecordRunInputAdded, agent.RunInputAddedRecord{RunID: "typed-run", Text: "saved question"})
	appendApplicationRecord(t, source, agent.RecordRunFinished, agent.RunFinishedRecord{RunID: "typed-run", Status: agent.RunCompleted})
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := application.ResumeSession(context.Background(), session.ShortID(sourceID)); err != nil {
		t.Fatal(err)
	}
	// The session itself is the evidence for a protocol Skot cannot infer, so
	// reopening it must not degrade a route which was already running.
	choices := application.ModelChoices()
	current := slices.IndexFunc(choices, func(choice ModelChoice) bool {
		return strings.EqualFold(choice.URI, "opencode-go/ox-alpha-free")
	})
	if current < 0 || choices[current].Unavailable || choices[current].Protocol != "anthropic_messages" ||
		!choices[current].ProtocolExplicit {
		t.Fatalf("resumed route choice = %#v", choices)
	}
}

// createUnavailableModelSession records a route this build cannot rebuild: its
// gateway needs a declaration, and the protocol the session ran on is no longer
// one Skot implements.
func createUnavailableModelSession(t *testing.T, home, root string) string {
	t.Helper()
	source, sourceID, err := session.Create(home)
	if err != nil {
		t.Fatal(err)
	}
	appendApplicationRecord(t, source, agent.RecordSessionStarted, agent.SessionStartedRecord{
		SchemaVersion: agent.JournalSchemaVersion, SessionID: sourceID, Workspace: root,
	})
	appendApplicationRecord(t, source, agent.RecordModelSelected, agent.ModelSelectedRecord{
		Backend: "legacy_messages.opencode-go", Provider: "opencode-go", Model: "minimax-m2.5", Epoch: "epoch-removed",
	})
	appendApplicationRecord(t, source, agent.RecordRunStarted, agent.RunStartedRecord{RunID: "saved-run"})
	appendApplicationRecord(t, source, agent.RecordRunInputAdded, agent.RunInputAddedRecord{RunID: "saved-run", Text: "saved question"})
	appendApplicationRecord(t, source, agent.RecordRunFinished, agent.RunFinishedRecord{RunID: "saved-run", Status: agent.RunCompleted})
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	return sourceID
}

func TestApplicationResumeUsesSavedOpenRouterContextWhenLookupFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/chat/completions" {
			t.Errorf("request path = %q", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	originalProvider := modelProviderCatalog["openrouter"]
	provider := originalProvider
	provider.baseURL = server.URL
	modelProviderCatalog["openrouter"] = provider
	t.Cleanup(func() { modelProviderCatalog["openrouter"] = originalProvider })

	application, _ := newSessionApplication(t)
	if err := application.config.settings.SetAPIKey("openrouter", "test-key"); err != nil {
		t.Fatal(err)
	}
	lookups := 0
	application.config.metadataLookup = func(context.Context, string) (int, error) {
		lookups++
		return 0, errors.New("offline")
	}

	source, sourceID, err := session.Create(application.config.home)
	if err != nil {
		t.Fatal(err)
	}
	appendApplicationRecord(t, source, agent.RecordSessionStarted, agent.SessionStartedRecord{
		SchemaVersion: agent.JournalSchemaVersion, SessionID: sourceID, Workspace: application.config.root,
	})
	appendApplicationRecord(t, source, agent.RecordModelSelected, agent.ModelSelectedRecord{
		Backend: "chat_completions.openrouter", Provider: "openrouter", Model: "~x-ai/grok-latest",
		ProviderStateContract: defaultModelSpec("openrouter").ChatTraits.ProviderStateContract(), Epoch: "epoch-openrouter",
	})
	appendApplicationRecord(t, source, agent.RecordSessionConfigured, agent.EffectiveConfigSnapshot{
		ModelContext:  agent.ModelContextSnapshot{CompactionInstructions: "compact", ToolLimitInstructions: "limit tools"},
		RuntimePolicy: agent.RuntimePolicySnapshot{ContextWindow: 333_000, MaxModelAttempts: 1, MaxToolIterations: 1},
		Environment:   agent.ExecutionEnvironmentSnapshot{Endpoint: server.URL},
	})
	appendApplicationRecord(t, source, agent.RecordRunStarted, agent.RunStartedRecord{RunID: "saved-run"})
	appendApplicationRecord(t, source, agent.RecordRunInputAdded, agent.RunInputAddedRecord{RunID: "saved-run", Text: "saved task"})
	appendApplicationRecord(t, source, agent.RecordRunFinished, agent.RunFinishedRecord{RunID: "saved-run", Status: agent.RunCompleted})
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := application.ResumeSession(context.Background(), session.ShortID(sourceID)); err != nil {
		t.Fatal(err)
	}
	if _, err := application.Run(context.Background(), "continue", nil); err != nil {
		t.Fatal(err)
	}
	state, err := application.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if lookups != 1 || state.Configured == nil || state.Configured.RuntimePolicy.ContextWindow != 333_000 || state.Configured.RuntimePolicy.ContextWindowEstimated {
		t.Fatalf("lookups/configuration = %d / %#v", lookups, state.Configured)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestApplicationReselectUsesCurrentOpenRouterContextWhenLookupFails(t *testing.T) {
	application, _ := newSessionApplication(t)
	if err := application.config.settings.SetAPIKey("openrouter", "test-key"); err != nil {
		t.Fatal(err)
	}
	lookups := 0
	application.config.metadataLookup = func(context.Context, string) (int, error) {
		lookups++
		if lookups == 1 {
			return 333_000, nil
		}
		return 0, errors.New("offline")
	}

	if err := application.SwitchModel(context.Background(), "openrouter/~x-ai/grok-latest", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := application.SwitchModel(context.Background(), "openrouter/~x-ai/grok-latest", "high", ""); err != nil {
		t.Fatal(err)
	}
	state, err := application.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if lookups != 2 || state.Configured == nil || state.Configured.RuntimePolicy.ContextWindow != 333_000 || state.Configured.RuntimePolicy.ContextWindowEstimated || state.Selection.ReasoningEffort != "high" {
		t.Fatalf("lookups/state = %d / %#v", lookups, state)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenDoesNotTurnInternalRouteReviewStateIntoAStartupWarning(t *testing.T) {
	application, err := Open(context.Background(), Config{
		Home: t.TempDir(), Root: t.TempDir(), ModelURI: "deepseek/unreviewed-model",
		Interactive: true, Scope: workspacetools.ScopeMachine, ScopeExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = application.Close() }()
	if notices := application.StartupNotices(); len(notices) != 0 {
		t.Fatalf("startup notices = %#v", notices)
	}
}

func TestApplicationModelChoicesKeepCurrentEffectiveContext(t *testing.T) {
	runtime, err := newApplicationTestRuntime(agent.Config{
		Model: agent.ModelInfo{
			BackendID: "chat_completions.openrouter", Provider: "openrouter", Model: "~x-ai/grok-latest",
			ContextWindow: 333_000,
		},
		Backend: applicationTestModel{},
		Journal: &applicationMemoryJournal{},
	})
	if err != nil {
		t.Fatal(err)
	}
	application := &Application{state: applicationState{session: newLiveSession("", runtime, nil, false)}}
	for _, choice := range application.ModelChoices() {
		if choice.URI == "openrouter/~x-ai/grok-latest" {
			if choice.ContextWindow != 333_000 || choice.ContextWindowEstimated {
				t.Fatalf("current choice = %#v", choice)
			}
			return
		}
	}
	t.Fatal("current OpenRouter choice is missing")
}

func TestApplicationDoesNotWarnWhenSelectingAnUnreviewedRoute(t *testing.T) {
	settings, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	journal, err := session.Open(filepath.Join(t.TempDir(), "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	runtime, err := newApplicationTestRuntime(agent.Config{Backend: applicationTestModel{}, Journal: journal})
	if err != nil {
		t.Fatal(err)
	}
	application := &Application{
		config: applicationConfig{settings: settings},
		state:  applicationState{session: newLiveSession("", runtime, nil, false)},
	}
	t.Setenv("DEEPSEEK_API_KEY", "secret")

	for range 2 {
		if err := application.SwitchModel(context.Background(), "deepseek/unreviewed-model", "high", ""); err != nil {
			t.Fatal(err)
		}
	}
	if notices := application.StartupNotices(); len(notices) != 0 {
		t.Fatalf("startup notices = %#v", notices)
	}
}

func TestApplicationReportsRepairedJournalTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"sequence":1`), 0o600); err != nil {
		t.Fatal(err)
	}
	application, err := Open(context.Background(), Config{
		Home: t.TempDir(), Root: t.TempDir(), JournalPath: path,
		ModelURI: "deepseek/test-model", Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if notices := strings.Join(application.StartupNotices(), "\n"); !strings.Contains(notices, "repaired an incomplete journal tail") {
		t.Fatalf("startup notices = %q", notices)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("repaired journal size = %d", info.Size())
	}
}

func newSessionApplication(t *testing.T) (*Application, *session.Store) {
	t.Helper()
	home := t.TempDir()
	root := t.TempDir()
	settings, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	processes, err := workspacetools.NewProcessManager(root, home, t.TempDir(), workspacetools.ScopeMachine)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = processes.Close() })
	catalog, _, err := workspacetools.NewWorkspaceTools(root)
	if err != nil {
		t.Fatal(err)
	}
	catalog = append(catalog, processes.Tools()...)
	toolSets, err := toolpolicy.NewToolSets(catalog)
	if err != nil {
		t.Fatal(err)
	}
	selectedTools, err := toolSets.Tools(catalog, toolpolicy.ToolSetDefault)
	if err != nil {
		t.Fatal(err)
	}
	journal, id, err := session.Create(home)
	if err != nil {
		t.Fatal(err)
	}
	appendApplicationRecord(t, journal, agent.RecordSessionStarted, agent.SessionStartedRecord{SchemaVersion: agent.JournalSchemaVersion, SessionID: id, Workspace: root})
	appendApplicationRecord(t, journal, agent.RecordModelSelected, agent.ModelSelectedRecord{
		Backend: "chat_completions", Provider: "deepseek", Model: "initial-model", Epoch: "epoch-initial",
	})
	model := applicationDeepseekModel{}
	runtime, err := newApplicationTestRuntime(agent.Config{
		Model:   agent.ModelInfo{BackendID: "chat_completions", Provider: "deepseek", Model: "initial-model"},
		Backend: model, Journal: journal, SessionID: id, Workspace: root,
		Tools: selectedTools, UserShell: processes.RunShell,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Application{
		config: applicationConfig{
			settings: settings, root: root, home: home, tools: catalog, toolSets: toolSets,
			retryBudget: DefaultRetryBudget, streamIdleTimeout: DefaultStreamIdleTimeout,
			maxToolIterations: agent.DefaultMaxToolIterations,
		},
		state: applicationState{
			session: newLiveSession(id, runtime, journal, true), processes: processes,
			toolSet: toolpolicy.ToolSetDefault, security: securityState{Scope: workspacetools.ScopeMachine},
		},
	}, journal
}

func appendApplicationRecord(t *testing.T, journal *session.Store, kind agent.RecordKind, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), agent.PendingRecord{Kind: kind, Data: data}); err != nil {
		t.Fatal(err)
	}
}

type applicationTestModel struct{}

func newApplicationTestRuntime(config agent.Config) (*agent.Runtime, error) {
	if config.Model.BackendID == "" {
		config.Model = agent.ModelInfo{BackendID: "test", Provider: "test", Model: "initial"}
	}
	return agent.New(config)
}

type toolSetCaptureModel struct {
	t    *testing.T
	want string
}

func (model toolSetCaptureModel) Complete(_ context.Context, request agent.ModelRequest, _ func(agent.ModelStreamEvent)) (agent.ModelResponse, error) {
	names := make([]string, 0, len(request.Tools))
	for _, tool := range request.Tools {
		names = append(names, tool.Name)
	}
	if got := strings.Join(names, ","); got != model.want {
		model.t.Fatalf("tools = %q, want %q", got, model.want)
	}
	return agent.ModelResponse{Items: []agent.Item{{Kind: agent.ItemAssistantText, Text: "done"}}}, nil
}

func applicationTool(name string) agent.Tool {
	return agent.Tool{
		Spec: agent.ToolSpec{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run:  func(context.Context, string) (agent.ToolOutput, error) { return agent.ToolOutput{}, nil },
	}
}

func configuredToolNames(state agent.State) string {
	if state.Configured == nil {
		return ""
	}
	names := make([]string, 0, len(state.Configured.ModelContext.Tools))
	for _, tool := range state.Configured.ModelContext.Tools {
		names = append(names, tool.Name)
	}
	return strings.Join(names, ",")
}

type applicationMemoryJournal struct {
	records []agent.Record
}

func (journal *applicationMemoryJournal) Append(_ context.Context, pending agent.PendingRecord) (agent.Record, error) {
	record := agent.Record{
		Sequence: uint64(len(journal.records) + 1), Time: time.Now().UTC(), Kind: pending.Kind,
		Data: append(json.RawMessage(nil), pending.Data...),
	}
	journal.records = append(journal.records, record)
	return record, nil
}

func (journal *applicationMemoryJournal) Records(context.Context) ([]agent.Record, error) {
	return append([]agent.Record(nil), journal.records...), nil
}

func (applicationTestModel) Complete(context.Context, agent.ModelRequest, func(agent.ModelStreamEvent)) (agent.ModelResponse, error) {
	return agent.ModelResponse{}, errors.New("unused")
}

type applicationDeepseekModel struct{}

func (applicationDeepseekModel) Complete(context.Context, agent.ModelRequest, func(agent.ModelStreamEvent)) (agent.ModelResponse, error) {
	return agent.ModelResponse{}, errors.New("unused")
}

func (model toolSetCaptureModel) ProjectModelItems(items []agent.Item) []agent.Item { return items }

func (applicationTestModel) ProjectModelItems(items []agent.Item) []agent.Item { return items }

func (applicationDeepseekModel) ProjectModelItems(items []agent.Item) []agent.Item { return items }
