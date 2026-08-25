package tools

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
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

func TestWebSearchProvidersPreferPublicKeenable(t *testing.T) {
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
	keenable, ok := providers[0].(*keenableSearchProvider)
	if !ok || keenable.endpoint != keenablePublicSearchEndpoint {
		t.Fatalf("default search provider = %#v", providers[0])
	}

	tokens["keenable"] = "keen_account"
	providers, err = web.searchProviders()
	if err != nil {
		t.Fatal(err)
	}
	keenable, ok = providers[0].(*keenableSearchProvider)
	if !ok || keenable.endpoint != keenableSearchEndpoint || keenable.token != "keen_account" {
		t.Fatalf("authenticated search provider = %#v", providers[0])
	}
}

func TestKeenableSearchUsesPublicOrAuthenticatedAPI(t *testing.T) {
	for _, test := range []struct {
		name     string
		token    string
		endpoint string
	}{
		{name: "public", endpoint: keenablePublicSearchEndpoint},
		{name: "authenticated", token: "keen_test", endpoint: keenableSearchEndpoint},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost {
					t.Errorf("method = %s", request.Method)
				}
				if test.token == "" {
					if got := request.Header.Get("X-Keenable-Title"); got != "Skot" {
						t.Errorf("public title = %q", got)
					}
				} else if got := request.Header.Get("X-API-Key"); got != test.token {
					t.Errorf("API key = %q", got)
				}
				var body struct {
					Query            string `json:"query"`
					MaxResults       int    `json:"max_results"`
					SnippetMaxLength int    `json:"snippet_max_length"`
				}
				if err := decodeWebJSON(request.Body, &body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if body.Query != "agent search" || body.MaxResults != 3 || body.SnippetMaxLength != webSearchSnippetChars {
					t.Errorf("request body = %#v", body)
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, `{"results":[{"title":"Result","url":"https://example.com/result","description":"Fallback summary","snippet":""}]}`)
			}))
			defer server.Close()

			provider := newKeenableSearchProvider(test.token)
			if provider.endpoint != test.endpoint {
				t.Fatalf("endpoint = %q, want %q", provider.endpoint, test.endpoint)
			}
			provider.endpoint = server.URL
			results, err := provider.Search(context.Background(), webSearchRequest{Query: "agent search", Limit: 3})
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 || results[0].Title != "Result" || results[0].URL != "https://example.com/result" || results[0].Snippet != "Fallback summary" {
				t.Fatalf("results = %#v", results)
			}
		})
	}
}

func TestKeenableFetchUsesPublicOrAuthenticatedAPI(t *testing.T) {
	for _, test := range []struct {
		name     string
		token    string
		endpoint string
	}{
		{name: "public", endpoint: keenablePublicFetchEndpoint},
		{name: "authenticated", token: "keen_test", endpoint: keenableFetchEndpoint},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet {
					t.Errorf("method = %s", request.Method)
				}
				if test.token == "" {
					if got := request.Header.Get("X-Keenable-Title"); got != "Skot" {
						t.Errorf("public title = %q", got)
					}
				} else if got := request.Header.Get("X-API-Key"); got != test.token {
					t.Errorf("API key = %q", got)
				}
				if got := request.URL.Query().Get("url"); got != "https://example.com/a?b=c" {
					t.Errorf("fetch URL = %q", got)
				}
				if got := request.URL.Query().Get("max_chars"); got != strconv.Itoa(webMaxTextBytes) {
					t.Errorf("max_chars = %q", got)
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, `{"url":"https://example.com/final","title":"Fetched","content":"# Page"}`)
			}))
			defer server.Close()

			backend := newKeenableFetchBackend(test.token)
			if backend.endpoint != test.endpoint {
				t.Fatalf("endpoint = %q, want %q", backend.endpoint, test.endpoint)
			}
			backend.endpoint = server.URL
			result, err := backend.Fetch(context.Background(), webFetchRequest{URL: "https://example.com/a?b=c"})
			if err != nil {
				t.Fatal(err)
			}
			if result.URL != "https://example.com/final" || result.Title != "Fetched" || result.Text != "# Page" {
				t.Fatalf("fetch result = %#v", result)
			}
		})
	}
}

