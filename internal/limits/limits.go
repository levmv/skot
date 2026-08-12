// Package limits contains product-wide emergency size ceilings. These are
// corruption and runaway-output safeguards, not normal response tuning.
package limits

const (
	// MaxPipedInputBytes bounds input read eagerly from stdin.
	MaxPipedInputBytes = 8 << 20

	// MaxModelCompletionBytes bounds the accumulated payload of one streamed
	// model completion, including text, reasoning, and tool-call JSON.
	MaxModelCompletionBytes = 16 << 20

	// MaxModelRequestBytes bounds one fully assembled provider request. Session
	// history itself remains unbounded and is managed by compaction/pruning.
	MaxModelRequestBytes = 128 << 20

	// MaxSSETokenBytes leaves room for protocol framing around a completion at
	// the aggregate limit while still bounding a single scanner token.
	MaxSSETokenBytes = MaxModelCompletionBytes + (64 << 10)

	// MaxJournalRecordBytes is deliberately much larger than a completion. It
	// covers JSON escaping in the worst case, large user input, and record
	// metadata without imposing a practical session-size limit.
	MaxJournalRecordBytes = MaxModelRequestBytes
)
