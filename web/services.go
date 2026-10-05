package web

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/levmv/skot/internal/webtext"
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

func (client *Client) fetchKeenable(ctx context.Context, request FetchRequest) (result FetchResponse, err error) {
	endpoint := keenableFetchEndpoint
	if client.apiKey == "" {
		endpoint = keenablePublicFetchEndpoint
	}
	query := url.Values{
		"url":       {request.URL},
		"max_chars": {strconv.Itoa(fetchMaxCharacters)},
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return result, err
	}
	setKeenableHeaders(httpRequest, client.apiKey)
	var payload struct {
		URL     string `json:"url"`
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	result.Usage, err = client.doJSON(httpRequest, &payload)
	if err != nil {
		return result, err
	}
	resultURL := strings.TrimSpace(payload.URL)
	if resultURL == "" {
		resultURL = request.URL
	}
	return FetchResponse{Usage: result.Usage, URL: resultURL, Title: payload.Title, Text: payload.Content}, nil
}

func (client *Client) searchKeenable(ctx context.Context, request SearchRequest) (result SearchResponse, err error) {
	endpoint := keenableSearchEndpoint
	if client.apiKey == "" {
		endpoint = keenablePublicSearchEndpoint
	}
	body, err := json.Marshal(struct {
		Query            string `json:"query"`
		MaxResults       int    `json:"max_results"`
		SnippetMaxLength int    `json:"snippet_max_length"`
	}{request.Query, request.Limit, searchSnippetChars}, json.Deterministic(true))
	if err != nil {
		return result, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	setKeenableHeaders(httpRequest, client.apiKey)
	var payload struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
			Snippet     string `json:"snippet"`
		} `json:"results"`
	}
	result.Usage, err = client.doJSON(httpRequest, &payload)
	if err != nil {
		return result, err
	}
	result.Results = make([]SearchResult, 0, len(payload.Results))
	for _, entry := range payload.Results {
		snippet := entry.Snippet
		if strings.TrimSpace(snippet) == "" {
			snippet = entry.Description
		}
		result.Results = append(result.Results, SearchResult{Title: entry.Title, URL: entry.URL, Snippet: snippet})
	}
	return result, nil
}

func (client *Client) fetchFirecrawl(ctx context.Context, request FetchRequest) (result FetchResponse, err error) {
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
		return result, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, firecrawlScrapeEndpoint, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+client.apiKey)
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
	result.Usage, err = client.doJSON(httpRequest, &payload)
	if err != nil {
		return result, err
	}
	if !payload.Success {
		message := webtext.Compact(payload.Error, 240)
		if message == "" {
			message = "scrape was unsuccessful"
		}
		return result, errors.New(message)
	}
	resultURL := strings.TrimSpace(payload.Data.Metadata.SourceURL)
	if resultURL == "" {
		resultURL = request.URL
	}
	return FetchResponse{Usage: result.Usage, URL: resultURL, Title: payload.Data.Metadata.Title, Text: payload.Data.Markdown}, nil
}

func (client *Client) fetchExa(ctx context.Context, request FetchRequest) (result FetchResponse, err error) {
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
		}{MaxCharacters: fetchMaxCharacters},
		MaxAgeHours: 24, LivecrawlTimeout: 12_000,
	}, json.Deterministic(true))
	if err != nil {
		return result, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, exaContentsEndpoint, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("x-api-key", client.apiKey)
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
	result.Usage, err = client.doJSON(httpRequest, &payload)
	if err != nil {
		return result, err
	}
	if len(payload.Results) == 0 {
		for _, status := range payload.Statuses {
			if status.Status == "error" && status.Error.Tag != "" {
				return result, fmt.Errorf("contents: %s", webtext.Compact(status.Error.Tag, 120))
			}
		}
		return result, errors.New("contents returned no result")
	}
	page := payload.Results[0]
	resultURL := strings.TrimSpace(page.URL)
	if resultURL == "" {
		resultURL = request.URL
	}
	return FetchResponse{Usage: result.Usage, URL: resultURL, Title: page.Title, Text: page.Text}, nil
}

func (client *Client) searchTavily(ctx context.Context, request SearchRequest) (result SearchResponse, err error) {
	body, err := json.Marshal(struct {
		IncludeUsage bool   `json:"include_usage"`
		Query        string `json:"query"`
		SearchDepth  string `json:"search_depth"`
		MaxResults   int    `json:"max_results"`
	}{true, request.Query, "basic", request.Limit}, json.Deterministic(true))
	if err != nil {
		return result, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, tavilySearchEndpoint, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+client.apiKey)
	var payload struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	result.Usage, err = client.doJSON(httpRequest, &payload)
	if err != nil {
		return result, err
	}
	result.Results = make([]SearchResult, 0, len(payload.Results))
	for _, entry := range payload.Results {
		result.Results = append(result.Results, SearchResult{Title: entry.Title, URL: entry.URL, Snippet: entry.Content})
	}
	return result, nil
}

func (client *Client) searchExa(ctx context.Context, request SearchRequest) (result SearchResponse, err error) {
	body, err := json.Marshal(struct {
		Query      string `json:"query"`
		NumResults int    `json:"numResults"`
	}{request.Query, request.Limit}, json.Deterministic(true))
	if err != nil {
		return result, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, exaSearchEndpoint, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("x-api-key", client.apiKey)
	var payload struct {
		Results []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
			Text  string `json:"text"`
		} `json:"results"`
	}
	result.Usage, err = client.doJSON(httpRequest, &payload)
	if err != nil {
		return result, err
	}
	result.Results = make([]SearchResult, 0, len(payload.Results))
	for _, entry := range payload.Results {
		result.Results = append(result.Results, SearchResult{Title: entry.Title, URL: entry.URL, Snippet: entry.Text})
	}
	return result, nil
}

func setKeenableHeaders(request *http.Request, token string) {
	request.Header.Set("Accept", "application/json")
	if token == "" {
		request.Header.Set("X-Keenable-Title", "Skot")
		return
	}
	request.Header.Set("X-API-Key", token)
}
