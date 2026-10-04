package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	modelapi "github.com/levmv/skot/model"
)

// ModelRequestPolicy bounds one logical model request. MaxAttempts counts the
// first attempt; -1 means attempts are bounded only by RetryBudget. The budget
// covers attempts and delays, and starts afresh after every successful model
// response. StreamIdleTimeout separately bounds silence within one attempt.
type ModelRequestPolicy struct {
	MaxAttempts       int
	RetryBudget       time.Duration
	BaseDelay         time.Duration
	MaxDelay          time.Duration
	StreamIdleTimeout time.Duration
}

const (
	// DefaultMaxToolIterations is an emergency fuse, not an expected working
	// budget. It is deliberately high enough for long unattended runs.
	DefaultMaxToolIterations  = 128
	maxModelAttemptErrorBytes = 32 * 1024
	maxModelAttemptFieldBytes = 4 * 1024

	toolLimitInstructions = "Tool call limit reached. Do not call any more tools; answer with what you have, and be explicit about anything you could not verify."
)

// runRequestSpec describes the parts of a run request which are not already
// durable session state. Its zero value is the ordinary tools-enabled request.
type runRequestSpec struct {
	omitTools     bool
	extraUserText string
}

type Config struct {
	// Model always identifies the selected model and its effective, secret-free
	// configuration. Backend may be nil when a persisted selection remains
	// inspectable but cannot be executed by the current application.
	Model   modelapi.Info
	Backend modelapi.Backend

	Journal       Journal
	Tools         []Tool
	Instructions  string
	SessionID     string
	Workspace     string
	RequestPolicy ModelRequestPolicy
	// MaxToolIterations bounds completed model-to-tool cycles in one run. Zero
	// selects DefaultMaxToolIterations; -1 disables the fuse.
	MaxToolIterations int
	UserShell         ShellFunc
	ExternalWork      ExternalWork
	Sanitize          func(string) string
	// Metadata supplies resolved product facts which agent.Runtime cannot
	// infer from backend and tool interfaces. It is recorded but not interpreted.
	Metadata ConfigurationMetadata
}

type ConfigurationMetadata struct {
	ToolSet           string
	Build             BuildSnapshot
	Scope             ScopeSnapshot
	AwaitRequiredJobs bool
	ProgramTools      []ProgramToolSnapshot
}

type Runtime struct {
	// runMu excludes concurrent turns and shell/maintenance operations. While
	// a turn owns it, model/tool changes are queued until a request boundary.
	runMu sync.Mutex
	// statusMu protects only the last fully calculated session status. Status
	// calculation happens before taking this lock so readers never wait on a
	// journal replay or model projection.
	statusMu       sync.RWMutex
	sessionStatus  SessionStatus
	statusSequence uint64
	// queueMu owns pendingInputs independently of a running turn.
	queueMu       sync.Mutex
	pendingInputs []string
	// configMu synchronizes observer reads with configuration changes.
	// Scope changes deliberately take it without runMu so subsequently started
	// work in an active turn can observe the new boundary.
	configMu      sync.RWMutex
	turnActive    bool
	pendingConfig *runtimeConfiguration
	runtimeConfiguration

	journal           Journal
	instructions      string
	sessionID         string
	workspace         string
	requestPolicy     ModelRequestPolicy
	maxToolIterations int
	userShell         ShellFunc
	externalWork      ExternalWork
	sanitize          func(string) string
	build             BuildSnapshot
	scope             ScopeSnapshot
	awaitRequiredJobs bool
}

// New constructs an inert Runtime: it starts no background work and does not
// take ownership of Journal or ExternalWork. An unused Runtime may be discarded
// without cleanup.
func New(config Config) (*Runtime, error) {
	modelInfo, err := modelapi.NormalizeInfo(config.Model)
	if err != nil {
		return nil, err
	}
	if config.Journal == nil {
		return nil, errors.New("journal is required")
	}
	requestPolicy := config.RequestPolicy
	if requestPolicy.MaxAttempts == 0 {
		requestPolicy.MaxAttempts = 2
	}
	if requestPolicy.MaxAttempts < -1 {
		return nil, errors.New("max model attempts must be positive or -1 for budget-bounded retries")
	}
	if requestPolicy.RetryBudget < 0 || requestPolicy.BaseDelay < 0 || requestPolicy.MaxDelay < 0 || requestPolicy.StreamIdleTimeout < 0 {
		return nil, errors.New("model request durations cannot be negative")
	}
	if requestPolicy.MaxAttempts == -1 && requestPolicy.RetryBudget <= 0 {
		return nil, errors.New("unlimited model attempts require a positive retry budget")
	}
	if requestPolicy.MaxAttempts == -1 && requestPolicy.BaseDelay <= 0 {
		return nil, errors.New("unlimited model attempts require a positive retry base delay")
	}
	iterations := config.MaxToolIterations
	if iterations == 0 {
		iterations = DefaultMaxToolIterations
	}
	if iterations < -1 {
		return nil, errors.New("max tool iterations must be positive or -1 for unlimited")
	}

	tools, toolByName, err := normalizeTools(config.Tools)
	if err != nil {
		return nil, err
	}
	instructions := strings.TrimSpace(config.Instructions)
	// Redact original bytes before repairing UTF-8 so secrets still match.
	redact := config.Sanitize
	sanitize := func(text string) string {
		if redact != nil {
			text = redact(text)
		}
		return strings.ToValidUTF8(text, "�")
	}
	instructions = sanitize(instructions)
	programTools, err := normalizeProgramToolSnapshots(config.Metadata.ProgramTools, sanitize)
	if err != nil {
		return nil, err
	}
	build := config.Metadata.Build
	build.Version = sanitize(strings.TrimSpace(build.Version))
	build.Revision = sanitize(strings.TrimSpace(build.Revision))
	if build.Modified != nil {
		modified := *build.Modified
		build.Modified = &modified
	}

	runtime := &Runtime{
		runtimeConfiguration: runtimeConfiguration{
			backend:      config.Backend,
			modelInfo:    modelInfo,
			tools:        tools,
			toolByName:   toolByName,
			toolSet:      strings.TrimSpace(config.Metadata.ToolSet),
			programTools: programTools,
		},
		journal:           config.Journal,
		instructions:      instructions,
		sessionID:         strings.TrimSpace(config.SessionID),
		workspace:         strings.TrimSpace(config.Workspace),
		requestPolicy:     requestPolicy,
		maxToolIterations: iterations,
		userShell:         config.UserShell,
		externalWork:      config.ExternalWork,
		sanitize:          sanitize,
		build:             build,
		scope:             sanitizeScopeSnapshot(config.Metadata.Scope, sanitize),
		awaitRequiredJobs: config.Metadata.AwaitRequiredJobs,
	}
	runtime.sessionStatus = runtime.calculateSessionStatus(State{})
	return runtime, nil
}

