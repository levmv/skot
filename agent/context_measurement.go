package agent

import (
	"strconv"
	"strings"

	"github.com/levmv/skot/model"
)

type contextSample struct {
	requestStart uint64
	attemptID    string
	input        int
	local        int
	localKnown   bool
}

func (sample contextSample) predict(local int) int {
	if prediction := sample.input + (local - sample.local); prediction > 0 {
		return prediction
	}
	return local
}

func (runtime *Runtime) reduceContextRecords(records []Record) (*stateReducer, error) {
	live, err := foldRecords(records, true)
	if err == nil {
		runtime.restoreContextSamples(&live.state)
	}
	return live, err
}

func (runtime *Runtime) restoreContextSamples(state *State) {
	if runtime.backend == nil || state.Selection.Backend != runtime.modelInfo.BackendID ||
		state.Selection.ProviderStateContract != runtime.modelInfo.ProviderStateContract {
		state.contextSamples = [2]contextSample{}
		return
	}
	if state.SessionID == runtime.sessionID {
		for i, sample := range state.contextSamples {
			for _, saved := range runtime.lastContextSamples {
				if saved.localKnown && saved.requestStart == sample.requestStart &&
					saved.attemptID == sample.attemptID && saved.input == sample.input {
					state.contextSamples[i] = saved
					break
				}
			}
		}
	}
	state.restoreContextSamples(runtime.itemProjection())
}

func (state *State) restoreContextSamples(projection model.ItemProjection) {
	for index, sample := range state.contextSamples {
		if sample.input == 0 || sample.localKnown {
			continue
		}
		// All samples are from the current configuration and checkpoint. An
		// image observation can fall between the attempt and its acceptance.
		// Its resulting delivery mode describes the successful request.
		prefix := state.historyBefore(sample.requestStart)
		request := prefix.contextRequest(prefix.modelItems(), true, projection)
		sample.local = estimateContextReport(prefix, 0, request, 0, projection.OpaqueReasoningBytes).TotalInputTokens
		sample.localKnown = true
		state.contextSamples[index] = sample
	}
}

func (report *ContextReport) applyMeasurement(samples [2]contextSample) {
	report.TotalInputTokens = report.localInputTokens()
	report.MeasuredAdjustment = 0
	report.updateAvailableInput()
	previous, latest := samples[0], samples[1]
	if !latest.localKnown {
		return
	}
	if previous.localKnown {
		predicted := previous.predict(latest.local)
		low, high := min(predicted, latest.input), max(predicted, latest.input)
		// A gateway with an erratic input counter needs one consistent pair
		// before measurements can resume driving the context estimate.
		if high-low > minContextReserve && high-low > low {
			return
		}
	}
	report.TotalInputTokens = latest.predict(report.TotalInputTokens)
	report.MeasuredAdjustment = report.TotalInputTokens - report.localInputTokens()
	report.updateAvailableInput()
}

type contextAttempt struct {
	start  ModelAttemptRecord
	finish ModelAttemptRecord
	sample contextSample
}

// Attempt records are optional evidence. Bad or missing evidence falls back
// to local estimation and cannot invalidate semantic journal replay.
func (reducer *stateReducer) observeContextAttempt(record Record) {
	attempt, err := record.decode[ModelAttemptRecord]()
	if err != nil || attempt.RunID == "" || attempt.AttemptID == "" {
		return
	}
	reducer.runsWithAttempts[attempt.RunID] = true
	switch record.Kind {
	case RecordModelAttemptStarted:
		reducer.contextAttempts[attempt.AttemptID] = contextAttempt{
			start: attempt, sample: contextSample{requestStart: record.Sequence, attemptID: attempt.AttemptID},
		}
	case RecordModelAttemptFinished:
		measured := reducer.contextAttempts[attempt.AttemptID]
		measured.finish = attempt
		reducer.contextAttempts[attempt.AttemptID] = measured
	case RecordModelAttemptFailed:
		delete(reducer.contextAttempts, attempt.AttemptID)
	}
}

func (reducer *stateReducer) observeContextResponse(record Record, response ModelResponseRecord, block ConversationBlock) {
	state := &reducer.state
	attempt := reducer.contextAttempts[response.AttemptID]
	delete(reducer.contextAttempts, response.AttemptID)
	incomplete := model.IsIncompleteStopReason(response.StopReason)
	legacyFinalizing := response.ToolLimitFinal == nil && legacyToolLimitFinal(block)
	finalizing := legacyFinalizing || (response.ToolLimitFinal != nil && *response.ToolLimitFinal)
	legacy := response.AttemptID == "" && !reducer.runsWithAttempts[response.RunID] && !incomplete
	sample := contextSample{}
	output := 0
	switch {
	case attempt.finish.Outcome == ModelAttemptCompleted && attempt.finish.Usage.Status == model.UsageFinal &&
		contextAttemptMatches(attempt.finish, response):
		counts := attempt.finish.Usage.Tokens.Known()
		output = counts.OutputTokens
		if attempt.start.Outcome == ModelAttemptStarted && attempt.start.RequestID == attempt.finish.RequestID &&
			contextAttemptMatches(attempt.start, response) {
			sample = attempt.sample
			sample.input = counts.InputTokens
		}
	case legacy:
		sample = contextSample{requestStart: record.Sequence, input: response.Usage.InputTokens}
		output = response.Usage.OutputTokens
	}
	if output > 0 && !incomplete && !response.ReplayIncomplete && !legacyFinalizing {
		// Only opaque responses can use the measured output as a group.
		for _, item := range response.Items {
			if item.Kind == model.ItemReasoning && len(item.ProviderData) != 0 {
				state.responseTokens[item.ResponseID] = responseTokenCount{items: len(response.Items), tokens: output}
			}
		}
	}
	if response.StopReason == "user_shell" || finalizing {
		return
	}
	if sample.input <= 0 || sample.requestStart <= reducer.lastContextChange || state.Configured == nil {
		state.contextSamples = [2]contextSample{}
		return
	}
	state.contextSamples = [2]contextSample{state.contextSamples[1], sample}
}

func contextAttemptMatches(attempt ModelAttemptRecord, response ModelResponseRecord) bool {
	return attempt.Purpose == ModelRequestRun && attempt.RunID == response.RunID &&
		attempt.Backend == response.Backend && attempt.Model == response.Model && attempt.ProviderEpoch == response.Epoch
}

const toolLimitResultPrefix = "tool iteration limit reached after "

func legacyToolLimitFinal(block ConversationBlock) bool {
	// Older journals identify the final request by its rejected tool results.
	// A run that failed before sending that request still has its ordinary
	// tool-calling response immediately before these results.
	for index := len(block.Entries) - 1; index >= 0; index-- {
		result := block.Entries[index].Item.ToolResult
		if result == nil {
			return false
		}
		if result.Error && isToolLimitResult(result.Content.Text()) {
			return true
		}
	}
	return false
}

func isToolLimitResult(text string) bool {
	tool, count, ok := strings.Cut(text, " error: "+toolLimitResultPrefix)
	if !ok || !strings.HasPrefix(tool, "tool ") {
		return false
	}
	count, ok = strings.CutSuffix(count, " iterations")
	iterations, err := strconv.Atoi(count)
	return ok && err == nil && iterations >= 0
}
