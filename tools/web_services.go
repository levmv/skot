package tools

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	keenableSearchEndpoint       = "https://api.keenable.ai/v1/search"
	keenablePublicSearchEndpoint = "https://api.keenable.ai/v1/search/public"
	keenableFetchEndpoint        = "https://api.keenable.ai/v1/fetch"
	keenablePublicFetchEndpoint  = "https://api.keenable.ai/v1/fetch/public"
	firecrawlScrapeEndpoint      = "https://api.firecrawl.dev/v2/scrape"
	exaContentsEndpoint          = "https://api.exa.ai/contents"
	tavilySearchEndpoint         = "https://api.tavily.com/search"
	exaSearchEndpoint            = "https://api.exa.ai/search"
)

type keenableFetchBackend struct {
	token    string
	endpoint string
	client   *http.Client
}

func newKeenableFetchBackend(token string) *keenableFetchBackend {
	token = strings.TrimSpace(token)
	endpoint := keenableFetchEndpoint
	if token == "" {
		endpoint = keenablePublicFetchEndpoint
	}
	return &keenableFetchBackend{
		token: token, endpoint: endpoint,
		client: &http.Client{Timeout: keenableFetchTimeout},
	}
}

func (*keenableFetchBackend) Name() string { return "keenable" }

func (backend *keenableFetchBackend) Fetch(ctx context.Context, request webFetchRequest) (webFetchResult, error) {
	query := url.Values{
		"url":       {request.URL},
		"max_chars": {strconv.Itoa(webMaxTextBytes)},
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, backend.endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return webFetchResult{}, err
	}
	setKeenableHeaders(httpRequest, backend.token)
	response, err := backend.client.Do(httpRequest)
	if err != nil {
		return webFetchResult{}, fmt.Errorf("request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		discardWebErrorBody(response.Body)
		return webFetchResult{}, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var payload struct {
		URL     string `json:"url"`
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := decodeWebJSON(response.Body, &payload); err != nil {
		return webFetchResult{}, fmt.Errorf("decode response: %w", err)
	}
	resultURL := strings.TrimSpace(payload.URL)
	if resultURL == "" {
		resultURL = request.URL
	}
	return webFetchResult{URL: resultURL, Title: payload.Title, Text: payload.Content}, nil
}

type keenableSearchProvider struct {
	token    string
	endpoint string
	client   *http.Client
}

func newKeenableSearchProvider(token string) *keenableSearchProvider {
	token = strings.TrimSpace(token)
	endpoint := keenableSearchEndpoint
	if token == "" {
		endpoint = keenablePublicSearchEndpoint
	}
	return &keenableSearchProvider{
		token: token, endpoint: endpoint,
		client: &http.Client{Timeout: webSearchAttemptTimeout},
	}
}

func (*keenableSearchProvider) Name() string { return "keenable" }

func (provider *keenableSearchProvider) Search(ctx context.Context, request webSearchRequest) ([]webSearchResult, error) {
	body, err := json.Marshal(struct {
		Query            string `json:"query"`
		MaxResults       int    `json:"max_results"`
		SnippetMaxLength int    `json:"snippet_max_length"`
	}{request.Query, request.Limit, webSearchSnippetChars}, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	setKeenableHeaders(httpRequest, provider.token)
	response, err := provider.client.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		discardWebErrorBody(response.Body)
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var payload struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
			Snippet     string `json:"snippet"`
		} `json:"results"`
	}
	if err := decodeWebJSON(response.Body, &payload); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	results := make([]webSearchResult, 0, min(request.Limit, len(payload.Results)))
	for _, result := range payload.Results {
		snippet := result.Snippet
		if strings.TrimSpace(snippet) == "" {
			snippet = result.Description
		}
		results = append(results, webSearchResult{Title: result.Title, URL: result.URL, Snippet: snippet})
	}
	return results, nil
}

func setKeenableHeaders(request *http.Request, token string) {
	request.Header.Set("Accept", "application/json")
	if token == "" {
		request.Header.Set("X-Keenable-Title", "Skot")
		return
	}
	request.Header.Set("X-API-Key", token)
}

type firecrawlFetchBackend struct {
	token    string
	endpoint string
	client   *http.Client
}

func newFirecrawlFetchBackend(token string) *firecrawlFetchBackend {
	return &firecrawlFetchBackend{
		token: strings.TrimSpace(token), endpoint: firecrawlScrapeEndpoint,
		client: &http.Client{Timeout: firecrawlFetchTimeout},
	}
}

func (*firecrawlFetchBackend) Name() string { return "firecrawl" }

func (backend *firecrawlFetchBackend) Fetch(ctx context.Context, request webFetchRequest) (webFetchResult, error) {
	body, err := json.Marshal(struct {
		URL             string   `json:"url"`
		Formats         []string `json:"formats"`
		OnlyMainContent bool     `json:"onlyMainContent"`
		Proxy           string   `json:"proxy"`
		Timeout         int      `json:"timeout"`
	}{
		URL: request.URL, Formats: []string{"markdown"}, OnlyMainContent: true,
		Proxy: "basic", Timeout: 45_000,
	}, json.Deterministic(true))
	if err != nil {
		return webFetchResult{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, backend.endpoint, bytes.NewReader(body))
	if err != nil {
		return webFetchResult{}, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+backend.token)
	response, err := backend.client.Do(httpRequest)
	if err != nil {
		return webFetchResult{}, fmt.Errorf("request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		discardWebErrorBody(response.Body)
		return webFetchResult{}, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var payload struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Data    struct {
			Markdown string `json:"markdown"`
			Metadata struct {
				Title     string `json:"title"`
				SourceURL string `json:"sourceURL"`
			} `json:"metadata"`
		} `json:"data"`
	}
	if err := decodeWebJSON(response.Body, &payload); err != nil {
		return webFetchResult{}, fmt.Errorf("decode response: %w", err)
	}
	if !payload.Success {
		message := compactWebText(payload.Error, 240)
		if message == "" {
			message = "scrape was unsuccessful"
		}
		return webFetchResult{}, errors.New(message)
	}
	resultURL := strings.TrimSpace(payload.Data.Metadata.SourceURL)
	if resultURL == "" {
		resultURL = request.URL
	}
	return webFetchResult{URL: resultURL, Title: payload.Data.Metadata.Title, Text: payload.Data.Markdown}, nil
}

type exaFetchBackend struct {
	token    string
	endpoint string
	client   *http.Client
}

func newExaFetchBackend(token string) *exaFetchBackend {
	return &exaFetchBackend{
		token: strings.TrimSpace(token), endpoint: exaContentsEndpoint,
		client: &http.Client{Timeout: exaFetchTimeout},
	}
}

func (*exaFetchBackend) Name() string { return "exa" }

func (backend *exaFetchBackend) Fetch(ctx context.Context, request webFetchRequest) (webFetchResult, error) {
	body, err := json.Marshal(struct {
		URLs []string `json:"urls"`
		Text struct {
			MaxCharacters int `json:"maxCharacters"`
		} `json:"text"`
		MaxAgeHours      int `json:"maxAgeHours"`
		LivecrawlTimeout int `json:"livecrawlTimeout"`
	}{
		URLs: []string{request.URL},
		Text: struct {
			MaxCharacters int `json:"maxCharacters"`
		}{MaxCharacters: exaFetchMaxCharacters},
		MaxAgeHours: 24, LivecrawlTimeout: 12_000,
	}, json.Deterministic(true))
	if err != nil {
		return webFetchResult{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, backend.endpoint, bytes.NewReader(body))
	if err != nil {
		return webFetchResult{}, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("x-api-key", backend.token)
	response, err := backend.client.Do(httpRequest)
	if err != nil {
		return webFetchResult{}, fmt.Errorf("request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		discardWebErrorBody(response.Body)
		return webFetchResult{}, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var payload struct {
		Results []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
			Text  string `json:"text"`
		} `json:"results"`
		Statuses []struct {
			Status string `json:"status"`
			Error  struct {
				Tag string `json:"tag"`
			} `json:"error"`
		} `json:"statuses"`
	}
	if err := decodeWebJSON(response.Body, &payload); err != nil {
		return webFetchResult{}, fmt.Errorf("decode response: %w", err)
	}
	if len(payload.Results) == 0 {
		for _, status := range payload.Statuses {
			if status.Status == "error" && status.Error.Tag != "" {
				return webFetchResult{}, fmt.Errorf("contents: %s", compactWebText(status.Error.Tag, 120))
			}
		}
		return webFetchResult{}, errors.New("contents returned no result")
	}
	result := payload.Results[0]
	resultURL := strings.TrimSpace(result.URL)
	if resultURL == "" {
		resultURL = request.URL
	}
	return webFetchResult{URL: resultURL, Title: result.Title, Text: result.Text}, nil
}

type tavilySearchProvider struct {
	token    string
	endpoint string
	client   *http.Client
}

func newTavilySearchProvider(token string) *tavilySearchProvider {
	return &tavilySearchProvider{token: strings.TrimSpace(token), endpoint: tavilySearchEndpoint, client: &http.Client{Timeout: webSearchAttemptTimeout}}
}

func (*tavilySearchProvider) Name() string { return "tavily" }

func (provider *tavilySearchProvider) Search(ctx context.Context, request webSearchRequest) ([]webSearchResult, error) {
	body, err := json.Marshal(struct {
		Query       string `json:"query"`
		SearchDepth string `json:"search_depth"`
		MaxResults  int    `json:"max_results"`
	}{request.Query, "basic", request.Limit}, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+provider.token)
	response, err := provider.client.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		discardWebErrorBody(response.Body)
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var payload struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := decodeWebJSON(response.Body, &payload); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	results := make([]webSearchResult, 0, min(request.Limit, len(payload.Results)))
	for _, result := range payload.Results {
		results = append(results, webSearchResult{Title: result.Title, URL: result.URL, Snippet: result.Content})
	}
	return results, nil
}

type exaSearchProvider struct {
	token    string
	endpoint string
	client   *http.Client
}

func newExaSearchProvider(token string) *exaSearchProvider {
	return &exaSearchProvider{token: strings.TrimSpace(token), endpoint: exaSearchEndpoint, client: &http.Client{Timeout: webSearchAttemptTimeout}}
}

func (*exaSearchProvider) Name() string { return "exa" }

func (provider *exaSearchProvider) Search(ctx context.Context, request webSearchRequest) ([]webSearchResult, error) {
	body, err := json.Marshal(struct {
		Query      string `json:"query"`
		NumResults int    `json:"numResults"`
	}{request.Query, request.Limit}, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("x-api-key", provider.token)
	response, err := provider.client.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		discardWebErrorBody(response.Body)
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var payload struct {
		Results []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
			Text  string `json:"text"`
		} `json:"results"`
	}
	if err := decodeWebJSON(response.Body, &payload); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	results := make([]webSearchResult, 0, min(request.Limit, len(payload.Results)))
	for _, result := range payload.Results {
		results = append(results, webSearchResult{Title: result.Title, URL: result.URL, Snippet: result.Text})
	}
	return results, nil
}

func decodeWebJSON(reader io.Reader, target any) error {
	raw, err := io.ReadAll(io.LimitReader(reader, webMaxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > webMaxResponseBytes {
		return errors.New("response exceeds size limit")
	}
	return json.Unmarshal(raw, target)
}

// discardWebErrorBody reads and discards a bounded third-party error body.
func discardWebErrorBody(reader io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(reader, 64*1024))
}