func normalizeProgramToolSnapshots(input []ProgramToolSnapshot, sanitize func(string) string) ([]ProgramToolSnapshot, error) {
	result := make([]ProgramToolSnapshot, len(input))
	seen := make(map[string]struct{}, len(input))
	for index, snapshot := range input {
		snapshot.Name = strings.TrimSpace(snapshot.Name)
		if snapshot.Name == "" {
			return nil, errors.New("program tool snapshot name is required")
		}
		if _, exists := seen[snapshot.Name]; exists {
			return nil, fmt.Errorf("duplicate program tool snapshot %q", snapshot.Name)
		}
		seen[snapshot.Name] = struct{}{}
		snapshot.Program = sanitize(strings.TrimSpace(snapshot.Program))
		snapshot.Command = append([]string(nil), snapshot.Command...)
		for argument := range snapshot.Command {
			snapshot.Command[argument] = sanitize(snapshot.Command[argument])
		}
		snapshot.Workdir = sanitize(strings.TrimSpace(snapshot.Workdir))
		snapshot.EnvironmentNames = append([]string(nil), snapshot.EnvironmentNames...)
		if err := validateProgramToolSnapshot(snapshot); err != nil {
			return nil, err
		}
		result[index] = snapshot
	}
	return result, nil
}

func normalizeTools(input []Tool) ([]Tool, map[string]Tool, error) {
	tools := make([]Tool, len(input))
	copy(tools, input)
	specs := make([]modelapi.ToolSpec, len(tools))
	for index := range tools {
		specs[index] = tools[index].Spec
	}
	normalizedSpecs, err := modelapi.NormalizeToolSpecs(specs)
	if err != nil {
		return nil, nil, err
	}
	toolByName := make(map[string]Tool, len(tools))
	for index, tool := range tools {
		name := normalizedSpecs[index].Name
		if tool.Run == nil {
			return nil, nil, fmt.Errorf("tool %q has no runner", name)
		}
		tool.Spec = normalizedSpecs[index]
		tools[index] = tool
		toolByName[name] = tool
	}
	return tools, toolByName, nil
}

// NormalizeTools validates a complete tool catalog and returns an owned,
// canonical copy. The returned slice and mutable ToolSpec fields do not alias
// input; Run functions intentionally retain their original captured state.
func NormalizeTools(input []Tool) ([]Tool, error) {
	tools, _, err := normalizeTools(input)
	return tools, err
}

func (runtime *Runtime) CurrentModel() string {
	return modelURI(runtime.CurrentModelInfo())
}

// CurrentSessionID returns the configured journal identity or the identity
// established when the first run initializes or resumes a journal.
func (runtime *Runtime) CurrentSessionID() string {
	runtime.configMu.RLock()
	defer runtime.configMu.RUnlock()
	return runtime.sessionID
}

func (runtime *Runtime) CurrentReasoningEffort() string {
	return runtime.CurrentModelInfo().ReasoningEffort
}

// CurrentModelInfo returns the selected, secret-free model configuration. A
// selection made during a turn takes effect at the next request boundary.
func (runtime *Runtime) CurrentModelInfo() modelapi.Info {
	runtime.configMu.RLock()
	defer runtime.configMu.RUnlock()
	return runtime.selectedConfigurationLocked().modelInfo
}

// SwitchModel applies a selection immediately when idle, or before the next
// model request when a turn is active. The current response and its tools finish
// with the configuration under which they were requested.
func (runtime *Runtime) SwitchModel(ctx context.Context, modelInfo modelapi.Info, backend modelapi.Backend) error {
	if backend == nil {
		return errors.New("model backend is required")
	}
	modelInfo, err := modelapi.NormalizeInfo(modelInfo)
	if err != nil {
		return err
	}
	return runtime.reconfigure(ctx, func(configuration *runtimeConfiguration) {
		configuration.modelInfo = modelInfo
		configuration.backend = backend
	})
}

func modelURI(modelInfo modelapi.Info) string {
	return modelInfo.Provider + "/" + modelInfo.Model
}

// SetTools replaces the model-visible tool set at the next request boundary,
// or immediately when idle.
// Tool set names and exact membership deliberately live in the application
// assembling the runtime.
func (runtime *Runtime) SetTools(ctx context.Context, input []Tool, toolSet string) error {
	return runtime.setTools(ctx, input, toolSet, nil, false)
}

// SetToolsWithProgramTools atomically replaces the model-visible tools and
// the resolved executable metadata recorded with them. Applications which
// bind executable-backed tools lazily use this so a failed reconfiguration
// cannot publish a tool without its matching journal snapshot (or vice versa).
func (runtime *Runtime) SetToolsWithProgramTools(ctx context.Context, input []Tool, toolSet string, inputPrograms []ProgramToolSnapshot) error {
	return runtime.setTools(ctx, input, toolSet, inputPrograms, true)
}

func (runtime *Runtime) setTools(ctx context.Context, input []Tool, toolSet string, inputPrograms []ProgramToolSnapshot, replacePrograms bool) error {
	tools, toolByName, err := normalizeTools(input)
	if err != nil {
		return err
	}
	var programTools []ProgramToolSnapshot
	if replacePrograms {
		programTools, err = normalizeProgramToolSnapshots(inputPrograms, runtime.sanitize)
		if err != nil {
			return err
		}
	}
	toolSet = runtime.sanitize(strings.TrimSpace(toolSet))
	return runtime.reconfigure(ctx, func(configuration *runtimeConfiguration) {
		configuration.tools = tools
		configuration.toolByName = toolByName
		configuration.toolSet = toolSet
		if replacePrograms {
			configuration.programTools = programTools
		}
	})
}

func (runtime *Runtime) ToolStatus(id string) ([]modelapi.Detail, bool) {
	if runtime.externalWork == nil {
		return nil, false
	}
	details, ok := runtime.externalWork.Status(strings.TrimSpace(id))
	if !ok {
		return nil, false
	}
	normalized, err := runtime.sanitizeOutputDetails(details)
	if err != nil {
		return nil, false
	}
	return normalized, true
}

