package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/levmv/skot/model"
)

const (
	maxCompactionSummaryBytes       = 32 * 1024
	maxCompactionRuntimeFactBytes   = 8 * 1024
	compactionCheckpointTokenBudget = 8 * 1024
	minCompactionVerbatimTokens     = 8 * 1024
	maxCompactionVerbatimTokens     = 32 * 1024
	compactionVerbatimPercentage    = 15
)

const compactionInstructions = `Act as a compaction engine for this coding session. Summarize the conversation above for reliable continuation. Preserve the objective and acceptance criteria, user constraints, decisions and reasons, modified files and current state, commands and tests with their results, errors and abandoned approaches, outstanding work and the next step, and explicit uncertainty. Be compact but concrete. Do not claim work was done unless the conversation says it was. Do not call tools. Output only the summary.`

var (
	errNoCompactionBoundary = errors.New("no completed older exchange is available to compact")
	errCompactionNotNeeded  = errors.New("context is already within the compaction target")
)

// compactionPlan is tied to an exact journal state and a completed exchange.
type compactionPlan struct {
	SessionID              string
	BaseRequiredSequence   uint64
	CoveredThroughSequence uint64
	FirstVerbatimSequence  uint64
	RuntimeFacts           string
}

// Compact generates and commits one rolling summary. Its model call preserves
// the ordinary request prefix, and provisional output is never journaled.
// If reloading instructions fails afterwards, the summary remains committed
// and the next operation which prepares the session retries the reload.
func (runtime *Runtime) Compact(ctx context.Context) (ContextCompactedRecord, error) {
	if !runtime.runMu.TryLock() {
		return ContextCompactedRecord{}, ErrRunActive
	}
	defer runtime.runMu.Unlock()
	if err := runtime.requireBackend(); err != nil {
		return ContextCompactedRecord{}, err
	}
	records, err := runtime.journal.Records(ctx)
	if err != nil {
		return ContextCompactedRecord{}, fmt.Errorf("read journal for compaction: %w", err)
	}
	live, err := reduceRecords(records)
	if err != nil {
		return ContextCompactedRecord{}, err
	}
	runtime.publishSessionStatus(live.state)
	if live.state.hasUnfinishedWork() {
		return ContextCompactedRecord{}, errors.New("cannot compact unfinished work")
	}
	if err := runtime.prepareSession(ctx, live); err != nil {
		return ContextCompactedRecord{}, err
	}
	return runtime.compactLocked(ctx, live, runRequestSpec{}, nil)
}

func (runtime *Runtime) compactLocked(ctx context.Context, live *stateReducer, spec runRequestSpec, emit EmitFunc) (ContextCompactedRecord, error) {
	state := live.state
	if state.Configured == nil {
		return ContextCompactedRecord{}, errors.New("session has no effective configuration")
	}
	plan, err := runtime.planCompactionForModelBoundary(state, spec)
	if err != nil {
		return ContextCompactedRecord{}, err
	}
	request, err := runtime.compactionRequest(state, spec, plan)
	if err != nil {
		return ContextCompactedRecord{}, err
	}
	emitEvent(emit, Event{Kind: EventStatus, Text: "compacting context"})
	response, err := runtime.completeRequest(ctx, "", request, withoutStreamedModelContent(emit))
	if err != nil {
		return ContextCompactedRecord{}, fmt.Errorf("summarize context: %w", sanitizeError(err, runtime.sanitize))
	}
	if model.IsIncompleteStopReason(response.StopReason) {
		return ContextCompactedRecord{}, fmt.Errorf("compaction model returned an incomplete summary: stop reason %s", response.StopReason)
	}
	for _, item := range response.Items {
		switch item.Kind {
		case model.ItemAssistantText, model.ItemReasoning:
		case model.ItemToolCall:
			// Tool schemas preserve the ordinary cached prefix, but compaction
			// never executes calls. Keep a usable text summary if the model also
			// emitted a call; a call-only response still fails as empty below.
		default:
			return ContextCompactedRecord{}, fmt.Errorf("compaction model returned unsupported item kind %q", item.Kind)
		}
	}
	summary := responseText(response.Items)
	if strings.TrimSpace(summary) == "" {
		return ContextCompactedRecord{}, errors.New("compaction model returned an empty summary")
	}
	compaction, record, err := commitCompaction(ctx, runtime.journal, plan, summary, response.Usage.Tokens.Known(), response.attemptID)
	if err != nil {
		return ContextCompactedRecord{}, err
	}
	if err := live.apply(record); err != nil {
		return compaction, fmt.Errorf("apply committed context compaction: %w", err)
	}
	runtime.instructionsDirty = runtime.loadInstructions != nil
	runtime.publishSessionStatus(live.state)
	emitEvent(emit, Event{Sequence: record.Sequence, Kind: EventContextCompacted, Text: "context compacted"})
	if runtime.instructionsDirty {
		if err := runtime.recordCurrentEffectiveConfigurationAndApply(ctx, live); err != nil {
			return compaction, err
		}
	}
	return compaction, nil
}

