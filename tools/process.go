package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/canonicalpath"
	"github.com/levmv/skot/internal/privatefs"
	"github.com/levmv/skot/model"
)

const (
	defaultBashYield              = 10 * time.Second
	defaultBashTimeout            = 10 * time.Minute
	maxBashTimeout                = time.Hour
	maxBackgroundTimeout          = 7 * 24 * time.Hour
	defaultCommandPreview         = 32 * 1024
	defaultCommandLogLimit        = 256 * 1024
	maxJobReadBytes               = 256 * 1024
	processFailureTailSize        = 4 * 1024
	jobStopTimeout                = 3 * time.Second
	processGroupKillRetryInterval = 20 * time.Millisecond
	processGroupPollInterval      = time.Second
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
	timeout          time.Duration
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
	// awaitGroup keeps the job open for processes its command leaves running.
	// leftovers is closed when the command exits while such processes are live,
	// and stopRequested when the first stop is requested.
	awaitGroup    bool
	leftovers     chan struct{}
	stopRequested chan struct{}
}

type jobState struct {
	status           string
	exitCode         *int
	errText          string
	outputError      string
	stopReason       string
	managedProcesses int
	timeout          time.Duration
	startedAt        time.Time
	finishedAt       time.Time
	completionSeen   bool
	userInitiated    bool
	joinRequired     bool
	scope            Scope
	separateStderr   bool
	supervised       bool
	detached         bool
	// leftover is observed when a result is reported: the command has exited,
	// but processes it started still run in the job.
	leftover bool
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
		timeout:          job.timeout,
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
	readOnlyPaths  []string
	stdin          io.Reader
	separateStderr bool
	supervised     bool
	detach         bool
	awaitGroup     bool
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
	TimeoutSeconds *int   `json:"timeout,omitzero"`
	Background     bool   `json:"background,omitzero"`
}

type jobArgs struct {
	Action         string `json:"action"`
	JobID          string `json:"job_id,omitempty"`
	TimeoutSeconds int    `json:"timeout,omitzero"`
}

type jobResultOptions struct {
	outputLimit int
	managed     bool
}

type processOutput struct {
	data      []byte
	truncated bool
	readErr   error
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
	access, err := NewFilesystemAccess(root, scope, nil, protection)
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
	if access.current.Load() == nil {
		return nil, errors.New("filesystem access is uninitialized")
	}
	stateHome = canonicalpath.Resolve(stateHome)
	jobHome := filepath.Join(stateHome, "jobs")
	return &ProcessManager{
		access:         access,
		toolHomeRoot:   strings.TrimSpace(toolHomeRoot),
		jobHome:        jobHome,
		logLimit:       defaultCommandLogLimit,
		bashYield:      defaultBashYield,
		jobs:           make(map[string]*processJob),
		closedCh:       make(chan struct{}),
		loadedSessions: make(map[string]struct{}),
		attachNotices:  make(map[string][]string),
	}, nil
}

