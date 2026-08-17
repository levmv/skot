package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/canonicalpath"
)

func TestWorkspaceToolsExposeStandaloneCatalog(t *testing.T) {
	tools, root, err := NewWorkspaceTools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(root) {
		t.Fatalf("root = %q", root)
	}
	var names []string
	for index, tool := range tools {
		names = append(names, tool.Spec.Name)
		if len(tool.Spec.InputSchema) == 0 || tool.Run == nil {
			t.Fatalf("invalid tool %#v", tool.Spec)
		}
		if (index < 4) != tool.Spec.ParallelSafe {
			t.Fatalf("tool policy = %#v", tool.Spec)
		}
	}
	if got, want := strings.Join(names, ","), "read,ls,grep,glob,edit,write"; got != want {
		t.Fatalf("tools = %q, want %q", got, want)
	}
	if _, err := agent.New(agent.Config{Model: inertModel{}, Journal: inertJournal{}, Tools: tools}); err != nil {
		t.Fatalf("agent rejected tool catalog: %v", err)
	}
}

func TestSharedFilesystemConstructorsRejectUninitializedAccess(t *testing.T) {
	access := &FilesystemAccess{}
	if _, _, err := NewWorkspaceToolsWithAccess(access); err == nil || !strings.Contains(err.Error(), "uninitialized") {
		t.Fatalf("workspace tools error = %v", err)
	}
	if _, err := NewProcessManagerWithAccess(access, t.TempDir(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "uninitialized") {
		t.Fatalf("process manager error = %v", err)
	}
}

func TestReadAndLSReturnBoundedStructuredText(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "docs", "note.txt"), "one\ntwo\nthree\n")
	mustWriteFile(t, filepath.Join(root, "two words.txt"), "spaced\n")
	if err := os.Symlink("docs", filepath.Join(root, "current")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	tools, _, err := NewWorkspaceTools(root)
	if err != nil {
		t.Fatal(err)
	}
	read := mustRunTool(t, tools, "read", `{"path":"docs/note.txt","offset":2,"limit":1}`)
	if !strings.Contains(read, "     2\ttwo") || !strings.Contains(read, "offset=3") ||
		!regexp.MustCompile(`(?m)^sha256: [0-9a-f]{64}$`).MatchString(read) {
		t.Fatalf("read result = %q", read)
	}
	list := mustRunTool(t, tools, "ls", `{}`)
	if !strings.Contains(list, "dir\tdocs/") || !strings.Contains(list, "symlink\tcurrent -> docs") ||
		!strings.Contains(list, "file\t\"two words.txt\"") {
		t.Fatalf("ls result = %q", list)
	}
}

func TestSearchToolsHonorIgnoresAndNeedNoExternalBinary(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".git", "secret.go"), "needle\n")
	mustWriteFile(t, filepath.Join(root, ".config", "settings.go"), "needle\n")
	mustWriteFile(t, filepath.Join(root, "ignored", "inside.go"), "needle\n")
	mustWriteFile(t, filepath.Join(root, ".gitignore"), "ignored/\n")
	mustWriteFile(t, filepath.Join(root, "pkg", "visible.go"), "needle\n")
	tools, _, err := NewWorkspaceTools(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	grep := mustRunTool(t, tools, "grep", `{"pattern":"needle","include":"*.go"}`)
	if !strings.Contains(grep, "pkg/visible.go") || !strings.Contains(grep, ".config/settings.go") ||
		strings.Contains(grep, ".git/") || strings.Contains(grep, "ignored/") {
		t.Fatalf("grep result = %q", grep)
	}
	glob := mustRunTool(t, tools, "glob", `{"pattern":"**/*.go"}`)
	if !strings.Contains(glob, "pkg/visible.go") || !strings.Contains(glob, ".config/settings.go") ||
		!strings.Contains(glob, "ignored/inside.go") || strings.Contains(glob, ".git/") {
		t.Fatalf("glob result = %q", glob)
	}
}

