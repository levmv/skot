package agent

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"strings"
	"time"

	"github.com/levmv/skot/model"
)

type ToolOutput struct {
	Content model.Content
	Details []model.Detail
}

type Tool struct {
	Spec model.ToolSpec
	Run  func(context.Context, string) (ToolOutput, error)
}

type ShellFunc func(context.Context, string) (ToolOutput, error)

// BoundaryEvent is product-owned context delivered between model rounds.
// JobID is an idempotency key for its external source; FinishedAt records
// when the work ended rather than when the journal delivered the event.
type BoundaryEvent struct {
	JobID      string
	FinishedAt time.Time
	Content    string
	Details    []model.Detail
}

// ExternalWork connects asynchronous work to journaled model
// boundaries. Implementations may keep richer state outside the journal, but
// must acknowledge events and tool results only through the commit callbacks.
// Await reports whether a new model request is required before the preceding
// response may be accepted as final. DetachedJobs reports work which makes the
// session durable beyond the current application process; implementations with
// no such work return nil.
type ExternalWork interface {
	Status(id string) ([]model.Detail, bool)
	PendingEvents(sessionID string) []BoundaryEvent
	EventCommitted(jobID string)
	ToolResultCommitted(model.ToolResult)
	Await(context.Context, string) (bool, error)
	DetachedJobs(sessionID string) []string
}

type EventKind string

const (
	EventRunStarted            EventKind = "run_started"
	EventModelAttemptStarted   EventKind = "model_attempt_started"
	EventTextDelta             EventKind = "text_delta"
	EventReasoningSummaryDelta EventKind = "reasoning_summary_delta"
	EventModelAttemptDiscarded EventKind = "model_attempt_discarded"
	EventModelRetryScheduled   EventKind = "model_retry_scheduled"
	EventToolStarted           EventKind = "tool_started"
	EventToolFinished          EventKind = "tool_finished"
	EventToolRejected          EventKind = "tool_rejected"
	EventQueuedInputDelivered  EventKind = "queued_input_delivered"
	EventBoundaryDelivered     EventKind = "boundary_delivered"
	EventStatus                EventKind = "status"
	EventContextCompacted      EventKind = "context_compacted"
	EventToolResultsPruned     EventKind = "tool_results_pruned"
	EventRunFinished           EventKind = "run_finished"
)

type Event struct {
	// Sequence is non-zero when the event reports a fact already committed to
	// the journal; it then equals that record's sequence. Such events are
	// authoritative: a consumer holding State at LastSequence N applies them
	// idempotently by ignoring Sequence <= N, and can always recover the full
	// state from Replay(Records()).
	//
	// Sequence zero marks a transient progress hint. Hints carry no journaled
	// fact, may be dropped, coalesced, or never delivered at all, and must never
	// be the only source of rendered state.
	Sequence  uint64
	Kind      EventKind
	RunID     string
	AttemptID string
	Text      string
	Call      *model.ToolCall
	Result    *model.ToolResult
	Details   []model.Detail
	Status    RunStatus
	// ToolLimitReached reports that the run hit its model-to-tool iteration
	// fuse. A completed run may still carry this diagnostic marker.
	ToolLimitReached bool
	DetachedJobs     []string
}

type EmitFunc func(Event)

type RunStatus string

const (
	RunCompleted   RunStatus = "completed"
	RunFailed      RunStatus = "failed"
	RunIncomplete  RunStatus = "incomplete"
	RunCancelled   RunStatus = "cancelled"
	RunInterrupted RunStatus = "interrupted"
)

var ErrRunIncomplete = errors.New("run is incomplete")

// RunIncompleteError reports a response that ended deliberately but did not
// constitute a complete answer. Any partial assistant text remains journaled and
// available through RunResult and Replay.
type RunIncompleteError struct {
	StopReason string
}

func (err RunIncompleteError) Error() string {
	reason := strings.TrimSpace(err.StopReason)
	if reason == "" {
		return ErrRunIncomplete.Error()
	}
	return "model response is incomplete: stop reason " + reason
}

