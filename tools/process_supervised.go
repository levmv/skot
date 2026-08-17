package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	productlimits "github.com/levmv/skot/internal/limits"
)

const (
	jobWorkerPollInterval       = 100 * time.Millisecond
	jobWorkerHealthCheck        = time.Second
	jobWorkerDiagnosticTailSize = 8 * 1024
)

// AttachSession validates and adopts durable jobs belonging to a session.
// A session-wide registry read failure remains fatal. An invalid individual
// entry is left untouched and reported through AttachSessionNotices so one job
// cannot make the rest of the session unavailable or break a possibly live
// worker by moving its pathname.
func (manager *ProcessManager) AttachSession(sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	manager.loadMu.Lock()
	defer manager.loadMu.Unlock()
	manager.mu.Lock()
	closed := manager.closed
	manager.mu.Unlock()
	if closed {
		return errors.New("process manager is closed")
	}
	if _, loaded := manager.loadedSessions[sessionID]; loaded {
		return nil
	}
	if err := inspectPrivateDirectory(manager.jobHome, "job home"); err != nil {
		return err
	}

	home := sessionJobHome(manager.jobHome, sessionID)
	entries, err := os.ReadDir(home)
	if errors.Is(err, os.ErrNotExist) {
		manager.loadedSessions[sessionID] = struct{}{}
		delete(manager.attachNotices, sessionID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read durable jobs for session %s: %w", sessionID, err)
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
	var notices []string
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "job-") {
			continue
		}
		jobDir := filepath.Join(home, entry.Name())
		delivered, err := jobDelivered(jobDir)
		if err != nil {
			notices = append(notices, fmt.Sprintf(
				"durable job %s is unobservable and was left untouched: read delivery marker: %v",
				entry.Name(), err,
			))
			continue
		}
		if delivered {
			if err := os.RemoveAll(jobDir); err != nil {
				notices = append(notices, fmt.Sprintf(
					"delivered durable job %s could not be removed and was left for a later cleanup: %v",
					entry.Name(), err,
				))
			}
			continue
		}
		job, err := manager.loadSupervisedJob(jobDir, sessionID)
		if err != nil {
			notices = append(notices, fmt.Sprintf(
				"durable job %s is unobservable and was left untouched: %v",
				entry.Name(), err,
			))
			continue
		}
		manager.mu.Lock()
		if existing := manager.jobs[job.id]; existing == nil {
			manager.jobs[job.id] = job
			manager.mu.Unlock()
			if job.status == ProcessRunning {
				go manager.monitorAdoptedJob(job)
			}
		} else {
			manager.mu.Unlock()
		}
	}
	manager.loadedSessions[sessionID] = struct{}{}
	manager.attachNotices[sessionID] = notices
	_ = os.Remove(home)
	return nil
}

// AttachSessionNotices reports non-fatal per-job registry failures observed by
// the most recent AttachSession. The directories named by these notices remain
// untouched for a later recovery attempt or explicit repair.
func (manager *ProcessManager) AttachSessionNotices(sessionID string) []string {
	sessionID = strings.TrimSpace(sessionID)
	manager.loadMu.Lock()
	defer manager.loadMu.Unlock()
	return append([]string(nil), manager.attachNotices[sessionID]...)
}

func (manager *ProcessManager) loadSupervisedJob(jobDir, sessionID string) (*processJob, error) {
	metadata, err := loadJobMetadata(jobDir)
	if err != nil {
		return nil, fmt.Errorf("load metadata: %w", err)
	}
	if err := validateJobSessionOwnership(metadata, sessionID); err != nil {
		return nil, err
	}
	job := supervisedJobFromMetadata(metadata, jobDir)
	result, terminal, live, err := observeSupervisedJobState(jobDir, job.id, func() (bool, error) {
		return manager.probeSupervisedJob(job)
	})
	if err != nil {
		return nil, err
	}
	if terminal {
		manager.applyTerminalResult(job, result)
		return job, nil
	}
	if live {
		return job, nil
	}
	manager.deriveAbandoned(job, "worker disappeared without a terminal result")
	return job, nil
}

func supervisedJobFromMetadata(metadata jobMetadata, jobDir string) *processJob {
	return &processJob{
		id:             metadata.JobID,
		sessionID:      metadata.SessionID,
		command:        metadata.Command,
		done:           make(chan struct{}),
		status:         ProcessRunning,
		startedAt:      metadata.StartedAt,
		scope:          metadata.Scope,
		separateStderr: metadata.SeparateStderr,
		supervised:     true,
		detached:       metadata.Detach,
		jobDir:         jobDir,
	}
}

