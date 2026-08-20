package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
	workspacetools "github.com/levmv/skot/tools"
)

func TestCommittedProcessToolResultSuppressesDuplicateCompletion(t *testing.T) {
	manager, err := workspacetools.NewProcessManager(t.TempDir(), t.TempDir(), t.TempDir(), workspacetools.ScopeMachine)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	work := processExternalWork{processes: manager}
	ctx := agent.WithToolSessionID(context.Background(), "session-test")
	tools := manager.Tools()
	started, err := tools[0].Run(ctx, `{"command":"sleep 0.05; printf done","background":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(started.Details) != 1 {
		t.Fatalf("started details = %#v", started.Details)
	}
	running, ok := agent.ProcessResultFromDetail(started.Details[0])
	if !ok || running.JobID == "" || running.Status != agent.ProcessRunning {
		t.Fatalf("started process = %#v, %v", running, ok)
	}
	work.ToolResultCommitted(agent.ToolResult{Details: started.Details})
	deadline := time.Now().Add(3 * time.Second)
	for {
		status, ok := manager.Status(running.JobID)
		if ok && status.Status != agent.ProcessRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background process did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if events := manager.PendingCompletionEvents("session-test"); len(events) != 1 {
		t.Fatalf("running tool result suppressed completion: %#v", events)
	}
	read, err := tools[1].Run(ctx, fmt.Sprintf(`{"action":"output","job_id":%q}`, running.JobID))
	if err != nil {
		t.Fatal(err)
	}
	work.ToolResultCommitted(agent.ToolResult{Details: read.Details})
	if events := manager.PendingCompletionEvents("session-test"); len(events) != 0 {
		t.Fatalf("committed terminal tool result left duplicate completion: %#v", events)
	}
}
