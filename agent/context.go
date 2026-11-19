package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/levmv/skot/model"
)

const (
	minContextReserve       = 8 * 1024
	maxContextReserve       = 32 * 1024
	perMessageTokens        = 6
	perToolResultTokens     = 8
	perToolCallTokens       = 10
	perToolDefinitionTokens = 12
)

// ContextReport is a tokenizer-independent estimate of the next model input.
// Window zero means that no context window is known for the selected model.
type ContextReport struct {
	Window               int
	InputLimit           int
	InstructionTokens    int
	ToolTokens           int
	SummaryTokens        int
	HistoryTokens        int
	PendingTokens        int
	MeasuredAdjustment   int
	TotalInputTokens     int
	AvailableInputTokens int
	CompactionCount      int
	ToolPruningCount     int
}

// ContextLimitError reports a request that context maintenance could not fit.
// AttachedTokens estimates instructions retained outside the ordinary summary.
type ContextLimitError struct {
	Report         ContextReport
	AttachedTokens int
	Cause          error
}

func (err *ContextLimitError) Error() string {
	message := fmt.Sprintf("estimated model input is %d tokens (limit %d) and automatic context reduction failed: %v", err.Report.TotalInputTokens, err.Report.InputLimit, err.Cause)
	if err.AttachedTokens > 0 {
		message += fmt.Sprintf("; attached instructions retain approximately %d tokens", err.AttachedTokens)
	}
	return message
}

func (err *ContextLimitError) Unwrap() error { return err.Cause }

// SessionStatus is the coherent, presentation-neutral status of one journal
// projection. Runtime publishes complete values at operation boundaries so
// observers never have to replay the journal or assemble a partial view.
type SessionStatus struct {
	ContextReport ContextReport
	Usage         model.TokenCounts
	ImageDelivery ImageDeliveryStatus
}

func (runtime *Runtime) SessionStatus() SessionStatus {
	runtime.statusMu.RLock()
	defer runtime.statusMu.RUnlock()
	return runtime.sessionStatus
}

// publishSessionStatus always uses the canonical next-request projection:
// tools are included and only still-queued input is treated as pending.
func (runtime *Runtime) publishSessionStatus(state State) {
	runtime.publishStatus(state, nil)
}

func (runtime *Runtime) publishStatus(state State, preflight *ContextReport) {
	runtime.lastContextSamples = state.contextSamples
	runtime.storeSessionStatus(state.LastSequence, runtime.calculateSessionStatus(state, preflight))
}

func (runtime *Runtime) calculateSessionStatus(state State, preflight *ContextReport) SessionStatus {
	runtime.queueMu.Lock()
	inputs := slices.Clone(runtime.pendingInputs)
	runtime.queueMu.Unlock()
	var report ContextReport
	if preflight != nil && len(inputs) == 0 {
		report = *preflight
		runtime.applyRequestMeasurement(state, &report)
	} else {
		report = runtime.requestReport(state, runRequestSpec{pendingInputs: inputs})
	}
	return SessionStatus{
		ContextReport: report,
		Usage:         state.Usage,
		ImageDelivery: runtime.visibleImageDelivery(state),
	}
}

func (runtime *Runtime) visibleImageDelivery(state State) ImageDeliveryStatus {
	firstSequence := state.firstVerbatimSequence()
	for _, block := range state.Blocks {
		if firstSequence != 0 && block.EndSequence < firstSequence {
			continue
		}
		for _, entry := range block.Entries {
			if entry.Sequence < firstSequence {
				continue
			}
			if state.ToolPruning != nil && entry.Sequence <= state.ToolPruning.ThroughSequence {
				continue
			}
			if entry.Item.ToolResult != nil && entry.Item.ToolResult.Content.HasImage() {
				return runtime.effectiveImageDelivery(state.ImageDelivery.Status)
			}
		}
	}
	return ImageDeliveryUnknown
}

func (runtime *Runtime) storeSessionStatus(sequence uint64, status SessionStatus) {
	runtime.statusMu.Lock()
	defer runtime.statusMu.Unlock()
	if sequence < runtime.statusSequence {
		return
	}
	runtime.statusSequence = sequence
	runtime.sessionStatus = status
}

type preparedRunRequest struct {
	request model.Request
	report  ContextReport
}