func (runtime *Runtime) Run(ctx context.Context, input string, emit EmitFunc) (result RunResult, runErr error) {
	runtime.configMu.Lock()
	if !runtime.runMu.TryLock() {
		runtime.configMu.Unlock()
		return RunResult{}, ErrRunActive
	}
	runtime.turnActive = true
	runtime.configMu.Unlock()
	var live *stateReducer
	defer func() {
		runtime.configMu.Lock()
		defer runtime.configMu.Unlock()
		defer runtime.runMu.Unlock()
		runtime.turnActive = false
		// A final answer or cancellation may leave no next request in this run.
		// Still apply the user's selection before another operation can start.
		if live != nil && !live.state.hasUnfinishedWork() {
			if err := runtime.applyPendingConfigurationLocked(context.WithoutCancel(ctx), live); err != nil {
				runErr = errors.Join(runErr, err)
			}
		}
	}()

	input, err := normalizeInput(input)
	if err != nil {
		return RunResult{}, err
	}
	if err := runtime.requireBackend(); err != nil {
		return RunResult{}, err
	}
	input = runtime.sanitize(input)
	records, err := runtime.journal.Records(ctx)
	if err != nil {
		return RunResult{}, fmt.Errorf("read journal: %w", err)
	}
	live, err = reduceRecords(records)
	if err != nil {
		return RunResult{}, err
	}
	runtime.publishSessionStatus(live.state)
	if live.state.hasUnfinishedWork() {
		return RunResult{}, unfinishedWorkError("starting another run")
	}
	if err := runtime.prepareSession(ctx, live); err != nil {
		return RunResult{}, err
	}
	_, err = runtime.prepareRunRequestContext(ctx, live, runRequestSpec{extraUserText: input}, emit)
	if err != nil {
		return RunResult{}, err
	}
	runtime.syncExternalWorkCommits(live.state)

	runID, err := newID("run")
	if err != nil {
		return RunResult{}, err
	}
	if _, err := appendRecordAndApply(ctx, runtime.journal, live, RecordRunStarted, RunStartedRecord{RunID: runID}); err != nil {
		return RunResult{}, err
	}
	inputRecord, err := appendRecordAndApply(ctx, runtime.journal, live, RecordRunInputAdded, RunInputAddedRecord{RunID: runID, Text: input})
	if err != nil {
		return RunResult{}, err
	}
	// RunStarted reports that both the run and its initial input are committed, so
	// its watermark is the later of those two records.
	emitEvent(emit, Event{Sequence: inputRecord.Sequence, Kind: EventRunStarted, RunID: runID})

	toolIterations := 0
	for {
		// Keep one configuration through context preparation, request retries,
		// and every tool call accepted from the response.
		if err := runtime.applyPendingConfiguration(ctx, live); err != nil {
			return runtime.finishError(ctx, live, emit, runID, err)
		}
		delivered, deliveryErr := runtime.deliverQueuedInput(ctx, live, runID)
		for _, queuedInput := range delivered {
			emitEvent(emit, Event{Sequence: queuedInput.Sequence, Kind: EventQueuedInputDelivered, RunID: runID, Text: queuedInput.Text})
		}
		if deliveryErr != nil {
			return runtime.finishError(ctx, live, emit, runID, deliveryErr)
		}
		boundary, boundaryErr := runtime.deliverBoundaryEvents(ctx, live, runID, live.state.SessionID)
		for _, event := range boundary {
			emitEvent(emit, Event{Sequence: event.Sequence, Kind: EventBoundaryDelivered, RunID: runID, Text: event.Content, Details: cloneDetails(event.Details)})
		}
		if boundaryErr != nil {
			return runtime.finishError(ctx, live, emit, runID, boundaryErr)
		}
		requestSpec := runRequestSpec{}
		report, contextErr := runtime.prepareRunRequestContext(ctx, live, requestSpec, emit)
		if contextErr != nil {
			return runtime.finishError(ctx, live, emit, runID, contextErr)
		}
		runtime.publishSessionStatus(live.state)
		response, callErr := runtime.completeRunRequest(ctx, runID, live, requestSpec, report, emit)
		if callErr != nil {
			return runtime.finishError(ctx, live, emit, runID, callErr)
		}
		if ctx.Err() != nil {
			return runtime.finish(ctx, live, emit, runID, "", RunCancelled, ctx.Err())
		}
		committed, err := runtime.acceptAndCommitResponse(ctx, live, runID, response, responseCommitOptions{})
		if err != nil {
			if _, ok := errors.AsType[*responseAcceptanceError](err); ok {
				return runtime.finish(ctx, live, emit, runID, "", RunFailed, err)
			}
			return RunResult{RunID: runID}, err
		}
		runtime.publishSessionStatus(live.state)
		if committed.incomplete {
			return runtime.finish(ctx, live, emit, runID, committed.answer, RunIncomplete, RunIncompleteError{StopReason: committed.response.StopReason})
		}

		calls := responseToolCalls(committed.response.Items)
		if len(calls) == 0 {
			if runtime.externalWork != nil {
				continueRun, waitErr := runtime.externalWork.Await(ctx, live.state.SessionID)
				if waitErr != nil {
					return runtime.finishError(ctx, live, emit, runID, waitErr)
				}
				if continueRun {
					continue
				}
			}
			return runtime.finish(ctx, live, emit, runID, committed.answer, RunCompleted, nil)
		}
		if runtime.maxToolIterations >= 0 && toolIterations >= runtime.maxToolIterations {
			return runtime.finalizeToolLimit(ctx, live, emit, runID, calls, toolIterations)
		}
		toolIterations++
		cancelled, toolErr := runtime.executeToolCalls(ctx, live, emit, runID, calls)
		if cancelled {
			if err := runtime.settleCancelledToolCalls(ctx, live, emit, runID); err != nil {
				return RunResult{RunID: runID}, err
			}
			return runtime.finish(ctx, live, emit, runID, "", RunCancelled, ctx.Err())
		}
		if toolErr != nil {
			return RunResult{RunID: runID}, toolErr
		}
	}
}

// settleCancelledToolCalls preserves the journal invariant that every accepted
// tool call has a result before its run finishes. A cancelled tool may have
// performed an external side effect before observing the context, so its
// outcome is deliberately recorded as unknown rather than retried or guessed.
func (runtime *Runtime) settleCancelledToolCalls(ctx context.Context, live *stateReducer, emit EmitFunc, runID string) error {
	journalCtx := context.WithoutCancel(ctx)
	for {
		var pending *PendingTool
		for index := range live.state.PendingTools {
			candidate := live.state.PendingTools[index]
			if candidate.RunID == runID {
				pending = &candidate
				break
			}
		}
		if pending == nil {
			return nil
		}
		result := modelapi.ToolResult{
			CallID:  pending.Call.ID,
			Content: modelapi.TextContent(fmt.Sprintf("tool %s outcome is unknown because its run was cancelled; the call was not replayed", pending.Call.Name)),
			Error:   true,
			Unknown: true,
		}
		if err := runtime.commitToolResult(journalCtx, live, emit, runID, pending.Call, result); err != nil {
			return err
		}
	}
}

