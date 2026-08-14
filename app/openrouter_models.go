package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const openRouterModelsURL = "https://openrouter.ai/api/v1"

var openRouterMetadataClient = &http.Client{Timeout: 2 * time.Second}

type modelContextLookup func(context.Context, string) (int, error)

func openRouterContextWindow(ctx context.Context, modelID string) (int, error) {
	return fetchOpenRouterContextWindow(ctx, openRouterMetadataClient, openRouterModelsURL, modelID)
}

func fetchOpenRouterContextWindow(ctx context.Context, client *http.Client, baseURL, modelID string) (int, error) {
	parts := strings.Split(strings.TrimSpace(modelID), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return 0, fmt.Errorf("invalid OpenRouter model ID %q", modelID)
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/model/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Skot")
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return 0, fmt.Errorf("OpenRouter model metadata: %s", response.Status)
	}
	var payload struct {
		Data struct {
			ContextLength int `json:"context_length"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return 0, fmt.Errorf("decode OpenRouter model metadata: %w", err)
	}
	if payload.Data.ContextLength <= 0 {
		return 0, errors.New("OpenRouter model metadata has no context length")
	}
	return payload.Data.ContextLength, nil
}