// ToolHome resolves the disposable workspace home on first use and validates
// it on every call without creating it. An empty toolHomeRoot passed to the
// constructor selects the platform cache root.
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
		}
		root = canonicalpath.Resolve(root)
		// Keep the workspace entry itself: resolving it would turn a symlink
		// into a different directory with a sandbox write grant.
		manager.toolHome = WorkspaceToolHome(root, policy.workspace)
	}
	if err := privatefs.InspectDirectory(manager.toolHome, fmt.Sprintf("tool home %q", manager.toolHome)); err != nil {
		return "", err
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
	bashSchema := `{"type":"object","properties":{"command":{"type":"string","description":"Bash command to run."},"workdir":{"type":"string","description":"Starting directory; defaults to the workspace. Use instead of cd. Relative paths start at the workspace."},"timeout":{"type":"integer","minimum":0,"maximum":604800,"description":"Hard timeout in seconds. Defaults to 600. Foreground commands allow up to 3600; background work up to a week, or 0 for no limit."},"background":{"type":"boolean","description":"Return immediately instead of waiting, for a server, watcher, download or other long work. Background jobs keep running after Skot exits until they finish, are stopped, or reach their timeout, so use this rather than nohup or setsid. Set timeout to cover the work; use 0 only when the user wants it running after this conversation, such as a service they asked you to start. Stop background work once you no longer need it. Ordinary commands stay in the foreground and hand back a job id on their own if they are still running after about 10 seconds. Either way, use job to inspect, wait for, or stop work that is still running."}},"required":["command"],"additionalProperties":false}`
	return []agent.Tool{
		{
			Spec: model.ToolSpec{
				Name:         "bash",
				Description:  "Run Bash in a fresh shell with process-group cancellation, bounded output, and a hard timeout. Long commands become managed jobs. Non-zero exits are structured results, not tool errors. Check exit codes and investigate failures before continuing.",
				InputSchema:  jsontext.Value(bashSchema),
				ParallelSafe: false,
			},
			Run: manager.bash,
		},
		{
			Spec: model.ToolSpec{
				Name:         "job",
				Description:  "List, inspect, wait for, or stop managed Bash and configured program processes. Stop asks the supervisor to kill its payload and record the result.",
				InputSchema:  jsontext.Value(`{"type":"object","properties":{"action":{"type":"string","enum":["list","output","wait","stop"]},"job_id":{"type":"string","description":"Required except for list."},"timeout":{"type":"integer","minimum":1,"maximum":3600,"description":"For wait: seconds to block. Defaults to 60."}},"required":["action"],"additionalProperties":false}`),
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
	timeout := defaultBashTimeout
	if args.TimeoutSeconds != nil {
		seconds, limit := *args.TimeoutSeconds, maxBashTimeout
		if args.Background {
			limit = maxBackgroundTimeout
		}
		switch {
		case seconds == 0 && !args.Background:
			return agent.ToolOutput{}, errors.New("timeout 0 (no limit) requires background")
		case seconds < 0 || seconds > int(limit/time.Second):
			return agent.ToolOutput{}, fmt.Errorf("timeout must be between 0 and %d seconds", int(limit/time.Second))
		}
		timeout = time.Duration(seconds) * time.Second
	}
	job, err := manager.start(processSpec{
		readOnlyPaths: agent.ToolReadOnlyPaths(ctx),
		command:       args.Command,
		timeout:       timeout,
		origin:        origin,
		sessionID:     sessionID,
		supervised:    args.Background,
		// Background Bash runs until it finishes, is stopped, or times out;
		// Skot's exit does not end it.
		detach: args.Background,
		// Configured programs and the user's own shell may start work meant to
		// outlive them; the model's Bash commands may not.
		awaitGroup: origin == processOriginModel,
		prepare: func(policy *filesystemPolicy) (string, error) {
			workdirPolicy := policy
			enforceProtection := true
			if origin == processOriginUser {
				workdirPolicy = policy.workspaceOnly()
				enforceProtection = false
			}
			workdir, display, info, err := workdirPolicy.resolveExistingPath(args.Workdir, enforceProtection)
			if err != nil {
				return "", fmt.Errorf("prepare workdir %q: %w", filepath.Clean(args.Workdir), err)
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
		return manager.yieldForeground(job)
	case <-job.leftovers:
		// Its output is complete; waiting out the yield would only add latency.
		return manager.yieldForeground(job)
	case <-ctx.Done():
		_, _ = manager.stop(context.Background(), job.id, "tool call cancelled")
		manager.forget(job)
		return agent.ToolOutput{}, ctx.Err()
	}
}

// yieldForeground hands a foreground command that is still running back as a
// job that the run must join.
func (manager *ProcessManager) yieldForeground(job *processJob) (agent.ToolOutput, error) {
	job.mu.Lock()
	job.joinRequired = true
	job.mu.Unlock()
	select {
	case <-job.done:
		return manager.completedForegroundResult(job)
	default:
	}
	return manager.result(job, jobResultOptions{outputLimit: defaultCommandPreview, managed: true})
}

func (manager *ProcessManager) completedForegroundResult(job *processJob) (agent.ToolOutput, error) {
	if !job.snapshot().supervised {
		manager.forget(job)
	}
	return manager.result(job, jobResultOptions{outputLimit: defaultCommandPreview})
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
		return agent.ToolOutput{Content: model.TextContent(content)}, nil
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
	outputLimit := maxJobReadBytes
	switch action {
	case "wait":
		if args.TimeoutSeconds < 0 || args.TimeoutSeconds > int(maxBashTimeout/time.Second) {
			return agent.ToolOutput{}, fmt.Errorf("timeout must be between 1 and %d seconds", int(maxBashTimeout/time.Second))
		}
		if err := waitForJob(ctx, job, args.TimeoutSeconds); err != nil {
			return agent.ToolOutput{}, err
		}
	case "stop":
		stopped, err := manager.stop(ctx, job.id, "stopped by job tool")
		if err != nil {
			return agent.ToolOutput{}, err
		}
		job = stopped
		outputLimit = defaultCommandPreview
	}
	return manager.result(job, jobResultOptions{outputLimit: outputLimit, managed: true})
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
		if state.status == ProcessRunning {
			fmt.Fprintf(&text, " timeout=%s", timeoutText(state, "%s left=%s"))
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
		policy = policy.withReadOnlyPaths(spec.readOnlyPaths)
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
	// Only a supervised worker can run without a time limit.
	if spec.timeout < 0 || (spec.timeout == 0 && !spec.supervised) {
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
		timeout:        spec.timeout,
		startedAt:      time.Now().UTC(),
		userInitiated:  spec.origin == processOriginUser,
		separateStderr: spec.separateStderr,
		awaitGroup:     spec.awaitGroup,
		leftovers:      make(chan struct{}),
		stopRequested:  make(chan struct{}),
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
		if job.awaitGroup {
			if processGroupExists(job.cmd) && processGroupMemberCount(job.cmd) > 0 {
				close(job.leftovers)
			}
			var end processGroupEnd
			end, managedProcesses = awaitProcessGroup(job.cmd, timer.C, job.stopRequested)
			timedOut = end == processGroupTimedOut
		}
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
	case job.stopReason != "":
		// A requested stop keeps its reason even if the timeout fired meanwhile.
		job.status = ProcessKilled
	case timedOut:
		job.status = ProcessTimedOut
		job.stopReason = fmt.Sprintf("timeout after %s", timeout)
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
	return manager.processResult(job, job.snapshot(), true), true
}

func (manager *ProcessManager) StatusDetails(jobID string) ([]model.Detail, bool) {
	result, ok := manager.Status(jobID)
	if !ok {
		return nil, false
	}
	detail, err := agent.NewDetail(agent.ProcessResultDetailKind, result)
	if err != nil {
		return nil, false
	}
	return []model.Detail{detail}, true
}

// PendingCompletionEvents reports jobs already registered with the manager.
// Application lifecycle code attaches a durable session before exposing its
// runtime; callers adopting jobs directly must call AttachSession first.
func (manager *ProcessManager) PendingCompletionEvents(sessionID string) []agent.BoundaryEvent {
	sessionID = strings.TrimSpace(sessionID)
	jobs := manager.sessionJobs(sessionID)
	var events []agent.BoundaryEvent
	for _, job := range jobs {
		state := job.snapshot()
		if state.supervised {
			_ = manager.refreshSupervisedJob(job)
			state = job.snapshot()
		}
		if state.status == ProcessRunning || state.completionSeen {
			continue
		}
		result := manager.processResult(job, state, true)
		content := fmt.Sprintf("Background job %s (%q) finished: status=%s", job.id, result.Command, state.status)
		if state.exitCode != nil {
			content += fmt.Sprintf(", exit_code=%d", *state.exitCode)
		}
		if state.stopReason != "" {
			content += ", stop_reason=" + state.stopReason
		}
		if state.errText != "" {
			content += ", error=" + state.errText
		}
		if state.outputError != "" {
			content += ", output_error=" + state.outputError
		}
		if note := leftoverProcessNote(state); note != "" {
			content += "; " + note
		} else {
			content += "."
		}
		content += " Inspect output with job(action=\"output\", job_id=\"" + job.id + "\")."
		event := agent.BoundaryEvent{JobID: job.id, FinishedAt: state.finishedAt, Content: content}
		// Preserve the text notice even if its presentation details cannot be encoded.
		if detail, err := agent.NewDetail(agent.ProcessResultDetailKind, result); err == nil {
			event.Details = []model.Detail{detail}
		}
		events = append(events, event)
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
	if job.snapshot().supervised {
		_ = manager.refreshSupervisedJob(job)
	}
	job.mu.Lock()
	// Only finished work has a completion to deliver. Replayed or tampered
	// journal state naming a running job must neither hide its real completion
	// nor mark its durable state for deletion.
	if job.status == ProcessRunning {
		job.mu.Unlock()
		return
	}
	job.completionSeen = true
	supervised := job.supervised
	job.mu.Unlock()
	if supervised {
		manager.markSupervisedDelivered(job)
	}
}

// DetachedJobs reports supervised work which is deliberately left running at
// a clean session or application boundary. It inspects the current registry;
// callers adopting jobs directly must call AttachSession first.
func (manager *ProcessManager) DetachedJobs(sessionID string) []string {
	var ids []string
	for _, job := range manager.RunningDetachedJobs(sessionID) {
		ids = append(ids, job.ID)
	}
	sort.Strings(ids)
	return ids
}

// DetachedJob describes detached work that is still running.
type DetachedJob struct {
	ID      string
	Command string
	// Timeout is the job's limit, or zero when it has none.
	Timeout   time.Duration
	Remaining time.Duration
}

// RunningDetachedJobs reports a session's detached work that is still running,
// in start order. Like DetachedJobs, it inspects the current registry.
func (manager *ProcessManager) RunningDetachedJobs(sessionID string) []DetachedJob {
	var running []DetachedJob
	for _, job := range manager.sessionJobs(strings.TrimSpace(sessionID)) {
		state := job.snapshot()
		if state.supervised {
			_ = manager.refreshSupervisedJob(job)
			state = job.snapshot()
		}
		if !state.detached || state.status != ProcessRunning {
			continue
		}
		detached := DetachedJob{ID: job.id, Command: summarizeCommand(job.command), Timeout: state.timeout}
		if state.timeout > 0 {
			detached.Remaining = max(0, time.Until(state.startedAt.Add(state.timeout)))
		}
		running = append(running, detached)
	}
	return running
}

// SetFilesystemPolicyAfter publishes a complete filesystem policy at the same
// linearized boundary used for process launch. A callback failure leaves the
// policy unchanged.
func (manager *ProcessManager) SetFilesystemPolicyAfter(scope Scope, additions *AddedDirectoryPolicy, protection *ProtectedPathPolicy, beforeApply func() error) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return errors.New("process manager is closed")
	}
	next, err := manager.access.policyFor(scope, additions, protection)
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
	firstStop := job.stopReason == ""
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
	if firstStop {
		close(job.stopRequested)
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

type processGroupEnd uint8

const (
	processGroupExited processGroupEnd = iota
	processGroupTimedOut
	processGroupStopped
)

// awaitProcessGroup runs after the payload's own process has been reaped. A
// command can leave processes in its group with output redirected away from
// Skot's pipes, so a completed wait does not mean that the job has finished.
// They stay part of the job until they exit, and the job timeout or a stop
// request kills them as it would have killed the shell. Polling starts fast so
// briefly lingering members do not delay ordinary commands.
func awaitProcessGroup(command *exec.Cmd, timeout <-chan time.Time, stop <-chan struct{}) (processGroupEnd, int) {
	watch := processGroupWatch{command: command}
	interval := processGroupKillRetryInterval
	poll := time.NewTimer(interval)
	defer poll.Stop()
	for !watch.finished() {
		select {
		case <-timeout:
			return processGroupTimedOut, killProcessGroupUntilEmpty(command)
		case <-stop:
			return processGroupStopped, killProcessGroupUntilEmpty(command)
		case <-poll.C:
			interval = min(2*interval, processGroupPollInterval)
			poll.Reset(interval)
		}
	}
	return processGroupExited, 0
}

// killProcessGroupUntilEmpty tears down a group whose leader was already
// reaped, so no wait path can report completion. The member count is only
// reported. The bound keeps a process stuck in uninterruptible sleep from
// holding the job open indefinitely.
func killProcessGroupUntilEmpty(command *exec.Cmd) int {
	watch := processGroupWatch{command: command}
	managedProcesses := 0
	deadline := time.Now().Add(jobStopTimeout)
	for {
		managedProcesses = max(managedProcesses, processGroupMemberCount(command))
		_ = killProcessGroup(command)
		if watch.finished() || time.Now().After(deadline) {
			return managedProcesses
		}
		time.Sleep(processGroupKillRetryInterval)
	}
}

// processGroupWatch decides when a payload group has finished. The kernel's
// group lookup is atomic, but it counts zombies, which never leave the group
// when init does not reap orphans. A /proc scan is not atomic: a member that
// forks and exits during one can hide its child. A zombie-only group therefore
// counts as finished only after two scans see the same zombies; a hidden live
// member would show up in the second scan or leave a new zombie behind.
type processGroupWatch struct {
	command *exec.Cmd
	zombies []int
}

func (watch *processGroupWatch) finished() bool {
	if !processGroupExists(watch.command) {
		return true
	}
	zombies := processGroupZombies(watch.command)
	settled := len(zombies) > 0 && slices.Equal(zombies, watch.zombies)
	watch.zombies = zombies
	return settled
}

// hasLeftoverProcesses reports processes still running in a payload group
// after the payload's own process exited. The leader's pid cannot be reused
// while its process group has members, so its absence identifies that exit.
func hasLeftoverProcesses(command *exec.Cmd) bool {
	return processExited(command) && processGroupMemberCount(command) > 0
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
	var output processOutput
	if options.outputLimit > 0 {
		output = manager.jobOutput(job, options.outputLimit)
	}
	state := job.snapshot()
	if state.status == ProcessRunning && job.awaitGroup && !state.supervised {
		state.leftover = hasLeftoverProcesses(job.cmd)
	}
	// A later output read can recover. Keep read errors on this result without
	// changing the job's status or its recorded output storage errors.
	if output.readErr != nil {
		if state.outputError != "" {
			state.outputError += "; "
		}
		state.outputError += output.readErr.Error()
	}
	meta := manager.processResult(job, state, options.managed)
	detail, err := agent.NewDetail(agent.ProcessResultDetailKind, meta)
	if err != nil {
		return agent.ToolOutput{}, fmt.Errorf("encode process result: %w", err)
	}
	return agent.ToolOutput{
		Content: model.TextContent(formatJob(job.id, state, output, options)),
		Details: []model.Detail{detail},
	}, nil
}

func (manager *ProcessManager) jobOutput(job *processJob, limit int) processOutput {
	state := job.snapshot()
	if state.supervised {
		return manager.durableJobOutput(job, limit)
	}
	stdout := job.log.snapshot(limit)
	if job.errLog == nil {
		return stdout
	}
	return combineStreams(stdout, job.errLog.snapshot(limit))
}

func combineStreams(stdout, stderr processOutput) processOutput {
	stdout.truncated = stdout.truncated || stderr.truncated
	stdout.readErr = errors.Join(stdout.readErr, stderr.readErr)
	if len(stderr.data) == 0 {
		return stdout
	}
	var output bytes.Buffer
	output.Grow(len(stdout.data) + len(stderr.data) + 10)
	output.Write(stdout.data)
	if len(stdout.data) > 0 && stdout.data[len(stdout.data)-1] != '\n' {
		output.WriteByte('\n')
	}
	output.WriteString("stderr:\n")
	output.Write(stderr.data)
	stdout.data = output.Bytes()
	return stdout
}

func formatJob(jobID string, state jobState, output processOutput, options jobResultOptions) string {
	var text strings.Builder
	if options.managed {
		fmt.Fprintf(&text, "job_id: %s\n", jobID)
	}
	fmt.Fprintf(&text, "status: %s\n", state.status)
	if state.status == ProcessRunning {
		fmt.Fprintf(&text, "timeout: %s\n", timeoutText(state, "%s (%s left)"))
	}
	if state.exitCode != nil {
		fmt.Fprintf(&text, "exit_code: %d\n", *state.exitCode)
	}
	if state.status != ProcessRunning && state.stopReason != "" {
		fmt.Fprintf(&text, "stop_reason: %s\n", state.stopReason)
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
	if output.truncated {
		text.WriteString("truncated: true\n")
	}
	if note := leftoverProcessNote(state); note != "" {
		fmt.Fprintf(&text, "note: %s\n", note)
	}
	if state.status == ProcessRunning {
		if state.detached {
			text.WriteString("detached: true\n")
		}
		text.WriteString("continue: job(action=\"wait\"|\"output\"|\"stop\", job_id=\"" + jobID + "\")\n")
	}
	if options.outputLimit > 0 {
		text.WriteByte('\n')
		text.Write(output.data)
		if len(output.data) > 0 && output.data[len(output.data)-1] != '\n' {
			text.WriteByte('\n')
		}
	}
	return text.String()
}

// leftoverProcessNote explains jobs kept alive, or killed, only because of
// processes the command left behind. A command killed with its group reports a
// signal, so an ordinary exit code on a stopped job means it had already exited
// by itself.
func leftoverProcessNote(state jobState) string {
	switch state.status {
	case ProcessRunning:
		if !state.leftover {
			return ""
		}
		return "the command has exited, but processes it started are still running in this job and will be killed at its timeout. Run long work with background=true and a timeout that covers it."
	case ProcessTimedOut, ProcessKilled:
		if state.exitCode == nil || *state.exitCode < 0 {
			return ""
		}
		cause := "timeout"
		if state.status == ProcessKilled {
			cause = "stop"
		}
		return fmt.Sprintf("the command exited with code %d before the %s; the %s killed processes it had left running.", *state.exitCode, cause, cause)
	}
	return ""
}

// timeoutText shows a running job's limit and how much of it is left, so the
// model can compare it with the work it expects. Zero means no limit.
func timeoutText(state jobState, format string) string {
	if state.timeout == 0 {
		return "none"
	}
	remaining := max(0, time.Until(state.startedAt.Add(state.timeout))).Round(time.Second)
	return fmt.Sprintf(format, state.timeout, remaining)
}

func (manager *ProcessManager) processResult(job *processJob, state jobState, managed bool) agent.ProcessResult {
	result := agent.ProcessResult{
		Command:          summarizeCommand(job.command),
		Status:           state.status,
		Error:            state.errText,
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
		tail := manager.jobOutput(job, processFailureTailSize)
		result.FailureTail = strings.TrimSpace(string(tail.data))
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

func (buffer *jobBuffer) snapshot(limit int) processOutput {
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
	return processOutput{data: data, truncated: read < len(buffer.data) || buffer.discarded > 0}
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
	if exitError, ok := errors.AsType[*exec.ExitError](err); ok {
		code := exitError.ExitCode()
		return &code
	}
	return nil
}

func processErrorText(err error) string {
	if err == nil {
		return ""
	}
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
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