func (runtime *Runtime) finalizeToolLimit(ctx context.Context, live *stateReducer, emit EmitFunc, runID string, calls []modelapi.ToolCall, iterations int) (RunResult, error) {
	for _, call := range calls {
		result := modelapi.ToolResult{
			CallID:  call.ID,
			Content: modelapi.TextContent(runtime.sanitize(fmt.Sprintf("tool %s error: tool iteration limit reached after %d iterations", call.Name, iterations))),
			Error:   true,
		}
		if err := runtime.commitRejectedToolResult(context.WithoutCancel(ctx), live, emit, runID, call, result); err != nil {
			return RunResult{RunID: runID, ToolLimitReached: true}, err
		}
	}
	if ctx.Err() != nil {
		return runtime.finishToolLimited(ctx, live, emit, runID, "", RunCancelled, ctx.Err())
	}
	if err := runtime.applyPendingConfiguration(ctx, live); err != nil {
		status, err := runFailure(ctx, err)
		return runtime.finishToolLimited(ctx, live, emit, runID, "", status, err)
	}

	requestSpec := runRequestSpec{
		omitTools:     true,
		extraUserText: live.state.Configured.ModelContext.ToolLimitInstructions,
	}
	report, contextErr := runtime.prepareRunRequestContext(ctx, live, requestSpec, emit)
	if contextErr != nil {
		status, contextErr := runFailure(ctx, contextErr)
		return runtime.finishToolLimited(ctx, live, emit, runID, "", status, contextErr)
	}
	runtime.publishSessionStatus(live.state)
	response, callErr := runtime.completeRunRequest(ctx, runID, live, requestSpec, report, emit)
	if callErr != nil {
		status, callErr := runFailure(ctx, callErr)
		return runtime.finishToolLimited(ctx, live, emit, runID, "", status, callErr)
	}
	if ctx.Err() != nil {
		return runtime.finishToolLimited(ctx, live, emit, runID, "", RunCancelled, ctx.Err())
	}

	committed, err := runtime.acceptAndCommitResponse(ctx, live, runID, response, responseCommitOptions{
		stripToolCalls: true,
		errorContext:   "finalize after tool iteration limit",
	})
	if err != nil {
		if _, ok := errors.AsType[*responseAcceptanceError](err); ok {
			return runtime.finishToolLimited(ctx, live, emit, runID, "", RunFailed, err)
		}
		return RunResult{RunID: runID, ToolLimitReached: true}, err
	}
	if committed.incomplete {
		return runtime.finishToolLimited(ctx, live, emit, runID, committed.answer, RunIncomplete, RunIncompleteError{StopReason: committed.response.StopReason})
	}
	if committed.answer == "" {
		return runtime.finishToolLimited(ctx, live, emit, runID, "", RunFailed, errors.New("tool loop limit reached: final model response contained no answer"))
	}
	return runtime.finishToolLimited(ctx, live, emit, runID, committed.answer, RunCompleted, nil)
}

type responseCommitOptions struct {
	stripToolCalls bool
	errorContext   string
}

type attemptResponse struct {
	modelapi.Response
	attemptID string
}

type committedModelResponse struct {
	response   modelapi.Response
	answer     string
	incomplete bool
}

type responseAcceptanceError struct {
	err error
}

func (err *responseAcceptanceError) Error() string { return err.err.Error() }
func (err *responseAcceptanceError) Unwrap() error { return err.err }

func (runtime *Runtime) acceptAndCommitResponse(ctx context.Context, live *stateReducer, runID string, response attemptResponse, options responseCommitOptions) (committedModelResponse, error) {
	incomplete := modelapi.IsIncompleteStopReason(response.StopReason)
	if incomplete || options.stripToolCalls {
		response.Items = partialResponseItems(response.Items)
	}
	accepted, err := live.state.Selection.AcceptResponse(response.Response)
	if err != nil {
		if options.errorContext != "" {
			err = fmt.Errorf("%s: %w", options.errorContext, err)
		}
		return committedModelResponse{}, &responseAcceptanceError{err: err}
	}
	if _, err := appendRecordAndApply(ctx, runtime.journal, live, RecordModelResponse, ModelResponseRecord{
		AttemptID:  response.attemptID,
		RunID:      runID,
		Backend:    live.state.Selection.Backend,
		Model:      live.state.Selection.Model,
		Epoch:      live.state.Selection.Epoch,
		Items:      accepted.Items,
		Usage:      accepted.Usage.Tokens.Known(),
		StopReason: accepted.StopReason,
	}); err != nil {
		return committedModelResponse{}, err
	}
	return committedModelResponse{
		response: accepted, answer: responseText(accepted.Items), incomplete: incomplete,
	}, nil
}

func partialResponseItems(items []modelapi.Item) []modelapi.Item {
	partial := make([]modelapi.Item, 0, len(items))
	for _, item := range items {
		if item.Kind == modelapi.ItemAssistantText || item.Kind == modelapi.ItemReasoning {
			partial = append(partial, item.Clone())
		}
	}
	return partial
}

type deliveredBoundaryEvent struct {
	BoundaryEvent
	Sequence uint64
}

func (runtime *Runtime) deliverBoundaryEvents(ctx context.Context, reducer *stateReducer, runID, sessionID string) ([]deliveredBoundaryEvent, error) {
	if runtime.externalWork == nil {
		return nil, nil
	}
	events := runtime.externalWork.PendingEvents(sessionID)
	delivered := make([]deliveredBoundaryEvent, 0, len(events))
	seen := make(map[string]struct{}, len(events))
	for _, event := range events {
		event.JobID = strings.TrimSpace(event.JobID)
		event.Content = strings.TrimSpace(runtime.sanitize(event.Content))
		if event.JobID == "" || event.Content == "" {
			return delivered, errors.New("boundary event requires job ID and content")
		}
		if _, exists := seen[event.JobID]; exists {
			return delivered, fmt.Errorf("duplicate boundary event for job %q", event.JobID)
		}
		seen[event.JobID] = struct{}{}
		if _, committed := reducer.state.DeliveredJobs[event.JobID]; committed {
			runtime.externalWork.EventCommitted(event.JobID)
			continue
		}
		details, err := runtime.sanitizeOutputDetails(event.Details)
		if err != nil {
			return delivered, fmt.Errorf("invalid boundary event details: %w", err)
		}
		event.Details = details
		record, err := appendRecordAndApply(ctx, runtime.journal, reducer, RecordBoundaryEvent, BoundaryEventRecord{
			RunID:      runID,
			JobID:      event.JobID,
			FinishedAt: event.FinishedAt,
			Content:    event.Content,
			Details:    event.Details,
		})
		if err != nil {
			return delivered, err
		}
		delivered = append(delivered, deliveredBoundaryEvent{BoundaryEvent: event, Sequence: record.Sequence})
		runtime.externalWork.EventCommitted(event.JobID)
	}
	return delivered, nil
}

// syncExternalWorkCommits closes the crash window between a journal append and
// its source acknowledgement. Durable sources may offer an event again after a
// restart; the journal fold, not a mailbox marker, remains the authority.
func (runtime *Runtime) syncExternalWorkCommits(state State) {
	if runtime.externalWork == nil {
		return
	}
	for jobID := range state.DeliveredJobs {
		runtime.externalWork.EventCommitted(jobID)
	}
	for _, item := range state.Items {
		if item.ToolResult != nil {
			runtime.externalWork.ToolResultCommitted(*item.ToolResult.Clone())
		}
	}
}

