package app

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/levmv/skot/internal/modelconfig"
)

const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

func readCodexAccountQuota(ctx context.Context, authorizer modelconfig.CodexAuthorizer) (AccountQuota, [sha256.Size]byte, error) {
	tokens, err := authorizer.CurrentTokens(ctx)
	if err != nil {
		return AccountQuota{}, [sha256.Size]byte{}, err
	}
	credential := sha256.Sum256([]byte(tokens.AccessToken))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return AccountQuota{}, credential, err
	}
	modelconfig.SetCodexAccountHeaders(request, tokens)
	request.Header.Set("Accept", "application/json")
	observed := time.Now()
	client := authorizer.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := modelconfig.CodexHTTPClient(client).Do(request)
	if err != nil {
		return AccountQuota{}, credential, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		// The body can contain account details and is not diagnostic output.
		return AccountQuota{}, credential, fmt.Errorf("read ChatGPT allowance: HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil {
		return AccountQuota{}, credential, err
	}
	if len(body) > 1024*1024 {
		return AccountQuota{}, credential, errors.New("ChatGPT allowance response is too large")
	}
	quota, err := parseCodexAccountQuota(body, observed)
	return quota, credential, err
}

func parseCodexAccountQuota(body []byte, observed time.Time) (AccountQuota, error) {
	type window struct {
		UsedPercent *float64 `json:"used_percent"`
		Seconds     int64    `json:"limit_window_seconds"`
		ResetAt     int64    `json:"reset_at"`
	}
	var payload struct {
		RateLimit struct {
			Primary   *window `json:"primary_window"`
			Secondary *window `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return AccountQuota{}, errors.New("invalid ChatGPT allowance response")
	}
	// The weekly account bucket can be primary or secondary. Additional buckets
	// belong to particular models/features and must not replace the account one.
	for _, candidate := range []*window{payload.RateLimit.Primary, payload.RateLimit.Secondary} {
		if candidate == nil || candidate.Seconds != 7*24*60*60 || candidate.UsedPercent == nil ||
			*candidate.UsedPercent < 0 || *candidate.UsedPercent > 100 || candidate.ResetAt <= observed.Unix() {
			continue
		}
		return AccountQuota{
			RemainingPercent: 100 - *candidate.UsedPercent,
			Window:           7 * 24 * time.Hour,
			ResetsAt:         time.Unix(candidate.ResetAt, 0),
			ObservedAt:       observed,
		}, nil
	}
	return AccountQuota{}, nil
}
