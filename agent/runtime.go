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
	Model   ModelInfo
	Backend Backend

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
	// runMu excludes concurrent turns, shell/maintenance operations, and
	// model/tool configuration. Model/tool changes acquire it before configMu,
	// so a turn observes one coherent model/tool setup.
	runMu sync.Mutex
	// statusMu protects only the last fully calculated session status. Status
	// calculation happens before taking this lock so readers never wait on a
	// journal replay or model projection.
	statusMu       sync.RWMutex
	sessionStatus  SessionStatus
	statusSequence uint64
	// queueMu owns pendingInputs independently of a running turn.
	queueMu sync.Mutex
	// configMu synchronizes observer reads with configuration changes.
	// Scope changes deliberately take it without runMu so subsequently started
	// work in an active turn can observe the new boundary.
	configMu          sync.RWMutex
	pendingInputs     []string
	backend           Backend
	modelInfo         ModelInfo
	journal           Journal
	tools             []Tool
	toolByName        map[string]Tool
	instructions      string
	sessionID         string
	workspace         string
	requestPolicy     ModelRequestPolicy
	maxToolIterations int
	userShell         ShellFunc
	externalWork      ExternalWork
	sanitize          func(string) string
	toolSet           string
	build             BuildSnapshot
	scope             ScopeSnapshot
	awaitRequiredJobs bool
	programTools      []ProgramToolSnapshot
}

