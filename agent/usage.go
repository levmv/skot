package agent

import (
	"context"
	"fmt"
	"time"
)

// ReportedTokens preserves the difference between an absent counter and zero.
// Cache reads/writes are subsets of input; reasoning is a subset of output.
// TotalTokens is derived only when both input and output are known.
type ReportedTokens struct {
	InputTokens           *int `json:"input_tokens,omitzero"`
	CachedInputTokens     *int `json:"cached_input_tokens,omitzero"`
	CacheWriteInputTokens *int `json:"cache_write_input_tokens,omitzero"`
	OutputTokens          *int `json:"output_tokens,omitzero"`
	ReasoningTokens       *int `json:"reasoning_tokens,omitzero"`
	TotalTokens           *int `json:"total_tokens,omitzero"`
}

// Known returns the observed counters, using zero for absent fields.
// Keep ReportedTokens when the distinction between missing and zero matters.
func (tokens ReportedTokens) Known() ModelUsage {
	value := func(v *int) int {
		if v == nil {
			return 0
		}
		return *v
	}
	return ModelUsage{
		InputTokens: value(tokens.InputTokens), CachedInputTokens: value(tokens.CachedInputTokens),
		CacheWriteInputTokens: value(tokens.CacheWriteInputTokens), OutputTokens: value(tokens.OutputTokens),
		ReasoningTokens: value(tokens.ReasoningTokens), TotalTokens: value(tokens.TotalTokens),
	}
}

type UsageStatus string

const (
	UsageUnavailable UsageStatus = "unavailable"
	UsagePartial     UsageStatus = "partial"
	UsageFinal       UsageStatus = "final"
)

