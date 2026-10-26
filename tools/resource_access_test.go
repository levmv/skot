package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/canonicalpath"
)

func TestResourceDirectoriesExtendReadsWithoutWritesOrLeakingBetweenCalls(t *testing.T) {
	root := canonicalpath.Resolve(t.TempDir())
	resources := canonicalpath.Resolve(t.TempDir())
	outside := canonicalpath.Resolve(t.TempDir())
	reference := filepath.Join(resources, "reference.md")
	secret := filepath.Join(resources, "private", "secret.md")
	mustWriteFile(t, reference, "resource contents\n")
	mustWriteFile(t, secret, "private contents\n")
	mustWriteFile(t, filepath.Join(outside, "other.md"), "outside contents\n")
	if err := os.Symlink(outside, filepath.Join(resources, "escape")); err != nil {
		t.Fatal(err)
	}
	protection, err := NewProtectedPathPolicy(root, []string{filepath.Dir(secret)})
	if err != nil {
		t.Fatal(err)
	}
	access, err := NewFilesystemAccess(root, ScopeWorkspace, nil, protection)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := newWorkspaceWithAccess(access)
	if err != nil {
		t.Fatal(err)
	}
	ctx := agent.WithToolReadOnlyPaths(t.Context(), []string{resources})
	for _, check := range []struct {
		name string
		run  func(context.Context, string) (agent.ToolOutput, error)
		args map[string]any
		want string
	}{
		{"read", workspace.read, map[string]any{"path": reference}, "resource contents"},
		{"ls", workspace.ls, map[string]any{"path": resources}, "reference.md"},
		{"glob", workspace.glob, map[string]any{"path": resources, "pattern": "*.md"}, "reference.md"},
		{"grep", workspace.grep, map[string]any{"path": resources, "pattern": "resource"}, "reference.md"},
	} {
		raw := jsonArgs(t, check.args)
		output, err := check.run(ctx, raw)
		if err != nil || !strings.Contains(output.Content.Text(), check.want) {
			t.Fatalf("%s with resources: %q, %v", check.name, output.Content.Text(), err)
		}
		if _, err := check.run(t.Context(), raw); err == nil {
			t.Fatalf("%s grant leaked into another call", check.name)
		}
	}
	for _, path := range []string{secret, filepath.Join(resources, "escape", "other.md")} {
		if _, err := workspace.read(ctx, jsonArgs(t, map[string]any{"path": path})); err == nil {
			t.Fatalf("resource grant bypassed protection or allowed an escape: %s", path)
		}
	}
	if _, err := workspace.edit(ctx, jsonArgs(t, map[string]any{"path": reference, "old_text": "resource", "new_text": "changed"})); err == nil {
		t.Fatal("edit modified a read-only resource")
	}
	created := filepath.Join(resources, "new", "file.md")
	if _, err := workspace.write(ctx, jsonArgs(t, map[string]any{"path": created, "content": "changed"})); err == nil {
		t.Fatal("write created a read-only resource")
	}
	if _, err := os.Stat(filepath.Dir(created)); !os.IsNotExist(err) {
		t.Fatalf("write created a parent before checking read-only access: %v", err)
	}
	// A replaced grant root cannot redirect later calls to another directory.
	if err := os.Rename(resources, resources+"-old"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(resources + "-old") })
	if err := os.Symlink(outside, resources); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.read(ctx, jsonArgs(t, map[string]any{"path": filepath.Join(resources, "other.md")})); err == nil {
		t.Fatal("resource root retargeting widened access")
	}
}

func TestResourceDirectoriesApplyToBashAndProgramTools(t *testing.T) {
	root := canonicalpath.Resolve(t.TempDir())
	resources := canonicalpath.Resolve(t.TempDir())
	outside := canonicalpath.Resolve(t.TempDir())
	reference := filepath.Join(resources, "reference.md")
	secret := filepath.Join(resources, "private", "secret.md")
	mustWriteFile(t, reference, "resource contents\n")
	mustWriteFile(t, secret, "private contents\n")
	mustWriteFile(t, filepath.Join(outside, "other.md"), "outside contents\n")
	if err := os.Symlink(outside, filepath.Join(resources, "escape")); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(resources, "check.sh")
	quote := shellQuoteForProcessTest
	command := "#!/bin/sh\ncat " + quote(reference) + " || exit 10\n" +
		"if cat " + quote(secret) + "; then exit 11; fi\n" +
		"if cat " + quote(filepath.Join(resources, "escape", "other.md")) + "; then exit 12; fi\n" +
		"if printf changed > " + quote(reference) + "; then exit 13; fi\n" +
		"if touch " + quote(filepath.Join(resources, "new")) + "; then exit 14; fi\n" +
		"printf 'resource checks passed\\n'"
	mustWriteFile(t, script, command)
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	protection, err := NewProtectedPathPolicy(root, []string{filepath.Dir(secret)})
	if err != nil {
		t.Fatal(err)
	}
	access, err := NewFilesystemAccess(root, ScopeWorkspace, nil, protection)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProcessManagerWithAccess(access, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	ctx := agent.WithToolReadOnlyPaths(t.Context(), []string{resources})
	programs, err := manager.ResolveProgramTools([]ProgramTool{normalizedProgramTool(t, ProgramTool{
		Name: "check_resources", Description: "Read resources", Command: []string{script},
	})})
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		name string
		run  func(context.Context, string) (agent.ToolOutput, error)
		args string
	}{
		{"bash", manager.bash, jsonArgs(t, map[string]any{"command": quote(script)})},
		{"program", programs[0].Tool.Run, `{}`},
	} {
		output, err := check.run(ctx, check.args)
		result := processResultForTest(t, output)
		if err != nil || result.ExitCode == nil || *result.ExitCode != 0 || !strings.Contains(output.Content.Text(), "resource checks passed") {
			t.Fatalf("%s with resources: %q, %v", check.name, output.Content.Text(), err)
		}
		output, _ = check.run(t.Context(), check.args)
		result = processResultForTest(t, output)
		if result.ExitCode != nil && *result.ExitCode == 0 {
			t.Fatalf("%s resource grant leaked into another call", check.name)
		}
	}
}
