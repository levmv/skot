package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
	"golang.org/x/sys/unix"
)

func TestSessionJobsSnapshotsStartTimeWhileSorting(t *testing.T) {
	now := time.Now().UTC()
	first := &processJob{id: "first", sessionID: "session", status: ProcessRunning, startedAt: now, done: make(chan struct{})}
	second := &processJob{id: "second", sessionID: "session", status: ProcessRunning, startedAt: now.Add(time.Second), done: make(chan struct{})}
	manager := &ProcessManager{jobs: map[string]*processJob{first.id: first, second.id: second}}

	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		for index := 0; index < 1_000; index++ {
			first.mu.Lock()
			first.startedAt = now.Add(time.Duration(index%2) * 2 * time.Second)
			first.mu.Unlock()
		}
	}()
	for index := 0; index < 1_000; index++ {
		jobs := manager.sessionJobs("session")
		if len(jobs) != 2 {
			t.Fatalf("sessionJobs() returned %d jobs", len(jobs))
		}
	}
	wait.Wait()
}

func TestBashReportsExitAndUsesIsolatedEnvironment(t *testing.T) {
	manager := processManagerForTest(t)
	if err := manager.SetSandboxAfter(SandboxWorkspace, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("SK_TEST_SECRET", "must-not-leak")
	result := runProcessResult(t, manager.bash, bashArgs{Command: "printf 'hello\\n'; printf 'stderr\\n' >&2; env; exit 3"})
	if !strings.Contains(result.Content, "status: failed") || !strings.Contains(result.Content, "exit_code: 3") || !strings.Contains(result.Content, "hello") || !strings.Contains(result.Content, "stderr") {
		t.Fatalf("bash result = %q", result.Content)
	}
	if strings.Contains(result.Content, "SK_TEST_SECRET") || strings.Contains(result.Content, "must-not-leak") {
		t.Fatalf("bash leaked parent environment: %q", result.Content)
	}
	if !strings.Contains(result.Content, "HOME="+manager.toolHome) {
		t.Fatalf("bash HOME is not isolated: %q", result.Content)
	}
	if !strings.Contains(result.Content, "TMPDIR="+WorkspaceToolTemp(manager.toolHome)) {
		t.Fatalf("bash TMPDIR is not isolated: %q", result.Content)
	}
}

func TestProcessManagerCreatesPrivateToolTemp(t *testing.T) {
	manager := processManagerForTest(t)
	if _, err := os.Stat(manager.toolHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tool home was not lazy: %v", err)
	}
	if err := manager.SetSandboxAfter(SandboxWorkspace, nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(WorkspaceToolTemp(manager.toolHome))
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("tool temp mode = %v", info.Mode())
	}
}

func TestUserShellDoesNotPrepareModelToolHome(t *testing.T) {
	manager := processManagerForTest(t)
	if err := manager.SetSandboxAfter(SandboxWorkspace, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(manager.toolHome); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RunShell(context.Background(), "true"); err != nil {
		t.Fatalf("ambient user shell depends on model tool home: %v", err)
	}
	if _, err := os.Stat(manager.toolHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("user shell recreated model tool home: %v", err)
	}
}

func TestBashSandboxOffHidesSupervisorEnvironmentFromModel(t *testing.T) {
	manager := processManagerForTest(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ORDINARY_AMBIENT", "visible-to-model")
	t.Setenv("SK_TEST_AMBIENT", "hidden-setting")
	t.Setenv("TEST_PROVIDER_KEY", "hidden-secret")
	manager.HideModelEnvironment("TEST_PROVIDER_KEY")
	result := runProcessResult(t, manager.bash, bashArgs{Command: `printf 'HOME=%s\nORDINARY=%s\nSK=%s\nKEY=%s\n' "$HOME" "$ORDINARY_AMBIENT" "$SK_TEST_AMBIENT" "$TEST_PROVIDER_KEY"`})
	if !strings.Contains(result.Content, "HOME="+home) || !strings.Contains(result.Content, "ORDINARY=visible-to-model") {
		t.Fatalf("bash lost ordinary ambient environment: %q", result.Content)
	}
	if strings.Contains(result.Content, "hidden-setting") || strings.Contains(result.Content, "hidden-secret") {
		t.Fatalf("bash leaked supervisor environment: %q", result.Content)
	}
}

func TestProcessManagerRunShellInheritsAmbientEnvironment(t *testing.T) {
	manager := processManagerForTest(t)
	if err := manager.SetSandboxAfter(SandboxWorkspace, nil); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("ambient-shell"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SK_USER_SHELL_OUTSIDE", outside)
	result, err := manager.RunShell(context.Background(), `cat "$SK_USER_SHELL_OUTSIDE"`)
	if err != nil {
		t.Fatal(err)
	}
	meta := processResultForTest(t, result)
	if meta.Status != ProcessCompleted || meta.ExitCode == nil || *meta.ExitCode != 0 || meta.JobID != "" || !meta.UserInitiated {
		t.Fatalf("process result = %#v", meta)
	}
	if !strings.Contains(result.Content, "ambient-shell") {
		t.Fatalf("shell result = %q", result.Content)
	}
}

func TestProcessManagerRunShellCancellationCleansUpProcess(t *testing.T) {
	manager := processManagerForTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := manager.RunShell(ctx, "sleep 30")
		done <- err
	}()
	waitForJobCount(t, manager, 1)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunShell error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled user shell did not stop")
	}
	waitForJobCount(t, manager, 0)
}

func TestBashReturnsStructuredFailureStatus(t *testing.T) {
	manager := processManagerForTest(t)
	result := runProcessResult(t, manager.bash, bashArgs{Command: "printf 'first\\nlast\\n'; exit 7"})
	meta := processResultForTest(t, result)
	if meta.Status != ProcessFailed || meta.ExitCode == nil || *meta.ExitCode != 7 || meta.JobID != "" || meta.UserInitiated {
		t.Fatalf("process result = %#v", meta)
	}
	if strings.Contains(result.Content, "job_id:") || len(manager.jobs) != 0 {
		t.Fatalf("foreground command was retained: %q / %#v", result.Content, manager.jobs)
	}
	if meta.OutputBytes == 0 || !strings.Contains(meta.FailureTail, "last") {
		t.Fatalf("process output metadata = %#v", meta)
	}
}

func TestBashTimeoutKillsProcessGroup(t *testing.T) {
	manager := processManagerForTest(t)
	started := time.Now()
	result := runProcessResult(t, manager.bash, bashArgs{Command: "sleep 30 & wait", TimeoutSeconds: 1})
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	if meta := processResultForTest(t, result); meta.Status != ProcessTimedOut || meta.ManagedProcesses < 2 {
		t.Fatalf("timeout result = %#v / %q", meta, result.Content)
	}
	if !strings.Contains(result.Content, "managed_processes:") {
		t.Fatalf("timeout report = %q", result.Content)
	}
}

func TestBackgroundJobCanBeReadAndStopped(t *testing.T) {
	manager := processManagerForTest(t)
	for attempt := 0; attempt < 20; attempt++ {
		started := runProcessResult(t, manager.bash, bashArgs{Command: "sleep 30 & child=$!; printf ready; wait \"$child\"", Background: true})
		id := jobIDFromText(t, started.Content)
		job := manager.get(id)
		deadline := time.Now().Add(3 * time.Second)
		for {
			content, _ := manager.jobOutput(job, 32)
			if strings.Contains(string(content), "ready") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("background job did not produce initial output")
			}
			time.Sleep(5 * time.Millisecond)
		}
		output := runProcessResult(t, manager.job, jobArgs{Action: "output", JobID: id})
		if !strings.Contains(output.Content, "status: running") || !strings.Contains(output.Content, "ready") {
			t.Fatalf("job output = %q", output.Content)
		}
		stopped := runProcessResult(t, manager.job, jobArgs{Action: "stop", JobID: id})
		if meta := processResultForTest(t, stopped); meta.Status != ProcessKilled || meta.ManagedProcesses < 2 {
			t.Fatalf("stopped result = %#v / %q", meta, stopped.Content)
		}
	}
}

func TestJobListReportsManagedJobsInStartOrder(t *testing.T) {
	manager := processManagerForTest(t)
	empty := runProcessResult(t, manager.job, jobArgs{Action: "list"})
	if empty.Content != "no managed jobs\n" || len(empty.Details) != 0 {
		t.Fatalf("empty list = %#v", empty)
	}

	running := runProcessResult(t, manager.bash, bashArgs{Command: "sleep 30 # long-running marker", Background: true})
	runningID := jobIDFromText(t, running.Content)
	finished := runProcessResult(t, manager.bash, bashArgs{Command: "printf done # finished marker", Background: true})
	finishedID := jobIDFromText(t, finished.Content)
	select {
	case <-manager.get(finishedID).done:
	case <-time.After(3 * time.Second):
		t.Fatal("short background job did not finish")
	}

	listed := runProcessResult(t, manager.job, jobArgs{Action: "list"})
	if !strings.Contains(listed.Content, runningID+" running") || !strings.Contains(listed.Content, "long-running marker") {
		t.Fatalf("running job absent from list: %q", listed.Content)
	}
	if !strings.Contains(listed.Content, finishedID+" completed") || !strings.Contains(listed.Content, "exit_code=0") || !strings.Contains(listed.Content, "finished marker") {
		t.Fatalf("finished job absent from list: %q", listed.Content)
	}
	if strings.Index(listed.Content, runningID) > strings.Index(listed.Content, finishedID) {
		t.Fatalf("jobs are not in start order: %q", listed.Content)
	}
}

func TestJobWaitReturnsCompletionOrCurrentStateAtTimeout(t *testing.T) {
	manager := processManagerForTest(t)
	short := runProcessResult(t, manager.bash, bashArgs{Command: "sleep 0.05; printf finally", Background: true})
	shortID := jobIDFromText(t, short.Content)
	completed := runProcessResult(t, manager.job, jobArgs{Action: "wait", JobID: shortID, TimeoutSeconds: 2})
	if meta := processResultForTest(t, completed); meta.Status != ProcessCompleted {
		t.Fatalf("completed wait = %#v / %q", meta, completed.Content)
	}
	if !strings.Contains(completed.Content, "finally") {
		t.Fatalf("completed output = %q", completed.Content)
	}

	long := runProcessResult(t, manager.bash, bashArgs{Command: "sleep 30", Background: true})
	longID := jobIDFromText(t, long.Content)
	started := time.Now()
	waited := runProcessResult(t, manager.job, jobArgs{Action: "wait", JobID: longID, TimeoutSeconds: 1})
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("bounded wait took %s", elapsed)
	}
	if meta := processResultForTest(t, waited); meta.Status != ProcessRunning {
		t.Fatalf("timed wait = %#v / %q", meta, waited.Content)
	}

	raw, err := json.Marshal(jobArgs{Action: "wait", JobID: longID, TimeoutSeconds: 3601})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.job(context.Background(), string(raw)); err == nil || !strings.Contains(err.Error(), "timeout must be") {
		t.Fatalf("invalid timeout error = %v", err)
	}
}