func (RunIncompleteError) Unwrap() error { return ErrRunIncomplete }

func validRunStatus(status RunStatus) bool {
	switch status {
	case RunCompleted, RunFailed, RunIncomplete, RunCancelled, RunInterrupted:
		return true
	default:
		return false
	}
}

type RunResult struct {
	RunID            string
	Answer           string
	Status           RunStatus
	ToolLimitReached bool
	DetachedJobs     []string
}

type RecordKind string

// JournalSchemaVersion versions the required semantic projection of a journal,
// not its exact set of record kinds or optional payload fields. Incompatible
// semantic changes require an explicit migration rather than best-effort replay.
const JournalSchemaVersion = 3

const auxiliaryRecordKindPrefix = "aux/"

const (
	RecordSessionStarted        RecordKind = "session_started"
	RecordModelSelected         RecordKind = "model_selected"
	RecordSessionConfigured     RecordKind = "session_configured"
	RecordRunStarted            RecordKind = "run_started"
	RecordRunInputAdded         RecordKind = "run_input_added"
	RecordModelResponse         RecordKind = "model_response"
	RecordToolResult            RecordKind = "tool_result"
	RecordBoundaryEvent         RecordKind = "boundary_event"
	RecordRunFinished           RecordKind = "run_finished"
	RecordContextCompacted      RecordKind = "context_compacted"
	RecordToolResultsPruned     RecordKind = "tool_results_pruned"
	RecordImageDeliveryObserved RecordKind = "image_delivery_observed"

	// RecordModelAttemptFailed is observational: it preserves diagnostics for
	// one failed provider call without affecting the replayed session state.
	RecordModelAttemptFailed   RecordKind = "aux/model_attempt_failed"
	RecordModelAttemptStarted  RecordKind = "aux/model_attempt_started"
	RecordModelAttemptFinished RecordKind = "aux/model_attempt_finished"
)

type Record struct {
	Sequence uint64         `json:"sequence"`
	Time     time.Time      `json:"time"`
	Kind     RecordKind     `json:"kind"`
	Data     jsontext.Value `json:"data"`
}

type PendingRecord struct {
	Kind RecordKind
	Data jsontext.Value
}

type ModelRequestPurpose string

const (
	ModelRequestRun        ModelRequestPurpose = "run"
	ModelRequestCompaction ModelRequestPurpose = "compaction"
)

// ModelAttemptRecord describes a provider call. Attempts with the same RequestID
// are retries of one logical request; AttemptID identifies each individual call.
// Usage is independent of the outcome: a failed call can have final usage.
type ModelAttemptRecord struct {
	AttemptID      string                     `json:"attempt_id,omitempty"`
	RequestID      string                     `json:"request_id"`
	RunID          string                     `json:"run_id,omitempty"`
	Purpose        ModelRequestPurpose        `json:"purpose"`
	Attempt        int                        `json:"attempt"`
	Backend        string                     `json:"backend"`
	Provider       string                     `json:"provider,omitempty"`
	Model          string                     `json:"model"`
	ProviderEpoch  string                     `json:"provider_epoch,omitempty"`
	Error          string                     `json:"error,omitempty"`
	ErrorTruncated bool                       `json:"error_truncated,omitzero"`
	ProviderError  *ModelAttemptProviderError `json:"provider_error,omitzero"`
	Outcome        ModelAttemptOutcome        `json:"outcome,omitempty"`
	Usage          model.Usage                `json:"usage,omitzero"`
}

// ModelAttemptFailedRecord is the payload of a failed-attempt diagnostic record.
type ModelAttemptFailedRecord = ModelAttemptRecord

type ModelAttemptProviderError struct {
	StatusCode int                     `json:"status_code,omitzero"`
	Kind       model.ProviderErrorKind `json:"kind,omitempty"`
	Code       string                  `json:"code,omitempty"`
	Type       string                  `json:"type,omitempty"`
	Retryable  bool                    `json:"retryable"`
	RetryAfter string                  `json:"retry_after,omitempty"`
}

