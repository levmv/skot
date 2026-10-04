package model

import "strings"

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
