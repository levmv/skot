package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/canonicalpath"
	"github.com/levmv/skot/internal/privatefs"
)

const (
	defaultBashYield              = 10 * time.Second
	defaultBashTimeout            = 10 * time.Minute
	maxBashTimeout                = time.Hour
	defaultCommandPreview         = 32 * 1024
	defaultCommandLogLimit        = 256 * 1024
	maxJobReadBytes               = 256 * 1024
	processFailureTailSize        = 4 * 1024
	jobStopTimeout                = 3 * time.Second
	processGroupKillRetryInterval = 20 * time.Millisecond
	defaultJobWait                = time.Minute
	maxCommandSummary             = 120
)

const (
	ProcessRunning    = agent.ProcessRunning
	ProcessCompleted  = agent.ProcessCompleted
	ProcessFailed     = agent.ProcessFailed
	ProcessKilled     = agent.ProcessKilled
	ProcessTimedOut   = agent.ProcessTimedOut
	ProcessNotStarted = agent.ProcessNotStarted
	ProcessAbandoned  = agent.ProcessAbandoned
	ProcessUnknown    = agent.ProcessUnknown
)

type processOrigin uint8

const (
	processOriginModel processOrigin = iota
	processOriginUser
)

type CompletionEvent struct {
	JobID      string
	FinishedAt time.Time
	Content    string
}

type ProcessManager struct {
	access                 *FilesystemAccess
	toolHome               string
	toolHomeRoot           string
	jobHome                string
	logLimit               int64
	bashYield              time.Duration
	hiddenModelEnvironment map[string]struct{}
	toolHomeMu             sync.Mutex

	mu       sync.Mutex
	jobs     map[string]*processJob
	closed   bool
	closedCh chan struct{}

	loadMu         sync.Mutex
	loadedSessions map[string]struct{}
	attachNotices  map[string][]string
}

type processJob struct {
	mu sync.Mutex

	id               string
	sessionID        string
	command          string
	cmd              *exec.Cmd
	log              *jobBuffer
	errLog           *jobBuffer
	done             chan struct{}
	status           string
	exitCode         *int
	errText          string
	outputError      string
	stopReason       string
	managedProcesses int
	startedAt        time.Time
	finishedAt       time.Time
	completionSeen   bool
	userInitiated    bool
	joinRequired     bool
	scope            Scope
	separateStderr   bool
	supervised       bool
	detached         bool
	jobDir           string
	doneOnce         sync.Once
}

type jobState struct {
	status           string
	exitCode         *int
	errText          string
	outputError      string
	stopReason       string
	managedProcesses int
	startedAt        time.Time
	finishedAt       time.Time
	completionSeen   bool
	userInitiated    bool
	joinRequired     bool
	scope            Scope
	separateStderr   bool
	supervised       bool
	detached         bool
}

func (job *processJob) snapshot() jobState {
	job.mu.Lock()
	defer job.mu.Unlock()
	state := jobState{
		status:           job.status,
		errText:          job.errText,
		outputError:      job.outputError,
		stopReason:       job.stopReason,
		managedProcesses: job.managedProcesses,
		startedAt:        job.startedAt,
		finishedAt:       job.finishedAt,
		completionSeen:   job.completionSeen,
		userInitiated:    job.userInitiated,
		joinRequired:     job.joinRequired,
		scope:            job.scope,
		separateStderr:   job.separateStderr,
		supervised:       job.supervised,
		detached:         job.detached,
	}
	if job.exitCode != nil {
		exitCode := *job.exitCode
		state.exitCode = &exitCode
	}
	return state
}

type processSpec struct {
	command        string
	workdir        string
	timeout        time.Duration
	origin         processOrigin
	sessionID      string
	stdin          io.Reader
	separateStderr bool
	supervised     bool
	detach         bool
	environment    map[string]string
	prepare        func(policy *filesystemPolicy) (string, error)
	build          func(policy *filesystemPolicy, workdir string) (*exec.Cmd, error)
}

type jobBuffer struct {
	mu        sync.Mutex
	data      []byte
	start     int
	limit     int64
	received  int64
	discarded int64
}

type bashArgs struct {
	Command        string `json:"command"`
	Workdir        string `json:"workdir,omitempty"`
	TimeoutSeconds int    `json:"timeout,omitempty"`
	Background     bool   `json:"background,omitempty"`
}

type jobArgs struct {
	Action         string `json:"action"`
	JobID          string `json:"job_id,omitempty"`
	TimeoutSeconds int    `json:"timeout,omitempty"`
}

type jobResultOptions struct {
	output        []byte
	includeOutput bool
	managed       bool
	truncated     bool
}

// NewProcessManager builds a standalone manager. Use
// NewProcessManagerWithAccess when file tools must share its live scope.
func NewProcessManager(root, stateHome, toolHomeRoot string, scope Scope, protections ...*ProtectedPathPolicy) (*ProcessManager, error) {
	if len(protections) > 1 {
		return nil, errors.New("at most one protected-path policy may be supplied")
	}
	var protection *ProtectedPathPolicy
	if len(protections) == 1 {
		protection = protections[0]
	}
	access, err := NewFilesystemAccess(root, scope, protection)
	if err != nil {
		return nil, err
	}
	return NewProcessManagerWithAccess(access, stateHome, toolHomeRoot)
}