// New constructs an inert Runtime: it starts no background work and does not
// take ownership of Journal or ExternalWork. An unused Runtime may be discarded
// without cleanup.
func New(config Config) (*Runtime, error) {
	modelInfo, err := normalizeModelInfo(config.Model)
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
	sanitize := config.Sanitize
	if sanitize == nil {
		sanitize = func(text string) string { return text }
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
		backend:           config.Backend,
		modelInfo:         modelInfo,
		journal:           config.Journal,
		tools:             tools,
		toolByName:        toolByName,
		instructions:      instructions,
		sessionID:         strings.TrimSpace(config.SessionID),
		workspace:         strings.TrimSpace(config.Workspace),
		requestPolicy:     requestPolicy,
		maxToolIterations: iterations,
		userShell:         config.UserShell,
		externalWork:      config.ExternalWork,
		sanitize:          sanitize,
		toolSet:           strings.TrimSpace(config.Metadata.ToolSet),
		build:             build,
		scope:             sanitizeScopeSnapshot(config.Metadata.Scope, sanitize),
		awaitRequiredJobs: config.Metadata.AwaitRequiredJobs,
		programTools:      programTools,
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
	specs := make([]ToolSpec, len(tools))
	for index := range tools {
		specs[index] = tools[index].Spec
	}
	normalizedSpecs, err := NormalizeToolSpecs(specs)
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
	runtime.configMu.RLock()
	defer runtime.configMu.RUnlock()
	return modelURI(runtime.modelInfo)
}

// CurrentSessionID returns the configured journal identity or the identity
// established when the first run initializes or resumes a journal.
func (runtime *Runtime) CurrentSessionID() string {
	runtime.configMu.RLock()
	defer runtime.configMu.RUnlock()
	return runtime.sessionID
}

func (runtime *Runtime) CurrentReasoningEffort() string {
	runtime.configMu.RLock()
	defer runtime.configMu.RUnlock()
	return runtime.modelInfo.ReasoningEffort
}

// CurrentModelInfo returns the effective, secret-free model configuration.
func (runtime *Runtime) CurrentModelInfo() ModelInfo {
	runtime.configMu.RLock()
	defer runtime.configMu.RUnlock()
	return runtime.modelInfo
}

func (runtime *Runtime) SwitchModel(ctx context.Context, modelInfo ModelInfo, backend Backend) error {
	if backend == nil {
		return errors.New("model backend is required")
	}
	modelInfo, err := normalizeModelInfo(modelInfo)
	if err != nil {
		return err
	}
	if !runtime.runMu.TryLock() {
		return ErrRunActive
	}
	defer runtime.runMu.Unlock()
	runtime.configMu.Lock()
	defer runtime.configMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	records, err := runtime.journal.Records(ctx)
	if err != nil {
		return fmt.Errorf("read journal before model switch: %w", err)
	}
	live, err := reduceRecords(records)
	if err != nil {
		return err
	}
	runtime.publishSessionStatus(live.state)
	if live.state.hasUnfinishedWork() {
		return unfinishedWorkError("switching model")
	}
	if live.state.SessionID != "" && !selectionMatchesModel(live.state.Selection, modelInfo) {
		epoch, err := newID("epoch")
		if err != nil {
			return err
		}
		selection := ModelSelectedRecord{
			Backend:               modelInfo.BackendID,
			Provider:              modelInfo.Provider,
			Model:                 modelInfo.Model,
			ReasoningEffort:       modelInfo.ReasoningEffort,
			ProviderStateContract: modelInfo.ProviderStateContract,
			Epoch:                 epoch,
		}
		_, err = appendRecordAndApply(ctx, runtime.journal, live, RecordModelSelected, selection)
		if err != nil {
			return err
		}
	}
	snapshot := runtime.effectiveConfigSnapshotLocked(modelInfo, runtime.tools, runtime.toolSet, runtime.scope)
	if err := runtime.recordEffectiveConfigurationAndApply(ctx, live, snapshot); err != nil {
		return err
	}
	runtime.backend = backend
	runtime.modelInfo = modelInfo
	runtime.publishSessionStatus(live.state)
	return nil
}

func normalizeModelInfo(modelInfo ModelInfo) (ModelInfo, error) {
	modelInfo.BackendID = strings.TrimSpace(modelInfo.BackendID)
	modelInfo.Provider = strings.TrimSpace(modelInfo.Provider)
	modelInfo.Model = strings.TrimSpace(modelInfo.Model)
	modelInfo.ReasoningEffort = strings.ToLower(strings.TrimSpace(modelInfo.ReasoningEffort))
	modelInfo.ProviderStateContract = ProviderStateContract(strings.TrimSpace(string(modelInfo.ProviderStateContract)))
	modelInfo.Endpoint = strings.TrimSpace(modelInfo.Endpoint)
	if modelInfo.BackendID == "" || modelInfo.Provider == "" || modelInfo.Model == "" {
		return ModelInfo{}, errors.New("model backend ID, provider, and model name are required")
	}
	if modelInfo.ContextWindow < 0 {
		return ModelInfo{}, errors.New("model context window cannot be negative")
	}
	if modelInfo.MaxRequestBytes < 0 || modelInfo.MaxCompletionBytes < 0 {
		return ModelInfo{}, errors.New("model byte limits cannot be negative")
	}
	if modelInfo.ContextWindowEstimated && modelInfo.ContextWindow == 0 {
		return ModelInfo{}, errors.New("estimated model context window must be positive")
	}
	return modelInfo, nil
}

func modelURI(modelInfo ModelInfo) string {
	return modelInfo.Provider + "/" + modelInfo.Model
}

func selectionMatchesModel(selection ModelSelectedRecord, modelInfo ModelInfo) bool {
	return selection.Backend == modelInfo.BackendID &&
		selection.Provider == modelInfo.Provider &&
		selection.Model == modelInfo.Model &&
		selection.ReasoningEffort == modelInfo.ReasoningEffort &&
		selection.ProviderStateContract == modelInfo.ProviderStateContract
}

// SetTools atomically replaces the model-visible tool set between runs.
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
	if !runtime.runMu.TryLock() {
		return ErrRunActive
	}
	defer runtime.runMu.Unlock()
	runtime.configMu.Lock()
	defer runtime.configMu.Unlock()

	tools, toolByName, err := normalizeTools(input)
	if err != nil {
		return err
	}
	programTools := runtime.programTools
	if replacePrograms {
		programTools, err = normalizeProgramToolSnapshots(inputPrograms, runtime.sanitize)
		if err != nil {
			return err
		}
	}
	toolSet = runtime.sanitize(strings.TrimSpace(toolSet))
	snapshot := runtime.effectiveConfigSnapshotWithProgramToolsLocked(runtime.modelInfo, tools, toolSet, runtime.scope, programTools)
	if err := validateEffectiveConfigSnapshot(snapshot); err != nil {
		return err
	}
	records, err := runtime.journal.Records(ctx)
	if err != nil {
		return fmt.Errorf("read journal before tool reconfiguration: %w", err)
	}
	live, err := reduceRecords(records)
	if err != nil {
		return err
	}
	runtime.publishSessionStatus(live.state)
	if err := runtime.recordEffectiveConfigurationAndApply(ctx, live, snapshot); err != nil {
		return err
	}
	runtime.tools = tools
	runtime.toolByName = toolByName
	runtime.toolSet = toolSet
	if replacePrograms {
		runtime.programTools = programTools
	}
	runtime.publishSessionStatus(live.state)
	return nil
}