func TestCompletionEventsRepeatUntilDeliveredAndStayWithTheirSession(t *testing.T) {
	manager := processManagerForTest(t)
	sessionContext := agent.WithToolSessionID(context.Background(), "session-a")
	started := runProcessResultContext(t, sessionContext, manager.bash, bashArgs{Command: "printf complete", Background: true})
	id := jobIDFromText(t, started.Content)
	select {
	case <-manager.get(id).done:
	case <-time.After(3 * time.Second):
		t.Fatal("background job did not finish")
	}
	if events := manager.PendingCompletionEvents("session-b"); len(events) != 0 {
		t.Fatalf("completion crossed sessions: %#v", events)
	}
	otherSession := agent.WithToolSessionID(context.Background(), "session-b")
	if list := runProcessResultContext(t, otherSession, manager.job, jobArgs{Action: "list"}); list.Content != "no managed jobs\n" {
		t.Fatalf("job list crossed sessions: %q", list.Content)
	}
	raw, err := json.Marshal(jobArgs{Action: "output", JobID: id})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.job(otherSession, string(raw)); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("cross-session job lookup error = %v", err)
	}
	events := manager.PendingCompletionEvents("session-a")
	if len(events) != 1 || events[0].JobID != id || events[0].FinishedAt.IsZero() ||
		!strings.Contains(events[0].Content, "status=completed") || !strings.Contains(events[0].Content, "exit_code=0") {
		t.Fatalf("completion events = %#v", events)
	}
	if repeated := manager.PendingCompletionEvents("session-a"); len(repeated) != 1 || repeated[0].JobID != id {
		t.Fatalf("unacknowledged completion was consumed: %#v", repeated)
	}
	manager.MarkCompletionDelivered(id)
	if events := manager.PendingCompletionEvents("session-a"); len(events) != 0 {
		t.Fatalf("delivered completion remained pending: %#v", events)
	}

	started = runProcessResultContext(t, sessionContext, manager.bash, bashArgs{Command: "printf inspected", Background: true})
	inspectedID := jobIDFromText(t, started.Content)
	<-manager.get(inspectedID).done
	output := runProcessResultContext(t, sessionContext, manager.job, jobArgs{Action: "output", JobID: inspectedID})
	if !strings.Contains(output.Content, "inspected") {
		t.Fatalf("job output = %q", output.Content)
	}
	if events := manager.PendingCompletionEvents("session-a"); len(events) != 1 || events[0].JobID != inspectedID {
		t.Fatalf("unjournaled inspection consumed completion: %#v", events)
	}
	manager.MarkCompletionDelivered(inspectedID)
	if events := manager.PendingCompletionEvents("session-a"); len(events) != 0 {
		t.Fatalf("committed inspection was redelivered: %#v", events)
	}
}

