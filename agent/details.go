package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const maxToolDetailsBytes = 256 * 1024

const (
	FileChangeDetailKind    = "file_change"
	ProcessResultDetailKind = "process_result"

	ProcessRunning    = "running"
	ProcessCompleted  = "completed"
	ProcessFailed     = "failed"
	ProcessKilled     = "killed"
	ProcessTimedOut   = "timed_out"
	ProcessNotStarted = "not_started"
	ProcessAbandoned  = "abandoned"
	ProcessUnknown    = "unknown"
)

type FileChange struct {
	Type       string         `json:"type"`
	Path       string         `json:"path"`
	Operation  string         `json:"operation"`
	Additions  int            `json:"additions"`
	Deletions  int            `json:"deletions"`
	Hunks      []FileDiffHunk `json:"hunks,omitempty"`
	TotalHunks int            `json:"total_hunks"`
	Truncated  bool           `json:"truncated,omitempty"`
}

type FileDiffHunk struct {
	OldStart int            `json:"old_start"`
	OldLines int            `json:"old_lines"`
	NewStart int            `json:"new_start"`
	NewLines int            `json:"new_lines"`
	Lines    []FileDiffLine `json:"lines"`
}

type FileDiffLine struct {
	Kind      string `json:"kind"`
	OldLine   int    `json:"old_line,omitempty"`
	NewLine   int    `json:"new_line,omitempty"`
	Text      string `json:"text"`
	NoNewline bool   `json:"no_newline,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type ProcessResult struct {
	JobID            string `json:"job_id,omitempty"`
	Status           string `json:"status"`
	Scope            string `json:"scope,omitempty"`
	ExitCode         *int   `json:"exit_code,omitempty"`
	DurationMillis   int64  `json:"duration_ms"`
	OutputBytes      int64  `json:"output_bytes"`
	DiscardedBytes   int64  `json:"discarded_bytes,omitempty"`
	OutputError      string `json:"output_error,omitempty"`
	FailureTail      string `json:"failure_tail,omitempty"`
	ManagedProcesses int    `json:"managed_processes,omitempty"`
	UserInitiated    bool   `json:"user_initiated,omitempty"`
	Detached         bool   `json:"detached,omitempty"`
}

// NewDetail encodes one durable, kind-discriminated tool-result payload.
func NewDetail(kind string, value any) (Detail, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return Detail{}, errors.New("detail kind is required")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return Detail{}, fmt.Errorf("encode %s detail: %w", kind, err)
	}
	return Detail{Kind: kind, Data: data}, nil
}

func FileChangeFromDetail(detail Detail) (FileChange, bool) {
	if detail.Kind != FileChangeDetailKind || len(detail.Data) == 0 {
		return FileChange{}, false
	}
	var change FileChange
	if err := json.Unmarshal(detail.Data, &change); err != nil ||
		change.Path == "" || change.Operation == "" ||
		change.Type != "" && change.Type != FileChangeDetailKind {
		return FileChange{}, false
	}
	return change, true
}

func ProcessResultFromDetail(detail Detail) (ProcessResult, bool) {
	if detail.Kind != ProcessResultDetailKind || len(detail.Data) == 0 {
		return ProcessResult{}, false
	}
	var result ProcessResult
	if err := json.Unmarshal(detail.Data, &result); err != nil || result.Status == "" {
		return ProcessResult{}, false
	}
	return result, true
}

func normalizeDetails(details []Detail) ([]Detail, error) {
	if len(details) == 0 {
		return nil, nil
	}
	normalized := make([]Detail, len(details))
	total := 0
	for index, detail := range details {
		detail.Kind = strings.TrimSpace(detail.Kind)
		if detail.Kind == "" {
			return nil, fmt.Errorf("detail %d has no kind", index)
		}
		if len(detail.Data) == 0 || !json.Valid(detail.Data) {
			return nil, fmt.Errorf("detail %d (%s) is not valid JSON", index, detail.Kind)
		}
		total += len(detail.Kind) + len(detail.Data)
		if total > maxToolDetailsBytes {
			return nil, errors.New("tool details exceed size limit")
		}
		normalized[index] = Detail{Kind: detail.Kind, Data: append(json.RawMessage(nil), detail.Data...)}
	}
	return normalized, nil
}

func cloneDetails(details []Detail) []Detail {
	if len(details) == 0 {
		return nil
	}
	cloned := make([]Detail, len(details))
	for index, detail := range details {
		cloned[index] = Detail{Kind: detail.Kind, Data: append(json.RawMessage(nil), detail.Data...)}
	}
	return cloned
}