// observeSupervisedJobState closes the result-before-reader-close race during
// adoption. A conforming worker publishes result.json before releasing its
// FIFO reader, so ENXIO requires one final result read before abandoned can be
// derived.
func observeSupervisedJobState(jobDir, jobID string, probe func() (bool, error)) (jobTerminalResult, bool, bool, error) {
	result, terminal, err := readJobTerminalResult(jobDir, jobID)
	if err != nil {
		return result, false, false, fmt.Errorf("read terminal result: %w", err)
	}
	if terminal {
		return result, true, false, nil
	}
	live, err := probe()
	if err != nil {
		return result, false, false, fmt.Errorf("probe worker lifecycle: %w", err)
	}
	if live {
		return result, false, true, nil
	}
	result, terminal, err = readJobTerminalResult(jobDir, jobID)
	if err != nil {
		return result, false, false, fmt.Errorf("reread terminal result after worker disappearance: %w", err)
	}
	return result, terminal, false, nil
}

func (manager *ProcessManager) startSupervised(spec processSpec, process *exec.Cmd, scope Scope, id string) (*processJob, error) {
	if spec.origin != processOriginModel {
		return nil, errors.New("only model processes can use the supervised backend")
	}
	if strings.TrimSpace(spec.sessionID) != "" {
		if err := manager.AttachSession(spec.sessionID); err != nil {
			return nil, err
		}
	}
	stdin, err := readProcessInput(spec.stdin)
	if err != nil {
		return nil, fmt.Errorf("read process input: %w", err)
	}
	if process.Env == nil {
		process.Env = os.Environ()
	}
	startedAt := time.Now().UTC()
	jobDir := jobDirectory(manager.jobHome, spec.sessionID, id)
	if err := ensurePrivateDirectory(manager.jobHome, "job home"); err != nil {
		return nil, err
	}
	if err := ensurePrivateDirectory(filepath.Dir(jobDir), "session job home"); err != nil {
		return nil, err
	}
	if err := os.Mkdir(jobDir, 0o700); err != nil {
		return nil, fmt.Errorf("create durable job: %w", err)
	}
	cleanup := func(cause error) (*processJob, error) {
		return nil, errors.Join(cause, os.RemoveAll(jobDir))
	}
	metadata := jobMetadata{
		Version:        jobProtocolVersion,
		JobID:          id,
		SessionID:      strings.TrimSpace(spec.sessionID),
		Command:        spec.command,
		StartedAt:      startedAt,
		TimeoutMillis:  spec.timeout.Milliseconds(),
		SeparateStderr: spec.separateStderr,
		Scope:          scope,
		Detach:         spec.detach,
	}
	if err := writeJSONAtomic(filepath.Join(jobDir, jobMetadataFile), metadata, 0o600); err != nil {
		return cleanup(fmt.Errorf("write durable job metadata: %w", err))
	}
	control, err := createJobControl(jobControlPath(jobDir))
	if err != nil {
		return cleanup(err)
	}
	controlOpen := true
	closeControl := func() error {
		if !controlOpen {
			return nil
		}
		controlOpen = false
		return control.Close()
	}
	launch := jobWorkerSpec{
		Version:  jobWorkerProtocolVersion,
		JobDir:   jobDir,
		LogLimit: manager.logLimit,
		Program:  process.Path,
		Args:     append([]string(nil), process.Args...),
		Env:      append([]string(nil), process.Env...),
		Dir:      process.Dir,
		Stdin:    stdin,
	}
	payload, err := json.Marshal(launch)
	if err != nil {
		_ = closeControl()
		return cleanup(fmt.Errorf("encode worker launch: %w", err))
	}
	executable, err := os.Executable()
	if err != nil {
		_ = closeControl()
		return cleanup(fmt.Errorf("resolve worker executable: %w", err))
	}
	workerLog, err := os.OpenFile(filepath.Join(jobDir, jobWorkerLogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		_ = closeControl()
		return cleanup(fmt.Errorf("open job worker log: %w", err))
	}
	worker := exec.Command(executable, jobWorkerArg)
	worker.Stdin = bytes.NewReader(payload)
	worker.Stderr = workerLog
	worker.Env = minimalWorkerEnv()
	worker.ExtraFiles = []*os.File{control}
	configureProcessGroup(worker)
	if err := worker.Start(); err != nil {
		_ = workerLog.Close()
		_ = closeControl()
		return cleanup(fmt.Errorf("start job worker: %w", err))
	}
	_ = workerLog.Close()
	controlCloseErr := closeControl()
	wait := make(chan error, 1)
	go func() { wait <- worker.Wait() }()

	job := supervisedJobFromMetadata(metadata, jobDir)
	if controlCloseErr != nil {
		cause := fmt.Errorf("close manager job control reader: %w", controlCloseErr)
		stopErr := manager.requestJobStop(job)
		select {
		case <-wait:
			return cleanup(errors.Join(cause, stopErr))
		case <-time.After(jobStopTimeout):
			return nil, errors.Join(cause, stopErr, errors.New("timed out stopping job worker after control handoff failure"))
		}
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		cause := errors.New("process manager closed while starting job")
		stopErr := manager.requestJobStop(job)
		select {
		case <-wait:
			return cleanup(errors.Join(cause, stopErr))
		case <-time.After(jobStopTimeout):
			// Keep the durable state if the worker did not acknowledge shutdown.
			// Removing it here would turn a rare start/close race into an
			// unobservable orphan process.
			return nil, errors.Join(cause, stopErr, errors.New("timed out waiting for job worker to stop"))
		}
	}
	manager.jobs[id] = job
	manager.mu.Unlock()
	go manager.awaitOwnedWorker(job, wait)
	return job, nil
}

func readProcessInput(input io.Reader) ([]byte, error) {
	if input == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(input, productlimits.MaxModelCompletionBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > productlimits.MaxModelCompletionBytes {
		return nil, errors.New("process input is too large for the supervised worker")
	}
	return data, nil
}

func minimalWorkerEnv() []string {
	result := make([]string, 0, 4)
	for _, name := range []string{"PATH", "TMPDIR", "LANG", "LC_ALL", "TZ"} {
		if value, ok := os.LookupEnv(name); ok {
			result = append(result, name+"="+value)
		}
	}
	return result
}

func (manager *ProcessManager) awaitOwnedWorker(job *processJob, workerWait <-chan error) {
	waitErr := <-workerWait
	if err := manager.refreshSupervisedJob(job); err == nil {
		if job.snapshot().status != ProcessRunning {
			return
		}
	}
	requestedStop := strings.TrimSpace(job.snapshot().stopReason)
	reason := "job worker exited without a terminal result"
	if requestedStop != "" {
		reason = requestedStop + "; " + reason
	}
	if waitErr != nil {
		reason += ": " + waitErr.Error()
	}
	manager.deriveAbandoned(job, reason)
}

func (manager *ProcessManager) monitorAdoptedJob(job *processJob) {
	resultTicker := time.NewTicker(jobWorkerPollInterval)
	healthTicker := time.NewTicker(jobWorkerHealthCheck)
	defer resultTicker.Stop()
	defer healthTicker.Stop()
	for {
		select {
		case <-manager.closedCh:
			return
		case <-job.done:
			return
		case <-resultTicker.C:
			if err := manager.refreshSupervisedJob(job); err == nil {
				if job.snapshot().status != ProcessRunning {
					return
				}
			}
		case <-healthTicker.C:
			live, err := manager.probeSupervisedJob(job)
			if err != nil || live {
				continue
			}
			if err := manager.refreshSupervisedJob(job); err == nil {
				if job.snapshot().status != ProcessRunning {
					return
				}
			}
			manager.deriveAbandoned(job, "worker disappeared without a terminal result")
			return
		}
	}
}

func (manager *ProcessManager) probeSupervisedJob(job *processJob) (bool, error) {
	return probeJobControl(jobControlPath(job.jobDir))
}

func (manager *ProcessManager) requestJobStop(job *processJob) error {
	return writeJobControl(jobControlPath(job.jobDir), jobControlStop)
}

func (manager *ProcessManager) refreshSupervisedJob(job *processJob) error {
	result, terminal, err := readJobTerminalResult(job.jobDir, job.id)
	if err != nil {
		return err
	}
	if terminal {
		manager.applyTerminalResult(job, result)
	}
	return nil
}

func (manager *ProcessManager) applyTerminalResult(job *processJob, result jobTerminalResult) {
	job.mu.Lock()
	if job.status == ProcessRunning {
		job.status = result.Status
		job.exitCode = result.ExitCode
		job.errText = result.Error
		job.outputError = result.OutputError
		job.stopReason = result.StopReason
		job.managedProcesses = result.ManagedProcesses
		if !result.StartedAt.IsZero() {
			job.startedAt = result.StartedAt
		}
		job.finishedAt = result.FinishedAt
	}
	job.mu.Unlock()
	job.doneOnce.Do(func() { close(job.done) })
}

func (manager *ProcessManager) deriveAbandoned(job *processJob, reason string) {
	reason = abandonedReasonWithWorkerLog(job.jobDir, reason)
	job.mu.Lock()
	if job.status == ProcessRunning {
		job.status = ProcessAbandoned
		job.errText = strings.TrimSpace(reason)
		job.finishedAt = time.Now().UTC()
	}
	job.mu.Unlock()
	job.doneOnce.Do(func() { close(job.done) })
}

func abandonedReasonWithWorkerLog(jobDir, reason string) string {
	reason = strings.TrimSpace(reason)
	diagnostic, _ := readDurableTail(filepath.Join(jobDir, jobWorkerLogFile), jobWorkerDiagnosticTailSize)
	diagnosticText := strings.Join(strings.Fields(string(diagnostic)), " ")
	if diagnosticText == "" {
		return reason
	}
	if reason == "" {
		return "worker.log: " + diagnosticText
	}
	return reason + "; worker.log: " + diagnosticText
}

func (manager *ProcessManager) markSupervisedDelivered(job *processJob) {
	_ = markJobDelivered(job.jobDir)
}

// removeSettledJobState bounds the durable registry without taking output
// away during the process that delivered it. Once a terminal completion has
// been acknowledged, the journal is the durable account; a later process has
// no reason to retain the worker mailbox as a second history store.
func removeSettledJobState(job *processJob) (bool, error) {
	state := job.snapshot()
	settled := state.supervised && state.status != ProcessRunning && state.completionSeen
	jobDir := job.jobDir
	if !settled {
		return false, nil
	}
	if _, terminal, err := readJobTerminalResult(jobDir, job.id); err != nil {
		return false, err
	} else if !terminal {
		live, err := probeJobControl(jobControlPath(jobDir))
		if err != nil {
			return false, err
		}
		if live {
			return false, nil
		}
	}
	if err := os.RemoveAll(jobDir); err != nil {
		return false, err
	}
	// The session directory contains only job directories. Remove it when this
	// was the last settled job; another live job or concurrent cleanup makes a
	// failed removal harmless.
	_ = os.Remove(filepath.Dir(jobDir))
	return true, nil
}

func (manager *ProcessManager) durableJobOutput(job *processJob, limit int) ([]byte, bool) {
	stdout, stdoutTruncated := readDurableTail(filepath.Join(job.jobDir, jobStdoutFile), limit)
	_, discarded := manager.durableJobStats(job)
	stdoutTruncated = stdoutTruncated || discarded > 0
	if !job.snapshot().separateStderr {
		return stdout, stdoutTruncated
	}
	stderr, stderrTruncated := readDurableTail(filepath.Join(job.jobDir, jobStderrFile), limit)
	return combineStreams(stdout, stderr, stdoutTruncated, stderrTruncated)
}

func readDurableTail(path string, limit int) ([]byte, bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false
	}
	start := int64(0)
	if limit > 0 && info.Size() > int64(limit) {
		start = info.Size() - int64(limit)
	}
	data := make([]byte, int(info.Size()-start))
	if len(data) > 0 {
		read, readErr := file.ReadAt(data, start)
		if readErr != nil && readErr != io.EOF {
			return nil, false
		}
		data = data[:read]
	}
	if !utf8.Valid(data) {
		data = []byte(strings.ToValidUTF8(string(data), "�"))
	}
	return data, start > 0
}

func (manager *ProcessManager) durableJobStats(job *processJob) (stored, discarded int64) {
	result, terminal, err := readJobTerminalResult(job.jobDir, job.id)
	if err == nil && terminal {
		return result.StdoutBytes + result.StderrBytes, result.StdoutDiscarded + result.StderrDiscarded
	}
	for _, name := range []string{jobStdoutFile, jobStderrFile} {
		if info, statErr := os.Stat(filepath.Join(job.jobDir, name)); statErr == nil {
			stored += info.Size()
		}
	}
	return stored, 0
}
