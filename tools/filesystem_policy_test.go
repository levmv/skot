package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuiltInAndProcessPoliciesAgreeOnUserPathReach(t *testing.T) {
	workspace, added, outside := t.TempDir(), t.TempDir(), t.TempDir()
	inside := filepath.Join(workspace, "inside.txt")
	privateDir := filepath.Join(workspace, "private")
	secret := filepath.Join(privateDir, "secret.txt")
	addedFile := filepath.Join(added, "added.txt")
	addedSecret := filepath.Join(added, "secret.txt")
	external := filepath.Join(outside, "external.txt")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		inside: "inside\n", secret: "secret\n", addedFile: "added\n",
		addedSecret: "added secret\n", external: "external\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	additions, err := NewAddedDirectoryPolicy(workspace, []string{added})
	if err != nil {
		t.Fatal(err)
	}
	protection, err := NewProtectedPathPolicy(workspace, []string{"private", addedSecret})
	if err != nil {
		t.Fatal(err)
	}
	access, err := NewFilesystemAccess(workspace, ScopeMachine, additions, protection)
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
	assertRead(addedSecret, false)
	assertProcessRead(addedSecret, ProcessFailed)
	assertRead(secret, false)
	assertProcessRead(secret, ProcessFailed)

	if err := setScopeAfter(manager, ScopeWorkspace, nil); err != nil {
		t.Fatal(err)
	}
	assertRead(inside, true)
	assertProcessRead(inside, ProcessCompleted)
	assertRead(addedFile, true)
	assertProcessRead(addedFile, ProcessCompleted)
	assertRead(addedSecret, false)
	assertProcessRead(addedSecret, ProcessFailed)
	assertRead(external, false)
	assertProcessRead(external, ProcessFailed)
}

// setScopeAfter switches only the scope, keeping the manager's current path
// policies, the way tests which never touch added or protected paths expect.
func setScopeAfter(manager *ProcessManager, scope Scope, beforeApply func() error) error {
	current := manager.access.snapshot()
	return manager.SetFilesystemPolicyAfter(scope, current.additions, current.protection, beforeApply)
}