type Journal interface {
	// Append adds exactly one complete record and returns it with a non-zero,
	// strictly increasing sequence and timestamp. A successful return means a
	// later Records call observes the record; stable-storage checkpoint policy
	// belongs to the Journal implementation rather than the agent runtime.
	Append(context.Context, PendingRecord) (Record, error)
	// Records returns a consistent snapshot in strictly increasing sequence
	// order. Returned records must not alias storage that may mutate later.
	Records(context.Context) ([]Record, error)
}

type RunStartedRecord struct {
	RunID string `json:"run_id"`
}

type SessionStartedRecord struct {
	SchemaVersion int    `json:"schema_version"`
	SessionID     string `json:"session_id"`
	Workspace     string `json:"workspace,omitempty"`
}

// EffectiveConfigSnapshot is the secret-free configuration under which
// subsequent session work runs. Its JSON shape is versioned by
// JournalSchemaVersion. Values are effective values after defaults and
// resolution, not the raw flags used to obtain them. A new snapshot is journaled
// before a changed configuration is used; identical snapshots are omitted.
type EffectiveConfigSnapshot struct {
	ModelContext  ModelContextSnapshot         `json:"model_context"`
	RuntimePolicy RuntimePolicySnapshot        `json:"runtime_policy"`
	Environment   ExecutionEnvironmentSnapshot `json:"environment"`
}

type ModelContextSnapshot struct {
	Instructions           string           `json:"instructions,omitempty"`
	CompactionInstructions string           `json:"compaction_instructions"`
	ToolLimitInstructions  string           `json:"tool_limit_instructions"`
	ToolSet                string           `json:"tool_set,omitempty"`
	Tools                  []model.ToolSpec `json:"tools,omitempty"`
}

type RuntimePolicySnapshot struct {
	ContextWindow          int  `json:"context_window,omitzero"`
	ContextWindowEstimated bool `json:"context_window_estimated,omitzero"`
	ImageInputUnsupported  bool `json:"image_input_unsupported,omitzero"`
	// MaxModelAttempts is -1 when attempts are bounded only by RetryBudget.
	MaxModelAttempts  int    `json:"max_model_attempts"`
	RetryBudget       string `json:"retry_budget,omitempty"`
	RetryBaseDelay    string `json:"retry_base_delay,omitempty"`
	RetryMaxDelay     string `json:"retry_max_delay,omitempty"`
	StreamIdleTimeout string `json:"stream_idle_timeout,omitempty"`
	// MaxToolIterations is -1 when the emergency fuse is disabled.
	MaxToolIterations  int `json:"max_tool_iterations"`
	MaxRequestBytes    int `json:"max_request_bytes,omitzero"`
	MaxCompletionBytes int `json:"max_completion_bytes,omitzero"`
	// AwaitRequiredJobs preserves foreground semantics for commands that only
	// became managed jobs because they exceeded the synchronous yield.
	AwaitRequiredJobs bool `json:"await_required_jobs"`
}

type ExecutionEnvironmentSnapshot struct {
	Endpoint     string                `json:"endpoint,omitempty"`
	Build        BuildSnapshot         `json:"build,omitzero"`
	Scope        ScopeSnapshot         `json:"scope"`
	ProgramTools []ProgramToolSnapshot `json:"program_tools,omitempty"`
}

// BuildSnapshot identifies the host application build which produced an
// effective configuration. Modified is nil when VCS status is unavailable,
// false for a clean tree, and true for a dirty tree.
type BuildSnapshot struct {
	Version  string `json:"version,omitempty"`
	Revision string `json:"revision,omitempty"`
	Modified *bool  `json:"modified,omitzero"`
}