func TestWorkspaceToolsConfineReadsAndWrites(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, "secret.txt"), "secret\n")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	access, err := NewFilesystemAccess(root, ScopeWorkspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := newWorkspaceWithAccess(access)
	if err != nil {
		t.Fatal(err)
	}
	relativeOutside, err := filepath.Rel(root, filepath.Join(outside, "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.read(context.Background(), jsonArgs(t, map[string]any{"path": relativeOutside})); err == nil {
		t.Fatal("read accepted parent traversal outside the workspace")
	}
	if _, err := workspace.read(context.Background(), `{"path":"escape/secret.txt"}`); err == nil {
		t.Fatal("read through escaping symlink was accepted")
	}
	if _, err := workspace.write(context.Background(), `{"path":"escape/new/file.txt","content":"bad"}`); err == nil {
		t.Fatal("write through escaping symlink was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside directory was created: %v", err)
	}
}

func TestMachineScopeFileToolsReachExplicitExternalPaths(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	inside := filepath.Join(root, "inside.txt")
	external := filepath.Join(outside, "external.txt")
	mustWriteFile(t, inside, "workspace only\n")
	mustWriteFile(t, external, "external needle\n")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	access, err := NewFilesystemAccess(root, ScopeMachine, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, _, err := NewWorkspaceToolsWithAccess(access)
	if err != nil {
		t.Fatal(err)
	}
	canonicalExternal := filepath.ToSlash(canonicalpath.Resolve(external))

	read := mustRunTool(t, tools, "read", jsonArgs(t, map[string]any{"path": external}))
	if !strings.Contains(read, "external needle") {
		t.Fatalf("external read = %q", read)
	}
	symlinkRead := mustRunTool(t, tools, "read", `{"path":"escape/external.txt"}`)
	if !strings.Contains(symlinkRead, "external needle") {
		t.Fatalf("external symlink read = %q", symlinkRead)
	}
	relativeExternal, err := filepath.Rel(root, external)
	if err != nil {
		t.Fatal(err)
	}
	if read := mustRunTool(t, tools, "read", jsonArgs(t, map[string]any{"path": relativeExternal})); !strings.Contains(read, "external needle") {
		t.Fatalf("relative escape read = %q", read)
	}
	list := mustRunTool(t, tools, "ls", jsonArgs(t, map[string]any{"path": outside}))
	if !strings.Contains(list, "external.txt") {
		t.Fatalf("external ls = %q", list)
	}
	grep := mustRunTool(t, tools, "grep", jsonArgs(t, map[string]any{"pattern": "needle", "path": outside}))
	if !strings.Contains(grep, canonicalExternal+":1:external needle") {
		t.Fatalf("external grep = %q", grep)
	}
	grepFile := mustRunTool(t, tools, "grep", jsonArgs(t, map[string]any{"pattern": "needle", "path": external}))
	if !strings.Contains(grepFile, canonicalExternal+":1:external needle") {
		t.Fatalf("external-file grep = %q", grepFile)
	}
	glob := mustRunTool(t, tools, "glob", jsonArgs(t, map[string]any{"pattern": "*.txt", "path": outside}))
	if !strings.Contains(glob, canonicalExternal) {
		t.Fatalf("external glob = %q", glob)
	}
	if omitted := mustRunTool(t, tools, "grep", `{"pattern":"external needle"}`); omitted != "no matches\n" {
		t.Fatalf("omitted grep left workspace = %q", omitted)
	}

	edit, err := runTool(tools, "edit", jsonArgs(t, map[string]any{
		"path": external, "old_text": "external", "new_text": "changed",
	}))
	if err != nil {
		t.Fatal(err)
	}
	change, ok := FileChangeMetaFromDetail(edit.Details[0])
	if !ok || change.Path != canonicalExternal {
		t.Fatalf("external edit detail = %#v, ok=%v", change, ok)
	}
	created := filepath.Join(outside, "nested", "created.txt")
	write, err := runTool(tools, "write", jsonArgs(t, map[string]any{"path": created, "content": "created\n"}))
	if err != nil {
		t.Fatal(err)
	}
	change, ok = FileChangeMetaFromDetail(write.Details[0])
	if !ok || change.Path != filepath.ToSlash(canonicalpath.Resolve(created)) {
		t.Fatalf("external write detail = %#v, ok=%v", change, ok)
	}
	throughAlias := filepath.Join(outside, "through-alias.txt")
	write, err = runTool(tools, "write", `{"path":"escape/through-alias.txt","content":"alias\n"}`)
	if err != nil {
		t.Fatal(err)
	}
	change, ok = FileChangeMetaFromDetail(write.Details[0])
	if !ok || change.Path != filepath.ToSlash(canonicalpath.Resolve(throughAlias)) {
		t.Fatalf("external alias write detail = %#v, ok=%v", change, ok)
	}
}

func TestMachineScopeSearchesThroughExternalAliasIntoWorkspace(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	inside := filepath.Join(root, "inside.txt")
	mustWriteFile(t, inside, "workspace backlink\n")
	alias := filepath.Join(outside, "workspace-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	access, err := NewFilesystemAccess(root, ScopeMachine, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, _, err := NewWorkspaceToolsWithAccess(access)
	if err != nil {
		t.Fatal(err)
	}
	grep := mustRunTool(t, tools, "grep", jsonArgs(t, map[string]any{
		"pattern": "backlink", "path": alias,
	}))
	want := filepath.ToSlash(canonicalpath.Resolve(inside)) + ":1:workspace backlink"
	if !strings.Contains(grep, want) {
		t.Fatalf("external-alias grep = %q; want %q", grep, want)
	}
}

func TestWorkspaceScopeAcceptsAbsoluteInsideAndRejectsExternalPaths(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	inside := filepath.Join(root, "inside.txt")
	external := filepath.Join(outside, "external.txt")
	mustWriteFile(t, inside, "inside\n")
	mustWriteFile(t, external, "outside\n")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	workspaceAlias := filepath.Join(outside, "workspace-alias")
	if err := os.Symlink(root, workspaceAlias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	access, err := NewFilesystemAccess(root, ScopeWorkspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, _, err := NewWorkspaceToolsWithAccess(access)
	if err != nil {
		t.Fatal(err)
	}
	if read := mustRunTool(t, tools, "read", jsonArgs(t, map[string]any{"path": inside})); !strings.Contains(read, "inside") {
		t.Fatalf("absolute in-workspace read = %q", read)
	}
	if read := mustRunTool(t, tools, "read", jsonArgs(t, map[string]any{"path": filepath.Join(workspaceAlias, "inside.txt")})); !strings.Contains(read, "inside") {
		t.Fatalf("absolute workspace-alias read = %q", read)
	}
	createdInside := filepath.Join(canonicalpath.Resolve(root), "created.txt")
	write, err := runTool(tools, "write", jsonArgs(t, map[string]any{"path": createdInside, "content": "inside\n"}))
	if err != nil {
		t.Fatal(err)
	}
	change, ok := FileChangeMetaFromDetail(write.Details[0])
	if !ok || change.Path != "created.txt" {
		t.Fatalf("absolute in-workspace write detail = %#v, ok=%v", change, ok)
	}

	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{name: "read", args: map[string]any{"path": external}},
		{name: "read", args: map[string]any{"path": "escape/external.txt"}},
		{name: "ls", args: map[string]any{"path": outside}},
		{name: "grep", args: map[string]any{"pattern": "outside", "path": outside}},
		{name: "glob", args: map[string]any{"pattern": "*.txt", "path": outside}},
		{name: "edit", args: map[string]any{"path": external, "old_text": "outside", "new_text": "changed"}},
		{name: "write", args: map[string]any{"path": filepath.Join(outside, "new.txt"), "content": "bad"}},
	} {
		if _, err := runTool(tools, test.name, jsonArgs(t, test.args)); err == nil || !strings.Contains(err.Error(), "scope") {
			t.Fatalf("%s external error = %v", test.name, err)
		}
	}
}

func TestSharedFilesystemAccessSwitchesFileTools(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	external := filepath.Join(outside, "external.txt")
	mustWriteFile(t, external, "external\n")
	access, err := NewFilesystemAccess(root, ScopeMachine, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, _, err := NewWorkspaceToolsWithAccess(access)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProcessManagerWithAccess(access, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if _, err := runTool(tools, "read", jsonArgs(t, map[string]any{"path": external})); err != nil {
		t.Fatalf("machine read: %v", err)
	}
	if err := manager.SetScopeAfter(ScopeWorkspace, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(tools, "read", jsonArgs(t, map[string]any{"path": external})); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("file tools did not observe switched scope: %v", err)
	}
}

func TestMachineScopeStillFiltersExternalProtectedPaths(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	secretDir := filepath.Join(outside, "private")
	secret := filepath.Join(secretDir, "secret.txt")
	public := filepath.Join(outside, "public.txt")
	mustWriteFile(t, secret, "needle secret\n")
	mustWriteFile(t, public, "needle public\n")
	protection, err := NewProtectedPathPolicy(root, []string{secretDir})
	if err != nil {
		t.Fatal(err)
	}
	access, err := NewFilesystemAccess(root, ScopeMachine, protection)
	if err != nil {
		t.Fatal(err)
	}
	tools, _, err := NewWorkspaceToolsWithAccess(access)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(tools, "read", jsonArgs(t, map[string]any{"path": secret})); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("protected external read error = %v", err)
	}
	if _, err := runTool(tools, "write", jsonArgs(t, map[string]any{
		"path": filepath.Join(secretDir, "missing", "new.txt"), "content": "secret",
	})); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("protected external write error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(secretDir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("protected write created parent: %v", err)
	}
	list := mustRunTool(t, tools, "ls", jsonArgs(t, map[string]any{"path": outside}))
	if !strings.Contains(list, "public.txt") || strings.Contains(list, "private") {
		t.Fatalf("external protected ls = %q", list)
	}
	canonicalPublic := filepath.ToSlash(canonicalpath.Resolve(public))
	grep := mustRunTool(t, tools, "grep", jsonArgs(t, map[string]any{"pattern": "needle", "path": outside}))
	if !strings.Contains(grep, canonicalPublic) || strings.Contains(grep, "secret.txt") {
		t.Fatalf("external protected grep = %q", grep)
	}
	glob := mustRunTool(t, tools, "glob", jsonArgs(t, map[string]any{"pattern": "**/*.txt", "path": outside}))
	if !strings.Contains(glob, canonicalPublic) || strings.Contains(glob, "secret.txt") {
		t.Fatalf("external protected glob = %q", glob)
	}
}

func TestWorkspaceToolsHideProtectedPathsAndAliases(t *testing.T) {
	root := t.TempDir()
	protectedDir := filepath.Join(root, "private")
	mustWriteFile(t, filepath.Join(protectedDir, "secret.txt"), "needle secret\n")
	mustWriteFile(t, filepath.Join(root, "public.txt"), "needle public\n")
	if err := os.Symlink("private", filepath.Join(root, "private-alias")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	policy, err := NewProtectedPathPolicy(root, []string{"private"})
	if err != nil {
		t.Fatal(err)
	}
	tools, _, err := NewWorkspaceToolsWithProtection(root, policy)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		args string
	}{
		{name: "read", args: `{"path":"private/secret.txt"}`},
		{name: "read", args: `{"path":"private-alias/secret.txt"}`},
		{name: "edit", args: `{"path":"private/secret.txt","old_text":"secret","new_text":"changed"}`},
		{name: "write", args: `{"path":"private/new/note.txt","content":"changed"}`},
	} {
		if _, err := runTool(tools, test.name, test.args); err == nil || !strings.Contains(err.Error(), "protected") {
			t.Fatalf("%s(%s) error = %v", test.name, test.args, err)
		}
	}
	if _, err := os.Stat(filepath.Join(protectedDir, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("write created a protected parent: %v", err)
	}
	list := mustRunTool(t, tools, "ls", `{}`)
	if !strings.Contains(list, "public.txt") || strings.Contains(list, "private") {
		t.Fatalf("ls result = %q", list)
	}
	grep := mustRunTool(t, tools, "grep", `{"pattern":"needle"}`)
	if !strings.Contains(grep, "public.txt") || strings.Contains(grep, "secret.txt") {
		t.Fatalf("grep result = %q", grep)
	}
	glob := mustRunTool(t, tools, "glob", `{"pattern":"**/*.txt"}`)
	if !strings.Contains(glob, "public.txt") || strings.Contains(glob, "secret.txt") {
		t.Fatalf("glob result = %q", glob)
	}

}

func TestEditAndWriteUseHashesAndAtomicReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "note.txt")
	mustWriteFile(t, path, "alpha\nbeta\n")
	tools, _, err := NewWorkspaceTools(root)
	if err != nil {
		t.Fatal(err)
	}
	read := mustRunTool(t, tools, "read", `{"path":"note.txt"}`)
	digest := regexp.MustCompile(`(?m)^sha256: ([0-9a-f]{64})$`).FindStringSubmatch(read)
	if len(digest) != 2 {
		t.Fatalf("read digest missing: %q", read)
	}
	editOutput, err := runTool(tools, "edit", `{"path":"note.txt","old_text":"beta","new_text":"gamma"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^updated\nsha256: [0-9a-f]{64}$`).MatchString(editOutput.Content) {
		t.Fatalf("edit result = %q", editOutput.Content)
	}
	if len(editOutput.Details) != 1 {
		t.Fatalf("edit details = %#v", editOutput.Details)
	}
	editChange, ok := FileChangeMetaFromDetail(editOutput.Details[0])
	if !ok || editChange.Path != "note.txt" || editChange.Operation != "edited" || editChange.Additions != 1 || editChange.Deletions != 1 {
		t.Fatalf("edit change = %#v, ok=%v", editChange, ok)
	}
	if _, err := runTool(tools, "write", `{"path":"note.txt","content":"stale","expected_sha256":"`+digest[1]+`"}`); err == nil {
		t.Fatal("write accepted a stale hash")
	}
	writeOutput, err := runTool(tools, "write", `{"path":"new/nested.txt","content":"hello\n"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(writeOutput.Content, "created\nsha256: ") {
		t.Fatalf("write result = %q", writeOutput.Content)
	}
	if len(writeOutput.Details) != 1 {
		t.Fatalf("write details = %#v", writeOutput.Details)
	}
	writeChange, ok := FileChangeMetaFromDetail(writeOutput.Details[0])
	if !ok || writeChange.Path != "new/nested.txt" || writeChange.Operation != "created" || writeChange.Additions != 1 || writeChange.Deletions != 0 {
		t.Fatalf("write change = %#v, ok=%v", writeChange, ok)
	}
	data, err := os.ReadFile(filepath.Join(root, "new", "nested.txt"))
	if err != nil || string(data) != "hello\n" {
		t.Fatalf("written file = %q, error = %v", data, err)
	}
}

func TestToolArgumentsRejectUnknownFields(t *testing.T) {
	tools, _, err := NewWorkspaceTools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(tools, "ls", `{"surprise":true}`); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v", err)
	}
}

func runTool(tools []agent.Tool, name, arguments string) (agent.ToolOutput, error) {
	for _, tool := range tools {
		if tool.Spec.Name == name {
			return tool.Run(context.Background(), arguments)
		}
	}
	return agent.ToolOutput{}, errors.New("tool not found")
}

func mustRunTool(t *testing.T, tools []agent.Tool, name, arguments string) string {
	t.Helper()
	result, err := runTool(tools, name, arguments)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return result.Content
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func jsonArgs(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

type inertModel struct{}

func (inertModel) Info() agent.ModelInfo {
	return agent.ModelInfo{Backend: "test", Provider: "test", Model: "test"}
}

func (inertModel) Complete(context.Context, agent.ModelRequest, func(agent.ModelStreamEvent)) (agent.ModelResponse, error) {
	return agent.ModelResponse{}, errors.New("unused")
}

type inertJournal struct{}

func (inertJournal) Append(context.Context, agent.PendingRecord) (agent.Record, error) {
	return agent.Record{}, errors.New("unused")
}

func (inertJournal) Records(context.Context) ([]agent.Record, error) { return nil, nil }

func (inertModel) ProjectModelItems(items []agent.Item) []agent.Item { return items }
