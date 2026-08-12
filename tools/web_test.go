package tools

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
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

func TestWebSearchAvailabilityUsesCurrentCredentials(t *testing.T) {
	tokens := map[string]string{}
	lookup := func(provider string) (string, error) { return tokens[provider], nil }
	if available, err := WebSearchAvailable(lookup); err != nil || available {
		t.Fatalf("empty credentials = %t, %v", available, err)
	}
	tokens["exa"] = "token"
	if available, err := WebSearchAvailable(lookup); err != nil || !available {
		t.Fatalf("exa credential = %t, %v", available, err)
	}
	want := errors.New("store failed")
	if _, err := WebSearchAvailable(func(string) (string, error) { return "", want }); !errors.Is(err, want) {
		t.Fatalf("availability error = %v", err)
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

func TestHackerNewsFetchUsesBoundedDiscussionReader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/item/42.json":
			_, _ = writer.Write([]byte(`{"id":42,"type":"story","by":"alice","time":1700000000,"title":"A story","url":"https://example.test/story","score":7,"descendants":1,"kids":[43]}`))
		case "/item/43.json":
			_, _ = writer.Write([]byte(`{"id":43,"type":"comment","by":"bob","text":"Useful <b>comment</b>"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	backend := newHackerNewsFetchBackend()
	backend.client.apiBase = server.URL
	backend.client.client = server.Client()
	result, err := backend.Fetch(context.Background(), webFetchRequest{URL: "https://news.ycombinator.com/item?id=42"})
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://news.ycombinator.com/item?id=42" || result.Title != "A story" || !strings.Contains(result.Text, "bob [item 43]: Useful comment") {
		t.Fatalf("HN result = %#v", result)
	}
}