// compactionRequest preserves the ordinary request prefix through the selected
// history and appends the compaction directive as its only new user message.
// Prefix-caching routes can therefore reuse the work already done for the
// session instead of receiving a separately serialized transcript.
func (runtime *Runtime) compactionRequest(state State, spec runRequestSpec, plan compactionPlan) (model.Request, error) {
	prefix := state.historyBefore(plan.FirstVerbatimSequence)
	prompt := compactionPrompt(state.Configured.ModelContext.CompactionInstructions, plan.RuntimeFacts)
	request, err := runtime.modelRequestForRun(prefix, runRequestSpec{omitTools: spec.omitTools, extraUserText: prompt})
	if err != nil {
		return model.Request{}, err
	}
	if runtime.effectiveImageDelivery(prefix.ImageDelivery.Status) == ImageDeliveryUnknown {
		// Compaction is maintenance, not a route capability probe. After a model
		// switch its prefix contains only old completed work, so preserve image
		// metadata and ordering without making the internal request the first
		// image-bearing call on the new route.
		request.Items = omitImagesFromModelItems(request.Items)
	}
	return request, nil
}

// Automatic compaction is an internal model call. Its attempt and retry events
// remain observable, but its draft summary must never appear as the assistant's
// answer in a CLI/TUI transcript.
func withoutStreamedModelContent(emit EmitFunc) EmitFunc {
	if emit == nil {
		return nil
	}
	return func(event Event) {
		switch event.Kind {
		case EventTextDelta, EventReasoningSummaryDelta:
			return
		default:
			emit(event)
		}
	}
}

// Manual compaction, preflight, and provider-overflow recovery share the same
// exchange boundaries. The run may continue while its earlier steps are
// summarized; its authored inputs remain outside the summary.
func (runtime *Runtime) planCompactionForModelBoundary(state State, spec runRequestSpec) (compactionPlan, error) {
	if state.SessionID == "" {
		return compactionPlan{}, errors.New("session is not initialized")
	}
	boundaries := state.contextBoundaries()
	if state.Compaction != nil {
		first := 0
		for first < len(boundaries) && boundaries[first].first <= state.Compaction.FirstVerbatimSequence {
			first++
		}
		boundaries = boundaries[first:]
	}
	if len(boundaries) == 0 {
		return compactionPlan{}, errNoCompactionBoundary
	}
	targetEnd := runtime.compactionTargetEnd(state, spec, boundaries)
	if targetEnd < 0 {
		return compactionPlan{}, errCompactionNotNeeded
	}
	targetEnd = runtime.compactionInputTargetEnd(state, spec, boundaries, targetEnd)
	if targetEnd < 0 {
		return compactionPlan{}, errors.New("no complete exchange fits in the compaction model input")
	}
	boundary := boundaries[targetEnd]
	return compactionPlan{
		SessionID:              state.SessionID,
		BaseRequiredSequence:   state.lastRequiredSequence,
		CoveredThroughSequence: boundary.through,
		FirstVerbatimSequence:  boundary.first,
		RuntimeFacts:           compactionRuntimeFacts(state.historyBefore(boundary.first)),
	}, nil
}

func (runtime *Runtime) compactionTargetEnd(state State, spec runRequestSpec, boundaries []contextBoundary) int {
	report := runtime.requestReport(state, spec)
	budget := compactionVerbatimTokenBudget(report)
	if report.HistoryTokens <= budget {
		return -1
	}
	// Find the earliest boundary whose provider-projected tail fits. The lower
	// bound retains as much recent history as the budget permits. If the newest
	// exchange alone exceeds the soft budget, retain it in full.
	first, last := 0, len(boundaries)-1
	selected := last
	for first <= last {
		candidate := first + (last-first)/2
		if runtime.projectedTailTokens(state, spec, boundaries[candidate].first) <= budget {
			selected = candidate
			last = candidate - 1
		} else {
			first = candidate + 1
		}
	}
	return selected
}

// compactionInputTargetEnd keeps the auxiliary request inside the same input
// limit used for an ordinary request. A session imported from a larger route
// may therefore advance through more than one cache-aligned prefix.
func (runtime *Runtime) compactionInputTargetEnd(state State, spec runRequestSpec, boundaries []contextBoundary, desiredTargetEnd int) int {
	if runtime.modelInfo.ContextWindow == 0 {
		return desiredTargetEnd
	}
	// Including a newer instruction can replace a larger retained version, so
	// input size is not monotonic in the prefix length.
	for candidateEnd := desiredTargetEnd; candidateEnd >= 0; candidateEnd-- {
		candidate := state.historyBefore(boundaries[candidateEnd].first)
		prompt := compactionPrompt(
			state.Configured.ModelContext.CompactionInstructions,
			compactionRuntimeFacts(candidate),
		)
		report := runtime.requestReport(candidate, runRequestSpec{omitTools: spec.omitTools, extraUserText: prompt})
		if report.TotalInputTokens <= report.InputLimit {
			return candidateEnd
		}
	}
	return -1
}