// NewProcessManagerWithAccess builds process tools on the same filesystem
// authority state used by built-in file tools.
func NewProcessManagerWithAccess(access *FilesystemAccess, stateHome, toolHomeRoot string) (*ProcessManager, error) {
	if access == nil {
		return nil, errors.New("filesystem access is nil")
	}
	policy := access.current.Load()
	if policy == nil {
		return nil, errors.New("filesystem access is uninitialized")
	}
	stateHome = canonicalpath.Resolve(stateHome)
	jobHome := filepath.Join(stateHome, "jobs")
	toolHomeRoot = strings.TrimSpace(toolHomeRoot)
	toolHome := ""
	if toolHomeRoot != "" {
		toolHomeRoot = canonicalpath.Resolve(toolHomeRoot)
		toolHome = canonicalpath.Resolve(WorkspaceToolHome(toolHomeRoot, policy.workspace))
		if err := policy.processBoundary(toolHome).ValidateLayout(); err != nil {
			return nil, err
		}
	}
	return &ProcessManager{
		access:         access,
		toolHome:       toolHome,
		toolHomeRoot:   toolHomeRoot,
		jobHome:        jobHome,
		logLimit:       defaultCommandLogLimit,
		bashYield:      defaultBashYield,
		jobs:           make(map[string]*processJob),
		closedCh:       make(chan struct{}),
		loadedSessions: make(map[string]struct{}),
		attachNotices:  make(map[string][]string),
	}, nil
}

// ToolHome resolves the disposable workspace home without creating it. An
// empty toolHomeRoot passed to the constructor selects the platform cache root
// only when a process capability actually needs the path.
func (manager *ProcessManager) ToolHome() (string, error) {
	policy := manager.access.snapshot()
	return manager.resolveToolHome(policy)
}

func (manager *ProcessManager) resolveToolHome(policy *filesystemPolicy) (string, error) {
	manager.toolHomeMu.Lock()
	defer manager.toolHomeMu.Unlock()
	if manager.toolHome == "" {
		root := manager.toolHomeRoot
		if root == "" {
			var err error
			root, err = DefaultToolHomeRoot()
			if err != nil {
				return "", err
			}
			root = canonicalpath.Resolve(root)
			manager.toolHomeRoot = root
		}
		manager.toolHome = canonicalpath.Resolve(WorkspaceToolHome(root, policy.workspace))
	}
	if err := policy.processBoundary(manager.toolHome).ValidateLayout(); err != nil {
		return "", err
	}
	return manager.toolHome, nil
}

func (manager *ProcessManager) currentToolHome() string {
	manager.toolHomeMu.Lock()
	defer manager.toolHomeMu.Unlock()
	return manager.toolHome
}

func (manager *ProcessManager) ensureToolHome(policy *filesystemPolicy) error {
	toolHome, err := manager.resolveToolHome(policy)
	if err != nil {
		return err
	}
	if err := privatefs.EnsureDirectory(toolHome, "tool home"); err != nil {
		return err
	}
	privatefs.TryRestrictPermissions(toolHome)
	toolTemp := WorkspaceToolTemp(toolHome)
	if err := privatefs.EnsureDirectory(toolTemp, "tool temp"); err != nil {
		return err
	}
	privatefs.TryRestrictPermissions(toolTemp)
	return nil
}

// HideModelEnvironment removes ambient variables from every model-owned
// process. A configured program may deliberately restore one through its env
// overlay; user-owned shell commands always retain the complete environment.
func (manager *ProcessManager) HideModelEnvironment(names ...string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.hiddenModelEnvironment == nil {
		manager.hiddenModelEnvironment = make(map[string]struct{})
	}
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			manager.hiddenModelEnvironment[name] = struct{}{}
		}
	}
}