func TestWebFetchRejectsNonPublicTargetsBeforeNetwork(t *testing.T) {
	tool := NewWebTools(nil)[0]
	for _, raw := range []string{
		`{"url":"file:///etc/passwd"}`,
		`{"url":"http://localhost/admin"}`,
		`{"url":"http://127.0.0.1/admin"}`,
		`{"url":"http://100.64.0.1/admin"}`,
		`{"url":"http://198.18.0.1/admin"}`,
		`{"url":"http://169.254.169.254/latest/meta-data"}`,
		`{"url":"http://[::1]/admin"}`,
		`{"url":"http://[2001:db8::1]/admin"}`,
		`{"url":"http://[64:ff9b::7f00:1]/admin"}`,
	} {
		if _, err := tool.Run(context.Background(), raw); err == nil {
			t.Fatalf("target was accepted: %s", raw)
		}
	}
}

func TestPublicWebIPClassification(t *testing.T) {
	for _, address := range []string{"8.8.8.8", "2606:4700:4700::1111", "64:ff9b::808:808"} {
		if !isPublicWebIP(net.ParseIP(address)) {
			t.Errorf("public address rejected: %s", address)
		}
	}
	for _, address := range []string{
		"100.64.0.1", "192.0.2.1", "198.18.0.1", "203.0.113.1", "240.0.0.1",
		"100::1", "2001:db8::1", "3fff::1", "5f00::1", "fec0::1", "64:ff9b::7f00:1",
	} {
		if isPublicWebIP(net.ParseIP(address)) {
			t.Errorf("non-public address accepted: %s", address)
		}
	}
}

func TestExtractWebContentPrefersMainAndDropsExecutableMarkup(t *testing.T) {
	raw := []byte(`<html><head><title> Example </title></head><body><nav>ignore me</nav><main><h1>Heading</h1><p>Hello <b>world</b>.</p><script>secret()</script></main><footer>ignore footer</footer></body></html>`)
	text, title, err := extractWebContent(raw, "text/html")
	if err != nil {
		t.Fatal(err)
	}
	if title != "Example" || !strings.Contains(text, "Heading") || !strings.Contains(text, "Hello world .") || strings.Contains(text, "ignore") || strings.Contains(text, "secret") {
		t.Fatalf("title=%q text=%q", title, text)
	}
}

type testFetchBackend struct {
	name   string
	result webFetchResult
	err    error
}

func (backend testFetchBackend) Name() string { return backend.name }
func (backend testFetchBackend) Fetch(context.Context, webFetchRequest) (webFetchResult, error) {
	return backend.result, backend.err
}

func TestFetchWebFallsBackAndBoundsOutput(t *testing.T) {
	result, err := fetchWeb(context.Background(), webFetchRequest{URL: "https://example.test"}, []webFetchBackend{
		testFetchBackend{name: "first", err: errors.New("unavailable")},
		testFetchBackend{name: "second", result: webFetchResult{URL: "https://example.test/final", Text: strings.Repeat("x", webMaxTextBytes+100)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Backend != "second" || !result.Truncated || len(result.Text) <= webMaxTextBytes {
		t.Fatalf("fetch result = %#v, bytes=%d", result, len(result.Text))
	}
}

type testSearchProvider struct {
	name    string
	results []webSearchResult
	err     error
}

func (provider testSearchProvider) Name() string { return provider.name }
func (provider testSearchProvider) Search(context.Context, webSearchRequest) ([]webSearchResult, error) {
	return provider.results, provider.err
}

func TestSearchWebFallsBackNormalizesAndDeduplicates(t *testing.T) {
	results, provider, err := searchWeb(context.Background(), webSearchRequest{Query: " durable agents ", Limit: 2}, []webSearchProvider{
		testSearchProvider{name: "first", err: errors.New("rate limited")},
		testSearchProvider{name: "second", results: []webSearchResult{
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
