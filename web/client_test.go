package web

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (call roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return call(request)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestKeenableUsesPublicOrAuthenticatedAPI(t *testing.T) {
	for _, test := range []struct{ name, key string }{
		{name: "public"}, {name: "authenticated", key: "keen_test"},
	} {
		t.Run(test.name, func(t *testing.T) {
			key := test.key
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			deadline, _ := ctx.Deadline()
			resultURL := "https://example.com/final?ref=" + strings.Repeat("x", 2200)
			pageText := strings.Repeat("я", 50_000)
			client, err := NewClient(Config{Provider: "keenable", APIKey: key, HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if got, ok := request.Context().Deadline(); !ok || !got.Equal(deadline) {
					t.Fatalf("caller deadline changed: %v", got)
				}
				path := "/v1/fetch"
				if request.Method == http.MethodPost {
					path = "/v1/search"
				}
				if key == "" {
					path += "/public"
				}
				if request.URL.Host != "api.keenable.ai" || request.URL.Path != path {
					t.Fatalf("endpoint = %s", request.URL)
				}
				if key == "" {
					if request.Header.Get("X-Keenable-Title") != "Skot" {
						t.Fatalf("public request: %s, %v", request.URL, request.Header)
					}
				} else if request.Header.Get("X-API-Key") != key {
					t.Fatalf("authenticated request: %s, %v", request.URL, request.Header)
				}
				if request.Method == http.MethodPost {
					var body struct {
						Query            string `json:"query"`
						MaxResults       int    `json:"max_results"`
						SnippetMaxLength int    `json:"snippet_max_length"`
					}
					if err := json.UnmarshalRead(request.Body, &body); err != nil {
						t.Fatal(err)
					}
					if body.Query != "agent search" || body.MaxResults != 2 || body.SnippetMaxLength != searchSnippetChars {
						t.Fatalf("request body = %#v", body)
					}
					return jsonResponse(200, `{"results":[{"title":" One ","url":"`+resultURL+`","description":"a\n b","published_at":"2026-01-15T13:30:00+03:00"},{"title":"duplicate","url":"`+resultURL+`","acquired_at":"2026-01-16T08:12:34Z"}]}`), nil
				}
				if request.Method != http.MethodGet || request.URL.Query().Get("url") != "https://example.com/a?b=c" || request.URL.Query().Get("max_chars") != strconv.Itoa(fetchMaxCharacters) {
					t.Fatalf("fetch request: %s %s", request.Method, request.URL)
				}
				return jsonResponse(200, `{"url":"`+resultURL+`","title":"Fetched","content":"`+pageText+`","published_at":1768473000}`), nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			published := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)
			search, err := client.Search(ctx, SearchRequest{Query: " agent search ", Limit: 2})
			if err != nil || len(search.Results) != 2 || search.Results[0].Title != " One " || search.Results[0].Snippet != "a\n b" || search.Results[0].URL != resultURL || search.Results[1].URL != resultURL {
				t.Fatalf("search = %#v, error = %v", search, err)
			}
			// Indexing time is not a publication date.
			indexed := time.Date(2026, 1, 16, 8, 12, 34, 0, time.UTC)
			if first, second := search.Results[0], search.Results[1]; first.PublishedAt != published || !first.IndexedAt.IsZero() || !second.PublishedAt.IsZero() || second.IndexedAt != indexed {
				t.Fatalf("search dates = %#v", search.Results)
			}
			page, err := client.Fetch(ctx, FetchRequest{URL: "https://example.com/a?b=c"})
			if err != nil || page.URL != resultURL || page.Title != "Fetched" || page.Truncated || page.Text != pageText || page.PublishedAt != published {
				t.Fatalf("fetch URL=%q, title=%q, published=%v, truncated=%v, size=%d, error=%v", page.URL, page.Title, page.PublishedAt, page.Truncated, len(page.Text), err)
			}
		})
	}
}

func TestExaSearchSnippetsAndDates(t *testing.T) {
	long := strings.Repeat("я", searchSnippetChars)
	client, err := NewClient(Config{Provider: "exa", APIKey: "exa-key", HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/search" {
			var body struct {
				Contents struct {
					Highlights struct {
						MaxCharacters int `json:"maxCharacters"`
					} `json:"highlights"`
				} `json:"contents"`
			}
			if err := json.UnmarshalRead(request.Body, &body); err != nil || body.Contents.Highlights.MaxCharacters != searchSnippetChars {
				t.Fatalf("search must request highlights: %#v, %v", body, err)
			}
			return jsonResponse(200, `{"results":[{"title":"Dated","url":"https://example.com/a","highlights":["first","second"],"publishedDate":"2026-08-20T19:42:50.000Z"},{"title":"Undated","url":"https://example.com/b","highlights":["`+long+`ещё"]}]}`), nil
		}
		return jsonResponse(200, `{"results":[{"title":"Page","url":"https://example.com/a","text":"body","publishedDate":"2026-08-20T19:42:50.000Z"}]}`), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	published := time.Date(2026, 8, 20, 19, 42, 50, 0, time.UTC)
	search, err := client.Search(t.Context(), SearchRequest{Query: "go"})
	if err != nil || len(search.Results) != 2 || search.Results[0].Snippet != "first … second" || search.Results[0].PublishedAt != published || !search.Results[1].PublishedAt.IsZero() {
		t.Fatalf("search = %#v, error = %v", search, err)
	}
	// Exa overshoots its maxCharacters; the bound counts runes, not bytes.
	if search.Results[1].Snippet != long {
		t.Fatalf("long snippet has %d bytes", len(search.Results[1].Snippet))
	}
	page, err := client.Fetch(t.Context(), FetchRequest{URL: "https://example.com/a"})
	if err != nil || page.PublishedAt != published {
		t.Fatalf("fetch published = %v, error = %v", page.PublishedAt, err)
	}
}

func TestUsageForIndividualWebOperations(t *testing.T) {
	for _, test := range []struct {
		name, provider, body string
		fetch                bool
		status               int
		want                 Usage
		wantError            bool
	}{
		{name: "empty search with usage", provider: "tavily", status: 200, body: `{"results":[],"usage":{"credits":1},"request_id":"tavily-id"}`, want: Usage{Credits: "1", RequestID: "tavily-id"}},
		{name: "search cost precision", provider: "exa", status: 200, body: `{"results":[],"costDollars":{"total":0.004000000000000001},"requestId":"exa-search"}`, want: Usage{EstimatedCostUSD: "0.004000000000000001", RequestID: "exa-search"}},
		{name: "failed page with reported zero", provider: "exa", fetch: true, status: 200, body: `{"results":[],"statuses":[{"status":"error","error":{"tag":"CRAWL_TIMEOUT"}}],"costDollars":{"total":0},"requestId":"exa-fetch"}`, want: Usage{EstimatedCostUSD: "0", RequestID: "exa-fetch"}, wantError: true},
		{name: "empty scrape with usage", provider: "firecrawl", fetch: true, status: 200, body: `{"success":true,"data":{"markdown":"","metadata":{"creditsUsed":1,"scrapeId":"firecrawl-id"}}}`, want: Usage{Credits: "1", RequestID: "firecrawl-id"}},
		{name: "HTTP failure with request ID", provider: "exa", status: 429, body: `{"error":"rate limited","requestId":"failed-search"}`, want: Usage{RequestID: "failed-search"}, wantError: true},
		{name: "unreported usage", provider: "tavily", status: 200, body: `{"results":[]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client, err := NewClient(Config{Provider: test.provider, APIKey: "test-key", HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if test.provider == "tavily" {
					var body struct {
						IncludeUsage bool `json:"include_usage"`
					}
					if err := json.UnmarshalRead(request.Body, &body); err != nil || !body.IncludeUsage {
						t.Fatalf("request must include usage: %v", err)
					}
				}
				return jsonResponse(test.status, test.body), nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			var usage Usage
			if test.fetch {
				response, fetchErr := client.Fetch(t.Context(), FetchRequest{URL: "https://example.com"})
				usage, err = response.Usage, fetchErr
			} else {
				response, searchErr := client.Search(t.Context(), SearchRequest{Query: "example"})
				usage, err = response.Usage, searchErr
			}
			if calls != 1 || usage != test.want || (err != nil) != test.wantError {
				t.Fatalf("calls=%d, usage=%+v, error=%v", calls, usage, err)
			}
			if test.status != 200 {
				if httpErr, ok := errors.AsType[*HTTPError](err); !ok || httpErr.StatusCode != test.status {
					t.Fatalf("HTTP error = %v", err)
				}
			}
		})
	}
}

func TestFetchRejectsNonPublicTargetsBeforeNetwork(t *testing.T) {
	client, err := NewClient(Config{Provider: "keenable", HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("rejected URL reached the network")
		return nil, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"file:///etc/passwd", "http://localhost/admin", "http://127.0.0.1/admin",
		"http://100.64.0.1/admin", "http://198.18.0.1/admin", "http://169.254.169.254/latest/meta-data",
		"http://[::1]/admin", "http://[2001:db8::1]/admin", "http://[64:ff9b::7f00:1]/admin",
	} {
		if _, err := client.Fetch(t.Context(), FetchRequest{URL: target}); !errors.Is(err, ErrURLNotAllowed) {
			t.Fatalf("target=%s, error=%v", target, err)
		}
	}
}