func (runtime *Runtime) completeRunRequest(
	ctx context.Context,
	runID string,
	live *stateReducer,
	spec runRequestSpec,
	report ContextReport,
	emit EmitFunc,
) (attemptResponse, error) {
	for {
		request, err := runtime.modelRequestForRun(live.state, spec)
		if err != nil {
			return attemptResponse{}, err
		}
		probeImages := live.state.ImageDelivery.Status == ImageDeliveryUnknown && modelRequestHasImages(request)
		response, err := runtime.completeRequest(ctx, runID, request, emit)
		if err == nil {
			if probeImages {
				if observeErr := runtime.observeImageDelivery(ctx, live, ImageDeliveryAccepted); observeErr != nil {
					return attemptResponse{}, observeErr
				}
			}
			return response, nil
		}
		if ctx.Err() == nil && errors.Is(err, modelapi.ErrModelRequestTooLarge) {
			var shrinkErr error
			report, shrinkErr = runtime.shrinkRunRequestOnce(ctx, live, spec, report, emit)
			if shrinkErr != nil {
				return attemptResponse{}, errors.Join(
					err,
					fmt.Errorf("automatic context reduction after oversized model request failed: %w", shrinkErr),
				)
			}
			continue
		}
		if probeImages && runtime.imageFreeControlAllowed(err, report) && ctx.Err() == nil {
			emitEvent(emit, Event{Kind: EventStatus, RunID: runID, Text: "retrying without image content"})
			fallback := requestWithoutImages(request)
			fallbackResponse, fallbackErr := runtime.completeRequest(ctx, runID, fallback, emit)
			if fallbackErr == nil {
				if observeErr := runtime.observeImageDelivery(ctx, live, ImageDeliveryRejected); observeErr != nil {
					return attemptResponse{}, observeErr
				}
				return fallbackResponse, nil
			}
			// The control changed only image delivery. If it also failed, it did
			// not explain the original request rejection; preserve that error and
			// leave the epoch unknown.
			return attemptResponse{}, err
		}
		return response, err
	}
}

func modelRequestHasImages(request modelapi.Request) bool {
	for _, item := range request.Items {
		if item.ToolResult != nil && item.ToolResult.Content.HasImage() {
			return true
		}
	}
	return false
}

func (runtime *Runtime) imageFreeControlAllowed(err error, report ContextReport) bool {
	providerErr, ok := errors.AsType[*modelapi.ProviderError](err)
	if !ok || providerErr.Kind != modelapi.ProviderErrorRequest || providerErr.Retryable {
		return false
	}
	// A successful smaller control is not evidence about image delivery when
	// the original request may simply have exceeded an uncertain context
	// window. The ordinary input limit already holds back a context reserve.
	return !runtime.modelInfo.ContextWindowEstimated && report.Window > 0 && report.InputLimit > 0 && report.TotalInputTokens <= report.InputLimit
}

func requestWithoutImages(request modelapi.Request) modelapi.Request {
	request.Items = cloneModelItemsForProjection(request.Items)
	request.Items = omitImagesFromModelItems(request.Items)
	return request
}

func (runtime *Runtime) observeImageDelivery(ctx context.Context, live *stateReducer, status ImageDeliveryStatus) error {
	if live.state.ImageDelivery.Status == status {
		return nil
	}
	if live.state.ImageDelivery.Status != ImageDeliveryUnknown {
		return fmt.Errorf("image delivery already observed as %q for provider epoch %q", live.state.ImageDelivery.Status, live.state.Selection.Epoch)
	}
	payload := ImageDeliveryObservedRecord{ProviderEpoch: live.state.Selection.Epoch, Status: status}
	_, err := appendRecordAndApply(context.WithoutCancel(ctx), runtime.journal, live, RecordImageDeliveryObserved, payload)
	if err != nil {
		return fmt.Errorf("record image delivery observation: %w", err)
	}
	return nil
}

func (runtime *Runtime) completeRequest(ctx context.Context, runID string, request modelapi.Request, emit EmitFunc) (attemptResponse, error) {
	startedAt := time.Now()
	requestCtx := ctx
	cancel := func() {}
	if runtime.requestPolicy.RetryBudget > 0 {
		requestCtx, cancel = context.WithDeadline(ctx, startedAt.Add(runtime.requestPolicy.RetryBudget))
	}
	defer cancel()
	request.StreamIdleTimeout = runtime.requestPolicy.StreamIdleTimeout
	var lastErr error
	requestID, err := newID("request")
	if err != nil {
		return attemptResponse{}, err
	}
	for attempt := 1; runtime.attemptAllowed(attempt); attempt++ {
		if err := requestCtx.Err(); err != nil {
			if ctx.Err() == nil {
				err = modelapi.MarkProviderFailure(fmt.Errorf("%w after %s", ErrModelRequestBudget, runtime.requestPolicy.RetryBudget))
			}
			return attemptResponse{}, err
		}
		attemptID, err := newID("attempt")
		if err != nil {
			return attemptResponse{}, err
		}
		payload := runtime.modelAttemptRecord(requestID, attemptID, runID, request.ProviderEpoch, attempt)
		if _, err := appendRecord(requestCtx, runtime.journal, RecordModelAttemptStarted, payload); err != nil {
			return attemptResponse{}, err
		}
		emitEvent(emit, Event{Kind: EventModelAttemptStarted, RunID: runID, AttemptID: attemptID})
		var response modelapi.Response
		if err = requestCtx.Err(); err == nil {
			response, err = runtime.backend.Complete(requestCtx, request, func(event modelapi.StreamEvent) {
				switch event.Kind {
				case modelapi.EventTextDelta, modelapi.EventReasoningSummaryDelta:
					emitEvent(emit, Event{Kind: EventKind(event.Kind), RunID: runID, AttemptID: attemptID, Text: runtime.sanitize(event.Text)})
				}
			})
		}
		if errors.Is(requestCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			err = modelapi.MarkProviderFailure(fmt.Errorf("%w after %s", ErrModelRequestBudget, runtime.requestPolicy.RetryBudget))
		} else if ctx.Err() != nil {
			err = ctx.Err()
		}
		if response.Usage.Status == "" {
			response.Usage.Status = modelapi.UsageUnavailable
		}
		if journalErr := runtime.finishModelAttempt(context.WithoutCancel(ctx), payload, response.Usage, err); journalErr != nil {
			return attemptResponse{}, errors.Join(err, journalErr)
		}
		if err == nil {
			return attemptResponse{Response: runtime.sanitizeModelResponse(response), attemptID: attemptID}, nil
		}
		lastErr = sanitizeError(err, runtime.sanitize)
		emitEvent(emit, Event{Kind: EventModelAttemptDiscarded, RunID: runID, AttemptID: attemptID, Text: lastErr.Error()})
		if ctx.Err() != nil || errors.Is(err, modelapi.ErrInvalidRequest) || errors.Is(err, modelapi.ErrModelRequestTooLarge) ||
			errors.Is(err, ErrModelRequestBudget) || !runtime.retryable(err) || !runtime.attemptAllowed(attempt+1) {
			break
		}
		delay := runtime.retryDelay(err, attempt)
		if runtime.requestPolicy.RetryBudget > 0 && delay > max(time.Duration(0), time.Until(startedAt.Add(runtime.requestPolicy.RetryBudget))) {
			break
		}
		emitEvent(emit, Event{Kind: EventModelRetryScheduled, RunID: runID, AttemptID: attemptID, Text: "retrying in " + delay.Round(time.Millisecond).String()})
		if err := waitForModelRetry(requestCtx, delay); err != nil {
			if errors.Is(requestCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				lastErr = modelapi.MarkProviderFailure(fmt.Errorf("%w after %s", ErrModelRequestBudget, runtime.requestPolicy.RetryBudget))
			} else {
				lastErr = err
			}
			break
		}
	}
	return attemptResponse{}, lastErr
}

