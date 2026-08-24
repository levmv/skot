package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
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
	errNoCompactionBoundary = errors.New("no completed older conversation block is available to compact")
	errCompactionNotNeeded  = errors.New("context is already within the compaction target")
)

// compactionPlan is tied to an exact journal state and a whole-block boundary.
type compactionPlan struct {
	SessionID              string
	BaseRequiredSequence   uint64
	CoveredThroughSequence uint64
	FirstVerbatimSequence  uint64
	RuntimeFacts           string
}

// Compact generates and commits one rolling summary. Its model call preserves
// the ordinary request prefix, and provisional output is never journaled.
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
	compaction, record, err := runtime.compactLocked(ctx, live.state, runRequestSpec{}, nil)
	if err != nil {
		return ContextCompactedRecord{}, err
	}
	if err := live.apply(record); err != nil {
		return ContextCompactedRecord{}, err
	}
	runtime.publishSessionStatus(live.state)
	return compaction, nil
}

func (runtime *Runtime) compactLocked(ctx context.Context, state State, spec runRequestSpec, emit EmitFunc) (ContextCompactedRecord, Record, error) {
	if state.Configured == nil {
		return ContextCompactedRecord{}, Record{}, errors.New("session has no effective configuration")
	}
	plan, err := runtime.planCompactionForModelBoundary(state, spec)
	if err != nil {
		return ContextCompactedRecord{}, Record{}, err
	}
	request, err := runtime.compactionRequest(state, spec, plan)
	if err != nil {
		return ContextCompactedRecord{}, Record{}, err
	}
	emitEvent(emit, Event{Kind: EventStatus, Text: "compacting context"})
	response, err := runtime.completeRequest(ctx, "", request, withoutStreamedModelContent(emit))
	if err != nil {
		return ContextCompactedRecord{}, Record{}, fmt.Errorf("summarize context: %w", sanitizeError(err, runtime.sanitize))
	}
	response = runtime.sanitizeModelResponse(response)
	if IsIncompleteStopReason(response.StopReason) {
		return ContextCompactedRecord{}, Record{}, fmt.Errorf("compaction model returned an incomplete summary: stop reason %s", response.StopReason)
	}
	for _, item := range response.Items {
		switch item.Kind {
		case ItemAssistantText, ItemReasoning:
		case ItemToolCall:
			// Tool schemas preserve the ordinary cached prefix, but compaction
			// never executes calls. Keep a usable text summary if the model also
			// emitted a call; a call-only response still fails as empty below.
		default:
			return ContextCompactedRecord{}, Record{}, fmt.Errorf("compaction model returned unsupported item kind %q", item.Kind)
		}
	}
	summary := responseText(response.Items)
	if strings.TrimSpace(summary) == "" {
		return ContextCompactedRecord{}, Record{}, errors.New("compaction model returned an empty summary")
	}
	return commitCompaction(ctx, runtime.journal, plan, summary, response.Usage)
}

