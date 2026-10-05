package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/web"
)

func TestWebToolCatalogIsNativeAndValid(t *testing.T) {
	catalog := NewWebTools(nil)
	normalized, err := agent.NormalizeTools(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != 2 || normalized[0].Spec.Name != "web_fetch" || normalized[1].Spec.Name != "web_search" {
		t.Fatalf("web catalog = %#v", normalized)
	}
	if !normalized[0].Spec.ParallelSafe || normalized[1].Spec.ParallelSafe {
		t.Fatalf("web parallel policy = fetch %t, search %t", normalized[0].Spec.ParallelSafe, normalized[1].Spec.ParallelSafe)
	}
}

func TestWebSearchProviderOrder(t *testing.T) {
	tokens := map[string]string{"tavily": "tavily-token", "exa": "exa-token"}
	web := webTools{credential: func(provider string) (string, error) {
		return tokens[provider], nil
	}}
	providers, err := web.searchProviders()
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 3 || providers[0].Name() != "keenable" || providers[1].Name() != "tavily" || providers[2].Name() != "exa" {
		t.Fatalf("search providers = %#v", providers)
	}
}

type testFetchBackend struct {
	name   string
	result web.FetchResponse
	err    error
}

func (backend testFetchBackend) Name() string { return backend.name }
func (backend testFetchBackend) Fetch(context.Context, web.FetchRequest) (web.FetchResponse, error) {
	return backend.result, backend.err
}

func TestFetchWebFallsBackAndBoundsOutput(t *testing.T) {
	result, provider, err := fetchWeb(context.Background(), web.FetchRequest{URL: "https://example.test"}, []webFetchBackend{
		testFetchBackend{name: "first", err: errors.New("unavailable")},
		testFetchBackend{name: "second", result: web.FetchResponse{URL: "https://example.test/final", Text: strings.Repeat("x", webMaxTextBytes+100)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider != "second" || !result.Truncated || len(result.Text) >= webMaxTextBytes+100 {
		t.Fatalf("fetch result = %#v, bytes=%d", result, len(result.Text))
	}
}

type testSearchProvider struct {
	name    string
	results []web.SearchResult
	err     error
}

func (provider testSearchProvider) Name() string { return provider.name }
func (provider testSearchProvider) Search(context.Context, web.SearchRequest) (web.SearchResponse, error) {
	return web.SearchResponse{Results: provider.results}, provider.err
}

func TestSearchWebFallsBackNormalizesAndDeduplicates(t *testing.T) {
	results, provider, err := searchWeb(context.Background(), web.SearchRequest{Query: " durable agents ", Limit: 2}, []webSearchProvider{
		testSearchProvider{name: "first", err: errors.New("rate limited")},
		testSearchProvider{name: "second", results: []web.SearchResult{
			{Title: " One ", URL: "https://example.test/one", Snippet: "a\n b"},
			{Title: "duplicate", URL: "https://example.test/one"},
			{Title: "bad", URL: "file:///tmp/data"},
			{Title: "Two", URL: "https://example.test/two"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider != "second" || len(results) != 2 || results[0].Title != "One" || results[0].Snippet != "a b" || results[1].URL != "https://example.test/two" {
		t.Fatalf("provider=%q results=%#v", provider, results)
	}
}