// The proportional target adapts to ordinary model sizes. Absolute bounds keep
// small windows useful without making the first cold request after a large
// rollover carry hundreds of thousands of verbatim tokens.
func compactionVerbatimTokenBudget(report ContextReport) int {
	budget := maxCompactionVerbatimTokens
	if report.Window > 0 {
		budget = min(
			max(report.Window*compactionVerbatimPercentage/100, minCompactionVerbatimTokens),
			maxCompactionVerbatimTokens,
		)
	}
	if report.InputLimit > 0 {
		budget = min(budget, max(1, report.InputLimit/2))
	}
	return budget
}

func compactionPrompt(instructions, runtimeFacts string) string {
	if runtimeFacts == "" {
		return instructions
	}
	return "Durable runtime facts not represented in the conversation messages:\n" + runtimeFacts + "\n\n" + instructions
}

func (runtime *Runtime) projectedTailTokens(state State, spec runRequestSpec, first uint64) int {
	items := state.modelItemsFromSequence(first, state.inputsBeforeBoundary(first))
	items, boundary := runtime.projectedItemsForRequest(state, items, spec.extraUserText)
	return estimateItemsTokens(items[:boundary])
}

// commitCompaction appends a rolling summary only if the journal still has the
// exact state for which the plan was constructed.
func commitCompaction(ctx context.Context, journal Journal, plan compactionPlan, summary string, usage model.TokenCounts, attemptID string) (ContextCompactedRecord, Record, error) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return ContextCompactedRecord{}, Record{}, errors.New("compaction summary is empty")
	}
	if len(summary) > maxCompactionSummaryBytes {
		return ContextCompactedRecord{}, Record{}, fmt.Errorf("compaction summary exceeds %d bytes", maxCompactionSummaryBytes)
	}
	records, err := journal.Records(ctx)
	if err != nil {
		return ContextCompactedRecord{}, Record{}, fmt.Errorf("read journal before compaction: %w", err)
	}
	state, err := Replay(records)
	if err != nil {
		return ContextCompactedRecord{}, Record{}, err
	}
	if state.SessionID != plan.SessionID || state.lastRequiredSequence != plan.BaseRequiredSequence {
		return ContextCompactedRecord{}, Record{}, errors.New("session changed while compaction summary was being prepared")
	}
	payload := ContextCompactedRecord{
		AttemptID:              attemptID,
		CoveredThroughSequence: plan.CoveredThroughSequence,
		FirstVerbatimSequence:  plan.FirstVerbatimSequence,
		RetainedInputSequences: state.inputsBeforeBoundary(plan.FirstVerbatimSequence),
		Summary:                summary,
		Usage:                  usage,
	}
	if err := validateContextCheckpoint(state, payload, state.LastSequence+1); err != nil {
		return ContextCompactedRecord{}, Record{}, err
	}
	record, err := appendRecord(ctx, journal, RecordContextCompacted, payload)
	if err != nil {
		return ContextCompactedRecord{}, Record{}, err
	}
	return payload, record, nil
}

func compactionRuntimeFacts(prefix State) string {
	first := 0
	if prefix.Compaction != nil {
		for first < len(prefix.Blocks) && prefix.Blocks[first].EndSequence <= prefix.Compaction.CoveredThroughSequence {
			first++
		}
	}
	return formatCompactionRuntimeFacts(prefix.Blocks[first:])
}

// Model items contain the transcript but not run completion metadata. Preserve
// only exceptional host-owned facts in the trailing compaction instruction;
// ordinary completed markers would merely duplicate what the model can see.
func formatCompactionRuntimeFacts(blocks []ConversationBlock) string {
	var output strings.Builder
	for _, block := range blocks {
		if block.Status == "" || (block.Status == RunCompleted && !block.ToolLimitReached && block.Error == "" && len(block.DetachedJobs) == 0) {
			continue
		}
		fmt.Fprintf(&output, "- run seq=%d..%d status=%s", block.StartSequence, block.EndSequence, block.Status)
		if block.ToolLimitReached {
			output.WriteString(" tool_limit_reached=true")
		}
		if block.Error != "" {
			output.WriteString(" error=")
			output.WriteString(truncateCompactionFact(block.Error))
		}
		if len(block.DetachedJobs) != 0 {
			output.WriteString(" detached_jobs=")
			output.WriteString(strings.Join(block.DetachedJobs, ","))
		}
		output.WriteByte('\n')
	}
	return strings.TrimSpace(output.String())
}

func truncateCompactionFact(text string) string {
	if len(text) <= maxCompactionRuntimeFactBytes {
		return text
	}
	cut := maxCompactionRuntimeFactBytes
	for cut > 0 && !utf8.ValidString(text[:cut]) {
		cut--
	}
	return text[:cut] + " [… truncated …]"
}