// ReportedCost preserves a decimal amount supplied by the provider.
// Amount preserves the JSON number's decimal representation (including zero).
// OpenRouter's account_charge is denominated in USD credits; BYOK requests can
// also report upstream_inference in USD. These are not automatically added together.
type ReportedCost struct {
	Kind     string `json:"kind"`
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// ModelUsageDetails describes the latest observed usage for one provider call.
// Final means a terminal usage snapshot was observed, not that every optional
// counter or monetary charge is available.
type ModelUsageDetails struct {
	Status     UsageStatus    `json:"status,omitempty"`
	Tokens     ReportedTokens `json:"tokens"`
	Costs      []ReportedCost `json:"costs,omitempty"`
	RequestID  string         `json:"request_id,omitempty"`
	ResponseID string         `json:"response_id,omitempty"`
	Model      string         `json:"model,omitempty"`
	Provider   string         `json:"provider,omitempty"`
}

type ModelAttemptOutcome string

const (
	ModelAttemptStarted   ModelAttemptOutcome = "started"
	ModelAttemptCompleted ModelAttemptOutcome = "completed"
	ModelAttemptFailed    ModelAttemptOutcome = "failed"
	ModelAttemptCancelled ModelAttemptOutcome = "cancelled"
)

// ModelAttempt combines the start and latest observation of one call. A start
// without a finish has an unknown outcome, including after a process crash.
type ModelAttempt struct {
	ModelAttemptRecord
	StartedSequence  uint64    `json:"started_sequence"`
	FinishedSequence uint64    `json:"finished_sequence,omitzero"`
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at,omitzero"`
}

// UsageReport is an accounting projection, independent of conversation replay.
// Usage sums known counts once per attempt plus LegacyUsage. Complete is true
// only when every included attempt has final input/output counts and there is
// no legacy history. Optional counters and costs may still be absent; inspect
// Attempts when computing prices. No monetary amounts are estimated or summed.
type UsageReport struct {
	Usage             ModelUsage     `json:"usage"`
	Complete          bool           `json:"complete"`
	Attempts          []ModelAttempt `json:"attempts"`
	LegacyUsage       ModelUsage     `json:"legacy_usage,omitzero"`
	LegacyResponses   int            `json:"legacy_responses,omitzero"`
	UntrackedAttempts int            `json:"untracked_attempts,omitzero"`
	LastSequence      uint64         `json:"last_sequence"`
}

// Usage returns attempts observed after afterSequence (zero selects the whole
// session). An attempt begun earlier is included if its finish is newer. The
// returned LastSequence can be used as the next checkpoint.
// While a call is in flight its start can appear in one report and its finish
// in a later report. Consumers polling during runs should upsert by AttemptID,
// rather than add overlapping observations of the same attempt.
// After cancelling a run, read with a live context such as context.WithoutCancel(ctx).
func (runtime *Runtime) Usage(ctx context.Context, afterSequence uint64) (UsageReport, error) {
	records, err := runtime.journal.Records(ctx)
	if err != nil {
		return UsageReport{}, err
	}
	return ReplayUsage(records, afterSequence)
}

// ReplayUsage reads accounting independently of the semantic journal schema.
// Required records retain their original usage for old readers and for recovery
// when auxiliary records were removed. AttemptID only correlates observations;
// neither the response nor its replay requires the accounting record to exist.
func ReplayUsage(records []Record, afterSequence uint64) (UsageReport, error) {
	report := UsageReport{Complete: true, Attempts: []ModelAttempt{}}
	attempts := make(map[string]int)
	var all []ModelAttempt
	for _, record := range records {
		if record.Sequence <= report.LastSequence {
			return UsageReport{}, fmt.Errorf("usage record sequence %d is not increasing", record.Sequence)
		}
		report.LastSequence = record.Sequence
		switch record.Kind {
		case RecordModelAttemptStarted, RecordModelAttemptFinished, RecordModelAttemptFailed:
			payload, err := record.decode[ModelAttemptRecord]()
			if err != nil {
				return UsageReport{}, err
			}
			if payload.AttemptID == "" {
				if record.Kind == RecordModelAttemptFailed && record.Sequence > afterSequence {
					report.UntrackedAttempts++
				}
				continue
			}
			index, exists := attempts[payload.AttemptID]
			if !exists {
				index = len(all)
				attempts[payload.AttemptID] = index
				all = append(all, ModelAttempt{})
			}
			attempt := &all[index]
			if record.Kind == RecordModelAttemptStarted {
				if exists {
					return UsageReport{}, fmt.Errorf("duplicate usage attempt %q", payload.AttemptID)
				}
				attempt.StartedSequence, attempt.StartedAt = record.Sequence, record.Time
			} else {
				if attempt.FinishedSequence != 0 {
					return UsageReport{}, fmt.Errorf("duplicate usage finish %q", payload.AttemptID)
				}
				attempt.FinishedSequence, attempt.FinishedAt = record.Sequence, record.Time
			}
			attempt.ModelAttemptRecord = payload
		}
	}
	for _, attempt := range all {
		if max(attempt.StartedSequence, attempt.FinishedSequence) <= afterSequence {
			continue
		}
		report.Attempts = append(report.Attempts, attempt)
		report.Usage = report.Usage.Add(attempt.Usage.Tokens.Known())
		if attempt.StartedSequence == 0 || attempt.FinishedSequence == 0 || attempt.Usage.Status != UsageFinal ||
			attempt.Usage.Tokens.InputTokens == nil || attempt.Usage.Tokens.OutputTokens == nil {
			report.Complete = false
		}
	}
	for _, record := range records {
		if record.Sequence <= afterSequence {
			continue
		}
		if record.Kind != RecordModelResponse && record.Kind != RecordContextCompacted {
			continue
		}
		// Accounting needs only these fields, not the conversation payload.
		payload, err := record.decode[struct {
			AttemptID  string     `json:"attempt_id"`
			Usage      ModelUsage `json:"usage"`
			StopReason string     `json:"stop_reason"`
		}]()
		if err != nil {
			return UsageReport{}, err
		}
		// A user shell command records a synthetic response without a model call.
		if record.Kind == RecordModelResponse && payload.StopReason == "user_shell" {
			continue
		}
		if index, ok := attempts[payload.AttemptID]; ok && all[index].FinishedSequence != 0 {
			continue
		}
		report.LegacyUsage = report.LegacyUsage.Add(payload.Usage)
		report.LegacyResponses++
	}
	report.Usage = report.Usage.Add(report.LegacyUsage)
	if report.LegacyResponses != 0 || report.UntrackedAttempts != 0 {
		report.Complete = false
	}
	return report, nil
}

func normalizeUsage(details ModelUsageDetails, legacy ModelUsage) ModelUsageDetails {
	// Old/custom backends can still supply ModelUsage. Non-zero values are known,
	// but zero cannot prove presence, and no terminal usage receipt was supplied.
	if details.Status == "" {
		details.Status = UsageUnavailable
		if legacy != (ModelUsage{}) {
			details.Status = UsagePartial
			known := func(v int) *int {
				if v == 0 {
					return nil
				}
				return &v
			}
			details.Tokens = ReportedTokens{
				InputTokens: known(legacy.InputTokens), CachedInputTokens: known(legacy.CachedInputTokens),
				CacheWriteInputTokens: known(legacy.CacheWriteInputTokens), OutputTokens: known(legacy.OutputTokens),
				ReasoningTokens: known(legacy.ReasoningTokens), TotalTokens: known(legacy.TotalTokens),
			}
		}
	}
	return details
}
