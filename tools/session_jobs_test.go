package tools

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/canonicalpath"
)

func TestRemoveSessionJobsPreflightsAllWorkers(t *testing.T) {
	home := canonicalpath.Resolve(t.TempDir())
	makeJob := func(sessionID string) (string, *os.File) {
		t.Helper()
		id := "job-finished"
		dir := jobDirectory(filepath.Join(home, "jobs"), sessionID, id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		if err := writeJSONAtomic(filepath.Join(dir, jobMetadataFile), jobMetadata{
			Version: jobProtocolVersion, JobID: id, SessionID: sessionID,
			Command: "true", StartedAt: now, Scope: ScopeMachine,
		}, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeJSONAtomic(filepath.Join(dir, jobResultFile), jobTerminalResult{
			Version: jobProtocolVersion, JobID: id, Status: ProcessCompleted,
			Started: true, StartedAt: now, FinishedAt: now,
		}, 0o600); err != nil {
			t.Fatal(err)
		}
		reader, err := createJobControl(jobControlPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reader.Close() })
		return dir, reader
	}
	firstDir, first := makeJob("first")
	secondDir, second := makeJob("second")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	// A result can be persisted before the worker finishes its last I/O.
	// Even though the first session is removable, neither may be removed yet.
	if err := RemoveSessionJobs(t.Context(), home, "first", "second"); !errors.Is(err, ErrSessionJobsActive) {
		t.Fatalf("remove with an owned terminal job: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(jobControlPath(secondDir)); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSessionJobs(t.Context(), home, "first", "second"); err == nil {
		t.Fatal("removed jobs with unobservable worker ownership")
	}
	for _, dir := range []string{firstDir, secondDir} {
		if _, err := os.Stat(filepath.Join(dir, jobMetadataFile)); err != nil {
			t.Fatalf("failed preflight removed state: %v", err)
		}
	}
	reader, err := createJobControl(jobControlPath(secondDir))
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSessionJobs(t.Context(), home, "first", "second"); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{firstDir, secondDir} {
		if _, err := os.Stat(filepath.Dir(dir)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("job state remains: %v", err)
		}
	}
}

func TestRemoveSessionJobsPreservesWorkAfterWorkerCrash(t *testing.T) {
	home := t.TempDir()
	manager := newProcessManagerForTest(t, t.TempDir(), home)
	const sessionID = "session-worker-crash"
	pidFile := filepath.Join(t.TempDir(), "worker-payload-pids")
	started := runProcessResultContext(t, agent.WithToolSessionID(t.Context(), sessionID), manager.bash, bashArgs{
		Command: workerPayloadPIDCommand(pidFile), Background: true,
	})
	job := manager.get(jobIDFromText(t, started.Content.Text()))
	workerPID, payloadPID := waitForWorkerPayloadPIDs(t, pidFile)
	defer func() { _ = killProcessGroupForTest(payloadPID) }()
	if err := killProcessGroupForTest(workerPID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-job.done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker death was not observed")
	}
	if alive, err := processGroupExistsForTest(payloadPID); err != nil || !alive {
		t.Fatalf("payload did not survive the worker: alive=%v, error=%v", alive, err)
	}
	if err := RemoveSessionJobs(t.Context(), home, sessionID); err == nil {
		t.Fatal("removed work whose completion is unknown")
	}
	if _, err := os.Stat(filepath.Join(job.jobDir, jobMetadataFile)); err != nil {
		t.Fatalf("failed deletion removed job state: %v", err)
	}
	if alive, err := processGroupExistsForTest(payloadPID); err != nil || !alive {
		t.Fatalf("deletion stopped the surviving payload: alive=%v, error=%v", alive, err)
	}
}