// compactionRequest preserves the ordinary request prefix through the selected
// history and appends the compaction directive as its only new user message.
// Prefix-caching routes can therefore reuse the work already done for the
// session instead of receiving a separately serialized transcript.
func (runtime *Runtime) compactionRequest(state State, spec runRequestSpec, plan compactionPlan) (ModelRequest, error) {
	targetEnd := blockIndexAtSequence(state.Blocks, plan.FirstVerbatimSequence)
	if targetEnd < 0 {
		return ModelRequest{}, errors.New("compaction boundary is missing from replayed blocks")
	}
	prefix := state
	prefix.Blocks = prefix.Blocks[:targetEnd]
	prompt := compactionPrompt(state.Configured.ModelContext.CompactionInstructions, plan.RuntimeFacts)
	request, err := runtime.modelRequestForRun(prefix, runRequestSpec{omitTools: spec.omitTools, extraUserText: prompt})
	if err != nil {
		return ModelRequest{}, err
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

// planCompactionForModelBoundary selects an older complete prefix while
// preserving a token-bounded, whole-block tail. During an active run it stops
// before that run's oldest block, so newly delivered tool results, boundary
// events, or queued input can trigger maintenance without changing the active
// transcript.
func (runtime *Runtime) planCompactionForModelBoundary(state State, spec runRequestSpec) (compactionPlan, error) {
	if state.SessionID == "" {
		return compactionPlan{}, errors.New("session is not initialized")
	}
	activeStart := 0
	if state.Compaction != nil {
		activeStart = blockIndexAtSequence(state.Blocks, state.Compaction.FirstVerbatimSequence)
		if activeStart < 0 {
			return compactionPlan{}, errors.New("current compaction boundary is missing from replayed blocks")
		}
	}
	targetEnd := len(state.Blocks) - 1
	if state.hasUnfinishedWork() {
		unfinishedStart, ok := state.firstUnfinishedBlock()
		if !ok {
			return compactionPlan{}, errors.New("cannot locate unfinished conversation block")
		}
		targetEnd = unfinishedStart
	}
	if targetEnd <= activeStart {
		return compactionPlan{}, errNoCompactionBoundary
	}
	targetEnd = runtime.compactionTargetEnd(state, spec, activeStart, targetEnd)
	if targetEnd <= activeStart {
		return compactionPlan{}, errCompactionNotNeeded
	}
	targetEnd = runtime.compactionInputTargetEnd(state, spec, activeStart, targetEnd)
	if targetEnd <= activeStart {
		return compactionPlan{}, errors.New("no complete conversation block fits in the compaction model input")
	}
	coveredThrough := state.Blocks[targetEnd-1].EndSequence
	return compactionPlan{
		SessionID:              state.SessionID,
		BaseRequiredSequence:   state.lastRequiredSequence,
		CoveredThroughSequence: coveredThrough,
		FirstVerbatimSequence:  state.Blocks[targetEnd].StartSequence,
		RuntimeFacts:           formatCompactionRuntimeFacts(state.Blocks[activeStart:targetEnd]),
	}, nil
}

func (runtime *Runtime) compactionTargetEnd(state State, spec runRequestSpec, activeStart, latestTargetEnd int) int {
	report := runtime.requestReport(state, spec)
	budget := compactionVerbatimTokenBudget(report)
	if report.HistoryTokens <= budget {
		return activeStart
	}
	// Find the earliest boundary whose provider-projected tail fits. The lower
	// bound retains as much recent history as the budget permits. If the newest
	// block alone exceeds the soft budget, selected retains it and compacts every
	// older available block.
	first, last := activeStart+1, latestTargetEnd
	selected := latestTargetEnd
	for first <= last {
		candidate := first + (last-first)/2
		if runtime.projectedTailTokens(state, spec, candidate) <= budget {
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
func (runtime *Runtime) compactionInputTargetEnd(state State, spec runRequestSpec, activeStart, desiredTargetEnd int) int {
	if runtime.modelInfo.ContextWindow == 0 {
		return desiredTargetEnd
	}
	first, last := activeStart+1, desiredTargetEnd
	selected := activeStart
	for first <= last {
		candidateEnd := first + (last-first)/2
		candidate := state
		candidate.Blocks = candidate.Blocks[:candidateEnd]
		prompt := compactionPrompt(
			state.Configured.ModelContext.CompactionInstructions,
			formatCompactionRuntimeFacts(state.Blocks[activeStart:candidateEnd]),
		)
		report := runtime.requestReport(candidate, runRequestSpec{omitTools: spec.omitTools, extraUserText: prompt})
		if report.TotalInputTokens <= report.InputLimit {
			selected = candidateEnd
			first = candidateEnd + 1
		} else {
			last = candidateEnd - 1
		}
	}
	return selected
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

func (runtime *Runtime) projectedTailTokens(state State, spec runRequestSpec, firstBlock int) int {
	items := state.verbatimModelItemsFromSequence(state.Blocks[firstBlock].StartSequence)
	items, boundary := runtime.projectedItemsForRequest(state, items, spec.extraUserText)
	return estimateItemsTokens(items[:boundary])
}

// commitCompaction appends a rolling summary only if the journal still has the
// exact state for which the plan was constructed.
func commitCompaction(ctx context.Context, journal Journal, plan compactionPlan, summary string, usage ModelUsage) (ContextCompactedRecord, Record, error) {
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
		CoveredThroughSequence: plan.CoveredThroughSequence,
		FirstVerbatimSequence:  plan.FirstVerbatimSequence,
		Summary:                summary,
		Usage:                  usage,
	}
	if err := validateCompactionBoundary(state, payload, state.LastSequence+1, state.hasUnfinishedWork()); err != nil {
		return ContextCompactedRecord{}, Record{}, err
	}
	record, err := appendRecord(ctx, journal, RecordContextCompacted, payload)
	if err != nil {
		return ContextCompactedRecord{}, Record{}, err
	}
	return payload, record, nil
}

func blockIndexAtSequence(blocks []ConversationBlock, sequence uint64) int {
	for index, block := range blocks {
		if block.StartSequence == sequence {
			return index
		}
	}
	return -1
}

// Model items contain the transcript but not run completion metadata. Preserve
// only exceptional host-owned facts in the trailing compaction instruction;
// ordinary completed markers would merely duplicate what the model can see.
func formatCompactionRuntimeFacts(blocks []ConversationBlock) string {
	var output strings.Builder
	for _, block := range blocks {
		if block.Status == RunCompleted && !block.ToolLimitReached && block.Error == "" && len(block.DetachedJobs) == 0 {
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