func TestSupervisedCompletionAndDeliverySurviveManagerRestart(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	ctx := agent.WithToolSessionID(context.Background(), "session-restart")

	first := newProcessManagerForTest(t, root, home)
	started := runProcessResultContext(t, ctx, first.bash, bashArgs{Command: "printf durable", Background: true})
	id := jobIDFromText(t, started.Content)
	job := first.get(id)
	if job == nil || !job.supervised || job.detached {
		t.Fatalf("background job = %#v", job)
	}
	select {
	case <-job.done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervised job did not finish")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(jobControlPath(job.jobDir)); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("job-local FIFO = %v, %v", info, err)
	}

	second := newProcessManagerForTest(t, root, home)
	if err := second.AttachSession("session-restart"); err != nil {
		t.Fatal(err)
	}
	events := second.PendingCompletionEvents("session-restart")
	if len(events) != 1 || events[0].JobID != id {
		t.Fatalf("restored completion events = %#v", events)
	}
	output := runProcessResultContext(t, ctx, second.job, jobArgs{Action: "output", JobID: id})
	if !strings.Contains(output.Content, "durable") {
		t.Fatalf("restored output = %q", output.Content)
	}
	second.MarkCompletionDelivered(id)
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	jobDir := jobDirectory(filepath.Join(home, "jobs"), "session-restart", id)
	if _, err := os.Stat(jobDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settled durable job was not pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(jobDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty durable session directory was not pruned: %v", err)
	}

	third := newProcessManagerForTest(t, root, home)
	if err := third.AttachSession("session-restart"); err != nil {
		t.Fatal(err)
	}
	if events := third.PendingCompletionEvents("session-restart"); len(events) != 0 {
		t.Fatalf("delivered completion was restored as pending: %#v", events)
	}
	if status, ok := third.Status(id); ok {
		t.Fatalf("settled delivered job remained in registry: %#v", status)
	}
}

func TestCleanCloseStopsNonDetachedWorkerAndKeepsTerminalState(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	ctx := agent.WithToolSessionID(context.Background(), "session-close")
	first := newProcessManagerForTest(t, root, home)
	started := runProcessResultContext(t, ctx, first.bash, bashArgs{Command: "sleep 30 & printf ready; wait", Background: true})
	id := jobIDFromText(t, started.Content)
	job := first.get(id)
	deadline := time.Now().Add(3 * time.Second)
	for {
		output, _ := first.jobOutput(job, 32)
		if strings.Contains(string(output), "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background child did not start before close")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := newProcessManagerForTest(t, root, home)
	if err := second.AttachSession("session-close"); err != nil {
		t.Fatal(err)
	}
	status, ok := second.Status(id)
	if !ok || status.Status != ProcessKilled || status.ManagedProcesses < 2 {
		t.Fatalf("restored stopped job = %#v, %t", status, ok)
	}
	if events := second.PendingCompletionEvents("session-close"); len(events) != 1 || events[0].JobID != id {
		t.Fatalf("stopped completion events = %#v", events)
	}
}

func TestSupervisedWorkerCanBeAdoptedAfterAbruptManagerLoss(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	gate := filepath.Join(root, "release")
	ctx := agent.WithToolSessionID(context.Background(), "session-crash")
	first := newProcessManagerForTest(t, root, home)
	started := runProcessResultContext(t, ctx, first.bash, bashArgs{
		Command: "while [ ! -f release ]; do sleep 0.05; done; printf recovered", Background: true,
	})
	id := jobIDFromText(t, started.Content)
	firstJob := first.get(id)

	// Model the part of a crash relevant to the registry: the manager stops
	// participating without sending the worker a clean-exit stop request.
	first.mu.Lock()
	first.closed = true
	first.mu.Unlock()
	close(first.closedCh)

	second := newProcessManagerForTest(t, root, home)
	if err := second.AttachSession("session-crash"); err != nil {
		t.Fatal(err)
	}
	if status, ok := second.Status(id); !ok || status.Status != ProcessRunning {
		t.Fatalf("adopted running job = %#v, %t", status, ok)
	}
	if err := os.WriteFile(gate, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	secondJob := second.get(id)
	select {
	case <-secondJob.done:
	case <-time.After(5 * time.Second):
		t.Fatal("adopted job did not finish")
	}
	select {
	case <-firstJob.done:
	case <-time.After(5 * time.Second):
		t.Fatal("original worker observer did not finish")
	}
	output := runProcessResultContext(t, ctx, second.job, jobArgs{Action: "output", JobID: id})
	if !strings.Contains(output.Content, "recovered") {
		t.Fatalf("adopted output = %q", output.Content)
	}
}

func TestAttachRecordsMissingWorkerAsAbandoned(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	sessionID := "session-missing-worker"
	id := "job-fake-worker"

	jobDir := jobDirectory(filepath.Join(home, "jobs"), sessionID, id)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata := jobMetadata{
		Version: jobProtocolVersion, JobID: id, SessionID: sessionID, Command: "sleep 30",
		StartedAt: time.Now().UTC(), TimeoutMillis: int64((time.Minute) / time.Millisecond),
	}
	if err := writeJSONAtomic(filepath.Join(jobDir, jobMetadataFile), metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	control, err := createJobControl(jobControlPath(jobDir))
	if err != nil {
		t.Fatal(err)
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}

	manager := newProcessManagerForTest(t, root, home)
	if err := manager.AttachSession(sessionID); err != nil {
		t.Fatal(err)
	}
	status, ok := manager.Status(id)
	if !ok || status.Status != ProcessAbandoned {
		t.Fatalf("missing worker status = %#v, %t", status, ok)
	}
	if _, err := os.Stat(filepath.Join(jobDir, jobResultFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manager authored a terminal result for abandoned worker: %v", err)
	}
}

func TestAdoptionRereadsResultAfterWorkerDisappears(t *testing.T) {
	jobDir := t.TempDir()
	jobID := "job-result-probe-race"
	zero := 0
	want := jobTerminalResult{
		Version:     jobProtocolVersion,
		JobID:       jobID,
		Started:     true,
		Status:      ProcessCompleted,
		ExitCode:    &zero,
		FinishedAt:  time.Now().UTC(),
		OutputError: "test output degradation",
	}
	probeCalls := 0
	got, terminal, live, err := observeSupervisedJobState(jobDir, jobID, func() (bool, error) {
		probeCalls++
		if err := writeJSONAtomic(filepath.Join(jobDir, jobResultFile), want, 0o600); err != nil {
			return false, err
		}
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if probeCalls != 1 || !terminal || live {
		t.Fatalf("observed state: probes=%d terminal=%t live=%t", probeCalls, terminal, live)
	}
	if got.Status != want.Status || got.ExitCode == nil || *got.ExitCode != 0 || got.OutputError != want.OutputError {
		t.Fatalf("reread result = %#v", got)
	}
}

func TestWorkerLogCapturesEarlyLaunchFailureAndExplainsAbandonedJob(t *testing.T) {
	jobDir := t.TempDir()
	workerLog, err := os.OpenFile(filepath.Join(jobDir, jobWorkerLogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	worker := exec.Command(executable, jobWorkerArg)
	worker.Stdin = strings.NewReader(`{"version":`)
	worker.Stderr = workerLog
	worker.Env = minimalWorkerEnv()
	runErr := worker.Run()
	if err := workerLog.Close(); err != nil {
		t.Fatal(err)
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 125 {
		t.Fatalf("worker launch error = %v", runErr)
	}
	diagnostic, err := os.ReadFile(filepath.Join(jobDir, jobWorkerLogFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diagnostic), "job worker: decode launch specification") {
		t.Fatalf("worker diagnostic = %q", diagnostic)
	}
	reason := abandonedReasonWithWorkerLog(jobDir, "worker disappeared without a terminal result")
	if !strings.Contains(reason, "worker.log: job worker: decode launch specification") {
		t.Fatalf("abandoned reason = %q", reason)
	}
}

func TestSupervisedWorkerWritesItsFailureToJobLocalLog(t *testing.T) {
	manager := processManagerForTest(t)
	workdir := t.TempDir()
	missingProgram := filepath.Join(t.TempDir(), "missing-program")
	job, err := manager.startSupervised(processSpec{
		command:    "missing-program",
		workdir:    workdir,
		timeout:    time.Minute,
		origin:     processOriginModel,
		sessionID:  "session-worker-log",
		supervised: true,
	}, &exec.Cmd{
		Path: missingProgram,
		Args: []string{"missing-program"},
		Dir:  workdir,
	}, SandboxOff, "job-worker-log")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-job.done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not publish the launch failure")
	}
	status, ok := manager.Status(job.id)
	if !ok || status.Status != ProcessNotStarted {
		t.Fatalf("launch failure status = %#v, %t", status, ok)
	}
	diagnostic, err := os.ReadFile(filepath.Join(job.jobDir, jobWorkerLogFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diagnostic), "job worker: start process:") {
		t.Fatalf("worker diagnostic = %q", diagnostic)
	}
}

func TestWorkerLogsTerminalResultFailureAfterPayloadCompletes(t *testing.T) {
	root := t.TempDir()
	jobID := "job-result-storage-failure"
	jobDir := filepath.Join(root, jobID)
	if err := os.Mkdir(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata := jobMetadata{
		Version:       jobProtocolVersion,
		JobID:         jobID,
		SessionID:     "session-result-storage-failure",
		Command:       "wait then finish",
		StartedAt:     time.Now().UTC(),
		TimeoutMillis: int64(time.Minute / time.Millisecond),
	}
	if err := writeJSONAtomic(filepath.Join(jobDir, jobMetadataFile), metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	control, err := createJobControl(jobControlPath(jobDir))
	if err != nil {
		t.Fatal(err)
	}
	workerLog, err := os.OpenFile(filepath.Join(jobDir, jobWorkerLogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		_ = control.Close()
		t.Fatal(err)
	}
	shell, err := exec.LookPath("sh")
	if err != nil {
		_ = workerLog.Close()
		_ = control.Close()
		t.Skip("sh is unavailable")
	}
	startedMarker := filepath.Join(root, "payload-started")
	releaseMarker := filepath.Join(root, "release-payload")
	finishedMarker := filepath.Join(root, "payload-finished")
	command := "printf started > " + shellQuoteForProcessTest(startedMarker) +
		"; while [ ! -f " + shellQuoteForProcessTest(releaseMarker) + " ]; do sleep 0.01; done" +
		"; printf finished > " + shellQuoteForProcessTest(finishedMarker)
	launch, err := json.Marshal(jobWorkerSpec{
		Version:       jobProtocolVersion,
		JobDir:        jobDir,
		JobID:         jobID,
		TimeoutMillis: int64(time.Minute / time.Millisecond),
		LogLimit:      64,
		Program:       shell,
		Args:          []string{"sh", "-c", command},
		Env:           os.Environ(),
		Dir:           root,
	})
	if err != nil {
		_ = workerLog.Close()
		_ = control.Close()
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		_ = workerLog.Close()
		_ = control.Close()
		t.Fatal(err)
	}
	worker := exec.Command(executable, jobWorkerArg)
	worker.Stdin = bytes.NewReader(launch)
	worker.Stderr = workerLog
	worker.Env = minimalWorkerEnv()
	worker.ExtraFiles = []*os.File{control}
	configureProcessGroup(worker)
	if err := worker.Start(); err != nil {
		_ = workerLog.Close()
		_ = control.Close()
		t.Fatal(err)
	}
	_ = workerLog.Close()
	_ = control.Close()
	waited := false
	t.Cleanup(func() {
		_ = os.WriteFile(releaseMarker, []byte("release"), 0o600)
		if !waited {
			_ = killProcessGroupForTest(worker.Process.Pid)
			_ = worker.Wait()
		}
	})
	waitForPathForProcessTest(t, startedMarker)
	movedJobDir := jobDir + ".moved"
	if err := os.Rename(jobDir, movedJobDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releaseMarker, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	runErr := worker.Wait()
	waited = true
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 125 {
		t.Fatalf("worker result-storage error = %v", runErr)
	}
	waitForPathForProcessTest(t, finishedMarker)
	diagnostic, err := os.ReadFile(filepath.Join(movedJobDir, jobWorkerLogFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diagnostic), "job worker: write terminal result:") {
		t.Fatalf("worker diagnostic = %q", diagnostic)
	}
	if _, err := os.Stat(filepath.Join(movedJobDir, jobResultFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal result unexpectedly exists after storage failure: %v", err)
	}
	live, err := probeJobControl(jobControlPath(movedJobDir))
	if err != nil || live {
		t.Fatalf("worker lifecycle after result failure: live=%t err=%v", live, err)
	}
}

func TestJobControlFIFOTracksReaderLifetimeWithoutWorkerResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), jobControlFile)
	reader, err := createJobControl(path)
	if err != nil {
		t.Fatal(err)
	}
	live, err := probeJobControl(path)
	if err != nil || !live {
		t.Fatalf("live FIFO probe = %t, %v", live, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	live, err = probeJobControl(path)
	if err != nil || live {
		t.Fatalf("closed FIFO probe = %t, %v", live, err)
	}
}

func TestJobControlWriteReturnsEPIPEWhenReaderDiesAfterOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), jobControlFile)
	reader, err := createJobControl(path)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := openJobControlWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeJobControlCommand(writer, jobControlStop); !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("write after reader death = %v, want EPIPE", err)
	}
}

func TestJobControlWriteReportsFullFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), jobControlFile)
	reader, err := createJobControl(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	writer, err := openJobControlWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	chunk := bytes.Repeat([]byte{'x'}, 4096)
	for {
		if _, err := unix.Write(int(writer.Fd()), chunk); errors.Is(err, syscall.EAGAIN) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	if err := writeJobControl(path, jobControlStop); !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("write to full FIFO = %v, want EAGAIN", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("nonblocking FIFO write took %s", elapsed)
	}
}

func TestStoppedWorkerStaysLiveAndConsumesQueuedStopAfterContinue(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "worker-payload-pids")
	ctx := agent.WithToolSessionID(context.Background(), "session-stopped-worker")
	first := newProcessManagerForTest(t, root, home)
	started := runProcessResultContext(t, ctx, first.bash, bashArgs{
		Command:    workerPayloadPIDCommand(pidFile),
		Background: true,
	})
	id := jobIDFromText(t, started.Content)
	workerPID, payloadPID := waitForWorkerPayloadPIDs(t, pidFile)
	finished := false
	t.Cleanup(func() {
		if finished {
			return
		}
		_ = syscall.Kill(workerPID, syscall.SIGCONT)
		_ = killProcessGroupForTest(payloadPID)
		_ = killProcessGroupForTest(workerPID)
	})
	if err := syscall.Kill(workerPID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}

	// Simulate loss of the first manager without running its clean-close path.
	first.mu.Lock()
	first.closed = true
	first.mu.Unlock()
	close(first.closedCh)

	second := newProcessManagerForTest(t, root, home)
	if err := second.AttachSession("session-stopped-worker"); err != nil {
		t.Fatal(err)
	}
	job := second.get(id)
	if job == nil {
		t.Fatal("stopped worker was not adopted")
	}
	if status, ok := second.Status(id); !ok || status.Status != ProcessRunning {
		t.Fatalf("stopped worker status = %#v, %t", status, ok)
	}
	stopContext, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := second.stop(stopContext, id, "queued while worker stopped"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop while worker stopped = %v", err)
	}
	if err := syscall.Kill(workerPID, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-job.done:
	case <-time.After(5 * time.Second):
		t.Fatal("continued worker did not consume queued stop")
	}
	if status, ok := second.Status(id); !ok || status.Status != ProcessKilled {
		t.Fatalf("continued worker result = %#v, %t", status, ok)
	}
	finished = true
}

func TestPayloadDoesNotInheritWorkerLifecycleFIFO(t *testing.T) {
	manager := processManagerForTest(t)
	pidFile := filepath.Join(t.TempDir(), "worker-payload-pids")
	started := runProcessResult(t, manager.bash, bashArgs{
		Command:    workerPayloadPIDCommand(pidFile),
		Background: true,
	})
	id := jobIDFromText(t, started.Content)
	job := manager.get(id)
	workerPID, payloadPID := waitForWorkerPayloadPIDs(t, pidFile)
	defer func() {
		_ = killProcessGroupForTest(payloadPID)
	}()
	if err := killProcessGroupForTest(workerPID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-job.done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker death was not observed")
	}
	if status, ok := manager.Status(id); !ok || status.Status != ProcessAbandoned {
		t.Fatalf("worker death status = %#v, %t", status, ok)
	}
	live, err := probeJobControl(jobControlPath(job.jobDir))
	if err != nil || live {
		t.Fatalf("payload retained FIFO reader: live=%t err=%v", live, err)
	}
	payloadAlive, err := processGroupExistsForTest(payloadPID)
	if err != nil || !payloadAlive {
		t.Fatalf("test payload did not survive worker: alive=%t err=%v", payloadAlive, err)
	}
	if _, err := os.Stat(filepath.Join(job.jobDir, jobResultFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker death produced manager-authored result: %v", err)
	}
}

func TestAttachLeavesUnobservableJobUntouchedAndLoadsOtherJobs(t *testing.T) {
	home := t.TempDir()
	sessionID := "session-missing-control"
	id := "job-missing-control"
	jobDir := jobDirectory(filepath.Join(home, "jobs"), sessionID, id)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata := jobMetadata{
		Version: jobProtocolVersion, JobID: id, SessionID: sessionID, Command: "sleep 30",
		StartedAt: time.Now().UTC(), TimeoutMillis: int64(time.Minute / time.Millisecond),
	}
	if err := writeJSONAtomic(filepath.Join(jobDir, jobMetadataFile), metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	validID := "job-valid-result"
	validDir := jobDirectory(filepath.Join(home, "jobs"), sessionID, validID)
	if err := os.MkdirAll(validDir, 0o700); err != nil {
		t.Fatal(err)
	}
	validMetadata := metadata
	validMetadata.JobID = validID
	validMetadata.Command = "printf complete"
	if err := writeJSONAtomic(filepath.Join(validDir, jobMetadataFile), validMetadata, 0o600); err != nil {
		t.Fatal(err)
	}
	zero := 0
	if err := writeJSONAtomic(filepath.Join(validDir, jobResultFile), jobTerminalResult{
		Version:    jobProtocolVersion,
		JobID:      validID,
		Started:    true,
		Status:     ProcessCompleted,
		ExitCode:   &zero,
		FinishedAt: time.Now().UTC(),
	}, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := newProcessManagerForTest(t, t.TempDir(), home)
	if err := manager.AttachSession(sessionID); err != nil {
		t.Fatal(err)
	}
	if job := manager.get(validID); job == nil || job.status != ProcessCompleted {
		t.Fatalf("valid durable job was not loaded: %#v", job)
	}
	if job := manager.get(id); job != nil {
		t.Fatalf("unobservable durable job was invented: %#v", job)
	}
	notices := strings.Join(manager.AttachSessionNotices(sessionID), "\n")
	if !strings.Contains(notices, id) || !strings.Contains(notices, "control") || !strings.Contains(notices, "left untouched") {
		t.Fatalf("attach notices = %q", notices)
	}
	if _, err := os.Stat(jobDir); err != nil {
		t.Fatalf("unobservable job directory was mutated: %v", err)
	}
}

func TestJobBufferKeepsCircularTail(t *testing.T) {
	buffer := &jobBuffer{limit: 8}
	if _, err := buffer.Write([]byte("01234567")); err != nil {
		t.Fatal(err)
	}
	if _, err := buffer.Write([]byte("89ab")); err != nil {
		t.Fatal(err)
	}
	if len(buffer.data) != 8 || buffer.start != 4 {
		t.Fatalf("circular buffer after overwrite: bytes=%d start=%d", len(buffer.data), buffer.start)
	}
	data, truncated := buffer.snapshot(32)
	stored, discarded := buffer.stats()
	if string(data) != "456789ab" || !truncated || stored != 8 || discarded != 4 {
		t.Fatalf("logical tail = %q, truncated=%t, stored=%d, discarded=%d", data, truncated, stored, discarded)
	}

	if _, err := buffer.Write([]byte("cdefg")); err != nil {
		t.Fatal(err)
	}
	data, truncated = buffer.snapshot(32)
	stored, discarded = buffer.stats()
	if string(data) != "9abcdefg" || !truncated || stored != 8 || discarded != 9 || len(buffer.data) != 8 {
		t.Fatalf("wrapped tail = %q, truncated=%t, stored=%d, discarded=%d, physical=%d", data, truncated, stored, discarded, len(buffer.data))
	}
	if short, shortTruncated := buffer.snapshot(3); string(short) != "efg" || !shortTruncated {
		t.Fatalf("short wrapped tail = %q, truncated=%t", short, shortTruncated)
	}

	if _, err := buffer.Write([]byte("abcdefghijkl")); err != nil {
		t.Fatal(err)
	}
	if data, _ := buffer.snapshot(32); string(data) != "efghijkl" || buffer.start != 0 {
		t.Fatalf("oversized write tail = %q, start=%d", data, buffer.start)
	}
}

func TestBashCapsLogAndMarksTruncatedPreview(t *testing.T) {
	manager := processManagerForTest(t)
	manager.logLimit = 64
	result := runProcessResult(t, manager.bash, bashArgs{Command: "head -c 1024 /dev/zero | tr '\\0' x"})
	meta := processResultForTest(t, result)
	if meta.OutputBytes != 64 || meta.DiscardedBytes < 900 || !strings.Contains(result.Content, "truncated: true") {
		t.Fatalf("meta=%#v result=%q", meta, result.Content)
	}

	manager.logLimit = defaultCommandLogLimit
	result = runProcessResult(t, manager.bash, bashArgs{Command: "head -c 40000 /dev/zero | tr '\\0' x"})
	meta = processResultForTest(t, result)
	if meta.DiscardedBytes != 0 || meta.OutputBytes != 40000 || !strings.Contains(result.Content, "truncated: true") {
		t.Fatalf("preview meta=%#v result bytes=%d", meta, len(result.Content))
	}
}

func TestSupervisedJobKeepsOnlyABoundedDurableTail(t *testing.T) {
	manager := processManagerForTest(t)
	manager.logLimit = 64
	started := runProcessResult(t, manager.bash, bashArgs{
		Command: "head -c 1024 /dev/zero | tr '\\0' x", Background: true,
	})
	id := jobIDFromText(t, started.Content)
	job := manager.get(id)
	select {
	case <-job.done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervised output job did not finish")
	}
	result := runProcessResult(t, manager.job, jobArgs{Action: "output", JobID: id})
	meta := processResultForTest(t, result)
	if meta.OutputBytes != 64 || meta.DiscardedBytes < 900 || !strings.Contains(result.Content, "truncated: true") {
		t.Fatalf("durable meta=%#v result=%q", meta, result.Content)
	}
	info, err := os.Stat(filepath.Join(job.jobDir, jobStdoutFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > manager.logLimit {
		t.Fatalf("durable output size = %d, limit = %d", info.Size(), manager.logLimit)
	}
}

func TestDurableTailCompactionPublishesWholeSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tail.log")
	writer, err := newDurableTailWriter(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	invalid := make(chan []byte, 1)
	go func() {
		defer close(done)
		for index := 0; index < 2_000; index++ {
			data, _ := readDurableTail(path, 64)
			if len(data) != 0 && !bytes.Equal(data, bytes.Repeat([]byte{'x'}, len(data))) {
				invalid <- data
				return
			}
		}
	}()
	for index := 0; index < 500; index++ {
		if _, err := writer.Write(bytes.Repeat([]byte{'x'}, 17)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	select {
	case data := <-invalid:
		t.Fatalf("reader observed partial compaction: %q", data)
	default:
	}
	data, truncated := readDurableTail(path, 64)
	if len(data) != 64 || truncated || !bytes.Equal(data, bytes.Repeat([]byte{'x'}, 64)) {
		t.Fatalf("final durable tail = %d bytes, truncated=%t", len(data), truncated)
	}
}

func TestDurableTailCompactionThresholdIsIndependentFromTailLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tail.log")
	writer, err := newDurableTailWriter(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	if writer.compactAt != 64*durableTailCompactionMultiple {
		t.Fatalf("compaction threshold = %d", writer.compactAt)
	}
	if _, err := writer.Write(bytes.Repeat([]byte{'x'}, 1024)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 1024 {
		t.Fatalf("live file compacted at tail limit: size=%d", info.Size())
	}
	live, truncated := readDurableTail(path, 64)
	if len(live) != 64 || !truncated {
		t.Fatalf("live tail: bytes=%d truncated=%t", len(live), truncated)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 64 || writer.stored != 64 || writer.discarded != 960 {
		t.Fatalf("final tail: size=%d stored=%d discarded=%d", info.Size(), writer.stored, writer.discarded)
	}
}

func TestDurableTailWriteFailureKeepsDrainingPayloadOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tail.log")
	writer, err := newDurableTailWriter(path, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.file.Close(); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", "-c", "head -c 1048576 /dev/zero")
	command.Stdout = writer
	if err := command.Run(); err != nil {
		t.Fatalf("payload was affected by output storage failure: %v", err)
	}
	if err := writer.Close(); err == nil || !strings.Contains(err.Error(), "write durable output") {
		t.Fatalf("durable output error = %v", err)
	}
	if writer.received != 1<<20 || writer.stored != 0 || writer.discarded != writer.received {
		t.Fatalf("durable output accounting: received=%d stored=%d discarded=%d", writer.received, writer.stored, writer.discarded)
	}
}

func TestSupervisedOutputStorageFailureDoesNotChangePayloadResult(t *testing.T) {
	manager := processManagerForTest(t)
	manager.logLimit = 64
	markers := t.TempDir()
	startedMarker := filepath.Join(markers, "started")
	beginMarker := filepath.Join(markers, "begin")
	writtenMarker := filepath.Join(markers, "written")
	releaseMarker := filepath.Join(markers, "release")
	command := "printf started > " + shellQuoteForProcessTest(startedMarker) +
		"; while [ ! -f " + shellQuoteForProcessTest(beginMarker) + " ]; do sleep 0.01; done" +
		"; head -c 4096 /dev/zero | tr '\\0' x" +
		"; printf written > " + shellQuoteForProcessTest(writtenMarker) +
		"; while [ ! -f " + shellQuoteForProcessTest(releaseMarker) + " ]; do sleep 0.01; done"
	started := runProcessResult(t, manager.bash, bashArgs{Command: command, Background: true})
	id := jobIDFromText(t, started.Content)
	job := manager.get(id)
	waitForPathForProcessTest(t, startedMarker)

	movedJobDir := job.jobDir + ".temporarily-moved"
	moved := false
	t.Cleanup(func() {
		if moved {
			_ = os.Rename(movedJobDir, job.jobDir)
		}
		_ = os.WriteFile(releaseMarker, []byte("release"), 0o600)
	})
	if err := os.Rename(job.jobDir, movedJobDir); err != nil {
		t.Fatal(err)
	}
	moved = true
	if err := os.WriteFile(beginMarker, []byte("begin"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForPathForProcessTest(t, writtenMarker)
	if err := os.Rename(movedJobDir, job.jobDir); err != nil {
		t.Fatal(err)
	}
	moved = false
	if err := os.WriteFile(releaseMarker, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-job.done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervised job did not finish after output storage recovered")
	}
	result := runProcessResult(t, manager.job, jobArgs{Action: "output", JobID: id})
	meta := processResultForTest(t, result)
	if meta.Status != ProcessCompleted || meta.ExitCode == nil || *meta.ExitCode != 0 {
		t.Fatalf("payload result changed by output storage failure: %#v / %q", meta, result.Content)
	}
	if !strings.Contains(meta.OutputError, "replace durable output") || !strings.Contains(result.Content, "output_error:") {
		t.Fatalf("output degradation was not reported: %#v / %q", meta, result.Content)
	}
	if meta.OutputBytes != 0 || meta.DiscardedBytes < 4096 {
		t.Fatalf("degraded output accounting = %#v", meta)
	}
}

func TestProcessManagerCloseKillsBackgroundJobs(t *testing.T) {
	manager := processManagerForTest(t)
	started := runProcessResult(t, manager.bash, bashArgs{Command: "sleep 30", Background: true})
	id := jobIDFromText(t, started.Content)
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	job := manager.get(id)
	job.mu.Lock()
	status := job.status
	job.mu.Unlock()
	if status != ProcessKilled {
		t.Fatalf("job status = %q", status)
	}
}

func TestProcessManagerCloseSessionStopsAndForgetsOnlyThatSessionsJobs(t *testing.T) {
	manager := processManagerForTest(t)
	sessionA := agent.WithToolSessionID(context.Background(), "session-a")
	sessionB := agent.WithToolSessionID(context.Background(), "session-b")
	startedA := runProcessResultContext(t, sessionA, manager.bash, bashArgs{Command: "sleep 30", Background: true})
	startedB := runProcessResultContext(t, sessionB, manager.bash, bashArgs{Command: "sleep 30", Background: true})
	idA := jobIDFromText(t, startedA.Content)
	idB := jobIDFromText(t, startedB.Content)
	jobA := manager.get(idA)

	if err := manager.CloseSession("session-a"); err != nil {
		t.Fatal(err)
	}
	if manager.get(idA) != nil {
		t.Fatal("closed session job remained registered")
	}
	jobA.mu.Lock()
	statusA := jobA.status
	jobA.mu.Unlock()
	if statusA != ProcessKilled {
		t.Fatalf("closed session job status = %q", statusA)
	}
	if status, ok := manager.Status(idB); !ok || status.Status != ProcessRunning {
		t.Fatalf("other session job = %#v, %t", status, ok)
	}
}

func TestRunningProcessesRetainSandboxPolicyFromLaunch(t *testing.T) {
	manager := processManagerForTest(t)
	started := runProcessResult(t, manager.bash, bashArgs{Command: "sleep 30", Background: true})
	id := jobIDFromText(t, started.Content)
	if meta := processResultForTest(t, started); meta.SandboxPolicy != SandboxOff {
		t.Fatalf("launch sandbox = %q", meta.SandboxPolicy)
	}
	if err := manager.SetSandboxAfter(SandboxWorkspace, nil); err != nil {
		t.Fatal(err)
	}
	if policies := manager.RunningSandboxPolicies(); policies[SandboxOff] != 1 || policies[SandboxWorkspace] != 0 {
		t.Fatalf("running sandbox policies = %#v", policies)
	}
	if status, ok := manager.Status(id); !ok || status.SandboxPolicy != SandboxOff {
		t.Fatalf("running job status = %#v, %t", status, ok)
	}
}

func TestSandboxChangeCommitsBeforeBecomingVisibleToLaunches(t *testing.T) {
	manager, err := NewProcessManager(t.TempDir(), t.TempDir(), t.TempDir(), SandboxOff)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	called := false
	if err := manager.SetSandboxAfter(SandboxWorkspace, func() error {
		called = true
		if manager.sandbox != SandboxOff {
			t.Fatalf("sandbox became visible before commit: %q", manager.sandbox)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called || manager.sandbox != SandboxWorkspace {
		t.Fatalf("called/sandbox = %v/%q", called, manager.sandbox)
	}
	want := errors.New("journal unavailable")
	if err := manager.SetSandboxAfter(SandboxOff, func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("callback error = %v", err)
	}
	if manager.sandbox != SandboxWorkspace {
		t.Fatalf("failed commit changed sandbox to %q", manager.sandbox)
	}
}

func TestBashAlwaysExposesAndAcceptsBackground(t *testing.T) {
	manager := processManagerForTest(t)
	if !strings.Contains(string(manager.Tools()[0].Spec.InputSchema), `"background"`) {
		t.Fatalf("Bash schema does not expose background: %s", manager.Tools()[0].Spec.InputSchema)
	}
	started := runProcessResult(t, manager.bash, bashArgs{Command: "printf accepted", Background: true})
	if meta := processResultForTest(t, started); meta.JobID == "" {
		t.Fatalf("background call did not return a job: %#v", meta)
	}
}

func TestAwaitRequiredJobsJoinsOnlyYieldedForegroundWork(t *testing.T) {
	manager := processManagerForTest(t)
	ctx := agent.WithToolSessionID(context.Background(), "session-test")
	explicit := runProcessResultContext(t, ctx, manager.bash, bashArgs{Command: "sleep 30", Background: true})
	if pending, err := manager.AwaitRequiredJobs(context.Background(), "session-test"); err != nil || pending {
		t.Fatalf("explicit background was join-required: pending=%v err=%v", pending, err)
	}
	explicitID := jobIDFromText(t, explicit.Content)
	if job := manager.get(explicitID); job == nil || !job.supervised {
		t.Fatalf("explicit background did not use durable supervision: %#v", job)
	}
	runProcessResultContext(t, ctx, manager.job, jobArgs{Action: "stop", JobID: explicitID})
	manager.MarkCompletionDelivered(explicitID)

	manager.bashYield = 10 * time.Millisecond
	yielded := runProcessResultContext(t, ctx, manager.bash, bashArgs{Command: "sleep 0.1; printf done"})
	id := jobIDFromText(t, yielded.Content)
	if job := manager.get(id); job == nil || job.supervised {
		t.Fatalf("automatic foreground yield changed to durable supervision: %#v", job)
	}
	if pending, err := manager.AwaitRequiredJobs(context.Background(), "session-test"); err != nil || !pending {
		t.Fatalf("yielded foreground was not joined: pending=%v err=%v", pending, err)
	}
	if events := manager.PendingCompletionEvents("session-test"); len(events) != 1 || events[0].JobID != id {
		t.Fatalf("joined completion events = %#v", events)
	}
}

func TestBashRejectsWorkdirOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	manager, err := NewProcessManager(root, t.TempDir(), t.TempDir(), SandboxOff)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	raw, _ := json.Marshal(bashArgs{Command: "pwd", Workdir: "escape"})
	if _, err := manager.bash(context.Background(), string(raw)); err == nil {
		t.Fatal("bash accepted an escaping workdir")
	}
}

func processManagerForTest(t *testing.T) *ProcessManager {
	t.Helper()
	return newProcessManagerForTest(t, t.TempDir(), t.TempDir())
}

func newProcessManagerForTest(t *testing.T, root, home string) *ProcessManager {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable")
	}
	manager, err := NewProcessManager(root, home, home+"-tool-home", SandboxOff)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func runProcessResult[T any](t *testing.T, run func(context.Context, string) (agent.ToolOutput, error), args T) agent.ToolOutput {
	return runProcessResultContext(t, context.Background(), run, args)
}

func runProcessResultContext[T any](t *testing.T, ctx context.Context, run func(context.Context, string) (agent.ToolOutput, error), args T) agent.ToolOutput {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, err := run(ctx, string(raw))
	if err != nil {
		t.Fatalf("tool error = %v", err)
	}
	return result
}

func processResultForTest(t *testing.T, output agent.ToolOutput) ProcessResult {
	t.Helper()
	if len(output.Details) != 1 {
		t.Fatalf("process details = %#v", output.Details)
	}
	result, ok := ProcessResultFromDetail(output.Details[0])
	if !ok {
		t.Fatalf("invalid process detail = %#v", output.Details[0])
	}
	return result
}

func jobIDFromText(t *testing.T, text string) string {
	t.Helper()
	match := regexp.MustCompile(`(?m)^job_id: (job-[0-9a-f]+)$`).FindStringSubmatch(text)
	if len(match) != 2 {
		t.Fatalf("job id missing: %q", text)
	}
	return match[1]
}

func waitForJobCount(t *testing.T, manager *ProcessManager, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		manager.mu.Lock()
		got := len(manager.jobs)
		manager.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job count = %d, want %d", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForPathForProcessTest(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat marker %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("marker %s was not created", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func workerPayloadPIDCommand(path string) string {
	return `printf '%s %s\n' "$PPID" "$$" > ` + shellQuoteForProcessTest(path) + `; sleep 30`
}

func shellQuoteForProcessTest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func waitForWorkerPayloadPIDs(t *testing.T, path string) (int, int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			fields := strings.Fields(string(data))
			if len(fields) == 2 {
				workerPID, workerErr := strconv.Atoi(fields[0])
				payloadPID, payloadErr := strconv.Atoi(fields[1])
				if workerErr == nil && payloadErr == nil && workerPID > 1 && payloadPID > 1 && workerPID != payloadPID {
					return workerPID, payloadPID
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker/payload PIDs were not published by test payload: %q, %v", data, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func killProcessGroupForTest(pgid int) error {
	if pgid <= 1 {
		return syscall.EINVAL
	}
	err := syscall.Kill(-pgid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func processGroupExistsForTest(pgid int) (bool, error) {
	if pgid <= 1 {
		return false, nil
	}
	err := syscall.Kill(-pgid, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
}