func (runtime *Runtime) prepareRunRequestContext(ctx context.Context, live *stateReducer, spec runRequestSpec, emit EmitFunc) (preparedRunRequest, error) {
	prepared := runtime.projectRunRequest(live.state, spec)
	for prepared.report.Window > 0 && prepared.report.TotalInputTokens > prepared.report.InputLimit {
		var err error
		prepared, err = runtime.shrinkRunRequestOnce(ctx, live, spec, prepared, emit)
		if err != nil {
			attached := 0
			if live.state.hasInputContext() {
				attached = estimateItemsTokens(live.state.modelItemsFromSequence(live.state.LastSequence+1, nil))
			}
			return prepared, &ContextLimitError{Report: prepared.report, AttachedTokens: attached, Cause: err}
		}
	}
	return prepared, nil
}

// shrinkRunRequestOnce advances durable context maintenance by one step on a
// successful return. Cheap pruning of old tool-result projections gets the
// first chance; if it cannot advance its boundary, the oldest available
// conversation prefix is summarized. Only when compaction cannot advance may
// the latest text results be shortened. The returned request and report use
// the committed state.
func (runtime *Runtime) shrinkRunRequestOnce(ctx context.Context, live *stateReducer, spec runRequestSpec, prepared preparedRunRequest, emit EmitFunc) (preparedRunRequest, error) {
	pruningRecord, err := runtime.pruneOldToolResultsOnce(ctx, live.state, spec, prepared.report)
	if err != nil {
		return prepared, fmt.Errorf("prune old tool results: %w", err)
	}
	message := "pruned old tool results"
	if pruningRecord.Sequence == 0 {
		_, compactErr := runtime.compactLocked(ctx, live, spec, emit)
		prepared = runtime.projectRunRequest(live.state, spec)
		if compactErr == nil {
			return prepared, nil
		}
		if !errors.Is(compactErr, errNoCompactionBoundary) && !errors.Is(compactErr, errCompactionNotNeeded) && !errors.Is(compactErr, errCompactionInputTooLarge) {
			return prepared, fmt.Errorf("compact context: %w", compactErr)
		}
		pruningRecord, err = runtime.pruneLatestToolResultsOnce(ctx, live.state, spec, prepared.report)
		if err != nil {
			return prepared, fmt.Errorf("prune latest tool results: %w", err)
		}
		if pruningRecord.Sequence == 0 {
			return prepared, fmt.Errorf("compact context: %w", compactErr)
		}
		message = "shortened oversized tool results"
	}
	if err := live.apply(pruningRecord); err != nil {
		return prepared, fmt.Errorf("apply committed tool-result pruning: %w", err)
	}
	runtime.publishSessionStatus(live.state)
	emitEvent(emit, Event{Sequence: pruningRecord.Sequence, Kind: EventToolResultsPruned, Text: message})
	return runtime.projectRunRequest(live.state, spec), nil
}

func (runtime *Runtime) requestReport(state State, spec runRequestSpec) ContextReport {
	return runtime.projectRunRequest(state, spec).report
}

func (runtime *Runtime) projectRunRequest(state State, spec runRequestSpec) preparedRunRequest {
	projection := runtime.itemProjection()
	request, pending := state.projectRunRequest(spec, projection)
	includeTools := !spec.omitTools
	window := runtime.modelInfo.ContextWindow
	if state.Configured == nil {
		request.Instructions = runtime.instructions
		if includeTools {
			for _, tool := range runtime.tools {
				request.Tools = append(request.Tools, tool.Spec)
			}
		}
	}
	report := estimateContextReport(state, window, request, pending, projection.OpaqueReasoningBytes)
	if includeTools {
		runtime.applyRequestMeasurement(state, &report)
	}
	return preparedRunRequest{request: request, report: report}
}

func (runtime *Runtime) applyRequestMeasurement(state State, report *ContextReport) {
	samples := state.contextSamples
	reset := runtime.scopeContextReset.Load()
	for index, sample := range samples {
		if sample.requestStart <= reset {
			samples[index] = contextSample{}
		}
	}
	report.applyMeasurement(samples)
}

