package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuiltInAndProcessPoliciesAgreeOnUserPathReach(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	inside := filepath.Join(workspace, "inside.txt")
	privateDir := filepath.Join(workspace, "private")
	secret := filepath.Join(privateDir, "secret.txt")
	external := filepath.Join(outside, "external.txt")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		inside: "inside\n", secret: "secret\n", external: "external\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	protection, err := NewProtectedPathPolicy(workspace, []string{"private"})
	if err != nil {
		t.Fatal(err)
	}
	access, err := NewFilesystemAccess(workspace, ScopeMachine, protection)
	if err != nil {
		t.Fatal(err)
	}
	fileTools, _, err := NewWorkspaceToolsWithAccess(access)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProcessManagerWithAccess(access, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	assertRead := func(path string, allowed bool) {
		t.Helper()
		_, readErr := runTool(fileTools, "read", jsonArgs(t, map[string]any{"path": path}))
		if allowed && readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		if !allowed && readErr == nil {
			t.Fatalf("read unexpectedly reached %s", path)
		}
	}
	assertProcessRead := func(path, wantStatus string) {
		t.Helper()
		result := runProcessResult(t, manager.bash, bashArgs{Command: "cat " + shellQuoteForProcessTest(path)})
		if metadata := processResultForTest(t, result); metadata.Status != wantStatus {
			t.Fatalf("process read %s = %#v / %q", path, metadata, result.Content)
		}
	}

	assertRead(external, true)
	assertProcessRead(external, ProcessCompleted)
	assertRead(secret, false)
	assertProcessRead(secret, ProcessFailed)

	if err := manager.SetScopeAfter(ScopeWorkspace, nil); err != nil {
		t.Fatal(err)
	}
	assertRead(inside, true)
	assertProcessRead(inside, ProcessCompleted)
	assertRead(external, false)
	assertProcessRead(external, ProcessFailed)
}