func (runtime *Runtime) ToolStatus(id string) ([]Detail, bool) {
	if runtime.externalWork == nil {
		return nil, false
	}
	details, ok := runtime.externalWork.Status(strings.TrimSpace(id))
	if !ok {
		return nil, false
	}
	normalized, err := runtime.sanitizeToolDetails(details)
	if err != nil {
		return nil, false
	}
	return normalized, true
}

func (runtime *Runtime) Run(ctx context.Context, input string, emit EmitFunc) (RunResult, error) {
	if !runtime.runMu.TryLock() {
		return RunResult{}, ErrRunActive
	}
	defer runtime.runMu.Unlock()

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
	live, err := reduceRecords(records)
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
		delivered, deliveryErr := runtime.deliverQueuedInput(ctx, live, runID)
		for _, queuedInput := range delivered {
			emitEvent(emit, Event{Sequence: queuedInput.Sequence, Kind: EventQueuedInputDelivered, RunID: runID, Text: queuedInput.Text})
		}
		if deliveryErr != nil {
			return runtime.finishError(ctx, live, emit, runID, deliveryErr)
		}
		boundary, boundaryErr := runtime.deliverBoundaryEvents(ctx, live, runID, live.state.SessionID)
		for _, event := range boundary {
			emitEvent(emit, Event{Sequence: event.Sequence, Kind: EventBoundaryDelivered, RunID: runID, Text: event.Content})
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
			if errors.Is(toolErr, ErrToolFatal) {
				return runtime.finish(ctx, live, emit, runID, "", RunFailed, toolErr)
			}
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
		result := ToolResult{
			CallID:  pending.Call.ID,
			Content: TextContent(fmt.Sprintf("tool %s outcome is unknown because its run was cancelled; the call was not replayed", pending.Call.Name)),
			Error:   true,
			Unknown: true,
		}
		if err := runtime.commitToolResult(journalCtx, live, emit, runID, pending.Call, result); err != nil {
			return err
		}
	}
}

