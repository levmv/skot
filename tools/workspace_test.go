package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
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
	workspace, err := newWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := workspace.resolvePath("../outside"); err == nil {
		t.Fatal("parent traversal was accepted")
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

func TestWorkspaceToolsHideProtectedPathsAndAliases(t *testing.T) {
	root := t.TempDir()
	protectedDir := filepath.Join(root, "private")
	mustWriteFile(t, filepath.Join(protectedDir, "secret.txt"), "needle secret\n")
	mustWriteFile(t, filepath.Join(root, "public.txt"), "needle public\n")
	if err := os.Symlink("private", filepath.Join(root, "private-alias")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	policy, err := NewProtectedPathPolicy(root, []string{"private"}, true)
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

	policy.SetEnabled(false)
	read := mustRunTool(t, tools, "read", `{"path":"private/secret.txt"}`)
	if !strings.Contains(read, "needle secret") {
		t.Fatalf("off read result = %q", read)
	}
	list = mustRunTool(t, tools, "ls", `{}`)
	if !strings.Contains(list, "private/") || !strings.Contains(list, "private-alias") {
		t.Fatalf("off ls result = %q", list)
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