func (runtime *Runtime) modelAttemptRecord(requestID, attemptID, runID, providerEpoch string, attempt int) ModelAttemptRecord {
	purpose := ModelRequestRun
	if runID == "" {
		purpose = ModelRequestCompaction
	}
	backend, _ := boundedModelAttemptText(runtime.sanitize(runtime.modelInfo.BackendID), maxModelAttemptFieldBytes)
	provider, _ := boundedModelAttemptText(runtime.sanitize(runtime.modelInfo.Provider), maxModelAttemptFieldBytes)
	model, _ := boundedModelAttemptText(runtime.sanitize(runtime.modelInfo.Model), maxModelAttemptFieldBytes)
	return ModelAttemptRecord{
		RequestID: requestID, AttemptID: attemptID, RunID: runID, Purpose: purpose, Attempt: attempt,
		Backend: backend, Provider: provider, Model: model, ProviderEpoch: providerEpoch,
		Outcome: ModelAttemptStarted, Usage: modelapi.Usage{Status: modelapi.UsageUnavailable},
	}
}

func (runtime *Runtime) finishModelAttempt(ctx context.Context, payload ModelAttemptRecord, usage modelapi.Usage, cause error) error {
	payload.Usage = usage
	payload.Outcome = ModelAttemptCompleted
	kind := RecordModelAttemptFinished
	if cause != nil {
		kind, payload.Outcome = RecordModelAttemptFailed, ModelAttemptFailed
		if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
			payload.Outcome = ModelAttemptCancelled
		}
		payload.Error, payload.ErrorTruncated = boundedModelAttemptText(runtime.sanitize(cause.Error()), maxModelAttemptErrorBytes)
	}
	if providerErr, ok := errors.AsType[*modelapi.ProviderError](cause); ok {
		errorKind, _ := boundedModelAttemptText(runtime.sanitize(string(providerErr.Kind)), maxModelAttemptFieldBytes)
		code, _ := boundedModelAttemptText(runtime.sanitize(providerErr.Code), maxModelAttemptFieldBytes)
		errorType, _ := boundedModelAttemptText(runtime.sanitize(providerErr.Type), maxModelAttemptFieldBytes)
		payload.ProviderError = &ModelAttemptProviderError{
			StatusCode: providerErr.StatusCode, Kind: modelapi.ProviderErrorKind(errorKind),
			Code: code, Type: errorType, Retryable: providerErr.Retryable,
			RetryAfter: durationSnapshot(providerErr.RetryAfter),
		}
	}
	_, err := appendRecord(ctx, runtime.journal, kind, payload)
	return err
}

func boundedModelAttemptText(value string, limit int) (string, bool) {
	value = strings.ToValidUTF8(value, "�")
	if limit <= 0 || len(value) <= limit {
		return value, false
	}
	const marker = "\n[… truncated …]"
	cut := limit - len(marker)
	if cut <= 0 {
		return strings.Repeat(".", limit), true
	}
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut] + marker, true
}

func (runtime *Runtime) attemptAllowed(attempt int) bool {
	return runtime.requestPolicy.MaxAttempts < 0 || attempt <= runtime.requestPolicy.MaxAttempts
}

func (runtime *Runtime) retryable(err error) bool {
	if providerErr, ok := errors.AsType[*modelapi.ProviderError](err); ok {
		return providerErr.Retryable
	}
	// Unclassified backend errors use the generic retryable default.
	return !errors.Is(err, modelapi.ErrInvalidRequest)
}

func (runtime *Runtime) retryDelay(err error, attempt int) time.Duration {
	limit := time.Duration(1<<63 - 1)
	if runtime.requestPolicy.MaxDelay > 0 {
		limit = runtime.requestPolicy.MaxDelay
	}
	delay := min(runtime.requestPolicy.BaseDelay, limit)
	for step := 1; step < attempt && delay < limit; step++ {
		if delay > limit/2 {
			delay = limit
			break
		}
		delay *= 2
	}
	if jitter := delay / 4; jitter > 0 {
		extra := mathrand.N(jitter)
		if delay > limit-extra {
			delay = limit
		} else {
			delay += extra
		}
	}
	if providerErr, ok := errors.AsType[*modelapi.ProviderError](err); ok && providerErr.RetryAfter > delay {
		return providerErr.RetryAfter
	}
	return delay
}

func waitForModelRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (runtime *Runtime) modelRequestForRun(state State, spec runRequestSpec) (modelapi.Request, error) {
	if state.Configured == nil {
		return modelapi.Request{}, errors.New("session has no effective configuration")
	}
	summary := ""
	if state.Compaction != nil {
		summary = state.Compaction.Summary
	}
	items := state.verbatimModelItems()
	if spec.extraUserText != "" {
		items = append(items, modelapi.Item{Kind: modelapi.ItemUserText, Text: spec.extraUserText})
	}
	request := modelapi.Request{
		SessionID:     state.SessionID,
		ProviderEpoch: state.Selection.Epoch,
		Instructions:  state.Configured.ModelContext.Instructions,
		Summary:       summary,
		Items: runtime.projectModelItems(items, modelapi.ProviderContext{
			Backend: state.Selection.Backend,
			Epoch:   state.Selection.Epoch,
		}, state.ImageDelivery.Status),
	}
	if !spec.omitTools {
		request.Tools = cloneToolSpecs(state.Configured.ModelContext.Tools)
	}
	return request, nil
}

// projectModelItems applies runtime ownership filtering, the adapter's replay
// policy, and route-level image delivery. Requests and context estimates share
// this complete projection.
func (runtime *Runtime) projectModelItems(items []modelapi.Item, providerContext modelapi.ProviderContext, imageStatus ImageDeliveryStatus) []modelapi.Item {
	items = providerContext.ProjectItems(items, runtime.backend)
	return projectImagesForDelivery(items, runtime.effectiveImageDelivery(imageStatus))
}

// effectiveImageDelivery combines current-build route policy with durable
// evidence. A catalog hint affects projection but never impersonates an
// observation in the session journal.
func (runtime *Runtime) effectiveImageDelivery(observed ImageDeliveryStatus) ImageDeliveryStatus {
	if runtime.modelInfo.ImageInputUnsupported {
		return ImageDeliveryRejected
	}
	return observed
}

func (runtime *Runtime) requireBackend() error {
	runtime.configMu.RLock()
	defer runtime.configMu.RUnlock()
	if runtime.backend != nil {
		return nil
	}
	return modelapi.MarkInvalidRequest(modelUnavailableError{model: modelURI(runtime.modelInfo)})
}

func projectImagesForDelivery(items []modelapi.Item, status ImageDeliveryStatus) []modelapi.Item {
	if status != ImageDeliveryRejected {
		return items
	}
	return omitImagesFromModelItems(items)
}

