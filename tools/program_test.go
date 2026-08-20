package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/canonicalpath"
)

func TestLoadProgramToolsTreatsMissingAsEmptyAndRejectsUnknownFields(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	config, err := LoadProgramTools(missing)
	if err != nil || len(config.Tools) != 0 {
		t.Fatalf("missing config = %#v, %v", config, err)
	}
	path := writeProgramConfig(t, `{"tools":[{"name":"lookup","description":"look up","effect":"read","command":["true"]}]}`)
	if _, err := LoadProgramTools(path); err == nil || !strings.Contains(err.Error(), "unknown field \"effect\"") {
		t.Fatalf("unknown field was accepted: %v", err)
	}
	path = writeProgramConfig(t, `{"tools":[{"name":"lookup","description":"look up","command":["true"],"background":true}]}`)
	if _, err := LoadProgramTools(path); err == nil || !strings.Contains(err.Error(), "cannot unmarshal bool") {
		t.Fatalf("non-string background was accepted: %v", err)
	}
}

func TestLoadProgramToolsNormalizesSchemaModesAndResolvedDefaults(t *testing.T) {
	path := writeProgramConfig(t, `{"tools":[{
	  "name":"lookup","description":" look up ","command":["sh","-c","true"],
	  "parameters":{"properties":{"query":{"type":"string"}}},
	  "parallel_safe":true,"background":"auto","yield":2,"detach":true,
	  "env":{"LOOKUP_TOKEN":"secret"},"timeout":3600
	}]}`)
	config, err := LoadProgramTools(path)
	if err != nil {
		t.Fatal(err)
	}
	declaration := config.Tools[0]
	if declaration.Description != "look up" || declaration.Background != BackgroundAuto || !declaration.ParallelSafe || !declaration.Detach {
		t.Fatalf("declaration = %#v", declaration)
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(declaration.Parameters, &schema); err != nil || string(schema["type"]) != `"object"` {
		t.Fatalf("schema = %s, %v", declaration.Parameters, err)
	}
	manager := processManagerForTest(t)
	resolved, err := manager.ResolveProgramTools(config.Tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].Snapshot.Timeout != time.Hour.String() || resolved[0].Snapshot.Yield != "2s" ||
		resolved[0].Snapshot.Background != "auto" || !resolved[0].Snapshot.Detach || len(resolved[0].Snapshot.EnvironmentNames) != 1 ||
		resolved[0].Snapshot.EnvironmentNames[0] != "LOOKUP_TOKEN" || !resolved[0].CanBackground {
		t.Fatalf("resolved = %#v", resolved)
	}
	var visible struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(resolved[0].Tool.Spec.InputSchema, &visible); err != nil || visible.Properties[programBackgroundArg] == nil {
		t.Fatalf("visible schema = %s, %v", resolved[0].Tool.Spec.InputSchema, err)
	}
}

func TestProgramToolGetsObjectOnStdinKeepsStderrSeparateAndAppliesEnvironmentOverlay(t *testing.T) {
	root := t.TempDir()
	workdir := filepath.Join(root, "nested")
	if err := os.Mkdir(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewProcessManager(root, t.TempDir(), t.TempDir(), ScopeMachine)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	t.Setenv("SK_PROGRAM_AMBIENT", "ambient")
	declaration := normalizedProgramTool(t, ProgramTool{
		Name: "probe", Description: "probe execution", ParallelSafe: true,
		Command: []string{"sh", "-c", `printf 'stdin='; cat; printf '\nambient=%s overlay=%s pwd=%s\n' "$SK_PROGRAM_AMBIENT" "$SK_PROGRAM_OVERLAY" "$PWD"; printf 'diagnostic\n' >&2; exit 4`},
		Workdir: "nested", Env: map[string]string{"SK_PROGRAM_OVERLAY": "configured"},
	})
	resolved, err := manager.ResolveProgramTools([]ProgramTool{declaration})
	if err != nil {
		t.Fatal(err)
	}
	output, err := resolved[0].Tool.Run(context.Background(), `{"query":"book"}`)
	if err != nil {
		t.Fatal(err)
	}
	canonicalWorkdir := filepath.Join(canonicalpath.Resolve(root), "nested")
	for _, want := range []string{`stdin={"query":"book"}`, "ambient=", "overlay=configured", "pwd=" + canonicalWorkdir, "stderr:\ndiagnostic", "status: failed", "exit_code: 4"} {
		if !strings.Contains(output.Content, want) {
			t.Fatalf("output = %q, want %q", output.Content, want)
		}
	}
	if strings.Contains(output.Content, "ambient=ambient") {
		t.Fatalf("program inherited hidden Skot environment: %q", output.Content)
	}
	result := processResultForTest(t, output)
	if result.Status != ProcessFailed || result.ExitCode == nil || *result.ExitCode != 4 || result.JobID != "" || result.OutputBytes == 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestProgramForegroundReturnsMoreThanTheBashPreview(t *testing.T) {
	manager := processManagerForTest(t)
	declaration := normalizedProgramTool(t, ProgramTool{
		Name: "large", Description: "returns a bounded large result",
		Command: []string{"sh", "-c", `head -c 40000 /dev/zero | tr '\0' x`},
	})
	resolved, err := manager.ResolveProgramTools([]ProgramTool{declaration})
	if err != nil {
		t.Fatal(err)
	}
	output, err := resolved[0].Tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Content) < 40000 || strings.Contains(output.Content, "truncated: true") {
		t.Fatalf("configured program result bytes=%d, content was unexpectedly truncated", len(output.Content))
	}
}

func TestProgramBackgroundAutoStripsItsSyntheticArgumentAndUsesJobTool(t *testing.T) {
	manager := processManagerForTest(t)
	declaration := normalizedProgramTool(t, ProgramTool{
		Name: "worker", Description: "background worker", Background: BackgroundAuto,
		Command: []string{"sh", "-c", `cat; sleep 30`},
	})
	resolved, err := manager.ResolveProgramTools([]ProgramTool{declaration})
	if err != nil {
		t.Fatal(err)
	}
	ctx := agent.WithToolSessionID(context.Background(), "session-program")
	output, err := resolved[0].Tool.Run(ctx, `{"query":"<p>&</p>","background":true}`)
	if err != nil {
		t.Fatal(err)
	}
	id := jobIDFromText(t, output.Content)
	if pending, err := manager.AwaitRequiredJobs(context.Background(), "session-program"); err != nil || pending {
		t.Fatalf("explicit background unexpectedly requires joining: pending=%t err=%v", pending, err)
	}
	job := manager.get(id)
	deadline := time.Now().Add(3 * time.Second)
	for {
		content, _ := manager.jobOutput(job, 1024)
		if strings.Contains(string(content), `{"query":"<p>&</p>"}`) {
			if strings.Contains(string(content), "background") {
				t.Fatalf("synthetic argument reached program: %q", content)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("program did not receive input: %q", content)
		}
		time.Sleep(5 * time.Millisecond)
	}
	raw, _ := json.Marshal(jobArgs{Action: "stop", JobID: id})
	if _, err := manager.job(ctx, string(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestProgramYieldPreservesForegroundJoinObligation(t *testing.T) {
	manager := processManagerForTest(t)
	declaration := normalizedProgramTool(t, ProgramTool{
		Name: "patient", Description: "eventually answers", Yield: 1,
		Command: []string{"sh", "-c", `sleep 1.2; printf 'finished\n'`},
	})
	resolved, err := manager.ResolveProgramTools([]ProgramTool{declaration})
	if err != nil {
		t.Fatal(err)
	}
	ctx := agent.WithToolSessionID(context.Background(), "session-yield")
	started := time.Now()
	output, err := resolved[0].Tool.Run(ctx, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("yielded after %s", elapsed)
	}
	id := jobIDFromText(t, output.Content)
	if pending, err := manager.AwaitRequiredJobs(context.Background(), "session-yield"); err != nil || !pending {
		t.Fatalf("yielded foreground was not joined: pending=%t err=%v", pending, err)
	}
	job := manager.get(id)
	if job == nil {
		t.Fatal("yielded job disappeared")
	}
	select {
	case <-job.done:
	case <-time.After(3 * time.Second):
		t.Fatal("yielded program did not finish")
	}
	content, _ := manager.jobOutput(job, 1024)
	if !strings.Contains(string(content), "finished") {
		t.Fatalf("job output = %q", content)
	}
}

func TestDetachedProgramSurvivesManagerCloseAndCanBeReattached(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	gate := filepath.Join(root, "release")
	declaration := normalizedProgramTool(t, ProgramTool{
		Name: "detached_worker", Description: "detached worker",
		Command:    []string{"sh", "-c", `while [ ! -f release ]; do sleep 0.05; done; printf 'survived\n'`},
		Background: BackgroundAlways, Detach: true,
	})
	ctx := agent.WithToolSessionID(context.Background(), "session-detached")

	first := newProcessManagerForTest(t, root, home)
	resolved, err := first.ResolveProgramTools([]ProgramTool{declaration})
	if err != nil {
		t.Fatal(err)
	}
	output, err := resolved[0].Tool.Run(ctx, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	id := jobIDFromText(t, output.Content)
	if !strings.Contains(output.Content, "detached: true") {
		t.Fatalf("detached start output = %q", output.Content)
	}
	if job := first.get(id); job == nil || !job.supervised || !job.detached {
		t.Fatalf("detached job = %#v", job)
	}
	if ids := first.DetachedJobs("session-detached"); len(ids) != 1 || ids[0] != id {
		t.Fatalf("detached jobs = %#v", ids)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	jobDir := jobDirectory(filepath.Join(home, "jobs"), "session-detached", id)
	if info, err := os.Stat(jobControlPath(jobDir)); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("detached worker lost its FIFO: %v, %v", info, err)
	}

	second := newProcessManagerForTest(t, root, home)
	if err := second.AttachSession("session-detached"); err != nil {
		t.Fatal(err)
	}
	if status, ok := second.Status(id); !ok || status.Status != ProcessRunning || !status.Detached {
		t.Fatalf("reattached job = %#v, %t", status, ok)
	}
	if listed := runProcessResultContext(t, ctx, second.job, jobArgs{Action: "list"}); !strings.Contains(listed.Content, id+" running") || !strings.Contains(listed.Content, " detached ") {
		t.Fatalf("detached job list = %q", listed.Content)
	}
	if err := os.WriteFile(gate, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	job := second.get(id)
	select {
	case <-job.done:
	case <-time.After(5 * time.Second):
		t.Fatal("reattached detached job did not finish")
	}
	finished := runProcessResultContext(t, ctx, second.job, jobArgs{Action: "output", JobID: id})
	if meta := processResultForTest(t, finished); meta.Status != ProcessCompleted || !meta.Detached {
		t.Fatalf("detached result = %#v", meta)
	}
	if !strings.Contains(finished.Content, "survived") {
		t.Fatalf("detached output = %q", finished.Content)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResolvedProgramDisappearingIsFatal(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "vanishing")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewProcessManager(root, t.TempDir(), t.TempDir(), ScopeMachine)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	declaration := normalizedProgramTool(t, ProgramTool{Name: "vanishing", Description: "vanishes", Command: []string{"./vanishing"}})
	resolved, err := manager.ResolveProgramTools([]ProgramTool{declaration})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	_, err = resolved[0].Tool.Run(context.Background(), `{}`)
	if !errors.Is(err, agent.ErrToolFatal) {
		t.Fatalf("error = %v", err)
	}
}

func TestSupervisedForegroundProgramThatNeverStartsIsFatal(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "vanishing-supervised")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := newProcessManagerForTest(t, root, t.TempDir())
	declaration := normalizedProgramTool(t, ProgramTool{
		Name: "vanishing_supervised", Description: "vanishes after resolution",
		Command: []string{"./vanishing-supervised"}, Yield: 1, Detach: true,
	})
	resolved, err := manager.ResolveProgramTools([]ProgramTool{declaration})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	_, err = resolved[0].Tool.Run(agent.WithToolSessionID(context.Background(), "session-vanishing"), `{}`)
	if !errors.Is(err, agent.ErrToolFatal) {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "exit status 125") || !strings.Contains(err.Error(), "vanishing-supervised") {
		t.Fatalf("supervisor hid launch diagnostic: %v", err)
	}
}

func TestProgramToolValidationRejectsAmbiguousOrUnsafeDeclarations(t *testing.T) {
	for name, document := range map[string]string{
		"duplicate":       `{"tools":[{"name":"same","description":"d","command":["true"]},{"name":"same","description":"d","command":["true"]}]}`,
		"non-object":      `{"tools":[{"name":"bad","description":"d","command":["true"],"parameters":{"type":"array"}}]}`,
		"auto collision":  `{"tools":[{"name":"bad","description":"d","command":["true"],"background":"auto","parameters":{"type":"object","properties":{"background":{"type":"boolean"}}}}]}`,
		"always yield":    `{"tools":[{"name":"bad","description":"d","command":["true"],"background":"always","yield":1}]}`,
		"late yield":      `{"tools":[{"name":"bad","description":"d","command":["true"],"timeout":1,"yield":1}]}`,
		"orphan detach":   `{"tools":[{"name":"bad","description":"d","command":["true"],"detach":true}]}`,
		"long timeout":    `{"tools":[{"name":"bad","description":"d","command":["true"],"timeout":3601}]}`,
		"managed env":     `{"tools":[{"name":"bad","description":"d","command":["true"],"env":{"HOME":"elsewhere"}}]}`,
		"multiple values": `{"tools":[]} {"tools":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadProgramTools(writeProgramConfig(t, document)); err == nil {
				t.Fatal("invalid declaration was accepted")
			}
		})
	}
}

func normalizedProgramTool(t *testing.T, declaration ProgramTool) ProgramTool {
	t.Helper()
	if err := declaration.normalize(); err != nil {
		t.Fatal(err)
	}
	return declaration
}

func writeProgramConfig(t *testing.T, document string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tools.json")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
