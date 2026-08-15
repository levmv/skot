package main

import (
	"encoding/json"
	"io"

	"github.com/levmv/skot/agent"
)

const jsonResultVersion = 1

// jsonResult is the stable one-shot boundary for scripts. It reports the
// product run rather than one provider response: a run may span retries, tool
// calls, compaction, and multiple model responses.
type jsonResult struct {
	Version          int              `json:"version"`
	Reply            string           `json:"reply"`
	Usage            agent.ModelUsage `json:"usage"`
	Status           agent.RunStatus  `json:"status"`
	DurationMillis   int64            `json:"duration_ms"`
	Model            string           `json:"model"`
	ReasoningEffort  string           `json:"reasoning_effort"`
	ToolSet          string           `json:"tool_set"`
	ModelAttempts    int              `json:"model_attempts"`
	RunID            string           `json:"run_id,omitempty"`
	SessionID        string           `json:"session_id,omitempty"`
	ToolLimitReached bool             `json:"tool_limit_reached,omitempty"`
	DetachedJobs     []string         `json:"detached_jobs,omitempty"`
	Error            string           `json:"error,omitempty"`
}

type jsonRunMetadata struct {
	DurationMillis  int64
	Model           string
	ReasoningEffort string
	ToolSet         string
	ModelAttempts   int
}

func writeJSONResult(output io.Writer, run agent.RunResult, usage agent.ModelUsage, sessionID string, metadata jsonRunMetadata, runErr error) error {
	result := jsonResult{
		Version:          jsonResultVersion,
		Reply:            run.Answer,
		Usage:            usage,
		Status:           run.Status,
		DurationMillis:   metadata.DurationMillis,
		Model:            metadata.Model,
		ReasoningEffort:  metadata.ReasoningEffort,
		ToolSet:          metadata.ToolSet,
		ModelAttempts:    metadata.ModelAttempts,
		RunID:            run.RunID,
		SessionID:        sessionID,
		ToolLimitReached: run.ToolLimitReached,
		DetachedJobs:     append([]string(nil), run.DetachedJobs...),
	}
	if runErr != nil {
		result.Error = runErr.Error()
	}
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(result)
}