func omitImagesFromModelItems(items []modelapi.Item) []modelapi.Item {
	for index := range items {
		if items[index].ToolResult == nil || !items[index].ToolResult.Content.HasImage() {
			continue
		}
		items[index].ToolResult.Content = items[index].ToolResult.Content.WithoutImages(func(image modelapi.ImageContent) string {
			return fmt.Sprintf("\n[image omitted from model request; %s, %dx%d]\n", image.MediaType, image.Width, image.Height)
		})
	}
	return items
}

func (runtime *Runtime) prepareSession(ctx context.Context, reducer *stateReducer) error {
	state := &reducer.state
	if state.SessionID == "" {
		sessionID := runtime.sessionID
		if sessionID == "" {
			var err error
			sessionID, err = newID("session")
			if err != nil {
				return err
			}
		}
		_, err := appendRecordAndApply(ctx, runtime.journal, reducer, RecordSessionStarted, SessionStartedRecord{
			SchemaVersion: JournalSchemaVersion,
			SessionID:     sessionID,
			Workspace:     runtime.workspace,
		})
		if err != nil {
			return err
		}
	} else if runtime.sessionID != "" && state.SessionID != runtime.sessionID {
		return fmt.Errorf("journal session ID is %q, want %q", state.SessionID, runtime.sessionID)
	}
	if runtime.sessionID == "" {
		runtime.configMu.Lock()
		runtime.sessionID = state.SessionID
		runtime.configMu.Unlock()
	}
	if runtime.workspace != "" && state.Workspace != "" && state.Workspace != runtime.workspace {
		return fmt.Errorf("session workspace is %q, not %q", state.Workspace, runtime.workspace)
	}
	selection, err := runtime.replayContextForModel(state.Selection, runtime.modelInfo)
	if err != nil {
		return err
	}
	if selection.Epoch == state.Selection.Epoch {
		return runtime.recordCurrentEffectiveConfigurationAndApply(ctx, reducer)
	}
	_, err = appendRecordAndApply(ctx, runtime.journal, reducer, RecordModelSelected, selection)
	if err != nil {
		return err
	}
	return runtime.recordCurrentEffectiveConfigurationAndApply(ctx, reducer)
}

// Tool failures are results the model can act on. Only cancellation interrupts
// execution here; journal failures are handled when committing the result.
func (runtime *Runtime) executeTool(ctx context.Context, sessionID string, call modelapi.ToolCall) (modelapi.ToolResult, bool) {
	tool, exists := runtime.toolByName[call.Name]
	if !exists {
		names := make([]string, 0, len(runtime.tools))
		for _, available := range runtime.tools {
			names = append(names, available.Spec.Name)
		}
		sort.Strings(names)
		message := fmt.Sprintf("unknown tool %q", call.Name)
		if len(names) != 0 {
			message += "; available tools: " + strings.Join(names, ", ")
		}
		return modelapi.ToolResult{CallID: call.ID, Content: modelapi.TextContent(runtime.sanitize(message)), Error: true}, false
	}
	output, err := tool.Run(WithToolSessionID(ctx, sessionID), call.RawArguments)
	if ctx.Err() != nil {
		return modelapi.ToolResult{}, true
	}
	details, detailErr := runtime.sanitizeOutputDetails(output.Details)
	if detailErr != nil {
		err = errors.Join(err, fmt.Errorf("invalid tool output: %w", detailErr))
		details = nil
	}
	content, contentErr := modelapi.NormalizeContent(output.Content)
	if contentErr != nil {
		err = errors.Join(err, fmt.Errorf("invalid tool output: %w", contentErr))
		content = nil
	}
	if err != nil {
		message := strings.TrimSpace(content.Text())
		if message == "" {
			message = err.Error()
		} else {
			message += "\nerror: " + err.Error()
		}
		return modelapi.ToolResult{CallID: call.ID, Content: modelapi.TextContent(runtime.sanitize(message)), Details: details, Error: true}, false
	}
	return modelapi.ToolResult{CallID: call.ID, Content: runtime.sanitizeContent(content), Details: details}, false
}

func (runtime *Runtime) finish(ctx context.Context, reducer *stateReducer, emit EmitFunc, runID, answer string, status RunStatus, cause error) (RunResult, error) {
	return runtime.finishRun(ctx, reducer, emit, runID, answer, status, cause, false)
}

func (runtime *Runtime) finishError(ctx context.Context, reducer *stateReducer, emit EmitFunc, runID string, cause error) (RunResult, error) {
	status, cause := runFailure(ctx, cause)
	return runtime.finish(ctx, reducer, emit, runID, "", status, cause)
}

func runFailure(ctx context.Context, cause error) (RunStatus, error) {
	if err := ctx.Err(); err != nil {
		return RunCancelled, err
	}
	return RunFailed, cause
}

func (runtime *Runtime) finishToolLimited(ctx context.Context, reducer *stateReducer, emit EmitFunc, runID, answer string, status RunStatus, cause error) (RunResult, error) {
	return runtime.finishRun(ctx, reducer, emit, runID, answer, status, cause, true)
}

func (runtime *Runtime) finishRun(ctx context.Context, reducer *stateReducer, emit EmitFunc, runID, answer string, status RunStatus, cause error, toolLimitReached bool) (RunResult, error) {
	payload := RunFinishedRecord{RunID: runID, Status: status, ToolLimitReached: toolLimitReached}
	if runtime.externalWork != nil {
		payload.DetachedJobs = canonicalDetachedJobIDs(runtime.externalWork.DetachedJobs(reducer.state.SessionID), runtime.sanitize)
	}
	if cause != nil {
		cause = sanitizeError(cause, runtime.sanitize)
		payload.Error = cause.Error()
	}
	result := RunResult{
		RunID: runID, Answer: answer, Status: status, ToolLimitReached: toolLimitReached,
		DetachedJobs: append([]string(nil), payload.DetachedJobs...),
	}
	record, err := appendRecordAndApply(context.WithoutCancel(ctx), runtime.journal, reducer, RecordRunFinished, payload)
	if err != nil {
		return result, errors.Join(cause, err)
	}
	runtime.publishSessionStatus(reducer.state)
	emitEvent(emit, Event{Sequence: record.Sequence, Kind: EventRunFinished, RunID: runID, Text: answer, Status: status, ToolLimitReached: toolLimitReached, DetachedJobs: append([]string(nil), payload.DetachedJobs...)})
	return result, cause
}

