package tools

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	productlimits "github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/internal/privatefs"
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
	if err := privatefs.InspectDirectory(manager.jobHome, "job home"); err != nil {
		return err
	}
	privatefs.TryRestrictPermissions(manager.jobHome)

	home := sessionJobHome(manager.jobHome, sessionID)
	directory, err := openJobDirectory(home, false)
	if errors.Is(err, os.ErrNotExist) {
		manager.loadedSessions[sessionID] = struct{}{}
		delete(manager.attachNotices, sessionID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read durable jobs for session %s: %w", sessionID, err)
	}
	defer directory.Close()
	entries, err := fs.ReadDir(directory.FS(), ".")
	if err != nil {
		return fmt.Errorf("read durable jobs for session %s: %w", sessionID, err)
	}
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
			if err := directory.RemoveAll(entry.Name()); err != nil {
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
	_ = removeJobDirectory(home, false)
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
	jobHome, err := openJobDirectory(manager.jobHome, true)
	if err != nil {
		return nil, err
	}
	_ = jobHome.Close()
	sessionHome, err := openJobDirectory(filepath.Dir(jobDir), true)
	if err != nil {
		return nil, err
	}
	defer sessionHome.Close()
	if err := sessionHome.Mkdir(id, 0o700); err != nil {
		return nil, fmt.Errorf("create durable job: %w", err)
	}
	cleanup := func(cause error) (*processJob, error) {
		return nil, errors.Join(cause, sessionHome.RemoveAll(id))
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
	payload, err := json.Marshal(launch, json.Deterministic(true))
	if err != nil {
		_ = closeControl()
		return cleanup(fmt.Errorf("encode worker launch: %w", err))
	}
	executable, err := workerExecutable()
	if err != nil {
		_ = closeControl()
		return cleanup(fmt.Errorf("resolve worker executable: %w", err))
	}
	workerLog, err := openJobFile(filepath.Join(jobDir, jobWorkerLogFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
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
	diagnostic := readDurableTail(filepath.Join(jobDir, jobWorkerLogFile), jobWorkerDiagnosticTailSize)
	if diagnostic.readErr != nil {
		detail := fmt.Sprintf("read worker log: %v", diagnostic.readErr)
		if reason == "" {
			return detail
		}
		return reason + "; " + detail
	}
	diagnosticText := strings.Join(strings.Fields(string(diagnostic.data)), " ")
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
	if err := removeJobDirectory(jobDir, true); err != nil {
		return false, err
	}
	// The session directory contains only job directories. Remove it when this
	// was the last settled job; another live job or concurrent cleanup makes a
	// failed removal harmless.
	_ = removeJobDirectory(filepath.Dir(jobDir), false)
	return true, nil
}

func (manager *ProcessManager) durableJobOutput(job *processJob, limit int) processOutput {
	state := job.snapshot()
	read := func(name, stream string) processOutput {
		output := readDurableTail(filepath.Join(job.jobDir, name), limit)
		// A worker may not have created its logs yet, or may have failed before
		// creating them. Missing logs in those states do not establish output loss.
		if errors.Is(output.readErr, os.ErrNotExist) && (state.status == ProcessRunning || state.status == ProcessNotStarted || state.status == ProcessAbandoned) {
			return processOutput{}
		}
		if output.readErr != nil {
			output.readErr = fmt.Errorf("read %s log: %w", stream, output.readErr)
		}
		return output
	}
	stdout := read(jobStdoutFile, "stdout")
	_, discarded := manager.durableJobStats(job)
	stdout.truncated = stdout.truncated || discarded > 0
	if !state.separateStderr {
		return stdout
	}
	return combineStreams(stdout, read(jobStderrFile, "stderr"))
}

func readDurableTail(path string, limit int) processOutput {
	file, err := openJobFile(path, os.O_RDONLY, 0)
	if err != nil {
		return processOutput{readErr: err}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return processOutput{readErr: err}
	}
	if !info.Mode().IsRegular() {
		return processOutput{readErr: fmt.Errorf("%s is not a regular log file", path)}
	}
	start := int64(0)
	if limit > 0 && info.Size() > int64(limit) {
		start = info.Size() - int64(limit)
	}
	data := make([]byte, int(info.Size()-start))
	if len(data) > 0 {
		var read int
		read, err = file.ReadAt(data, start)
		if err == io.EOF {
			err = nil
		}
		data = data[:read]
	}
	if !utf8.Valid(data) {
		data = []byte(strings.ToValidUTF8(string(data), "�"))
	}
	return processOutput{data: data, truncated: start > 0, readErr: err}
}

func (manager *ProcessManager) durableJobStats(job *processJob) (stored, discarded int64) {
	result, terminal, err := readJobTerminalResult(job.jobDir, job.id)
	if err == nil && terminal {
		return result.StdoutBytes + result.StderrBytes, result.StdoutDiscarded + result.StderrDiscarded
	}
	directory, err := openJobDirectory(job.jobDir, false)
	if err != nil {
		return 0, 0
	}
	defer directory.Close()
	for _, name := range []string{jobStdoutFile, jobStderrFile} {
		if info, statErr := directory.Stat(name); statErr == nil && info.Mode().IsRegular() {
			stored += info.Size()
		}
	}
	return stored, 0
}
