package modelhttp

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPublicEndpointOmitsCredentials(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{" https://alice:password@gateway.example/v1?key=secret#private ", "https://gateway.example/v1"},
		{"https:alice:password@gateway.example/v1", ""},
		{"alice:password@gateway.example/v1", ""},
		{"alice@gateway.example/v1", ""},
	} {
		if got := PublicEndpoint(test.input); got != test.want {
			t.Errorf("PublicEndpoint(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestDefaultClientKeepsRedirectedRequestsWithinTheirOrigin(t *testing.T) {
	for _, test := range []struct {
		name, location string
		status, calls  int
	}{
		{name: "relative path", location: "/target", status: 307, calls: 2},
		{name: "same origin", location: "https://api.example.test/target", status: 308, calls: 2},
		{name: "default port", location: "https://api.example.test:443/target", status: 307, calls: 2},
		{name: "other host", location: "https://other.example.test/target", status: 307, calls: 1},
		{name: "subdomain", location: "https://child.api.example.test/target", status: 308, calls: 1},
		{name: "other port", location: "https://api.example.test:8443/target", status: 307, calls: 1},
		{name: "other scheme", location: "http://api.example.test/target", status: 307, calls: 1},
		{name: "POST becomes GET 301", location: "/target", status: 301, calls: 1},
		{name: "POST becomes GET 302", location: "/target", status: 302, calls: 1},
		{name: "POST becomes GET 303", location: "/target", status: 303, calls: 1},
		{name: "redirect loop", location: "/messages", status: 307, calls: 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := DefaultClient()
			client.Transport = modelHTTPRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.Body != nil {
					_ = request.Body.Close()
				}
				status := test.status
				if request.URL.Path == "/target" {
					status = http.StatusOK
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": {test.location}}, Body: io.NopCloser(strings.NewReader("response")), Request: request}, nil
			})
			request, err := http.NewRequest(http.MethodPost, "https://api.example.test/messages", strings.NewReader("body"))
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if calls != test.calls {
				t.Fatalf("requests = %d, want %d", calls, test.calls)
			}
		})
	}
}

type modelHTTPRoundTripFunc func(*http.Request) (*http.Response, error)

func (function modelHTTPRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestMarshalRequestJSONAvoidsHTMLEscapingAndTrailingNewline(t *testing.T) {
	body, err := MarshalRequestJSON(struct {
		Text string `json:"text"`
	}{Text: "<p>музей & архив</p>"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), `{"text":"<p>музей & архив</p>"}`; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, time.August, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "empty"},
		{name: "delay seconds", value: " 15 ", want: 15 * time.Second},
		{name: "zero delay", value: "0"},
		{name: "negative delay", value: "-2"},
		{name: "future date", value: now.Add(90 * time.Second).Format(http.TimeFormat), want: 90 * time.Second},
		{name: "elapsed date", value: now.Add(-time.Second).Format(http.TimeFormat)},
		{name: "invalid", value: "later"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ParseRetryAfter(test.value, now); got != test.want {
				t.Fatalf("ParseRetryAfter(%q) = %s, want %s", test.value, got, test.want)
			}
		})
	}
}
