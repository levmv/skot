package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/session"
	"github.com/levmv/skot/internal/state"
	"github.com/levmv/skot/internal/toolpolicy"
	workspacetools "github.com/levmv/skot/tools"
)

func TestApplicationSwitchesAndPersistsRequestedSandbox(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	settings, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	processes, err := workspacetools.NewProcessManager(root, home, t.TempDir(), workspacetools.SandboxMasked)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = processes.Close() })
	journal, err := session.Open(filepath.Join(t.TempDir(), "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	runtime, err := agent.New(agent.Config{Model: applicationTestModel{}, Journal: journal})
	if err != nil {
		t.Fatal(err)
	}
	application := &Application{
		config: applicationConfig{settings: settings, root: root, home: home},
		state: applicationState{
			session:          newLiveSession("", runtime, nil, false),
			processes:        processes,
			requestedSandbox: workspacetools.SandboxAuto,
			security:         securityState{RequestedPolicy: workspacetools.SandboxAuto, EffectivePolicy: workspacetools.SandboxMasked, Backend: "landlock", Container: "docker"},
		},
	}

	if err := application.SwitchSandbox(context.Background(), workspacetools.SandboxOff); err != nil {
		t.Fatal(err)
	}
	if application.CurrentSandbox() != workspacetools.SandboxOff || application.SecuritySummary() != "sandbox: off · network: inherited" {
		t.Fatalf("sandbox = %q, security = %q", application.CurrentSandbox(), application.SecuritySummary())
	}
	stored, err := settings.Settings()
	if err != nil || stored.Sandbox != workspacetools.SandboxOff {
		t.Fatalf("stored sandbox = %q, %v", stored.Sandbox, err)
	}
	if _, err := processes.RunShell(context.Background(), "true"); err != nil {
		t.Fatalf("process manager unusable after switch: %v", err)
	}
}

func TestApplicationRecordsResolvedProductConfiguration(t *testing.T) {
	application, err := Open(context.Background(), Config{
		Home: t.TempDir(), Root: t.TempDir(), ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		Profile: toolpolicy.ProfileReadOnly, ProfileExplicit: true,
		Sandbox: workspacetools.SandboxOff, SandboxExplicit: true, Interactive: true,
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
	if snapshot.ModelContext.ToolProfile != toolpolicy.ProfileReadOnly || len(snapshot.ModelContext.Tools) == 0 || snapshot.ModelContext.CompactionInstructions == "" {
		t.Fatalf("model context = %#v", snapshot.ModelContext)
	}
	if snapshot.Environment.Endpoint != "https://api.deepseek.com/v1" || snapshot.Environment.Sandbox.RequestedPolicy != workspacetools.SandboxOff || snapshot.Environment.Sandbox.EffectivePolicy != workspacetools.SandboxOff || snapshot.Environment.Sandbox.Network != "inherited" {
		t.Fatalf("environment = %#v", snapshot.Environment)
	}
	if snapshot.RuntimePolicy.AwaitRequiredJobs || snapshot.RuntimePolicy.ContextWindow != 1_000_000 || snapshot.RuntimePolicy.MaxRequestBytes == 0 || snapshot.RuntimePolicy.MaxCompletionBytes == 0 || snapshot.RuntimePolicy.MaxModelAttempts != -1 || snapshot.RuntimePolicy.RetryBudget != DefaultRetryBudget.String() || snapshot.RuntimePolicy.StreamIdleTimeout != DefaultStreamIdleTimeout.String() || snapshot.RuntimePolicy.MaxToolIterations != agent.DefaultMaxToolIterations {
		t.Fatalf("runtime policy = %#v", snapshot.RuntimePolicy)
	}
	if snapshot.ModelContext.ToolLimitInstructions == "" {
		t.Fatalf("model context = %#v", snapshot.ModelContext)
	}
}

func TestOpenMergesConfiguredProtectedPathsAndSandboxSwitchEnforcesThem(t *testing.T) {
	if workspacetools.SandboxBackend() == "" {
		t.Skip("platform sandbox is unavailable")
	}
	home, root := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"protected_paths":["settings-secret"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "settings-secret"), []byte("settings\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	application, err := Open(context.Background(), Config{
		Home: home, Root: root, ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		Sandbox: SandboxOff, SandboxExplicit: true, Interactive: true,
		ProtectedPaths: []string{"api-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	wantPaths := []string{
		canonicalSecurityPath(home),
		canonicalSecurityPath(filepath.Join(root, "settings-secret")),
		canonicalSecurityPath(filepath.Join(root, "api-secret")),
	}
	for _, want := range wantPaths {
		if !slices.Contains(application.config.protectedPaths, want) {
			t.Fatalf("protected paths = %#v; missing %q", application.config.protectedPaths, want)
		}
	}
	read := func() error {
		for _, tool := range application.config.tools {
			if tool.Spec.Name == "read" {
				_, err := tool.Run(context.Background(), `{"path":"settings-secret"}`)
				return err
			}
		}
		return errors.New("read tool not found")
	}
	if err := read(); err != nil {
		t.Fatalf("off read rejected: %v", err)
	}
	if err := application.SwitchSandbox(context.Background(), SandboxMasked); err != nil {
		t.Fatal(err)
	}
	if err := read(); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("masked read error = %v", err)
	}
}

func TestOpenConfiguresAndOwnsCompleteToolCatalog(t *testing.T) {
	var borrowed []agent.Tool
	seenDefaults := make(map[string]bool)
	application, err := Open(context.Background(), Config{
		Home: t.TempDir(), Root: t.TempDir(), ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		Profile: toolpolicy.ProfileFull, ProfileExplicit: true,
		Sandbox: workspacetools.SandboxOff, SandboxExplicit: true, Interactive: true,
		Profiles: map[string][]string{
			toolpolicy.ProfileReadOnly: {"ls", "grep", "glob"},
			toolpolicy.ProfileEdit:     {"ls", "grep", "glob", "edit", "write"},
			toolpolicy.ProfileFull:     {"grep", "glob", "edit", "write", "bash", "job", "custom"},
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

func TestOpenLoadsProgramToolsIntoExactProfilesAndRecordsResolvedRuntime(t *testing.T) {
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
	settings := `{"profiles":{"full":["read","grep","glob","edit","write","bash","job","lookup"]}}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	application, err := Open(context.Background(), Config{
		Home: home, Root: t.TempDir(), ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		Profile: toolpolicy.ProfileFull, ProfileExplicit: true,
		Sandbox: workspacetools.SandboxOff, SandboxExplicit: true, Interactive: true,
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
	if err := application.SwitchProfile(context.Background(), toolpolicy.ProfileReadOnly); err != nil {
		t.Fatal(err)
	}
	state, err = application.State(context.Background())
	if err != nil || state.Configured == nil || len(state.Configured.Environment.ProgramTools) != 0 {
		t.Fatalf("inactive program remained in snapshot: %#v, %v", state.Configured, err)
	}
}

func TestOpenRejectsBackgroundProgramProfileWithoutJob(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "tools.json"), []byte(`{"tools":[{"name":"worker","description":"work","command":["true"],"background":"always"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), Config{
		Home: home, Root: t.TempDir(), ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		Profile: "worker-only", ProfileExplicit: true,
		Profiles: map[string][]string{"worker-only": {"worker"}},
		Sandbox:  workspacetools.SandboxOff, SandboxExplicit: true, Interactive: true,
	})
	if !errors.Is(err, agent.ErrInvalidRequest) || !strings.Contains(err.Error(), `not required tool "job"`) {
		t.Fatalf("error = %v", err)
	}
}

// Bash needs the job tool for the same reason a background-capable program
// does, and needs it without anyone having asked for background: a foreground
// command still running after the yield hands the model a job id.
func TestOpenRejectsBashProfileWithoutJob(t *testing.T) {
	_, err := Open(context.Background(), Config{
		Home: t.TempDir(), Root: t.TempDir(), ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		Profile: "shell-only", ProfileExplicit: true,
		Profiles: map[string][]string{"shell-only": {"read", "bash"}},
		Sandbox:  workspacetools.SandboxOff, SandboxExplicit: true, Interactive: true,
	})
	if !errors.Is(err, agent.ErrInvalidRequest) || !strings.Contains(err.Error(), `not required tool "job"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenValidatesCompleteCatalogBeforeProfileFiltering(t *testing.T) {
	_, err := Open(context.Background(), Config{
		Home: t.TempDir(), Root: t.TempDir(), ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		Profile: toolpolicy.ProfileReadOnly, ProfileExplicit: true,
		Sandbox: workspacetools.SandboxOff, SandboxExplicit: true, Interactive: true,
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
		Sandbox: workspacetools.SandboxOff, SandboxExplicit: true, Interactive: true,
		Profiles: map[string][]string{
			toolpolicy.ProfileReadOnly: nil,
			toolpolicy.ProfileEdit:     nil,
			toolpolicy.ProfileFull:     nil,
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

func TestOpenLoadsCustomProfilesAndLetsConfigReplaceBuiltIns(t *testing.T) {
	home := t.TempDir()
	settings := `{"profiles":{"full":["read"],"review":["read","custom"]}}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	configuredProfiles := map[string][]string{toolpolicy.ProfileFull: {"custom"}}
	application, err := Open(context.Background(), Config{
		Home: home, Root: t.TempDir(), ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true,
		Profile: toolpolicy.ProfileFull, ProfileExplicit: true,
		Profiles: configuredProfiles,
		Sandbox:  workspacetools.SandboxOff, SandboxExplicit: true, Interactive: true,
		ConfigureTools: func(catalog []agent.Tool) ([]agent.Tool, error) {
			return append(catalog, applicationTool("custom")), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	configuredProfiles[toolpolicy.ProfileFull][0] = "read"
	if got := strings.Join(application.Profiles(), ","); got != "read-only,edit,full,review" {
		t.Fatalf("profiles = %q", got)
	}

	if _, err := application.RunShell(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	state, err := application.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := configuredToolNames(state); got != "custom" {
		t.Fatalf("overridden full tools = %q", got)
	}

	if err := application.SwitchProfile(context.Background(), " REVIEW "); err != nil {
		t.Fatal(err)
	}
	state, err = application.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := configuredToolNames(state); got != "read,custom" || application.CurrentProfile() != "review" {
		t.Fatalf("review tools = %q, current = %q", got, application.CurrentProfile())
	}
	stored, err := application.config.settings.Settings()
	if err != nil || strings.Join(stored.Profiles["review"], ",") != "read,custom" {
		t.Fatalf("stored profiles = %#v, %v", stored.Profiles, err)
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

func TestApplicationRejectsUnknownSandboxWithoutChangingState(t *testing.T) {
	application := &Application{state: applicationState{requestedSandbox: workspacetools.SandboxAuto}}
	if err := application.SwitchSandbox(context.Background(), "magic"); err == nil {
		t.Fatal("unknown sandbox accepted")
	}
	if application.CurrentSandbox() != workspacetools.SandboxAuto {
		t.Fatalf("sandbox changed to %q", application.CurrentSandbox())
	}
}

func TestSecuritySummaryReportsRunningProcessesWithEarlierSandboxPolicy(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	processes, err := workspacetools.NewProcessManager(root, home, t.TempDir(), workspacetools.SandboxOff)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = processes.Close() })
	tool := processes.Tools()[0]
	ctx := agent.WithToolSessionID(context.Background(), "session-test")
	if _, err := tool.Run(ctx, `{"command":"sleep 30","background":true}`); err != nil {
		t.Fatal(err)
	}
	if err := processes.SetSandboxAfter(workspacetools.SandboxWorkspace, nil); err != nil {
		t.Fatal(err)
	}
	application := &Application{
		state: applicationState{
			processes: processes,
			security: securityState{
				RequestedPolicy: workspacetools.SandboxWorkspace,
				EffectivePolicy: workspacetools.SandboxWorkspace,
			},
		},
	}
	if summary := application.SecuritySummary(); !strings.Contains(summary, "running processes retain launch policy: off (1)") {
		t.Fatalf("security summary = %q", summary)
	}
}

func TestApplicationBuildsAndPersistsSelectedModel(t *testing.T) {
	home := t.TempDir()
	settings, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := session.Open(filepath.Join(t.TempDir(), "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	runtime, err := agent.New(agent.Config{
		Model:   applicationTestModel{},
		Journal: journal,
	})
	if err != nil {
		t.Fatal(err)
	}
	application := &Application{
		config: applicationConfig{settings: settings},
		state:  applicationState{session: newLiveSession("", runtime, nil, false)},
	}
	t.Setenv("DEEPSEEK_API_KEY", "secret")
	if err := application.SwitchModel(context.Background(), "deepseek/new-model", "high"); err != nil {
		t.Fatal(err)
	}
	stored, err := settings.Settings()
	if err != nil || stored.Model != "deepseek/new-model" || stored.ReasoningEffort != "high" || application.CurrentModel() != "deepseek/new-model" || application.CurrentReasoningEffort() != "high" {
		t.Fatalf("stored=%#v current=%q err=%v", stored, application.CurrentModel(), err)
	}
}

func TestApplicationOwnsAndPersistsToolProfile(t *testing.T) {
	settings, err := state.Open(t.TempDir())
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
	profiles, err := toolpolicy.NewProfiles(catalog)
	if err != nil {
		t.Fatal(err)
	}
	selectedTools, err := profiles.Tools(catalog, toolpolicy.ProfileFull)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.New(agent.Config{
		Model:   profileCaptureModel{t: t, want: "read,ls,grep,glob"},
		Journal: &applicationMemoryJournal{},
		Tools:   selectedTools,
	})
	if err != nil {
		t.Fatal(err)
	}
	application := &Application{
		config: applicationConfig{settings: settings, tools: catalog, profiles: profiles},
		state: applicationState{
			session: newLiveSession("", runtime, nil, false),
			profile: toolpolicy.ProfileFull,
		},
	}
	if err := application.SwitchProfile(context.Background(), toolpolicy.ProfileReadOnly); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "check tools", nil); err != nil {
		t.Fatal(err)
	}
	stored, err := settings.Settings()
	if err != nil || stored.Profile != toolpolicy.ProfileReadOnly || application.CurrentProfile() != toolpolicy.ProfileReadOnly {
		t.Fatalf("stored=%q current=%q err=%v", stored.Profile, application.CurrentProfile(), err)
	}
}

func TestApplicationRejectsRunWithoutCredentialBeforeJournalMutation(t *testing.T) {
	home := t.TempDir()
	settings, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := session.Open(filepath.Join(t.TempDir(), "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	runtime, err := agent.New(agent.Config{Model: applicationTestModel{}, Journal: journal})
	if err != nil {
		t.Fatal(err)
	}
	application := &Application{
		config: applicationConfig{settings: settings},
		state:  applicationState{session: newLiveSession("", runtime, nil, false)},
	}
	if _, err := application.Run(context.Background(), "do work", nil); err == nil || !strings.Contains(err.Error(), "/login test") || !errors.Is(err, agent.ErrInvalidRequest) {
		t.Fatalf("missing credential error = %v", err)
	}
	records, err := journal.Records(context.Background())
	if err != nil || len(records) != 0 {
		t.Fatalf("journal records = %#v, %v", records, err)
	}
}

func TestApplicationCustomBaseURLDoesNotRequireCredential(t *testing.T) {
	home := t.TempDir()
	settings, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := session.Open(filepath.Join(t.TempDir(), "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	runtime, err := agent.New(agent.Config{Model: applicationReplyModel{}, Journal: journal})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	application := &Application{
		config: applicationConfig{settings: settings, baseURL: "https://gateway.example/v1"},
		state:  applicationState{session: newLiveSession("", runtime, nil, false)},
	}
	result, err := application.Run(context.Background(), "do work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != agent.RunCompleted || result.Answer != "done" {
		t.Fatalf("run result = %#v", result)
	}
}

func TestApplicationClearSessionSwapsJournalAndPrunesEmptySession(t *testing.T) {
	application, oldJournal := newSessionApplication(t)
	oldID := application.SessionID()
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
	process, ok := workspacetools.ProcessResultFromDetail(started.Details[0])
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
		Backend: "chat_completions", Provider: "deepseek", Model: "resumed-model", Epoch: "epoch-resumed",
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
	if resumedID != sourceID || application.SessionID() != sourceID || application.CurrentModel() != "deepseek/resumed-model" {
		t.Fatalf("resumed=%q current=%q model=%q", resumedID, application.SessionID(), application.CurrentModel())
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
	processes, err := workspacetools.NewProcessManager(root, home, t.TempDir(), workspacetools.SandboxOff)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = processes.Close() })
	catalog, _, err := workspacetools.NewWorkspaceTools(root)
	if err != nil {
		t.Fatal(err)
	}
	catalog = append(catalog, processes.Tools()...)
	profiles, err := toolpolicy.NewProfiles(catalog)
	if err != nil {
		t.Fatal(err)
	}
	selectedTools, err := profiles.Tools(catalog, toolpolicy.ProfileFull)
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
	model := applicationDeepseekModel{model: "initial-model"}
	runtime, err := agent.New(agent.Config{
		Model: model, Journal: journal, SessionID: id, Workspace: root,
		Tools: selectedTools, UserShell: processes.RunShell,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Application{
		config: applicationConfig{
			settings: settings, root: root, home: home, tools: catalog, profiles: profiles,
			retryBudget: DefaultRetryBudget, streamIdleTimeout: DefaultStreamIdleTimeout,
			maxToolIterations: agent.DefaultMaxToolIterations,
		},
		state: applicationState{
			session: newLiveSession(id, runtime, journal, true), processes: processes,
			profile: toolpolicy.ProfileFull, requestedSandbox: workspacetools.SandboxOff,
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

type profileCaptureModel struct {
	t    *testing.T
	want string
}

func (model profileCaptureModel) Info() agent.ModelInfo {
	return agent.ModelInfo{Backend: "test", Model: "profile-capture"}
}

func (model profileCaptureModel) Complete(_ context.Context, request agent.ModelRequest, _ func(agent.ModelStreamEvent)) (agent.ModelResponse, error) {
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

func (applicationTestModel) Info() agent.ModelInfo {
	return agent.ModelInfo{Backend: "test", Provider: "test", Model: "initial"}
}

func (applicationTestModel) Complete(context.Context, agent.ModelRequest, func(agent.ModelStreamEvent)) (agent.ModelResponse, error) {
	return agent.ModelResponse{}, errors.New("unused")
}

type applicationReplyModel struct{}

func (applicationReplyModel) Info() agent.ModelInfo {
	return agent.ModelInfo{Backend: "test", Provider: "deepseek", Model: "local"}
}

func (applicationReplyModel) Complete(context.Context, agent.ModelRequest, func(agent.ModelStreamEvent)) (agent.ModelResponse, error) {
	return agent.ModelResponse{Items: []agent.Item{{Kind: agent.ItemAssistantText, Text: "done"}}}, nil
}

type applicationDeepseekModel struct{ model string }

func (model applicationDeepseekModel) Info() agent.ModelInfo {
	return agent.ModelInfo{Backend: "chat_completions", Provider: "deepseek", Model: model.model}
}

func (applicationDeepseekModel) Complete(context.Context, agent.ModelRequest, func(agent.ModelStreamEvent)) (agent.ModelResponse, error) {
	return agent.ModelResponse{}, errors.New("unused")
}
