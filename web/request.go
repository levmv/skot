package web

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// HTTPError reports the service's HTTP status for a failed request.
type HTTPError struct{ StatusCode int }

func (err *HTTPError) Error() string { return fmt.Sprintf("HTTP %d", err.StatusCode) }

func (client *Client) doJSON(request *http.Request, target any) (Usage, error) {
	response, err := client.http.Do(request)
	if err != nil {
		return Usage{}, fmt.Errorf("request: %w", err)
	}
	defer response.Body.Close()
	failed := response.StatusCode < 200 || response.StatusCode >= 300
	limit := maxResponseBytes
	if failed {
		limit = 64 * 1024
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, int64(limit+1)))
	var usage Usage
	if len(raw) <= limit {
		// Read accounting separately so a content decoding error cannot erase it.
		usage = client.readUsage(raw)
	}
	if usage.RequestID == "" {
		usage.RequestID = response.Header.Get("X-Request-ID")
	}
	if failed {
		return usage, &HTTPError{StatusCode: response.StatusCode}
	}
	if readErr != nil {
		return usage, fmt.Errorf("read response: %w", readErr)
	}
	if len(raw) > limit {
		return usage, errors.New("response exceeds size limit")
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return usage, fmt.Errorf("decode response: %w", err)
	}
	return usage, nil
}
