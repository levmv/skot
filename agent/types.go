package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type ItemKind string

const (
	ItemUserText      ItemKind = "user_text"
	ItemBoundaryText  ItemKind = "boundary_text"
	ItemAssistantText ItemKind = "assistant_text"
	ItemReasoning     ItemKind = "reasoning_summary"
	ItemToolCall      ItemKind = "tool_call"
	ItemToolResult    ItemKind = "tool_result"
)

type Item struct {
	Kind            ItemKind         `json:"kind"`
	ResponseID      string           `json:"response_id,omitempty"`
	ProviderContext *ProviderContext `json:"provider_context,omitempty"`
	// ProviderData carries bounded adapter-owned state whose opaque fields must
	// survive replay verbatim. Visible text remains in Text so it follows the
	// normal sanitization path. Ownership is inherited from ProviderContext; the
	// entries deliberately do not duplicate backend or epoch fields.
	ProviderData []ProviderData `json:"provider_data,omitempty"`
	Text         string         `json:"text,omitempty"`
	ToolCall     *ToolCall      `json:"tool_call,omitempty"`
	ToolResult   *ToolResult    `json:"tool_result,omitempty"`
}

type ProviderContext struct {
	Backend string `json:"backend"`
	Epoch   string `json:"epoch"`
}

type ProviderData struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type ToolCall struct {
	ID                 string              `json:"id"`
	Name               string              `json:"name"`
	RawArguments       string              `json:"raw_arguments"`
	ProviderReferences []ProviderReference `json:"provider_references,omitempty"`
}

// ProviderReference carries adapter-owned replay identity in journaled tool
// calls. Kind identifies the adapter payload; Backend and Epoch bind it to a
// provider selection. Empty Backend and Epoch identify a legacy reference.
type ProviderReference struct {
	Kind    string          `json:"kind"`
	Backend string          `json:"backend"`
	Epoch   string          `json:"epoch"`
	Data    json.RawMessage `json:"data"`
}

type ToolResult struct {
	CallID  string   `json:"call_id"`
	Content string   `json:"content"`
	Details []Detail `json:"details,omitempty"`
	Error   bool     `json:"error,omitempty"`
	Unknown bool     `json:"unknown,omitempty"`
}

type Detail struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type ToolSpec struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	ParallelSafe bool            `json:"parallel_safe,omitempty"`
}

type ToolOutput struct {
	Content string
	Details []Detail
}

type Tool struct {
	Spec ToolSpec
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
}

// ExternalWork connects asynchronous work to journaled model
// boundaries. Implementations may keep richer state outside the journal, but
// must acknowledge events and tool results only through the commit callbacks.
// Await reports whether a new model request is required before the preceding
// response may be accepted as final.
type ExternalWork interface {
	Status(id string) ([]Detail, bool)
	PendingEvents(sessionID string) []BoundaryEvent
	EventCommitted(jobID string)
	ToolResultCommitted(ToolResult)
	Await(context.Context, string) (bool, error)
}

// DetachedExternalWork is an optional extension for work allowed to survive
// the application process. Keeping it separate means an ExternalWork source
// that has no such lifecycle does not need to implement a meaningless method.
type DetachedExternalWork interface {
	DetachedJobs(sessionID string) []string
}

type ModelRequest struct {
	SessionID     string
	ProviderEpoch string
	Instructions  string
	Summary       string
	Items         []Item
	Tools         []ToolSpec
	// StreamIdleTimeout bounds silence between provider stream payloads. Zero
	// leaves this concern to the model implementation or caller context.
	StreamIdleTimeout time.Duration
}

type ModelInfo struct {
	Backend         string `json:"backend"`
	Provider        string `json:"provider,omitempty"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// ProviderStateContract is an adapter-owned, human-readable identifier for
	// the rules used to create and replay opaque provider state. A change must
	// rotate the session epoch even when the backend and model stay the same.
	ProviderStateContract  ProviderStateContract `json:"provider_state_contract,omitempty"`
	ContextWindow          int                   `json:"context_window,omitempty"`
	ContextWindowEstimated bool                  `json:"context_window_estimated,omitempty"`
	MaxRequestBytes        int                   `json:"max_request_bytes,omitempty"`
	MaxCompletionBytes     int                   `json:"max_completion_bytes,omitempty"`
	// Endpoint identifies the effective provider endpoint without credentials.
	// It is journaled diagnostic metadata, not authorization configuration.
	Endpoint string `json:"endpoint,omitempty"`
}

type ModelResponse struct {
	Items      []Item
	Usage      ModelUsage
	StopReason string
}

const StopReasonOutputLimit = "output_limit"

type ModelUsage struct {
	InputTokens int `json:"input_tokens,omitempty"`
	// CachedInputTokens is the cached subset of InputTokens, not additional
	// input.
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`
	OutputTokens      int `json:"output_tokens,omitempty"`
	// ReasoningTokens is the reported reasoning subset of OutputTokens, not an
	// additional token count.
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
	// TotalTokens counts InputTokens plus OutputTokens. Adapters must preserve
	// that invariant when mapping provider usage.
	TotalTokens int `json:"total_tokens,omitempty"`
}