func (state State) withPreviewInputs(inputs []Input) State {
	if len(inputs) == 0 {
		return state
	}
	state.Blocks = slices.Clone(state.Blocks)
	latest := state.latestInstructions()
	for index, input := range inputs {
		input = input.withNewInstructions(latest)
		sequence := state.LastSequence + uint64(index) + 1
		state.Blocks = append(state.Blocks, ConversationBlock{
			StartSequence: sequence, EndSequence: sequence, InputInstructions: input.Instructions,
			Entries: []ConversationEntry{{Sequence: sequence, Item: model.Item{Kind: model.ItemUserText, Text: input.modelText()}}},
		})
	}
	return state
}

func (state State) projectRunRequest(spec runRequestSpec, projection model.ItemProjection) (model.Request, int) {
	state = state.withPreviewInputs(spec.pendingInputs)
	return state.projectRequest(state.modelItems(), spec, projection)
}

func (state State) projectRequest(items []model.Item, spec runRequestSpec, projection model.ItemProjection) (model.Request, int) {
	pending := len(spec.pendingInputs)
	if spec.extraUserText != "" {
		items = append(items, model.Item{Kind: model.ItemUserText, Text: spec.extraUserText})
		pending++
	}
	return state.contextRequest(items, !spec.omitTools, projection), pending
}

// ContextReportForState estimates the next input using the last persisted
// configuration. It excludes transient queued input and does not reload host
// instructions. False means the journal has no effective configuration.
func ContextReportForState(state State, project func([]model.Item) []model.Item) (ContextReport, bool) {
	return state.ContextReport(model.ItemProjection{ProjectModelItems: project})
}

// ContextReport estimates the next request with the saved configuration and
// the selected route's item projection. ReplayWithContext also supplies usage
// measurements when a comparable request is available.
func (state State) ContextReport(projection model.ItemProjection) (ContextReport, bool) {
	if state.Configured == nil {
		return ContextReport{}, false
	}
	state.restoreContextSamples(projection)
	request := state.contextRequest(state.modelItems(), true, projection)
	report := estimateContextReport(state, state.Configured.RuntimePolicy.ContextWindow, request, 0, projection.OpaqueReasoningBytes)
	report.applyMeasurement(state.contextSamples)
	return report, true
}

func (runtime *Runtime) itemProjection() model.ItemProjection {
	var projection model.ItemProjection
	if runtime.backend != nil {
		projection.ProjectModelItems = runtime.backend.ProjectModelItems
	}
	if sizer, ok := runtime.backend.(model.OpaqueReasoningSizer); ok {
		projection.OpaqueReasoningBytes = sizer.OpaqueReasoningBytes
	}
	return projection
}

func (state State) contextRequest(items []model.Item, includeTools bool, projection model.ItemProjection) model.Request {
	request := model.Request{SessionID: state.SessionID, ProviderEpoch: state.Selection.Epoch}
	imageStatus := state.ImageDelivery.Status
	if config := state.Configured; config != nil {
		request.Instructions = config.ModelContext.Instructions
		if includeTools {
			request.Tools = config.ModelContext.Tools
		}
		if config.RuntimePolicy.ImageInputUnsupported {
			imageStatus = ImageDeliveryRejected
		}
	}
	if state.Compaction != nil {
		request.Summary = state.Compaction.Summary
	}
	items = (model.ProviderContext{Backend: state.Selection.Backend, Epoch: state.Selection.Epoch}).ProjectItems(items, nil)
	if projection.ProjectModelItems != nil {
		items = projection.ProjectModelItems(items)
	}
	request.Items = projectImagesForDelivery(items, imageStatus)
	return request
}

func estimateContextReport(state State, window int, request model.Request, pending int, opaqueBytes func([]model.Item, []int)) ContextReport {
	report := ContextReport{
		Window:           window,
		CompactionCount:  state.CompactionCount,
		ToolPruningCount: state.ToolPruningCount,
	}
	if window > 0 {
		reserve := min(max(window/5, minContextReserve), maxContextReserve)
		report.InputLimit = max(1, window-reserve)
	}
	report.InstructionTokens = estimateTextTokens(request.Instructions)
	if request.Instructions != "" {
		report.InstructionTokens += perMessageTokens
	}
	for _, tool := range request.Tools {
		report.ToolTokens += estimateTextTokens(tool.Name) + estimateTextTokens(tool.Description) + estimateTextTokens(string(tool.InputSchema)) + perToolDefinitionTokens
	}
	if request.Summary != "" {
		report.SummaryTokens = estimateTextTokens(model.ConversationSummaryPrefix+request.Summary) + perMessageTokens
	}
	// Adapter projections may drop reasoning only, so pending users remain the suffix.
	boundary := len(request.Items) - pending
	report.HistoryTokens = state.estimateProjectedTokens(request.Items[:boundary], opaqueBytes)
	report.PendingTokens = estimateItemsTokens(request.Items[boundary:])
	report.TotalInputTokens = report.localInputTokens()
	report.updateAvailableInput()
	return report
}