func (runtime *Runtime) finalizeToolLimit(ctx context.Context, live *stateReducer, emit EmitFunc, runID string, calls []ToolCall, iterations int) (RunResult, error) {
	for _, call := range calls {
		result := ToolResult{
			CallID:  call.ID,
			Content: TextContent(runtime.sanitize(fmt.Sprintf("tool %s error: tool iteration limit reached after %d iterations", call.Name, iterations))),
			Error:   true,
		}
		if err := runtime.commitRejectedToolResult(context.WithoutCancel(ctx), live, emit, runID, call, result); err != nil {
			return RunResult{RunID: runID, ToolLimitReached: true}, err
		}
	}
	if ctx.Err() != nil {
		return runtime.finishToolLimited(ctx, live, emit, runID, "", RunCancelled, ctx.Err())
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

type committedModelResponse struct {
	response   ModelResponse
	answer     string
	incomplete bool
}

type responseAcceptanceError struct {
	err error
}

func (err *responseAcceptanceError) Error() string { return err.err.Error() }
func (err *responseAcceptanceError) Unwrap() error { return err.err }

func (runtime *Runtime) acceptAndCommitResponse(ctx context.Context, live *stateReducer, runID string, response ModelResponse, options responseCommitOptions) (committedModelResponse, error) {
	incomplete := IsIncompleteStopReason(response.StopReason)
	if incomplete || options.stripToolCalls {
		response.Items = partialResponseItems(response.Items)
	}
	var accepted ModelResponse
	if incomplete && len(response.Items) == 0 {
		accepted = ModelResponse{Usage: response.Usage, StopReason: response.StopReason}
	} else {
		var err error
		accepted, err = acceptResponse(response, ProviderContext{
			Backend: live.state.Selection.Backend,
			Epoch:   live.state.Selection.Epoch,
		})
		if err != nil {
			if options.errorContext != "" {
				err = fmt.Errorf("%s: %w", options.errorContext, err)
			}
			return committedModelResponse{}, &responseAcceptanceError{err: err}
		}
	}
	if _, err := appendRecordAndApply(ctx, runtime.journal, live, RecordModelResponse, ModelResponseRecord{
		RunID:      runID,
		Backend:    live.state.Selection.Backend,
		Model:      live.state.Selection.Model,
		Epoch:      live.state.Selection.Epoch,
		Items:      accepted.Items,
		Usage:      accepted.Usage,
		StopReason: accepted.StopReason,
	}); err != nil {
		return committedModelResponse{}, err
	}
	return committedModelResponse{
		response: accepted, answer: responseText(accepted.Items), incomplete: incomplete,
	}, nil
}

func partialResponseItems(items []Item) []Item {
	partial := make([]Item, 0, len(items))
	for _, item := range items {
		if item.Kind == ItemAssistantText || item.Kind == ItemReasoning {
			partial = append(partial, cloneItem(item))
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
		record, err := appendRecordAndApply(ctx, runtime.journal, reducer, RecordBoundaryEvent, BoundaryEventRecord{
			RunID:      runID,
			JobID:      event.JobID,
			FinishedAt: event.FinishedAt,
			Content:    event.Content,
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
			runtime.externalWork.ToolResultCommitted(*cloneToolResult(item.ToolResult))
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
) (ModelResponse, error) {
	for {
		request, err := runtime.modelRequestForRun(live.state, spec)
		if err != nil {
			return ModelResponse{}, err
		}
		probeImages := live.state.ImageDelivery.Status == ImageDeliveryUnknown && modelRequestHasImages(request)
		response, err := runtime.completeRequest(ctx, runID, request, emit)
		if err == nil {
			if probeImages {
				if observeErr := runtime.observeImageDelivery(ctx, live, ImageDeliveryAccepted); observeErr != nil {
					return ModelResponse{}, observeErr
				}
			}
			return response, nil
		}
		if ctx.Err() == nil && errors.Is(err, ErrModelRequestTooLarge) {
			var shrinkErr error
			report, shrinkErr = runtime.shrinkRunRequestOnce(ctx, live, spec, report, emit)
			if shrinkErr != nil {
				return ModelResponse{}, errors.Join(
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
					return ModelResponse{}, observeErr
				}
				return fallbackResponse, nil
			}
			// The control changed only image delivery. If it also failed, it did
			// not explain the original request rejection; preserve that error and
			// leave the epoch unknown.
			return ModelResponse{}, err
		}
		return response, err
	}
}

func modelRequestHasImages(request ModelRequest) bool {
	for _, item := range request.Items {
		if item.ToolResult != nil && item.ToolResult.Content.HasImage() {
			return true
		}
	}
	return false
}

func (runtime *Runtime) imageFreeControlAllowed(err error, report ContextReport) bool {
	providerErr, ok := errors.AsType[*ProviderError](err)
	if !ok || providerErr.Kind != ProviderErrorRequest || providerErr.Retryable {
		return false
	}
	// A successful smaller control is not evidence about image delivery when
	// the original request may simply have exceeded an uncertain context
	// window. The ordinary input limit already holds back a context reserve.
	return !runtime.modelInfo.ContextWindowEstimated && report.Window > 0 && report.InputLimit > 0 && report.TotalInputTokens <= report.InputLimit
}

func requestWithoutImages(request ModelRequest) ModelRequest {
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

func (runtime *Runtime) completeRequest(ctx context.Context, runID string, request ModelRequest, emit EmitFunc) (ModelResponse, error) {
	startedAt := time.Now()
	requestCtx := ctx
	cancel := func() {}
	if runtime.requestPolicy.RetryBudget > 0 {
		requestCtx, cancel = context.WithDeadline(ctx, startedAt.Add(runtime.requestPolicy.RetryBudget))
	}
	defer cancel()
	request.StreamIdleTimeout = runtime.requestPolicy.StreamIdleTimeout
	var lastErr error
	requestID := ""
	for attempt := 1; runtime.attemptAllowed(attempt); attempt++ {
		attemptID := ""
		if emit != nil {
			var err error
			attemptID, err = newID("attempt")
			if err != nil {
				return ModelResponse{}, err
			}
			emitEvent(emit, Event{Kind: EventModelAttemptStarted, RunID: runID, AttemptID: attemptID})
		}
		response, err := runtime.backend.Complete(requestCtx, request, func(event ModelStreamEvent) {
			switch event.Kind {
			case EventTextDelta, EventReasoningSummaryDelta:
				emitEvent(emit, Event{Kind: event.Kind, RunID: runID, AttemptID: attemptID, Text: runtime.sanitize(event.Text)})
			}
		})
		if err == nil {
			return runtime.sanitizeModelResponse(response), nil
		}
		if errors.Is(requestCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			err = MarkProviderFailure(fmt.Errorf("%w after %s", ErrModelRequestBudget, runtime.requestPolicy.RetryBudget))
		}
		lastErr = sanitizeError(err, runtime.sanitize)
		if ctx.Err() == nil {
			if requestID == "" {
				var idErr error
				requestID, idErr = newID("request")
				if idErr != nil {
					return ModelResponse{}, errors.Join(lastErr, idErr)
				}
			}
			if journalErr := runtime.recordModelAttemptFailure(
				context.WithoutCancel(ctx), requestID, runID, request.ProviderEpoch, attempt, lastErr,
			); journalErr != nil {
				return ModelResponse{}, errors.Join(lastErr, journalErr)
			}
		}
		emitEvent(emit, Event{Kind: EventModelAttemptDiscarded, RunID: runID, AttemptID: attemptID, Text: lastErr.Error()})
		if ctx.Err() != nil || errors.Is(err, ErrInvalidRequest) || errors.Is(err, ErrModelRequestTooLarge) ||
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
				lastErr = MarkProviderFailure(fmt.Errorf("%w after %s", ErrModelRequestBudget, runtime.requestPolicy.RetryBudget))
			} else {
				lastErr = err
			}
			break
		}
	}
	return ModelResponse{}, lastErr
}

func (runtime *Runtime) recordModelAttemptFailure(
	ctx context.Context,
	requestID, runID, providerEpoch string,
	attempt int,
	cause error,
) error {
	purpose := ModelRequestRun
	if runID == "" {
		purpose = ModelRequestCompaction
	}
	errorText, errorTruncated := boundedModelAttemptText(cause.Error(), maxModelAttemptErrorBytes)
	backend, _ := boundedModelAttemptText(runtime.sanitize(runtime.modelInfo.BackendID), maxModelAttemptFieldBytes)
	provider, _ := boundedModelAttemptText(runtime.sanitize(runtime.modelInfo.Provider), maxModelAttemptFieldBytes)
	model, _ := boundedModelAttemptText(runtime.sanitize(runtime.modelInfo.Model), maxModelAttemptFieldBytes)
	payload := ModelAttemptFailedRecord{
		RequestID: requestID, RunID: runID, Purpose: purpose, Attempt: attempt,
		Backend: backend, Provider: provider, Model: model, ProviderEpoch: providerEpoch,
		Error: errorText, ErrorTruncated: errorTruncated,
	}
	if providerErr, ok := errors.AsType[*ProviderError](cause); ok {
		kind, _ := boundedModelAttemptText(runtime.sanitize(string(providerErr.Kind)), maxModelAttemptFieldBytes)
		code, _ := boundedModelAttemptText(runtime.sanitize(providerErr.Code), maxModelAttemptFieldBytes)
		errorType, _ := boundedModelAttemptText(runtime.sanitize(providerErr.Type), maxModelAttemptFieldBytes)
		payload.ProviderError = &ModelAttemptProviderError{
			StatusCode: providerErr.StatusCode, Kind: ProviderErrorKind(kind),
			Code: code, Type: errorType, Retryable: providerErr.Retryable,
			RetryAfter: durationSnapshot(providerErr.RetryAfter),
		}
	}
	_, err := appendRecord(ctx, runtime.journal, RecordModelAttemptFailed, payload)
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
	if providerErr, ok := errors.AsType[*ProviderError](err); ok {
		return providerErr.Retryable
	}
	// Unclassified backend errors use the generic retryable default.
	return !errors.Is(err, ErrInvalidRequest)
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
	if providerErr, ok := errors.AsType[*ProviderError](err); ok && providerErr.RetryAfter > delay {
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

func (runtime *Runtime) modelRequestForRun(state State, spec runRequestSpec) (ModelRequest, error) {
	if state.Configured == nil {
		return ModelRequest{}, errors.New("session has no effective configuration")
	}
	summary := ""
	if state.Compaction != nil {
		summary = state.Compaction.Summary
	}
	items := state.verbatimModelItems()
	if spec.extraUserText != "" {
		items = append(items, Item{Kind: ItemUserText, Text: spec.extraUserText})
	}
	request := ModelRequest{
		SessionID:     state.SessionID,
		ProviderEpoch: state.Selection.Epoch,
		Instructions:  state.Configured.ModelContext.Instructions,
		Summary:       summary,
		Items: runtime.projectModelItems(items, ProviderContext{
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
func (runtime *Runtime) projectModelItems(items []Item, providerContext ProviderContext, imageStatus ImageDeliveryStatus) []Item {
	items = projectOwnedModelItems(items, providerContext)
	if runtime.backend != nil {
		items = runtime.backend.ProjectModelItems(items)
	} else {
		// Without the adapter's replay policy we retain extra reasoning items.
		// That can only overestimate context use, and this Runtime cannot send a
		// request until an executable backend is attached.
	}
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
	return MarkInvalidRequest(modelUnavailableError{model: modelURI(runtime.modelInfo)})
}

// projectOwnedModelItems consumes an already-owned item snapshot. Callers must
// pass a disposable projection source (as the runtime does) or clone it first.
func projectOwnedModelItems(items []Item, providerContext ProviderContext) []Item {
	projected := items[:0]
	for _, item := range items {
		if item.Kind == ItemReasoning {
			if item.ProviderContext == nil || item.ProviderContext.Backend != providerContext.Backend || item.ProviderContext.Epoch != providerContext.Epoch {
				continue
			}
		}
		if item.ToolCall != nil {
			references := item.ToolCall.ProviderReferences[:0]
			for _, reference := range item.ToolCall.ProviderReferences {
				if reference.Backend == providerContext.Backend && reference.Epoch == providerContext.Epoch {
					references = append(references, reference)
				}
			}
			item.ToolCall.ProviderReferences = references
		}
		if item.ToolResult != nil {
			// Details are journaled product metadata for UI/JSON renderers. Model
			// backends receive only semantic tool content and cannot accidentally
			// couple prompts to presentation-specific payloads.
			item.ToolResult.Details = nil
		}
		projected = append(projected, item)
	}
	return projected
}

func projectImagesForDelivery(items []Item, status ImageDeliveryStatus) []Item {
	if status != ImageDeliveryRejected {
		return items
	}
	return omitImagesFromModelItems(items)
}

func omitImagesFromModelItems(items []Item) []Item {
	for index := range items {
		if items[index].ToolResult == nil || !items[index].ToolResult.Content.HasImage() {
			continue
		}
		items[index].ToolResult.Content = items[index].ToolResult.Content.WithoutImages(func(image ImageContent) string {
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
	if selectionMatchesModel(state.Selection, runtime.modelInfo) {
		return runtime.recordCurrentEffectiveConfigurationAndApply(ctx, reducer)
	}
	epoch, err := newID("epoch")
	if err != nil {
		return err
	}
	selection := ModelSelectedRecord{
		Backend:               runtime.modelInfo.BackendID,
		Provider:              runtime.modelInfo.Provider,
		Model:                 runtime.modelInfo.Model,
		ReasoningEffort:       runtime.modelInfo.ReasoningEffort,
		ProviderStateContract: runtime.modelInfo.ProviderStateContract,
		Epoch:                 epoch,
	}
	_, err = appendRecordAndApply(ctx, runtime.journal, reducer, RecordModelSelected, selection)
	if err != nil {
		return err
	}
	return runtime.recordCurrentEffectiveConfigurationAndApply(ctx, reducer)
}

func (runtime *Runtime) executeTool(ctx context.Context, sessionID string, call ToolCall) (ToolResult, bool, error) {
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
		return ToolResult{CallID: call.ID, Content: TextContent(runtime.sanitize(message)), Error: true}, false, nil
	}
	output, err := tool.Run(WithToolSessionID(ctx, sessionID), call.RawArguments)
	if ctx.Err() != nil {
		return ToolResult{}, true, nil
	}
	details, detailErr := runtime.sanitizeToolDetails(output.Details)
	if detailErr != nil {
		err = errors.Join(err, fmt.Errorf("invalid tool output: %w", detailErr))
		details = nil
	}
	content, contentErr := normalizeContent(output.Content)
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
		result := ToolResult{CallID: call.ID, Content: TextContent(runtime.sanitize(message)), Details: details, Error: true}
		if errors.Is(err, ErrToolFatal) {
			return result, false, err
		}
		return result, false, nil
	}
	return ToolResult{CallID: call.ID, Content: runtime.sanitizeContent(content), Details: details}, false, nil
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

func (runtime *Runtime) sanitizeModelResponse(response ModelResponse) ModelResponse {
	response.Items = runtime.sanitizeItems(response.Items)
	return response
}

func (runtime *Runtime) sanitizeItems(items []Item) []Item {
	sanitized := cloneItems(items)
	for index := range sanitized {
		sanitized[index].Text = runtime.sanitize(sanitized[index].Text)
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

func (runtime *Runtime) sanitizeContent(content Content) Content {
	sanitized := content.Clone()
	for index := range sanitized {
		if sanitized[index].Kind == ContentPartText {
			sanitized[index].Text = runtime.sanitize(sanitized[index].Text)
		}
	}
	return sanitized
}

func (runtime *Runtime) sanitizeDetails(details []Detail) []Detail {
	sanitized := make([]Detail, len(details))
	for index, detail := range details {
		sanitized[index] = Detail{Kind: detail.Kind}
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

// sanitizeToolDetails validates both sides of redaction. Replacing a short
// secret can expand otherwise valid JSON beyond the durable details limit.
func (runtime *Runtime) sanitizeToolDetails(details []Detail) ([]Detail, error) {
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

func acceptResponse(response ModelResponse, providerContext ProviderContext) (ModelResponse, error) {
	accepted := ModelResponse{Usage: response.Usage, StopReason: response.StopReason, Items: make([]Item, 0, len(response.Items))}
	responseID, err := newID("response")
	if err != nil {
		return ModelResponse{}, err
	}
	for _, item := range response.Items {
		item = cloneItem(item)
		item.ResponseID = responseID
		switch item.Kind {
		case ItemAssistantText:
			if len(item.ProviderData) != 0 || item.ToolCall != nil || item.ToolResult != nil {
				return ModelResponse{}, fmt.Errorf("%s item has unrelated payload", item.Kind)
			}
			item.ProviderContext = nil
		case ItemReasoning:
			if item.ToolCall != nil || item.ToolResult != nil {
				return ModelResponse{}, fmt.Errorf("%s item has unrelated payload", item.Kind)
			}
			item.ProviderData, err = normalizeProviderData(item.ProviderData)
			if err != nil {
				return ModelResponse{}, fmt.Errorf("invalid reasoning provider data: %w", err)
			}
			item.ProviderContext = &ProviderContext{Backend: providerContext.Backend, Epoch: providerContext.Epoch}
		case ItemToolCall:
			if len(item.ProviderData) != 0 || item.ToolCall == nil || strings.TrimSpace(item.ToolCall.Name) == "" {
				return ModelResponse{}, errors.New("tool call name is required")
			}
			arguments, err := NormalizeToolArguments(item.ToolCall.RawArguments)
			if err != nil {
				return ModelResponse{}, fmt.Errorf("tool call %q: %w", strings.TrimSpace(item.ToolCall.Name), err)
			}
			for _, reference := range item.ToolCall.ProviderReferences {
				if strings.TrimSpace(reference.Kind) == "" || !reference.Data.IsValid() {
					return ModelResponse{}, errors.New("tool call provider reference is invalid")
				}
			}
			for index := range item.ToolCall.ProviderReferences {
				item.ToolCall.ProviderReferences[index].Backend = providerContext.Backend
				item.ToolCall.ProviderReferences[index].Epoch = providerContext.Epoch
			}
			item.ProviderContext = nil
			id, err := newID("call")
			if err != nil {
				return ModelResponse{}, err
			}
			item.ToolCall.ID = id
			item.ToolCall.Name = strings.TrimSpace(item.ToolCall.Name)
			item.ToolCall.RawArguments = arguments
		default:
			return ModelResponse{}, fmt.Errorf("model returned unsupported item kind %q", item.Kind)
		}
		accepted.Items = append(accepted.Items, item)
	}
	if len(accepted.Items) == 0 {
		return ModelResponse{}, errors.New("model returned no items")
	}
	return accepted, nil
}

func normalizeAcceptedItem(item Item) (Item, error) {
	switch item.Kind {
	case ItemAssistantText:
		if item.ResponseID == "" || len(item.ProviderData) != 0 || item.ToolCall != nil || item.ToolResult != nil {
			return Item{}, fmt.Errorf("%s item has unrelated payload", item.Kind)
		}
		if item.ProviderContext != nil {
			return Item{}, errors.New("assistant text item has provider context")
		}
	case ItemReasoning:
		if item.ResponseID == "" || item.ProviderContext == nil || item.ProviderContext.Backend == "" || item.ProviderContext.Epoch == "" || item.ToolCall != nil || item.ToolResult != nil {
			return Item{}, errors.New("reasoning item requires provider context")
		}
		if err := validateProviderData(item.ProviderData); err != nil {
			return Item{}, fmt.Errorf("reasoning item has invalid provider data: %w", err)
		}
	case ItemToolCall:
		if item.ResponseID == "" || len(item.ProviderData) != 0 || item.ToolCall == nil || item.ToolCall.ID == "" || strings.TrimSpace(item.ToolCall.Name) == "" {
			return Item{}, errors.New("accepted tool call requires ID and name")
		}
		arguments, err := NormalizeToolArguments(item.ToolCall.RawArguments)
		if err != nil {
			return Item{}, fmt.Errorf("accepted tool call has invalid arguments: %w", err)
		}
		call := cloneToolCall(*item.ToolCall)
		call.Name = strings.TrimSpace(call.Name)
		call.RawArguments = arguments
		item.ToolCall = &call
	default:
		return Item{}, fmt.Errorf("unsupported accepted item kind %q", item.Kind)
	}
	return item, nil
}

func responseToolCalls(items []Item) []ToolCall {
	var calls []ToolCall
	for _, item := range items {
		if item.Kind == ItemToolCall && item.ToolCall != nil {
			calls = append(calls, cloneToolCall(*item.ToolCall))
		}
	}
	return calls
}

func responseText(items []Item) string {
	var parts []string
	for _, item := range items {
		if item.Kind == ItemAssistantText && strings.TrimSpace(item.Text) != "" {
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

func cloneItems(items []Item) []Item {
	out := make([]Item, len(items))
	for index, item := range items {
		out[index] = cloneItem(item)
	}
	return out
}

func cloneItem(item Item) Item {
	return cloneItemForProjection(item, true)
}

func cloneItemForProjection(item Item, includeDetails bool) Item {
	if item.ProviderContext != nil {
		context := *item.ProviderContext
		item.ProviderContext = &context
	}
	item.ProviderData = cloneProviderData(item.ProviderData)
	item.ToolCall = cloneToolCallPointer(item.ToolCall)
	if includeDetails {
		item.ToolResult = cloneToolResult(item.ToolResult)
	} else if item.ToolResult != nil {
		result := *item.ToolResult
		result.Content = cloneContentForProjection(item.ToolResult.Content)
		result.Details = nil
		item.ToolResult = &result
	}
	return item
}

func cloneModelItemsForProjection(items []Item) []Item {
	cloned := make([]Item, len(items))
	for index, item := range items {
		cloned[index] = cloneItemForProjection(item, false)
	}
	return cloned
}

func cloneToolCallPointer(call *ToolCall) *ToolCall {
	if call == nil {
		return nil
	}
	cloned := cloneToolCall(*call)
	return &cloned
}

func cloneToolCall(call ToolCall) ToolCall {
	call.ProviderReferences = append([]ProviderReference(nil), call.ProviderReferences...)
	for index := range call.ProviderReferences {
		call.ProviderReferences[index].Data = append([]byte(nil), call.ProviderReferences[index].Data...)
	}
	return call
}

func cloneToolResult(result *ToolResult) *ToolResult {
	if result == nil {
		return nil
	}
	cloned := *result
	cloned.Content = result.Content.Clone()
	cloned.Details = cloneDetails(result.Details)
	return &cloned
}
