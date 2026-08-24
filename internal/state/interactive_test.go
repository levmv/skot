package state

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	interactiveMutationProcessEnv = "SKOT_TEST_INTERACTIVE_MUTATION_PROCESS"
	interactiveMutationHomeEnv    = "SKOT_TEST_INTERACTIVE_MUTATION_HOME"
	interactiveMutationRootEnv    = "SKOT_TEST_INTERACTIVE_MUTATION_ROOT"
	interactiveMutationFieldEnv   = "SKOT_TEST_INTERACTIVE_MUTATION_FIELD"
	interactiveMutationValueEnv   = "SKOT_TEST_INTERACTIVE_MUTATION_VALUE"
	interactiveMutationReadyEnv   = "SKOT_TEST_INTERACTIVE_MUTATION_READY"
	interactiveMutationGoEnv      = "SKOT_TEST_INTERACTIVE_MUTATION_GO"
	interactiveMutationAttemptEnv = "SKOT_TEST_INTERACTIVE_MUTATION_ATTEMPT"
	interactiveMutationDoneEnv    = "SKOT_TEST_INTERACTIVE_MUTATION_DONE"
)

func TestInteractiveStoreReadOnlyOpenCreatesNeitherStateNorLock(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	if _, err := Open(home); err != nil {
		t.Fatal(err)
	}
	store, err := OpenInteractive(home, root)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Theme != "" || settings.Workspace.Model != "" || settings.Workspace.ReasoningEffort != nil || len(settings.ModelHistory) != 0 {
		t.Fatalf("empty settings = %#v", settings)
	}
	for _, name := range []string{"interactive.json", "interactive.lock"} {
		if _, err := os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Fatalf("read-only open created %s: %v", name, err)
		}
	}
}

