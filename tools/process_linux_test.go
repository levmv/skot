//go:build linux

package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSupervisedJobUsesRunningExecutableAfterReplacement(t *testing.T) {
	const helperEnv = "SKOT_TEST_WORKER_EXECUTABLE"
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if copied := os.Getenv(helperEnv); copied != "" {
		if executable != copied {
			t.Fatalf("helper executable = %q, want isolated copy %q", executable, copied)
		}
		// Replace only the isolated copy, leaving the test runner untouched.
		replacement := copied + ".next"
		if err := os.WriteFile(replacement, []byte("replacement file\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, copied); err != nil {
			t.Fatal(err)
		}
		for _, scope := range []Scope{ScopeWorkspace, ScopeMachine} {
			t.Run(string(scope), func(t *testing.T) {
				manager, err := NewProcessManager(t.TempDir(), t.TempDir(), t.TempDir(), scope)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = manager.Close() })
				started := runProcessResult(t, manager.bash, bashArgs{Command: "printf worker-output", Background: true})
				metadata := processResultForTest(t, started)
				result := runProcessResult(t, manager.job, jobArgs{Action: "wait", JobID: metadata.JobID, TimeoutSeconds: 10})
				if finished := processResultForTest(t, result); finished.Status != ProcessCompleted ||
					!strings.Contains(result.Content.Text(), "worker-output") {
					t.Fatalf("supervised result after replacement = %#v / %q", finished, result.Content.Text())
				}
			})
		}
		return
	}

	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(directory, "skot-test")
	if err := os.WriteFile(copied, binary, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, copied, "-test.run=^"+t.Name()+"$", "-test.count=1")
	command.Env = append(os.Environ(), helperEnv+"="+copied)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("isolated executable replacement test: %v\n%s", err, output)
	}
}
