package modelhttp

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"time"
)

// GoogleRetryDelay reads google.rpc.RetryInfo from an error's details array.
// RetryInfo specifies a minimum delay, so keep the longest recognized value.
// Malformed or unrelated details do not invalidate the provider error.
func GoogleRetryDelay(raw jsontext.Value) time.Duration {
	var details []jsontext.Value
	if err := json.Unmarshal(raw, &details); err != nil {
		return 0
	}
	var delay time.Duration
	for _, rawDetail := range details {
		var detail struct {
			Type       string `json:"@type"`
			RetryDelay string `json:"retryDelay"`
		}
		if json.Unmarshal(rawDetail, &detail) != nil || detail.Type != "type.googleapis.com/google.rpc.RetryInfo" {
			continue
		}
		parsed, err := time.ParseDuration(detail.RetryDelay)
		if err == nil && parsed > 0 {
			delay = max(delay, parsed)
		}
	}
	return delay
}
