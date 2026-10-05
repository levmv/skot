// Package web provides individual web search and page-fetch operations.
// Clients do not retry or choose fallback providers. Responses include any
// reported usage even when an operation fails or returns no content.
package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const (
	fetchMaxCharacters = 96 * 1024
	maxResponseBytes   = 2 * 1024 * 1024
	defaultResults     = 5
	maxResults         = 20
	searchSnippetChars = 1_200
)

// Config selects one service. Keys are supplied explicitly, without reading
// application settings or environment variables.
type Config struct {
	// Provider is keenable, tavily, exa, firecrawl, or http (direct page fetch).
	Provider string
	// APIKey is optional for Keenable and unused for direct HTTP fetching.
	APIKey string
	// HTTPClient is borrowed for service API calls. Direct HTTP fetching uses
	// its own transport to check destination addresses; it requires nil here.
	// Use the request context to set a deadline for either kind of client.
	HTTPClient *http.Client
}

type Client struct {
	provider string
	apiKey   string
	http     *http.Client
}

// NewClient creates a reusable client without making any requests.
func NewClient(config Config) (*Client, error) {
	name := strings.ToLower(strings.TrimSpace(config.Provider))
	key := strings.TrimSpace(config.APIKey)
	switch name {
	case "keenable", "http":
	case "tavily", "exa", "firecrawl":
		if key == "" {
			return nil, fmt.Errorf("%s API key is required", name)
		}
	default:
		return nil, fmt.Errorf("unknown web provider %q", config.Provider)
	}
	if name == "http" {
		if config.HTTPClient != nil {
			return nil, errors.New("direct HTTP fetching requires the built-in transport")
		}
		return &Client{provider: name, http: directHTTPClient}, nil
	}
	base := config.HTTPClient
	if base == nil {
		base = http.DefaultClient
	}
	httpClient := *base
	// A service invocation must not silently repeat a POST after a redirect.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{provider: name, apiKey: key, http: &httpClient}, nil
}

// Name returns the configured provider name.
func (client *Client) Name() string { return client.provider }

type SearchRequest struct {
	Query string
	// Limit defaults to 5 and is capped at 20.
	Limit int
}

type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

type SearchResponse struct {
	Results []SearchResult
	Usage   Usage
}

type FetchRequest struct{ URL string }

type FetchResponse struct {
	URL   string
	Title string
	Text  string
	// Truncated is set when direct HTTP fetching reaches its read limit.
	// Content limits applied by a service are not reflected here.
	Truncated bool
	Usage     Usage
}

// Search makes one API call to Keenable, Tavily (basic search), or Exa.
// It preserves the returned titles, URLs, and snippets. An empty result is
// returned with any reported usage.
func (client *Client) Search(ctx context.Context, request SearchRequest) (SearchResponse, error) {
	request.Query = strings.TrimSpace(request.Query)
	if request.Query == "" {
		return SearchResponse{}, errors.New("query is required")
	}
	if request.Limit <= 0 {
		request.Limit = defaultResults
	}
	request.Limit = min(request.Limit, maxResults)
	switch client.provider {
	case "keenable":
		return client.searchKeenable(ctx, request)
	case "tavily":
		return client.searchTavily(ctx, request)
	case "exa":
		return client.searchExa(ctx, request)
	default:
		return SearchResponse{}, fmt.Errorf("%s does not support web search", client.provider)
	}
}

// Fetch reads one public HTTP(S) page through Keenable, Firecrawl, Exa, or
// direct HTTP. Service calls do not retry; direct HTTP may follow redirects
// after checking each destination and reads at most 2 MiB of the page.
func (client *Client) Fetch(ctx context.Context, request FetchRequest) (FetchResponse, error) {
	target, err := validatePublicURL(request.URL)
	if err != nil {
		return FetchResponse{}, err
	}
	request.URL = target.String()
	switch client.provider {
	case "keenable":
		return client.fetchKeenable(ctx, request)
	case "firecrawl":
		return client.fetchFirecrawl(ctx, request)
	case "exa":
		return client.fetchExa(ctx, request)
	case "http":
		return client.fetchHTTP(ctx, request)
	default:
		return FetchResponse{}, fmt.Errorf("%s does not support page fetch", client.provider)
	}
}