func (manager *ProcessManager) Tools() []agent.Tool {
	bashSchema := `{"type":"object","properties":{"command":{"type":"string","description":"Bash command to run."},"workdir":{"type":"string","description":"Starting directory. Relative paths start at the workspace; any path resolving outside it requires machine scope. Defaults to the workspace."},"timeout":{"type":"integer","minimum":1,"maximum":3600,"description":"Hard timeout in seconds. Defaults to 600."},"background":{"type":"boolean","description":"Return immediately instead of waiting, for a server, watcher or other work whose output you do not need in this reply. Ordinary commands stay in the foreground and hand back a job id on their own if they are still running after about 10 seconds. Either way, use job to inspect, wait for, or stop work that is still running."}},"required":["command"],"additionalProperties":false}`
	return []agent.Tool{
		{
			Spec: agent.ToolSpec{
				Name:         "bash",
				Description:  "Run Bash with environment, starting directory, and filesystem access determined by the current scope, process-group cancellation, bounded output, and a hard timeout. Long commands become managed jobs. Non-zero exits are structured results, not tool errors.",
				InputSchema:  json.RawMessage(bashSchema),
				ParallelSafe: false,
			},
			Run: manager.bash,
		},
		{
			Spec: agent.ToolSpec{
				Name:         "job",
				Description:  "List, inspect, wait for, or stop managed Bash and configured program processes. Stop asks the supervisor to kill its payload and record the result.",
				InputSchema:  json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["list","output","wait","stop"]},"job_id":{"type":"string","description":"Required except for list."},"timeout":{"type":"integer","minimum":1,"maximum":3600,"description":"For wait: seconds to block. Defaults to 60."}},"required":["action"],"additionalProperties":false}`),
				ParallelSafe: false,
			},
			Run: manager.job,
		},
	}
}

func (manager *ProcessManager) RunShell(ctx context.Context, command string) (agent.ToolOutput, error) {
	return manager.runBash(ctx, bashArgs{Command: command}, processOriginUser, "")
}

func (manager *ProcessManager) bash(ctx context.Context, raw string) (agent.ToolOutput, error) {
	var args bashArgs
	if err := decodeArgs(raw, &args); err != nil {
		return agent.ToolOutput{}, err
	}
	return manager.runBash(ctx, args, processOriginModel, agent.ToolSessionID(ctx))
}

func (manager *ProcessManager) runBash(ctx context.Context, args bashArgs, origin processOrigin, sessionID string) (agent.ToolOutput, error) {
	args.Command = strings.TrimSpace(args.Command)
	if args.Command == "" {
		return agent.ToolOutput{}, errors.New("command is required")
	}
	if err := ctx.Err(); err != nil {
		return agent.ToolOutput{}, err
	}
	if args.TimeoutSeconds < 0 || args.TimeoutSeconds > int(maxBashTimeout/time.Second) {
		return agent.ToolOutput{}, fmt.Errorf("timeout must be between 1 and %d seconds", int(maxBashTimeout/time.Second))
	}
	timeout := defaultBashTimeout
	if args.TimeoutSeconds > 0 {
		timeout = time.Duration(args.TimeoutSeconds) * time.Second
	}
	job, err := manager.start(processSpec{
		command:    args.Command,
		timeout:    timeout,
		origin:     origin,
		sessionID:  sessionID,
		supervised: args.Background,
		prepare: func(policy *filesystemPolicy) (string, error) {
			workdirPolicy := policy
			enforceProtection := true
			if origin == processOriginUser {
				workdirPolicy = policy.workspaceOnly()
				enforceProtection = false
			}
			workdir, display, info, err := workdirPolicy.resolveExistingPath(args.Workdir, enforceProtection)
			if err != nil {
				return "", err
			}
			if !info.IsDir() {
				return "", fmt.Errorf("workdir %s is not a directory", display)
			}
			return workdir, nil
		},
		build: func(policy *filesystemPolicy, workdir string) (*exec.Cmd, error) {
			if origin == processOriginUser {
				command := ambientBashCommand(args.Command, workdir)
				command.Env = os.Environ()
				return command, nil
			}
			return sandboxedBashCommand(args.Command, workdir, policy.processBoundary(manager.currentToolHome()))
		},
	})
	if err != nil {
		return agent.ToolOutput{}, err
	}
	if args.Background {
		return manager.result(job, jobResultOptions{managed: true})
	}
	if origin == processOriginUser {
		select {
		case <-job.done:
			return manager.completedForegroundResult(job)
		case <-ctx.Done():
			_, _ = manager.stop(context.Background(), job.id, "user shell cancelled")
			manager.forget(job)
			return agent.ToolOutput{}, ctx.Err()
		}
	}

	timer := time.NewTimer(manager.bashYield)
	defer timer.Stop()
	select {
	case <-job.done:
		return manager.completedForegroundResult(job)
	case <-timer.C:
		output, truncated := manager.jobOutput(job, defaultCommandPreview)
		job.mu.Lock()
		job.joinRequired = true
		job.mu.Unlock()
		select {
		case <-job.done:
			return manager.completedForegroundResult(job)
		default:
		}
		return manager.result(job, jobResultOptions{output: output, includeOutput: true, managed: true, truncated: truncated})
	case <-ctx.Done():
		_, _ = manager.stop(context.Background(), job.id, "tool call cancelled")
		manager.forget(job)
		return agent.ToolOutput{}, ctx.Err()
	}
}

func (manager *ProcessManager) completedForegroundResult(job *processJob) (agent.ToolOutput, error) {
	output, truncated := manager.jobOutput(job, defaultCommandPreview)
	if !job.snapshot().supervised {
		manager.forget(job)
	}
	return manager.result(job, jobResultOptions{output: output, includeOutput: true, truncated: truncated})
}

func (manager *ProcessManager) job(ctx context.Context, raw string) (agent.ToolOutput, error) {
	var args jobArgs
	if err := decodeArgs(raw, &args); err != nil {
		return agent.ToolOutput{}, err
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	sessionID := agent.ToolSessionID(ctx)
	if action == "list" {
		content, err := manager.listJobs(sessionID)
		if err != nil {
			return agent.ToolOutput{}, err
		}
		return agent.ToolOutput{Content: content}, nil
	}
	if action != "output" && action != "wait" && action != "stop" {
		return agent.ToolOutput{}, errors.New("action must be one of: list, output, wait, stop")
	}
	if strings.TrimSpace(args.JobID) == "" {
		return agent.ToolOutput{}, fmt.Errorf("job_id is required for %s", action)
	}
	job := manager.get(args.JobID)
	if job == nil || job.sessionID != sessionID {
		return agent.ToolOutput{}, fmt.Errorf("job %q not found", args.JobID)
	}
	if action == "output" {
		content, truncated := manager.jobOutput(job, maxJobReadBytes)
		return manager.result(job, jobResultOptions{output: content, includeOutput: true, managed: true, truncated: truncated})
	}
	if action == "wait" {
		if args.TimeoutSeconds < 0 || args.TimeoutSeconds > int(maxBashTimeout/time.Second) {
			return agent.ToolOutput{}, fmt.Errorf("timeout must be between 1 and %d seconds", int(maxBashTimeout/time.Second))
		}
		if err := waitForJob(ctx, job, args.TimeoutSeconds); err != nil {
			return agent.ToolOutput{}, err
		}
		content, truncated := manager.jobOutput(job, maxJobReadBytes)
		return manager.result(job, jobResultOptions{output: content, includeOutput: true, managed: true, truncated: truncated})
	}
	job, err := manager.stop(ctx, job.id, "stopped by job tool")
	if err != nil {
		return agent.ToolOutput{}, err
	}
	content, truncated := manager.jobOutput(job, defaultCommandPreview)
	return manager.result(job, jobResultOptions{output: content, includeOutput: true, managed: true, truncated: truncated})
}

func waitForJob(ctx context.Context, job *processJob, timeoutSeconds int) error {
	timeout := defaultJobWait
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-job.done:
		return nil
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (manager *ProcessManager) listJobs(sessionID string) (string, error) {
	if err := manager.AttachSession(sessionID); err != nil {
		return "", fmt.Errorf("attach session jobs: %w", err)
	}
	jobs := manager.sessionJobs(sessionID)
	if len(jobs) == 0 {
		return "no managed jobs\n", nil
	}
	var text strings.Builder
	for _, job := range jobs {
		state := job.snapshot()
		if state.supervised {
			_ = manager.refreshSupervisedJob(job)
			state = job.snapshot()
		}
		duration := jobDuration(state.startedAt, state.finishedAt).Truncate(time.Second)
		fmt.Fprintf(&text, "%s %s %s", job.id, state.status, duration)
		if state.exitCode != nil {
			fmt.Fprintf(&text, " exit_code=%d", *state.exitCode)
		}
		if state.detached && state.status == ProcessRunning {
			text.WriteString(" detached")
		}
		fmt.Fprintf(&text, " %s\n", summarizeCommand(job.command))
	}
	return text.String(), nil
}

func summarizeCommand(command string) string {
	command = strings.TrimSpace(command)
	if line, _, found := strings.Cut(command, "\n"); found {
		command = strings.TrimSpace(line) + " ..."
	}
	if runes := []rune(command); len(runes) > maxCommandSummary {
		command = strings.TrimSpace(string(runes[:maxCommandSummary])) + "..."
	}
	return command
}

func (manager *ProcessManager) start(spec processSpec) (*processJob, error) {
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return nil, errors.New("process manager is closed")
	}
	policy := manager.access.snapshot()
	hiddenModelEnvironment := maps.Clone(manager.hiddenModelEnvironment)
	manager.mu.Unlock()
	if spec.origin != processOriginModel && spec.origin != processOriginUser {
		return nil, fmt.Errorf("unknown process origin %d", spec.origin)
	}
	if spec.origin == processOriginModel {
		if policy.scope == ScopeWorkspace {
			if err := manager.ensureToolHome(policy); err != nil {
				return nil, err
			}
		}
		if err := policy.processBoundary(manager.currentToolHome()).ValidateLayout(); err != nil {
			return nil, err
		}
	}
	if spec.build == nil {
		return nil, errors.New("process builder is required")
	}
	if spec.timeout <= 0 {
		return nil, errors.New("process timeout must be positive")
	}
	if spec.prepare != nil {
		workdir, err := spec.prepare(policy)
		if err != nil {
			return nil, err
		}
		spec.workdir = workdir
	}
	if strings.TrimSpace(spec.workdir) == "" {
		return nil, errors.New("process workdir is required")
	}
	if spec.origin == processOriginModel {
		if err := policy.checkScope(spec.workdir); err != nil {
			return nil, err
		}
		if err := policy.checkProtected(spec.workdir, policy.displayPath(spec.workdir)); err != nil {
			return nil, errors.New("process workdir is protected")
		}
	}

	id, err := newJobID()
	if err != nil {
		return nil, err
	}
	process, err := spec.build(policy, spec.workdir)
	if err != nil {
		return nil, fmt.Errorf("prepare process: %w", err)
	}
	if process == nil {
		return nil, errors.New("process builder returned nil")
	}
	if spec.origin == processOriginModel {
		process.Env = modelProcessEnvironment(process.Env, hiddenModelEnvironment, spec.environment)
	}
	process.Dir = spec.workdir
	if spec.supervised {
		return manager.startSupervised(spec, process, policy.scope, id)
	}
	process.Stdin = spec.stdin
	if spec.origin == processOriginModel {
		configureProcessSession(process)
	} else {
		configureProcessGroup(process)
	}
	log := &jobBuffer{limit: manager.logLimit}
	process.Stdout = log
	var errLog *jobBuffer
	if spec.separateStderr {
		errLog = &jobBuffer{limit: manager.logLimit}
		process.Stderr = errLog
	} else {
		process.Stderr = log
	}
	if err := process.Start(); err != nil {
		return nil, fmt.Errorf("start process: %w", err)
	}
	job := &processJob{
		id:             id,
		sessionID:      strings.TrimSpace(spec.sessionID),
		command:        spec.command,
		cmd:            process,
		log:            log,
		errLog:         errLog,
		done:           make(chan struct{}),
		status:         ProcessRunning,
		startedAt:      time.Now().UTC(),
		userInitiated:  spec.origin == processOriginUser,
		separateStderr: spec.separateStderr,
	}
	if spec.origin == processOriginModel {
		job.scope = policy.scope
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		wait := make(chan error, 1)
		go func() { wait <- process.Wait() }()
		_, _ = waitWhileKillingProcessGroup(process, wait)
		return nil, errors.New("process manager closed while starting job")
	}
	manager.jobs[id] = job
	manager.mu.Unlock()
	go manager.monitor(job, spec.timeout)
	return job, nil
}

func (manager *ProcessManager) monitor(job *processJob, timeout time.Duration) {
	wait := make(chan error, 1)
	go func() { wait <- job.cmd.Wait() }()
	var waitErr error
	timedOut := false
	managedProcesses := 0
	timer := time.NewTimer(timeout)
	select {
	case waitErr = <-wait:
		timer.Stop()
	case <-timer.C:
		timedOut = true
		managedProcesses, waitErr = waitWhileKillingProcessGroup(job.cmd, wait)
	}

	job.mu.Lock()
	job.finishedAt = time.Now().UTC()
	job.exitCode = processExitCode(waitErr)
	job.errText = processErrorText(waitErr)
	job.managedProcesses = max(job.managedProcesses, managedProcesses)
	switch {
	case timedOut:
		job.status = ProcessTimedOut
		job.stopReason = fmt.Sprintf("timeout after %s", timeout)
	case job.stopReason != "":
		job.status = ProcessKilled
	case waitErr != nil:
		job.status = ProcessFailed
	default:
		job.status = ProcessCompleted
		zero := 0
		job.exitCode = &zero
	}
	job.mu.Unlock()
	job.doneOnce.Do(func() { close(job.done) })
}

func (manager *ProcessManager) Status(jobID string) (agent.ProcessResult, bool) {
	job := manager.get(jobID)
	if job == nil {
		return agent.ProcessResult{}, false
	}
	if job.snapshot().supervised {
		_ = manager.refreshSupervisedJob(job)
	}
	return manager.processResult(job, true), true
}

func (manager *ProcessManager) StatusDetails(jobID string) ([]agent.Detail, bool) {
	result, ok := manager.Status(jobID)
	if !ok {
		return nil, false
	}
	detail, err := agent.NewDetail(agent.ProcessResultDetailKind, result)
	if err != nil {
		return nil, false
	}
	return []agent.Detail{detail}, true
}

// PendingCompletionEvents reports jobs already registered with the manager.
// Application lifecycle code attaches a durable session before exposing its
// runtime; callers adopting jobs directly must call AttachSession first.
func (manager *ProcessManager) PendingCompletionEvents(sessionID string) []CompletionEvent {
	sessionID = strings.TrimSpace(sessionID)
	jobs := manager.sessionJobs(sessionID)
	var events []CompletionEvent
	for _, job := range jobs {
		state := job.snapshot()
		if state.supervised {
			_ = manager.refreshSupervisedJob(job)
			state = job.snapshot()
		}
		if state.status == ProcessRunning || state.completionSeen {
			continue
		}
		content := fmt.Sprintf("Background job %s completed: status=%s", job.id, state.status)
		if state.exitCode != nil {
			content += fmt.Sprintf(", exit_code=%d", *state.exitCode)
		}
		if state.errText != "" {
			content += ", error=" + state.errText
		}
		if state.outputError != "" {
			content += ", output_error=" + state.outputError
		}
		content += ". Inspect output with job(action=\"output\", job_id=\"" + job.id + "\")."
		events = append(events, CompletionEvent{JobID: job.id, FinishedAt: state.finishedAt, Content: content})
	}
	return events
}

// AwaitRequiredJobs preserves foreground semantics across an automatic
// process yield. A yielded command returned a job id to keep the model turn responsive,
// not because the model chose to abandon its result. A non-interactive run
// therefore waits for those jobs before accepting a final response, then makes
// one more model request so their ordinary completion events can be observed.
// Explicit background jobs are process-scoped but have no join obligation.
func (manager *ProcessManager) AwaitRequiredJobs(ctx context.Context, sessionID string) (bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	jobs := manager.sessionJobs(sessionID)

	pending := false
	for _, job := range jobs {
		state := job.snapshot()
		required := state.joinRequired && !state.completionSeen
		if !required {
			continue
		}
		pending = true
		if state.status != ProcessRunning {
			continue
		}
		select {
		case <-job.done:
		case <-ctx.Done():
			return pending, ctx.Err()
		}
	}
	return pending, nil
}

func (manager *ProcessManager) MarkCompletionDelivered(jobID string) {
	job := manager.get(jobID)
	if job == nil {
		return
	}
	job.mu.Lock()
	job.completionSeen = true
	job.mu.Unlock()
	if job.snapshot().supervised {
		manager.markSupervisedDelivered(job)
	}
}

// DetachedJobs reports supervised work which is deliberately left running at
// a clean session or application boundary. It inspects the current registry;
// callers adopting jobs directly must call AttachSession first.
func (manager *ProcessManager) DetachedJobs(sessionID string) []string {
	sessionID = strings.TrimSpace(sessionID)
	jobs := manager.sessionJobs(sessionID)
	var ids []string
	for _, job := range jobs {
		state := job.snapshot()
		if state.supervised {
			_ = manager.refreshSupervisedJob(job)
			state = job.snapshot()
		}
		if state.detached && state.status == ProcessRunning {
			ids = append(ids, job.id)
		}
	}
	sort.Strings(ids)
	return ids
}

// SetScopeAfter makes beforeApply and publishing the shared filesystem policy
// one linearized boundary with process launch. A model process which acquired
// its launch policy before the callback keeps it; later process and file-tool
// calls get the new policy. A callback failure leaves access unchanged.
func (manager *ProcessManager) SetScopeAfter(scope Scope, beforeApply func() error) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return errors.New("process manager is closed")
	}
	next, err := manager.access.policyForScope(scope)
	if err != nil {
		return err
	}
	if toolHome := manager.currentToolHome(); toolHome != "" {
		if err := next.processBoundary(toolHome).ValidateLayout(); err != nil {
			return err
		}
	}
	if beforeApply != nil {
		if err := beforeApply(); err != nil {
			return err
		}
	}
	manager.access.current.Store(next)
	return nil
}

// RunningScopes reports the launch scope of currently running model processes.
// Changing the shared scope only affects later process starts.
func (manager *ProcessManager) RunningScopes() map[Scope]int {
	scopes := make(map[Scope]int)
	for _, job := range manager.allJobs() {
		state := job.snapshot()
		if state.status == ProcessRunning && state.scope != "" {
			scopes[state.scope]++
		}
	}
	return scopes
}

func (manager *ProcessManager) stop(ctx context.Context, id, reason string) (*processJob, error) {
	job := manager.get(id)
	if job == nil {
		return nil, fmt.Errorf("job %q not found", id)
	}
	job.mu.Lock()
	if job.status != ProcessRunning {
		job.mu.Unlock()
		return job, nil
	}
	job.stopReason = reason
	supervised := job.supervised
	job.mu.Unlock()
	if supervised {
		if err := manager.requestJobStop(job); err != nil {
			if refreshErr := manager.refreshSupervisedJob(job); refreshErr != nil {
				return job, errors.Join(err, refreshErr)
			}
			job.mu.Lock()
			running := job.status == ProcessRunning
			job.mu.Unlock()
			if running {
				live, probeErr := manager.probeSupervisedJob(job)
				if probeErr != nil {
					return job, errors.Join(err, probeErr)
				}
				if live {
					return job, err
				}
				if refreshErr := manager.refreshSupervisedJob(job); refreshErr != nil {
					return job, errors.Join(err, refreshErr)
				}
				job.mu.Lock()
				running = job.status == ProcessRunning
				job.mu.Unlock()
				if running {
					manager.deriveAbandoned(job, "worker disappeared before accepting stop; payload fate is unknown")
				}
			}
		}
		select {
		case <-job.done:
			return job, nil
		case <-ctx.Done():
			return job, ctx.Err()
		case <-time.After(jobStopTimeout):
			return job, errors.New("timed out waiting for job worker to stop; the queued stop may still complete if the worker resumes")
		}
	}
	stopped := make(chan struct{})
	counts := make(chan int, 1)
	go func() { counts <- keepKillingProcessGroup(job.cmd, stopped) }()
	defer func() {
		close(stopped)
		managedProcesses := <-counts
		job.mu.Lock()
		job.managedProcesses = max(job.managedProcesses, managedProcesses)
		job.mu.Unlock()
	}()
	select {
	case <-job.done:
		return job, nil
	case <-ctx.Done():
		return job, ctx.Err()
	case <-time.After(jobStopTimeout):
		return job, errors.New("timed out waiting for job to stop")
	}
}

func waitWhileKillingProcessGroup(command *exec.Cmd, wait <-chan error) (int, error) {
	stopped := make(chan struct{})
	counts := make(chan int, 1)
	go func() { counts <- keepKillingProcessGroup(command, stopped) }()
	err := <-wait
	close(stopped)
	return <-counts, err
}

// keepKillingProcessGroup signals a group until its wait path has completed.
// A child can join while the kernel is delivering one signal, survive it and
// keep an inherited output pipe open, so one kill is not a reliable teardown.
func keepKillingProcessGroup(command *exec.Cmd, stop <-chan struct{}) int {
	ticker := time.NewTicker(processGroupKillRetryInterval)
	defer ticker.Stop()
	managedProcesses := 0
	for {
		managedProcesses = max(managedProcesses, processGroupMemberCount(command))
		_ = killProcessGroup(command)
		select {
		case <-stop:
			return managedProcesses
		case <-ticker.C:
		}
	}
}

func (manager *ProcessManager) get(id string) *processJob {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.jobs[strings.TrimSpace(id)]
}

func (manager *ProcessManager) allJobs() []*processJob {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	jobs := make([]*processJob, 0, len(manager.jobs))
	for _, job := range manager.jobs {
		jobs = append(jobs, job)
	}
	return jobs
}

// sessionJobs returns one session's jobs in start order. Sort keys are
// captured under each job lock so adoption cannot race with ordering.
func (manager *ProcessManager) sessionJobs(sessionID string) []*processJob {
	type orderedJob struct {
		job       *processJob
		startedAt time.Time
	}
	manager.mu.Lock()
	selected := make([]*processJob, 0, len(manager.jobs))
	for _, job := range manager.jobs {
		if job.sessionID == sessionID {
			selected = append(selected, job)
		}
	}
	manager.mu.Unlock()

	ordered := make([]orderedJob, len(selected))
	for index, job := range selected {
		ordered[index] = orderedJob{job: job, startedAt: job.snapshot().startedAt}
	}
	sort.SliceStable(ordered, func(left, right int) bool {
		return ordered[left].startedAt.Before(ordered[right].startedAt)
	})
	for index := range ordered {
		selected[index] = ordered[index].job
	}
	return selected
}

func (manager *ProcessManager) forget(job *processJob) {
	manager.mu.Lock()
	if manager.jobs[job.id] == job {
		delete(manager.jobs, job.id)
	}
	manager.mu.Unlock()
}

func (manager *ProcessManager) Close() error {
	manager.loadMu.Lock()
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		manager.loadMu.Unlock()
		return nil
	}
	manager.closed = true
	jobs := make([]*processJob, 0, len(manager.jobs))
	for _, job := range manager.jobs {
		jobs = append(jobs, job)
	}
	manager.mu.Unlock()
	manager.loadMu.Unlock()
	err := manager.closeJobs(jobs, "Skot exiting", false)
	close(manager.closedCh)
	return err
}

// CloseSession stops local and non-detached supervised jobs belonging to one
// session. Detached workers keep their durable identity and can be
// observed again when that session is resumed.
func (manager *ProcessManager) CloseSession(sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return errors.New("process manager is closed")
	}
	jobs := make([]*processJob, 0, len(manager.jobs))
	for _, job := range manager.jobs {
		if job.sessionID == sessionID {
			jobs = append(jobs, job)
		}
	}
	manager.mu.Unlock()
	err := manager.closeJobs(jobs, "session closed", true)
	manager.loadMu.Lock()
	delete(manager.loadedSessions, sessionID)
	delete(manager.attachNotices, sessionID)
	manager.loadMu.Unlock()
	return err
}

func (manager *ProcessManager) closeJobs(jobs []*processJob, reason string, forget bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var errs []error
	for _, job := range jobs {
		state := job.snapshot()
		if state.status == ProcessRunning && !state.detached {
			if _, err := manager.stop(ctx, job.id, reason); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if _, err := removeSettledJobState(job); err != nil {
			errs = append(errs, err)
		}
		state = job.snapshot()
		keepDetached := state.detached && state.status == ProcessRunning
		if forget && !keepDetached {
			manager.forget(job)
		}
	}
	return errors.Join(errs...)
}

func (manager *ProcessManager) result(job *processJob, options jobResultOptions) (agent.ToolOutput, error) {
	meta := manager.processResult(job, options.managed)
	detail, err := agent.NewDetail(agent.ProcessResultDetailKind, meta)
	if err != nil {
		return agent.ToolOutput{}, fmt.Errorf("encode process result: %w", err)
	}
	return agent.ToolOutput{
		Content: manager.formatJob(job, options.output, options.includeOutput, options.managed, options.truncated),
		Details: []agent.Detail{detail},
	}, nil
}

func (manager *ProcessManager) jobOutput(job *processJob, limit int) ([]byte, bool) {
	state := job.snapshot()
	if state.supervised {
		return manager.durableJobOutput(job, limit)
	}
	stdout, stdoutTruncated := job.log.snapshot(limit)
	if job.errLog == nil {
		return stdout, stdoutTruncated
	}
	stderr, stderrTruncated := job.errLog.snapshot(limit)
	return combineStreams(stdout, stderr, stdoutTruncated, stderrTruncated)
}

func combineStreams(stdout, stderr []byte, stdoutTruncated, stderrTruncated bool) ([]byte, bool) {
	if len(stderr) == 0 {
		return stdout, stdoutTruncated || stderrTruncated
	}
	var output bytes.Buffer
	output.Grow(len(stdout) + len(stderr) + 10)
	output.Write(stdout)
	if len(stdout) > 0 && stdout[len(stdout)-1] != '\n' {
		output.WriteByte('\n')
	}
	output.WriteString("stderr:\n")
	output.Write(stderr)
	return output.Bytes(), stdoutTruncated || stderrTruncated
}

func (manager *ProcessManager) formatJob(job *processJob, output []byte, includeOutput, managed, truncated bool) string {
	state := job.snapshot()
	var text strings.Builder
	if managed {
		fmt.Fprintf(&text, "job_id: %s\n", job.id)
	}
	fmt.Fprintf(&text, "status: %s\n", state.status)
	if state.exitCode != nil {
		fmt.Fprintf(&text, "exit_code: %d\n", *state.exitCode)
	}
	if state.errText != "" {
		fmt.Fprintf(&text, "error: %s\n", state.errText)
	}
	if state.outputError != "" {
		fmt.Fprintf(&text, "output_error: %s\n", state.outputError)
	}
	if state.managedProcesses > 1 {
		fmt.Fprintf(&text, "managed_processes: %d\n", state.managedProcesses)
	}
	if truncated {
		text.WriteString("truncated: true\n")
	}
	if state.status == ProcessRunning {
		if state.detached {
			text.WriteString("detached: true\n")
		}
		text.WriteString("continue: job(action=\"wait\"|\"output\"|\"stop\", job_id=\"" + job.id + "\")\n")
	}
	if includeOutput {
		text.WriteByte('\n')
		text.Write(output)
		if len(output) > 0 && output[len(output)-1] != '\n' {
			text.WriteByte('\n')
		}
	}
	return text.String()
}

func (manager *ProcessManager) processResult(job *processJob, managed bool) agent.ProcessResult {
	state := job.snapshot()
	result := agent.ProcessResult{
		Status:           state.status,
		Scope:            string(state.scope),
		ExitCode:         state.exitCode,
		DurationMillis:   jobDuration(state.startedAt, state.finishedAt).Milliseconds(),
		UserInitiated:    state.userInitiated,
		Detached:         state.detached,
		OutputError:      state.outputError,
		ManagedProcesses: state.managedProcesses,
	}
	if managed || state.supervised {
		result.JobID = job.id
	}
	if state.supervised {
		result.OutputBytes, result.DiscardedBytes = manager.durableJobStats(job)
	} else {
		result.OutputBytes, result.DiscardedBytes = job.log.stats()
		if job.errLog != nil {
			stored, discarded := job.errLog.stats()
			result.OutputBytes += stored
			result.DiscardedBytes += discarded
		}
	}
	if result.Status != ProcessRunning && result.Status != ProcessCompleted && result.OutputBytes > 0 {
		tail, _ := manager.jobOutput(job, processFailureTailSize)
		result.FailureTail = strings.TrimSpace(string(tail))
	}
	return result
}

func (buffer *jobBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	original := len(data)
	buffer.received += int64(original)
	if buffer.limit <= 0 {
		buffer.discarded = buffer.received
		return original, nil
	}
	capacity := int(buffer.limit)
	if len(data) >= capacity {
		buffer.data = append(buffer.data[:0], data[len(data)-capacity:]...)
		buffer.start = 0
		buffer.discarded = buffer.received - int64(len(buffer.data))
		return original, nil
	}
	if available := capacity - len(buffer.data); available > 0 {
		fill := min(available, len(data))
		buffer.data = append(buffer.data, data[:fill]...)
		data = data[fill:]
	}
	if len(data) > 0 {
		// Once full, overwrite the oldest bytes in place. This keeps writes O(n)
		// in the new data instead of copying the complete retained tail.
		first := min(len(data), capacity-buffer.start)
		copy(buffer.data[buffer.start:], data[:first])
		buffer.start = (buffer.start + first) % capacity
		data = data[first:]
		if len(data) > 0 {
			copy(buffer.data[buffer.start:], data)
			buffer.start = (buffer.start + len(data)) % capacity
		}
	}
	buffer.discarded = buffer.received - int64(len(buffer.data))
	return original, nil
}

func (buffer *jobBuffer) snapshot(limit int) ([]byte, bool) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	read := len(buffer.data)
	if limit > 0 && read > limit {
		read = limit
	}
	data := make([]byte, 0, read)
	if read > 0 {
		start := (buffer.start + len(buffer.data) - read) % len(buffer.data)
		first := min(read, len(buffer.data)-start)
		data = append(data, buffer.data[start:start+first]...)
		if first < read {
			data = append(data, buffer.data[:read-first]...)
		}
	}
	if !utf8.Valid(data) {
		data = []byte(strings.ToValidUTF8(string(data), "�"))
	}
	return data, read < len(buffer.data) || buffer.discarded > 0
}

func (buffer *jobBuffer) stats() (stored, discarded int64) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return int64(len(buffer.data)), buffer.discarded
}

func newJobID() (string, error) {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "job-" + hex.EncodeToString(raw[:]), nil
}

func processExitCode(err error) *int {
	if err == nil {
		zero := 0
		return &zero
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		code := exitError.ExitCode()
		return &code
	}
	return nil
}

func processErrorText(err error) string {
	if err == nil {
		return ""
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return ""
	}
	return err.Error()
}

func jobDuration(started, finished time.Time) time.Duration {
	if finished.IsZero() {
		finished = time.Now().UTC()
	}
	return max(time.Duration(0), finished.Sub(started))
}