func canonicalDetachedJobIDs(ids []string, sanitize func(string) string) []string {
	result := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(sanitize(id))
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

func (runtime *Runtime) sanitizeModelResponse(response modelapi.Response) modelapi.Response {
	response.Items = runtime.sanitizeItems(response.Items)
	return response
}

func (runtime *Runtime) sanitizeItems(items []modelapi.Item) []modelapi.Item {
	sanitized := cloneItems(items)
	for index := range sanitized {
		sanitized[index].Text = runtime.sanitize(sanitized[index].Text)
		sanitized[index].Details = runtime.sanitizeDetails(sanitized[index].Details)
		// ProviderData is signed/encrypted opaque state. Mutating bytes would
		// corrupt it, so cloning is the only sanitization operation applied.
		if sanitized[index].ToolCall != nil {
			sanitized[index].ToolCall.RawArguments = runtime.sanitize(sanitized[index].ToolCall.RawArguments)
		}
		if sanitized[index].ToolResult != nil {
			sanitized[index].ToolResult.Content = runtime.sanitizeContent(sanitized[index].ToolResult.Content)
			sanitized[index].ToolResult.Details = runtime.sanitizeDetails(sanitized[index].ToolResult.Details)
		}
	}
	return sanitized
}

func (runtime *Runtime) sanitizeContent(content modelapi.Content) modelapi.Content {
	sanitized := content.Clone()
	for index := range sanitized {
		if sanitized[index].Kind == modelapi.ContentPartText {
			sanitized[index].Text = runtime.sanitize(sanitized[index].Text)
		}
	}
	return sanitized
}

func (runtime *Runtime) sanitizeDetails(details []modelapi.Detail) []modelapi.Detail {
	sanitized := make([]modelapi.Detail, len(details))
	for index, detail := range details {
		sanitized[index] = modelapi.Detail{Kind: detail.Kind}
		var value any
		if err := json.Unmarshal(detail.Data, &value); err != nil {
			sanitized[index].Data = detail.Data.Clone()
			continue
		}
		data, err := json.Marshal(sanitizeJSONStrings(value, runtime.sanitize), json.Deterministic(true))
		if err != nil {
			sanitized[index].Data = detail.Data.Clone()
			continue
		}
		sanitized[index].Data = data
	}
	return sanitized
}

// sanitizeOutputDetails validates both sides of redaction. Replacing a short
// secret can expand otherwise valid JSON beyond the durable details limit.
func (runtime *Runtime) sanitizeOutputDetails(details []modelapi.Detail) ([]modelapi.Detail, error) {
	normalized, err := normalizeDetails(details)
	if err != nil {
		return nil, err
	}
	return normalizeDetails(runtime.sanitizeDetails(normalized))
}

func sanitizeJSONStrings(value any, sanitize func(string) string) any {
	switch value := value.(type) {
	case string:
		return sanitize(value)
	case []any:
		for index := range value {
			value[index] = sanitizeJSONStrings(value[index], sanitize)
		}
		return value
	case map[string]any:
		for key, child := range value {
			value[key] = sanitizeJSONStrings(child, sanitize)
		}
		return value
	default:
		return value
	}
}

type sanitizedError struct {
	text  string
	cause error
}

func (err sanitizedError) Error() string { return err.text }
func (err sanitizedError) Unwrap() error { return err.cause }

func sanitizeError(err error, sanitize func(string) string) error {
	if err == nil {
		return nil
	}
	safe := sanitize(err.Error())
	if safe == err.Error() {
		return err
	}
	return sanitizedError{text: safe, cause: err}
}

func normalizeAcceptedItem(item modelapi.Item) (modelapi.Item, error) {
	if len(item.Details) != 0 {
		return modelapi.Item{}, fmt.Errorf("%s item has product-owned details", item.Kind)
	}
	switch item.Kind {
	case modelapi.ItemAssistantText:
		if item.ResponseID == "" || len(item.ProviderData) != 0 || item.ToolCall != nil || item.ToolResult != nil {
			return modelapi.Item{}, fmt.Errorf("%s item has unrelated payload", item.Kind)
		}
		if item.ProviderContext != nil {
			return modelapi.Item{}, errors.New("assistant text item has provider context")
		}
	case modelapi.ItemReasoning:
		if item.ResponseID == "" || item.ProviderContext == nil || item.ProviderContext.Backend == "" || item.ProviderContext.Epoch == "" || item.ToolCall != nil || item.ToolResult != nil {
			return modelapi.Item{}, errors.New("reasoning item requires provider context")
		}
		if err := modelapi.ValidateProviderData(item.ProviderData); err != nil {
			return modelapi.Item{}, fmt.Errorf("reasoning item has invalid provider data: %w", err)
		}
	case modelapi.ItemToolCall:
		if item.ResponseID == "" || len(item.ProviderData) != 0 || item.ToolCall == nil || item.ToolCall.ID == "" || strings.TrimSpace(item.ToolCall.Name) == "" {
			return modelapi.Item{}, errors.New("accepted tool call requires ID and name")
		}
		arguments, err := modelapi.NormalizeToolArguments(item.ToolCall.RawArguments)
		if err != nil {
			return modelapi.Item{}, fmt.Errorf("accepted tool call has invalid arguments: %w", err)
		}
		call := item.ToolCall.Clone()
		call.Name = strings.TrimSpace(call.Name)
		call.RawArguments = arguments
		item.ToolCall = &call
	default:
		return modelapi.Item{}, fmt.Errorf("unsupported accepted item kind %q", item.Kind)
	}
	return item, nil
}

func responseToolCalls(items []modelapi.Item) []modelapi.ToolCall {
	var calls []modelapi.ToolCall
	for _, item := range items {
		if item.Kind == modelapi.ItemToolCall && item.ToolCall != nil {
			calls = append(calls, item.ToolCall.Clone())
		}
	}
	return calls
}

func responseText(items []modelapi.Item) string {
	var parts []string
	for _, item := range items {
		if item.Kind == modelapi.ItemAssistantText && strings.TrimSpace(item.Text) != "" {
			parts = append(parts, strings.TrimSpace(item.Text))
		}
	}
	return strings.Join(parts, "\n")
}

func newID(prefix string) (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("generate %s ID: %w", prefix, err)
	}
	return prefix + "_" + hex.EncodeToString(data[:]), nil
}

func emitEvent(emit EmitFunc, event Event) {
	if emit != nil {
		emit(event)
	}
}

func cloneItems(items []modelapi.Item) []modelapi.Item {
	out := make([]modelapi.Item, len(items))
	for index, item := range items {
		out[index] = item.Clone()
	}
	return out
}

func cloneItemForProjection(item modelapi.Item, includeDetails bool) modelapi.Item {
	if includeDetails {
		return item.Clone()
	}
	if item.ProviderContext != nil {
		context := *item.ProviderContext
		item.ProviderContext = &context
	}
	item.ProviderData = append([]modelapi.ProviderData(nil), item.ProviderData...)
	for i := range item.ProviderData {
		item.ProviderData[i].Data = item.ProviderData[i].Data.Clone()
	}
	item.ToolCall = cloneToolCallPointer(item.ToolCall)
	item.Details = nil
	if item.ToolResult != nil {
		result := *item.ToolResult
		result.Content = cloneContentForProjection(item.ToolResult.Content)
		result.Details = nil
		item.ToolResult = &result
	}
	return item
}

func cloneModelItemsForProjection(items []modelapi.Item) []modelapi.Item {
	cloned := make([]modelapi.Item, len(items))
	for index, item := range items {
		cloned[index] = cloneItemForProjection(item, false)
	}
	return cloned
}

func cloneToolCallPointer(call *modelapi.ToolCall) *modelapi.ToolCall {
	if call == nil {
		return nil
	}
	cloned := call.Clone()
	return &cloned
}