func (report ContextReport) localInputTokens() int {
	return report.InstructionTokens + report.ToolTokens + report.SummaryTokens + report.HistoryTokens + report.PendingTokens
}

func (report *ContextReport) updateAvailableInput() {
	if report.Window > 0 {
		report.AvailableInputTokens = max(0, report.InputLimit-report.TotalInputTokens)
	}
}

type responseTokenCount struct {
	items  int
	tokens int
}

func (state State) estimateProjectedTokens(items []model.Item, opaqueBytes func([]model.Item, []int)) int {
	if opaqueBytes == nil {
		return estimateItemsTokens(items)
	}
	tokens := 0
	for len(items) != 0 {
		end := 1
		id := items[0].ResponseID
		for id != "" && end < len(items) && items[end].ResponseID == id {
			end++
		}
		group := items[:end]
		items = items[end:]
		if !slices.ContainsFunc(group, func(item model.Item) bool { return len(item.ProviderData) != 0 }) {
			tokens += estimateItemsTokens(group)
			continue
		}
		sizes := make([]int, len(group))
		opaqueBytes(group, sizes)
		measured := state.responseTokens[id]
		if measured.items == len(group) && slices.ContainsFunc(sizes, func(size int) bool { return size > 0 }) {
			tokens += measured.tokens
			continue
		}
		for index, item := range group {
			if size := sizes[index]; item.Kind == model.ItemReasoning && size > 0 {
				tokens += max(estimateTextTokens(item.Text), size/4) + perMessageTokens
			} else {
				tokens += estimateItemTokens(item)
			}
		}
	}
	return tokens
}

func estimateItemsTokens(items []model.Item) int {
	tokens := 0
	for _, item := range items {
		tokens += estimateItemTokens(item)
	}
	return tokens
}

func estimateItemTokens(item model.Item) int {
	switch item.Kind {
	case model.ItemUserText:
		return estimateContentTokens(item.UserContent()) + perMessageTokens
	case model.ItemBoundaryText, model.ItemAssistantText:
		return estimateTextTokens(item.Text) + perMessageTokens
	case model.ItemReasoning:
		if item.Text != "" {
			return estimateTextTokens(item.Text) + perMessageTokens
		}
	case model.ItemToolCall:
		if item.ToolCall != nil {
			return estimateTextTokens(item.ToolCall.Name) + estimateTextTokens(item.ToolCall.RawArguments) + perToolCallTokens
		}
	case model.ItemToolResult:
		if item.ToolResult != nil {
			return estimateContentTokens(item.ToolResult.Content) + estimateTextTokens(item.ToolResult.CallID) + perToolResultTokens
		}
	}
	return 0
}

func estimateContentTokens(content model.Content) int {
	tokens := 0
	for _, part := range content {
		switch part.Kind {
		case model.ContentPartText:
			tokens += estimateTextTokens(part.Text)
		case model.ContentPartImage:
			if part.Image != nil {
				// Normalization bounds dimensions before content reaches this
				// layer. The 28x28 grid is a stable provider-neutral geometry
				// heuristic; exact visual-token accounting differs by route.
				tokens += ((part.Image.Width + 27) / 28) * ((part.Image.Height + 27) / 28)
			}
		}
	}
	return tokens
}

// estimateTextTokens intentionally favors a stable conservative estimate over
// a tokenizer dependency. ASCII prose/code averages about four bytes per
// token. Counting non-ASCII UTF-8 bytes in groups of three avoids treating
// every Cyrillic rune as a whole token while remaining conservative for CJK
// and other commonly three-byte scripts.
func estimateTextTokens(text string) int {
	if text == "" {
		return 0
	}
	ascii := 0
	nonASCIIBytes := 0
	for _, runeValue := range text {
		if runeValue < utf8.RuneSelf {
			ascii++
		} else {
			nonASCIIBytes += utf8.RuneLen(runeValue)
		}
	}
	return (ascii+3)/4 + (nonASCIIBytes+2)/3
}
