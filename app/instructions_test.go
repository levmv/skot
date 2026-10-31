package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workspacetools "github.com/levmv/skot/tools"
)

func TestApplicationRefreshesSystemPromptSources(t *testing.T) {
	root := canonicalApplicationTestRoot(t, t.TempDir())
	t.Chdir(root)
	writeSources := func(version string) {
		t.Helper()
		for name, prefix := range map[string]string{"AGENTS.md": "project ", "SOUL.md": "persona "} {
			if err := os.WriteFile(filepath.Join(root, name), []byte(prefix+version), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeSources("one")
	loads := 0
	application, err := Open(t.Context(), Config{
		Home: t.TempDir(), Root: root, Interactive: true,
		ModelURI: "deepseek/deepseek-v4-flash", ModelExplicit: true, ContextWindow: 256 * 1024,
		ToolSet: ToolSetNone, ToolSetExplicit: true, Scope: ScopeMachine, ScopeExplicit: true,
		SystemPrompt: "base instructions",
		LoadInstructions: func(_ context.Context, base string) (string, error) {
			loads++
			if strings.Contains(base, "persona ") {
				t.Fatal("callback received its previous output as the base")
			}
			data, err := os.ReadFile(filepath.Join(root, "SOUL.md"))
			return base + "\n\n" + string(data), err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	if _, err := application.State(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := application.ListSessions(); err != nil || loads != 0 {
		t.Fatalf("inspecting the application loaded instructions: %d, %v", loads, err)
	}
	backend := &skillCaptureModel{}
	installBackend := func() {
		t.Helper()
		runtime := application.runtimeOrNil()
		if err := runtime.SwitchModel(t.Context(), runtime.CurrentModelInfo(), backend); err != nil {
			t.Fatal(err)
		}
	}
	installBackend()
	run := func(input, version string, wantLoads int) {
		t.Helper()
		if _, err := application.Run(t.Context(), input, nil); err != nil {
			t.Fatal(err)
		}
		prompt := backend.requests[len(backend.requests)-1].Instructions
		if loads != wantLoads || strings.Count(prompt, "base instructions") != 1 || !strings.HasPrefix(prompt, "base instructions\n\n") ||
			!strings.Contains(prompt, "project "+version) || !strings.HasSuffix(prompt, "persona "+version) {
			t.Fatalf("system prompt = %q, loads = %d; want version %s, loads %d", prompt, loads, version, wantLoads)
		}
		state, err := application.State(t.Context())
		if err != nil || state.Configured == nil || state.Configured.ModelContext.Instructions != prompt {
			t.Fatalf("effective prompt was not recorded: %#v, %v", state.Configured, err)
		}
	}
	run(strings.Repeat("old context ", 13_000), "one", 1)
	writeSources("two")
	run("recent question", "one", 1)
	if _, err := application.Compact(t.Context()); err != nil {
		t.Fatal(err)
	}
	if prompt := backend.requests[len(backend.requests)-1].Instructions; !strings.Contains(prompt, "project one") || !strings.HasSuffix(prompt, "persona one") {
		t.Fatalf("compaction changed the previous prompt: %q", prompt)
	}
	run("continue", "two", 2)
	savedID := application.SessionID()
	writeSources("three")
	if _, err := application.ClearSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	installBackend()
	run("new session", "three", 3)
	writeSources("four")
	if _, err := application.ResumeSession(t.Context(), savedID); err != nil {
		t.Fatal(err)
	}
	installBackend()
	run("resume", "four", 4)
}

func TestLoadInstructionsUsesCurrentHierarchy(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "pkg", "feature")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "AGENTS.md"), []byte("nested rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)
	prompts, err := loadInstructions(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 || !strings.Contains(prompts[0], "root rules") || !strings.Contains(prompts[1], "pkg/AGENTS.md") || !strings.Contains(prompts[1], "nested rules") {
		t.Fatalf("prompts = %#v", prompts)
	}
	got := effectiveInstructions("custom", root, prompts)
	if !strings.HasPrefix(got, "custom\n\n") || !strings.Contains(got, "root rules") {
		t.Fatalf("effective instructions = %q", got)
	}
	// A custom prompt without the token stays exactly as its author wrote it.
	if strings.Contains(got, root) {
		t.Fatalf("custom prompt gained an unrequested workspace root: %q", got)
	}
	if substituted := effectiveInstructions("work in {{workspace_root}} only", root, nil); substituted != "work in "+root+" only" {
		t.Fatalf("substituted prompt = %q", substituted)
	}
	if standard := effectiveInstructions("", root, nil); !strings.Contains(standard, "Workspace root (default working directory): "+root+".") ||
		strings.Contains(standard, workspaceRootToken) {
		t.Fatalf("default prompt = %q", standard)
	}
}

func TestExplicitEmptyInstructionsDoNotInspectProjectFiles(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "AGENTS.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	instructions, err := loadEffectiveInstructions(" \n", true, root, nil)
	if err != nil || instructions != "" {
		t.Fatalf("instructions = %q, %v", instructions, err)
	}
}

func TestLoadInstructionsRejectsSymlinkOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "AGENTS.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := loadInstructions(root, nil); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("load error = %v", err)
	}
}

func TestLoadInstructionsAcceptsCanonicalWorkspaceAlias(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	nested := filepath.Join(real, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "AGENTS.md"), []byte("alias rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Chdir(filepath.Join(alias, "nested"))
	prompts, err := loadInstructions(alias, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 1 || !strings.Contains(prompts[0], "alias rules") {
		t.Fatalf("prompts = %#v", prompts)
	}
}

func TestLoadInstructionsSkipsProtectedDirectPathAndAlias(t *testing.T) {
	root := t.TempDir()
	protected := filepath.Join(root, "private")
	if err := os.Mkdir(protected, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(protected, "AGENTS.md"), []byte("private rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(protected, "AGENTS.md"), filepath.Join(root, "AGENTS.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	policy, err := workspacetools.NewProtectedPathPolicy(root, []string{"private"})
	if err != nil {
		t.Fatal(err)
	}
	prompts, err := loadInstructions(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 0 {
		t.Fatalf("protected instructions = %#v", prompts)
	}
}