func (usage ModelUsage) Add(other ModelUsage) ModelUsage {
	return ModelUsage{
		InputTokens:       usage.InputTokens + other.InputTokens,
		CachedInputTokens: usage.CachedInputTokens + other.CachedInputTokens,
		OutputTokens:      usage.OutputTokens + other.OutputTokens,
		ReasoningTokens:   usage.ReasoningTokens + other.ReasoningTokens,
		TotalTokens:       usage.TotalTokens + other.TotalTokens,
	}
}

type ModelStreamEvent struct {
	Kind EventKind
	Text string
}

type Model interface {
	Info() ModelInfo
	Complete(context.Context, ModelRequest, func(ModelStreamEvent)) (ModelResponse, error)
	// ProjectModelItems applies adapter replay policy after runtime ownership
	// filtering. It may filter reasoning items in place, but must preserve order
	// and every other item, and must not retain the slice. Runtime uses the result
	// for both request serialization and context estimates.
	ProjectModelItems([]Item) []Item
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
	Call      *ToolCall
	Result    *ToolResult
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

// IsIncompleteStopReason reports whether a normalized adapter stop reason
// represents a partial response. Adapters reject unknown provider reasons.
func IsIncompleteStopReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length", "max_tokens", "max_output_tokens", "content_filter", "refusal", "pause_turn",
		"model_context_window_exceeded", "incomplete", "insufficient_system_resource", StopReasonOutputLimit:
		return true
	default:
		return false
	}
}

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

// ProviderStateContract is intentionally readable in journal records. Agent
// compares it for epoch ownership but does not interpret adapter semantics.
type ProviderStateContract string

const (
	RecordSessionStarted    RecordKind = "session_started"
	RecordModelSelected     RecordKind = "model_selected"
	RecordSessionConfigured RecordKind = "session_configured"
	RecordRunStarted        RecordKind = "run_started"
	RecordRunInputAdded     RecordKind = "run_input_added"
	RecordModelResponse     RecordKind = "model_response"
	RecordToolResult        RecordKind = "tool_result"
	RecordBoundaryEvent     RecordKind = "boundary_event"
	RecordRunFinished       RecordKind = "run_finished"
	RecordContextCompacted  RecordKind = "context_compacted"
	RecordToolResultsPruned RecordKind = "tool_results_pruned"

	// RecordModelAttemptFailed is observational: it preserves diagnostics for
	// one failed provider call without affecting the replayed session state.
	RecordModelAttemptFailed RecordKind = "aux/model_attempt_failed"
)

type Record struct {
	Sequence uint64          `json:"sequence"`
	Time     time.Time       `json:"time"`
	Kind     RecordKind      `json:"kind"`
	Data     json.RawMessage `json:"data"`
}

type PendingRecord struct {
	Kind RecordKind
	Data json.RawMessage
}

type ModelRequestPurpose string

const (
	ModelRequestRun        ModelRequestPurpose = "run"
	ModelRequestCompaction ModelRequestPurpose = "compaction"
)

// ModelAttemptFailedRecord preserves one failed provider call, including calls
// recovered by a later retry. ProviderError is absent for failures without
// structured provider metadata.
type ModelAttemptFailedRecord struct {
	RequestID      string                     `json:"request_id"`
	RunID          string                     `json:"run_id,omitempty"`
	Purpose        ModelRequestPurpose        `json:"purpose"`
	Attempt        int                        `json:"attempt"`
	Backend        string                     `json:"backend"`
	Provider       string                     `json:"provider,omitempty"`
	Model          string                     `json:"model"`
	ProviderEpoch  string                     `json:"provider_epoch,omitempty"`
	Error          string                     `json:"error"`
	ErrorTruncated bool                       `json:"error_truncated,omitempty"`
	ProviderError  *ModelAttemptProviderError `json:"provider_error,omitempty"`
}