// ProgramToolSnapshot records how an executable-backed tool was resolved for
// this session. Environment values are deliberately absent: variable names are
// enough to explain the interface without journaling credentials.
type ProgramToolSnapshot struct {
	Name             string   `json:"name"`
	Program          string   `json:"program"`
	Command          []string `json:"command"`
	Workdir          string   `json:"workdir,omitempty"`
	Timeout          string   `json:"timeout"`
	ParallelSafe     bool     `json:"parallel_safe,omitzero"`
	Background       string   `json:"background"`
	Yield            string   `json:"yield,omitempty"`
	Detach           bool     `json:"detach,omitzero"`
	EnvironmentNames []string `json:"environment_names,omitempty"`
}

// ScopeSnapshot describes an execution boundary owned and enforced outside
// agent.Runtime. The runtime records it but does not interpret or implement it.
type ScopeSnapshot struct {
	// Scope keeps the historical "effective_scope" JSON name so schema-v3
	// journals remain compatible after requested and effective scopes merged.
	Scope          string   `json:"effective_scope,omitempty"`
	AddedPaths     []string `json:"added_paths,omitempty"`
	ProtectedPaths []string `json:"protected_paths,omitempty"`
	Backend        string   `json:"backend,omitempty"`
	Network        string   `json:"network,omitempty"`
}

type RunInputAddedRecord struct {
	RunID string `json:"run_id"`
	Text  string `json:"text"`
}

type ModelResponseRecord struct {
	// Optional correlation for accounting; semantic replay never depends on it.
	AttemptID  string            `json:"attempt_id,omitempty"`
	RunID      string            `json:"run_id"`
	Backend    string            `json:"backend"`
	Model      string            `json:"model"`
	Epoch      string            `json:"epoch"`
	Items      []model.Item      `json:"items"`
	Usage      model.TokenCounts `json:"usage"`
	StopReason string            `json:"stop_reason,omitempty"`
}

type ToolResultRecord struct {
	RunID  string           `json:"run_id"`
	Result model.ToolResult `json:"result"`
}

type ImageDeliveryStatus string

const (
	ImageDeliveryUnknown  ImageDeliveryStatus = ""
	ImageDeliveryAccepted ImageDeliveryStatus = "accepted"
	ImageDeliveryRejected ImageDeliveryStatus = "rejected"
)

// ImageDeliveryObservedRecord is session evidence for one provider epoch. It
// is deliberately narrower than a model capability declaration: accepted
// means only that the route accepted a request containing canonical images.
type ImageDeliveryObservedRecord struct {
	ProviderEpoch string              `json:"provider_epoch"`
	Status        ImageDeliveryStatus `json:"status"`
}

type BoundaryEventRecord struct {
	RunID      string         `json:"run_id"`
	JobID      string         `json:"job_id"`
	FinishedAt time.Time      `json:"finished_at,omitzero"`
	Content    string         `json:"content"`
	Details    []model.Detail `json:"details,omitempty"`
}

type RunFinishedRecord struct {
	RunID            string    `json:"run_id"`
	Status           RunStatus `json:"status"`
	Error            string    `json:"error,omitempty"`
	ToolLimitReached bool      `json:"tool_limit_reached,omitzero"`
	// Nil means the lifecycle was not observed (for example reconciliation
	// after a crash); an empty non-nil slice authoritatively means none were
	// left running.
	DetachedJobs []string `json:"detached_jobs"`
}

type ContextCompactedRecord struct {
	AttemptID              string            `json:"attempt_id,omitempty"`
	CoveredThroughSequence uint64            `json:"covered_through_sequence"`
	FirstVerbatimSequence  uint64            `json:"first_verbatim_sequence"`
	Summary                string            `json:"summary"`
	Usage                  model.TokenCounts `json:"usage"`
}

// ToolResultsPrunedRecord is a journaled model-context policy. Full tool output
// remains in its original journal record.
type ToolResultsPrunedRecord struct {
	ThroughSequence uint64 `json:"through_sequence"`
	HeadBytes       int    `json:"head_bytes"`
	TailBytes       int    `json:"tail_bytes"`
}

var ErrRunActive = errors.New("a run is already active")

func normalizeInput(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", errors.New("input is empty")
	}
	return input, nil
}