func TestInteractiveStoreReadPreservesStricterPermissions(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	path := filepath.Join(home, "interactive.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o400); err != nil {
		t.Fatal(err)
	}
	store, err := OpenInteractive(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Settings(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o400 {
		t.Fatalf("interactive mode = %v, %v", info, err)
	}
}

func TestInteractiveStoreRepairsSharedLockPermissions(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	path := filepath.Join(home, "interactive.lock")
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	store, err := OpenInteractive(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetThemeSelection("dark"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock mode = %v, %v", info, err)
	}
}

func TestInteractiveStorePersistsExplicitProviderDefaultAndWorkspaceMap(t *testing.T) {
	home, firstRoot, secondRoot := t.TempDir(), t.TempDir(), t.TempDir()
	if _, err := Open(home); err != nil {
		t.Fatal(err)
	}
	first, err := OpenInteractive(home, firstRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.SetModelSelection("openai/gpt-5", "", ""); err != nil {
		t.Fatal(err)
	}
	second, err := OpenInteractive(home, secondRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.SetToolSetSelection("edit"); err != nil {
		t.Fatal(err)
	}
	added, protected := filepath.Join(secondRoot, "shared"), filepath.Join(secondRoot, ".env")
	if err := second.SetFilesystemPaths([]string{added}, []string{protected}); err != nil {
		t.Fatal(err)
	}
	firstSettings, err := first.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if firstSettings.Workspace.Model != "openai/gpt-5" || firstSettings.Workspace.ReasoningEffort == nil || *firstSettings.Workspace.ReasoningEffort != "" {
		t.Fatalf("first workspace = %#v", firstSettings.Workspace)
	}
	secondSettings, err := second.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if secondSettings.Workspace.ToolSet != "edit" || secondSettings.Workspace.ReasoningEffort != nil ||
		len(secondSettings.Workspace.AddedPaths) != 1 || secondSettings.Workspace.AddedPaths[0] != added ||
		len(secondSettings.Workspace.ProtectedPaths) != 1 || secondSettings.Workspace.ProtectedPaths[0] != protected {
		t.Fatalf("second workspace = %#v", secondSettings.Workspace)
	}
	raw, err := os.ReadFile(filepath.Join(home, "interactive.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"reasoning_effort": "default"`) || !strings.Contains(string(raw), `"added_paths"`) ||
		!strings.Contains(string(raw), `"protected_paths"`) || !strings.Contains(string(raw), firstRoot) || !strings.Contains(string(raw), secondRoot) {
		t.Fatalf("interactive document = %s", raw)
	}
	for _, name := range []string{"interactive.json", "interactive.lock"} {
		info, err := os.Stat(filepath.Join(home, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, %v", name, info, err)
		}
	}
}

func TestLastModelSelectionIsVisibleFromWorkspacesWithoutOwnRecord(t *testing.T) {
	home, firstRoot, secondRoot := t.TempDir(), t.TempDir(), t.TempDir()
	if _, err := Open(home); err != nil {
		t.Fatal(err)
	}
	first, err := OpenInteractive(home, firstRoot)
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenInteractive(home, secondRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.SetModelSelection("openai/gpt-5", "high", ""); err != nil {
		t.Fatal(err)
	}
	settings, err := second.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Workspace.Model != "" || settings.LastModel().Model != "openai/gpt-5" ||
		settings.LastModel().ReasoningEffort == nil || *settings.LastModel().ReasoningEffort != "high" {
		t.Fatalf("second workspace = %#v, history = %#v", settings.Workspace, settings.ModelHistory)
	}
	// Reselecting the model this workspace already records must still publish it
	// for workspaces which have none.
	if err := second.SetModelSelection("deepseek/other-model", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := first.SetModelSelection("openai/gpt-5", "high", ""); err != nil {
		t.Fatal(err)
	}
	settings, err = first.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.LastModel().Model != "openai/gpt-5" {
		t.Fatalf("history = %#v", settings.ModelHistory)
	}
}

func TestLegacyRecentModelsKeyIsIgnoredAndDroppedOnTheNextWrite(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	if _, err := Open(home); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "interactive.json")
	legacy := `{"version":1,"ui":{"theme":"dark","recent_models":["openrouter/older"]}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenInteractive(home, root)
	if err != nil {
		t.Fatalf("a pre-history document must stay readable: %v", err)
	}
	settings, err := store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Theme != ThemeDark || len(settings.ModelHistory) != 0 {
		t.Fatalf("settings = %#v", settings)
	}
	if err := store.SetModelSelection("openai/gpt-5", "", ""); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "recent_models") || !strings.Contains(string(raw), "model_history") {
		t.Fatalf("rewritten document = %s", raw)
	}
}

func TestModelHistoryLeadsWithTheSelectionAndStaysUniqueAndBounded(t *testing.T) {
	history := []modelHistoryDocument{{Model: "OPENAI/Old", ReasoningEffort: "high"}, {Model: "deepseek/one"}}
	for index := range 30 {
		history = append(history, modelHistoryDocument{Model: fmt.Sprintf("provider/model-%02d", index)})
	}
	updated := pushModelHistory(history, modelHistoryDocument{Model: "openai/old", ReasoningEffort: "default"})
	if len(updated) != maxModelHistory || updated[0].Model != "openai/old" || updated[0].ReasoningEffort != "default" {
		t.Fatalf("history = %#v", updated)
	}
	if updated[1].Model != "deepseek/one" {
		t.Fatalf("earlier selections lost their order: %#v", updated)
	}
	for _, entry := range updated[1:] {
		if strings.EqualFold(entry.Model, "openai/old") {
			t.Fatalf("reselected model was kept twice: %#v", updated)
		}
	}
}

func TestInteractiveStoreConcurrentProcessMutationsDoNotLoseUpdates(t *testing.T) {
	home, firstRoot, secondRoot := t.TempDir(), t.TempDir(), t.TempDir()
	if _, err := Open(home); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireInteractiveLock(filepath.Join(home, "interactive.lock"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	t.Cleanup(func() {
		if locked {
			_ = releaseInteractiveLock(lock)
		}
	})

	coordination := t.TempDir()
	goPath := filepath.Join(coordination, "go")
	type childProcess struct {
		command                *exec.Cmd
		output                 *bytes.Buffer
		ready, attempted, done string
	}
	specs := []struct {
		root, field, value string
	}{
		{root: firstRoot, field: "scope", value: "machine"},
		{root: secondRoot, field: "tool_set", value: "read-only"},
	}
	children := make([]childProcess, 0, len(specs))
	for index, spec := range specs {
		ready := filepath.Join(coordination, fmt.Sprintf("ready-%d", index))
		attempted := filepath.Join(coordination, fmt.Sprintf("attempted-%d", index))
		done := filepath.Join(coordination, fmt.Sprintf("done-%d", index))
		output := new(bytes.Buffer)
		command := exec.Command(os.Args[0], "-test.run=^TestInteractiveStoreMutationProcess$")
		command.Env = append(os.Environ(),
			interactiveMutationProcessEnv+"=1",
			interactiveMutationHomeEnv+"="+home,
			interactiveMutationRootEnv+"="+spec.root,
			interactiveMutationFieldEnv+"="+spec.field,
			interactiveMutationValueEnv+"="+spec.value,
			interactiveMutationReadyEnv+"="+ready,
			interactiveMutationGoEnv+"="+goPath,
			interactiveMutationAttemptEnv+"="+attempted,
			interactiveMutationDoneEnv+"="+done,
		)
		command.Stdout = output
		command.Stderr = output
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = command.Process.Kill() })
		children = append(children, childProcess{
			command: command, output: output, ready: ready, attempted: attempted, done: done,
		})
	}
	readyPaths := make([]string, 0, len(children))
	for _, child := range children {
		readyPaths = append(readyPaths, child.ready)
	}
	if err := waitForInteractiveTestFiles(readyPaths, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goPath, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	attemptedPaths := make([]string, 0, len(children))
	for _, child := range children {
		attemptedPaths = append(attemptedPaths, child.attempted)
	}
	if err := waitForInteractiveTestFiles(attemptedPaths, time.Second); err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		if _, err := os.Stat(child.done); !os.IsNotExist(err) {
			t.Fatalf("mutation completed while the parent held the lock: %v", err)
		}
	}
	locked = false
	if err := releaseInteractiveLock(lock); err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		if err := child.command.Wait(); err != nil {
			t.Fatalf("mutation process: %v\n%s", err, child.output.String())
		}
	}

	first, err := OpenInteractive(home, firstRoot)
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenInteractive(home, secondRoot)
	if err != nil {
		t.Fatal(err)
	}
	firstSettings, err := first.Settings()
	if err != nil {
		t.Fatal(err)
	}
	secondSettings, err := second.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if firstSettings.Workspace.Scope != "machine" || secondSettings.Workspace.ToolSet != "read-only" {
		t.Fatalf("lost update: first=%#v second=%#v", firstSettings.Workspace, secondSettings.Workspace)
	}
}

func TestInteractiveStoreMutationProcess(t *testing.T) {
	if os.Getenv(interactiveMutationProcessEnv) != "1" {
		return
	}
	store, err := OpenInteractive(os.Getenv(interactiveMutationHomeEnv), os.Getenv(interactiveMutationRootEnv))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(interactiveMutationReadyEnv), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitForInteractiveTestFiles([]string{os.Getenv(interactiveMutationGoEnv)}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(interactiveMutationAttemptEnv), []byte("attempted"), 0o600); err != nil {
		t.Fatal(err)
	}
	switch os.Getenv(interactiveMutationFieldEnv) {
	case "scope":
		err = store.SetScopeSelection(os.Getenv(interactiveMutationValueEnv))
	case "tool_set":
		err = store.SetToolSetSelection(os.Getenv(interactiveMutationValueEnv))
	default:
		t.Fatalf("unknown mutation field %q", os.Getenv(interactiveMutationFieldEnv))
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(interactiveMutationDoneEnv), []byte("done"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitForInteractiveTestFiles(paths []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ready := true
		for _, path := range paths {
			if _, err := os.Stat(path); err != nil {
				if !os.IsNotExist(err) {
					return err
				}
				ready = false
			}
		}
		if ready {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timed out waiting for test files: %v", paths)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestInteractiveStoreIgnoresKnownInvalidValuesWithoutHealingOtherFields(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	if _, err := Open(home); err != nil {
		t.Fatal(err)
	}
	document := map[string]any{
		"version": 1,
		"ui":      map[string]any{"theme": "sepia"},
		"workspaces": map[string]any{root: map[string]any{
			"model": "openai/gpt-5", "reasoning_effort": "", "scope": "magic",
		}},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "interactive.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenInteractive(home, root)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Theme != "" || settings.Workspace.Scope != "" || settings.Workspace.ReasoningEffort != nil || len(settings.Notices) != 3 {
		t.Fatalf("validated settings = %#v", settings)
	}
	if err := store.SetToolSetSelection("edit"); err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`"theme": "sepia"`, `"reasoning_effort": ""`, `"scope": "magic"`, `"tool_set": "edit"`} {
		if !strings.Contains(string(persisted), value) {
			t.Fatalf("explicit mutation healed unrelated invalid value %s: %s", value, persisted)
		}
	}
}

func TestInteractiveStoreLockTimesOutWithoutOverwritingState(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	if _, err := Open(home); err != nil {
		t.Fatal(err)
	}
	store, err := OpenInteractive(home, root)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := acquireInteractiveLock(filepath.Join(home, "interactive.lock"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseInteractiveLock(lock)
	store.lockTimeout = 25 * time.Millisecond
	if err := store.SetScopeSelection("machine"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("lock error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "interactive.json")); !os.IsNotExist(err) {
		t.Fatalf("timed-out mutation created state: %v", err)
	}
}

func TestInteractiveStoreRejectsUnknownFieldsAndSymlinks(t *testing.T) {
	t.Run("unknown field", func(t *testing.T) {
		home, root := t.TempDir(), t.TempDir()
		if _, err := Open(home); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "interactive.json"), []byte(`{"version":1,"unknown":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenInteractive(home, root); err == nil || !strings.Contains(err.Error(), `unknown object member name "unknown"`) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		home, root := t.TempDir(), t.TempDir()
		if _, err := Open(home); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "interactive.json")
		if err := os.WriteFile(outside, []byte(`{"version":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(home, "interactive.json")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := OpenInteractive(home, root); err == nil {
			t.Fatal("symlinked interactive state was accepted")
		}
	})
}

func TestInteractiveStoreRemembersAndClearsSelectionAPI(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	if _, err := Open(home); err != nil {
		t.Fatal(err)
	}
	store, err := OpenInteractive(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetModelSelection("opencode-go/ox-alpha-free", "high", "Chat_Completions"); err != nil {
		t.Fatal(err)
	}
	settings, err := store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Workspace.ModelAPI != "chat_completions" || settings.LastModel().ModelAPI != "chat_completions" {
		t.Fatalf("stored selection API = %#v", settings)
	}
	// Selecting a route which describes itself must not leave the previous
	// protocol behind for it.
	if err := store.SetModelSelection("deepseek/deepseek-v4-flash", "", ""); err != nil {
		t.Fatal(err)
	}
	settings, err = store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Workspace.ModelAPI != "" || settings.LastModel().ModelAPI != "" {
		t.Fatalf("cleared selection API = %#v", settings)
	}
	if len(settings.ModelHistory) != 2 || settings.ModelHistory[1].ModelAPI != "chat_completions" {
		t.Fatalf("model history = %#v", settings.ModelHistory)
	}
}