type ModelAttemptProviderError struct {
	StatusCode int               `json:"status_code,omitempty"`
	Kind       ProviderErrorKind `json:"kind,omitempty"`
	Code       string            `json:"code,omitempty"`
	Type       string            `json:"type,omitempty"`
	Retryable  bool              `json:"retryable"`
	RetryAfter string            `json:"retry_after,omitempty"`
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

type ModelSelectedRecord struct {
	Backend               string                `json:"backend"`
	Provider              string                `json:"provider"`
	Model                 string                `json:"model"`
	ReasoningEffort       string                `json:"reasoning_effort,omitempty"`
	ProviderStateContract ProviderStateContract `json:"provider_state_contract,omitempty"`
	Epoch                 string                `json:"epoch"`
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
	Instructions           string     `json:"instructions,omitempty"`
	CompactionInstructions string     `json:"compaction_instructions"`
	ToolLimitInstructions  string     `json:"tool_limit_instructions"`
	ToolSet                string     `json:"tool_set,omitempty"`
	Tools                  []ToolSpec `json:"tools,omitempty"`
}

type RuntimePolicySnapshot struct {
	ContextWindow          int  `json:"context_window,omitempty"`
	ContextWindowEstimated bool `json:"context_window_estimated,omitempty"`
	// MaxModelAttempts is -1 when attempts are bounded only by RetryBudget.
	MaxModelAttempts  int    `json:"max_model_attempts"`
	RetryBudget       string `json:"retry_budget,omitempty"`
	RetryBaseDelay    string `json:"retry_base_delay,omitempty"`
	RetryMaxDelay     string `json:"retry_max_delay,omitempty"`
	StreamIdleTimeout string `json:"stream_idle_timeout,omitempty"`
	// MaxToolIterations is -1 when the emergency fuse is disabled.
	MaxToolIterations  int `json:"max_tool_iterations"`
	MaxRequestBytes    int `json:"max_request_bytes,omitempty"`
	MaxCompletionBytes int `json:"max_completion_bytes,omitempty"`
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
	Modified *bool  `json:"modified,omitempty"`
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
	ParallelSafe     bool     `json:"parallel_safe,omitempty"`
	Background       string   `json:"background"`
	Yield            string   `json:"yield,omitempty"`
	Detach           bool     `json:"detach,omitempty"`
	EnvironmentNames []string `json:"environment_names,omitempty"`
}

// ScopeSnapshot describes an execution boundary owned and enforced outside
// agent.Runtime. The runtime records it but does not interpret or implement it.
type ScopeSnapshot struct {
	RequestedScope     string `json:"requested_scope,omitempty"`
	EffectiveScope     string `json:"effective_scope,omitempty"`
	ProtectedPathCount int    `json:"protected_path_count,omitempty"`
	Backend            string `json:"backend,omitempty"`
	Container          string `json:"container,omitempty"`
	Network            string `json:"network,omitempty"`
}

type RunInputAddedRecord struct {
	RunID string `json:"run_id"`
	Text  string `json:"text"`
}

type ModelResponseRecord struct {
	RunID      string     `json:"run_id"`
	Backend    string     `json:"backend"`
	Model      string     `json:"model"`
	Epoch      string     `json:"epoch"`
	Items      []Item     `json:"items"`
	Usage      ModelUsage `json:"usage,omitempty"`
	StopReason string     `json:"stop_reason,omitempty"`
}

type ToolResultRecord struct {
	RunID  string     `json:"run_id"`
	Result ToolResult `json:"result"`
}

type BoundaryEventRecord struct {
	RunID      string    `json:"run_id"`
	JobID      string    `json:"job_id"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	Content    string    `json:"content"`
}

type RunFinishedRecord struct {
	RunID            string    `json:"run_id"`
	Status           RunStatus `json:"status"`
	Error            string    `json:"error,omitempty"`
	ToolLimitReached bool      `json:"tool_limit_reached,omitempty"`
	// Nil means the lifecycle was not observed (for example reconciliation
	// after a crash); an empty non-nil slice authoritatively means none were
	// left running.
	DetachedJobs []string `json:"detached_jobs"`
}

type ContextCompactedRecord struct {
	CoveredThroughSequence uint64     `json:"covered_through_sequence"`
	FirstVerbatimSequence  uint64     `json:"first_verbatim_sequence"`
	Summary                string     `json:"summary"`
	Usage                  ModelUsage `json:"usage,omitempty"`
}

// ToolResultsPrunedRecord is a journaled model-context policy. Full tool output
// remains in its original journal record.
type ToolResultsPrunedRecord struct {
	ThroughSequence uint64 `json:"through_sequence"`
	HeadBytes       int    `json:"head_bytes"`
	TailBytes       int    `json:"tail_bytes"`
}

var (
	ErrEmptyInput    = errors.New("input is empty")
	ErrRunActive     = errors.New("a run is already active")
	ErrToolLoopLimit = errors.New("tool loop limit reached")
)

func normalizeInput(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", ErrEmptyInput
	}
	return input, nil
}
