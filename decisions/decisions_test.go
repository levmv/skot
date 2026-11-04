package decisions_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/levmv/skot/decisions"
	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/transport"
)

func TestEvaluateSystemOne(t *testing.T) {
	for _, tc := range []struct {
		model, baseURL string
		profile        model.ResponseProfile
	}{
		{"jev-latest", "", ""},
		{"~typesafe/jev-latest", "https://openrouter.ai/api/v1/", model.ResponseProfileOpenRouter},
		{"microsoft/microsoft-decision-1", "https://openrouter.ai/api/v1", model.ResponseProfileOpenRouter},
		{"cloudflare/clef", "https://gateway.example/custom/v1", model.ResponseProfileOpenRouter},
	} {
		t.Run(tc.model, func(t *testing.T) {
			header := http.Header{"X-App": {"original"}}
			var calls int
			client, err := decisions.New(decisions.Config{
				Provider: "connection-label", Model: tc.model, BaseURL: tc.baseURL, ResponseProfile: tc.profile,
				APIKey: "test-key", Header: header,
				HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					calls++
					wantURL := strings.TrimRight(tc.baseURL, "/") + "/systemone"
					if tc.baseURL == "" {
						wantURL = "https://api.typesafe.ai/v1/systemone"
					}
					if request.Method != http.MethodPost || request.URL.String() != wantURL {
						t.Errorf("request = %s %s, want POST %s", request.Method, request.URL, wantURL)
					}
					for name, want := range map[string]string{
						"Authorization": "Bearer test-key", "Content-Type": "application/json",
						"Accept": "application/json", "X-App": "original", "TypeSafe-Danger-Zone-Images": "true",
					} {
						if got := request.Header.Get(name); got != want {
							t.Errorf("header %s = %q, want %q", name, got, want)
						}
					}
					body, err := io.ReadAll(request.Body)
					if err != nil {
						t.Fatal(err)
					}
					assertJSON(t, body, fmt.Sprintf(`{
						"model":%q,"state":{"ticket":"Refund <please>","photo":"photo-ref"},
						"questions":{
							"refund":{"type":"noul","instructions":"Is a refund requested?","criteria":{"true":{"meaning":"money back"}}},
							"team":{"type":"choice","instructions":["Choose a team",{"locale":"en"}],"criteria":{"billing":"Payments","other":null}},
							"urgency":{"type":"score","criteria":["Later",{"when":"today"}]},
							"location":{"type":"box","instructions":"Locate the receipt"}
						},
						"assets":{"photo-ref":{"type":"image","format":"png","data_base64":"AQID"}}
					}`, tc.model))
					response := jsonResponse(`{
						"model":"resolved-model","provider":"upstream","id":"decision-1","future_field":true,
						"answers":{
							"refund":{"type":"noul","noul":0,"future_field":true},
							"team":{"type":"choice","choice":"billing","confidence":0,"probabilities":{"billing":1,"other":0}},
							"urgency":{"type":"score","score":0.8,"confidence":0.6,"probabilities":{"0":0.2,"1":0.8},"legend":{"0":"Later","1":{"when":"today"}}},
							"location":{"type":"box","box":{"top":0,"bottom":1,"left":0.2,"right":0.9},"confidence":null}
						},
						"usage":{"input_tokens":120,"output_tokens":0,"cost":0.000012300,"future_counter":7}
					}`)
					if tc.baseURL == "" {
						response.Header.Del("X-Request-ID")
						response.Header.Set("X-TypeSafe-Request-ID", "request-1")
					}
					return response, nil
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			header.Set("X-App", "changed")
			result, err := client.Evaluate(context.Background(), decisions.Request{
				State: struct {
					Ticket string `json:"ticket"`
					Photo  string `json:"photo"`
				}{"Refund <please>", "photo-ref"},
				Questions: map[string]decisions.Question{
					"refund": {Type: decisions.Noul, Instructions: "Is a refund requested?", Criteria: map[string]any{"true": map[string]string{"meaning": "money back"}}},
					"team": {Type: decisions.Choice, Instructions: []any{"Choose a team", map[string]string{"locale": "en"}},
						Criteria: map[string]any{"billing": "Payments", "other": nil}},
					"urgency":  {Type: decisions.Score, Criteria: []any{"Later", map[string]string{"when": "today"}}},
					"location": {Type: decisions.Box, Instructions: "Locate the receipt"},
				},
				Assets: map[string]decisions.ImageAsset{"photo-ref": {Format: "png", Data: []byte{1, 2, 3}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(result.Answers) != 4 || result.Answers["refund"].Type != decisions.Noul || result.Answers["refund"].Noul != 0 {
				t.Fatalf("calls = %d, answers = %+v", calls, result.Answers)
			}
			team := result.Answers["team"]
			if team.Choice != "billing" || team.Confidence == nil || *team.Confidence != 0 || team.Probabilities["billing"] != 1 {
				t.Errorf("choice = %+v", team)
			}
			urgency := result.Answers["urgency"]
			if urgency.Score != 0.8 || urgency.Confidence == nil || *urgency.Confidence != 0.6 || urgency.Probabilities["1"] != 0.8 {
				t.Errorf("score = %+v", urgency)
			}
			assertJSON(t, urgency.Legend["1"], `{"when":"today"}`)
			location := result.Answers["location"]
			if location.Box == nil || *location.Box != (decisions.BoxCoordinates{Top: 0, Bottom: 1, Left: 0.2, Right: 0.9}) || location.Confidence != nil {
				t.Errorf("box = %+v", location)
			}
			usage := result.Usage
			if usage.Status != model.UsageFinal || usage.Model != "resolved-model" || usage.Provider != "upstream" || usage.ResponseID != "decision-1" || usage.RequestID != "request-1" {
				t.Errorf("usage = %+v", usage)
			}
			if usage.Tokens.InputTokens == nil || *usage.Tokens.InputTokens != 120 || usage.Tokens.OutputTokens == nil || *usage.Tokens.OutputTokens != 0 || usage.Tokens.TotalTokens == nil || *usage.Tokens.TotalTokens != 120 {
				t.Errorf("tokens = %+v", usage.Tokens)
			}
			if tc.profile == model.ResponseProfileOpenRouter {
				if !reflect.DeepEqual(usage.Costs, []model.ReportedCost{{Kind: "account_charge", Amount: "0.000012300", Currency: "USD"}}) {
					t.Errorf("costs = %+v", usage.Costs)
				}
			} else if len(usage.Costs) != 0 {
				t.Errorf("ordinary System One costs = %+v", usage.Costs)
			}
			assertJSON(t, result.RawUsage, `{"input_tokens":120,"output_tokens":0,"cost":0.000012300,"future_counter":7}`)
		})
	}
}

func TestEvaluateRejectsIncompleteDecisionsAndKeepsUsage(t *testing.T) {
	noul := decisions.Question{Type: decisions.Noul}
	choice := decisions.Question{Type: decisions.Choice, Criteria: map[string]string{"yes": "", "no": ""}}
	score := decisions.Question{Type: decisions.Score, Criteria: []string{"low", "high"}}
	box := decisions.Question{Type: decisions.Box}
	for _, tc := range []struct {
		name     string
		question decisions.Question
		answers  string
	}{
		{"missing answers", noul, `null`},
		{"empty answers", noul, `{}`},
		{"wrong name", noul, `{"other":{"type":"noul","noul":1}}`},
		{"wrong type", noul, `{"q":{"type":"score","score":0}}`},
		{"missing noul", noul, `{"q":{"type":"noul"}}`},
		{"null noul", noul, `{"q":{"type":"noul","noul":null}}`},
		{"invalid noul", noul, `{"q":{"type":"noul","noul":1.1}}`},
		{"unknown type", noul, `{"q":{"type":"future","noul":0.9}}`},
		{"missing choice", choice, `{"q":{"type":"choice","confidence":1,"probabilities":{"yes":1,"no":0}}}`},
		{"unknown choice", choice, `{"q":{"type":"choice","choice":"other","confidence":1,"probabilities":{"yes":1,"no":0}}}`},
		{"missing confidence", choice, `{"q":{"type":"choice","choice":"yes","probabilities":{"yes":1,"no":0}}}`},
		{"invalid confidence", choice, `{"q":{"type":"choice","choice":"yes","confidence":-1,"probabilities":{"yes":1,"no":0}}}`},
		{"null probability", choice, `{"q":{"type":"choice","choice":"yes","confidence":1,"probabilities":{"yes":null,"no":0}}}`},
		{"missing probability", choice, `{"q":{"type":"choice","choice":"yes","confidence":1,"probabilities":{"yes":1}}}`},
		{"wrong probability keys", choice, `{"q":{"type":"choice","choice":"yes","confidence":1,"probabilities":{"yes":1,"other":0}}}`},
		{"missing score", score, `{"q":{"type":"score","confidence":1,"probabilities":{"0":1,"1":0},"legend":{"0":"low","1":"high"}}}`},
		{"out of range score", score, `{"q":{"type":"score","score":2,"confidence":1,"probabilities":{"0":1,"1":0},"legend":{"0":"low","1":"high"}}}`},
		{"missing legend", score, `{"q":{"type":"score","score":0,"confidence":1,"probabilities":{"0":1,"1":0}}}`},
		{"missing box edge", box, `{"q":{"type":"box","box":{"top":0,"bottom":1,"left":0},"confidence":null}}`},
		{"null box edge", box, `{"q":{"type":"box","box":{"top":null,"bottom":1,"left":0,"right":1},"confidence":null}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := responseClient(t, `{"model":"resolved","answers":`+tc.answers+`,"usage":{"input_tokens":7,"output_tokens":0,"cost":0}}`)
			result, err := client.Evaluate(context.Background(), decisions.Request{State: "text", Questions: map[string]decisions.Question{"q": tc.question}})
			if !errors.Is(err, model.ErrProviderFailure) || errors.Is(err, model.ErrRequestNotSent) || result.Answers != nil {
				t.Fatalf("answers = %+v, error = %v", result.Answers, err)
			}
			if result.Usage.Status != model.UsageFinal || result.Usage.Tokens.Known().InputTokens != 7 || len(result.Usage.Costs) != 1 || result.Usage.Costs[0].Amount != "0" {
				t.Errorf("usage = %+v", result.Usage)
			}
		})
	}
}

func TestEvaluateEmptyChoiceAndZeroScore(t *testing.T) {
	client := responseClient(t, `{"model":"m","answers":{
		"choice":{"type":"choice","choice":"","confidence":0,"probabilities":{"":1}},
		"score":{"type":"score","score":0,"confidence":0,"probabilities":{"0":1},"legend":{"0":"only"}}
	}}`)
	result, err := client.Evaluate(context.Background(), decisions.Request{
		State: []any{"text", map[string]any{"count": 3}},
		Questions: map[string]decisions.Question{
			"choice": {Type: decisions.Choice, Criteria: map[string]any{"": nil}},
			"score":  {Type: decisions.Score, Criteria: []string{"only"}},
		},
	})
	if err != nil || len(result.Answers) != 2 || result.Answers["choice"].Choice != "" || result.Answers["score"].Score != 0 {
		t.Fatalf("response = %+v, error = %v", result, err)
	}
	if result.Usage.Status != model.UsageUnavailable || result.Usage.Tokens.InputTokens != nil || len(result.Usage.Costs) != 0 {
		t.Errorf("unreported usage = %+v", result.Usage)
	}
}

func TestEvaluateHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		status       int
		body         string
		kind         model.ProviderErrorKind
		retry        bool
		retryAfterMS string
		delay        time.Duration
	}{
		{401, `{"detail":"Invalid API key"}`, model.ProviderErrorAuthentication, false, "", 3 * time.Second},
		{422, `{"detail":[{"loc":["body","state"],"msg":"Field required","type":"missing"}]}`, model.ProviderErrorRequest, false, "", 3 * time.Second},
		{413, `{"detail":"Request too large"}`, model.ProviderErrorRequestTooLarge, false, "", 3 * time.Second},
		{429, `{"error":{"code":429,"message":"Slow down"}}`, model.ProviderErrorRateLimit, true, "", 3 * time.Second},
		{529, `{"detail":"Overloaded"}`, model.ProviderErrorUnavailable, true, "", 3 * time.Second},
		{429, `{"detail":"Slow down"}`, model.ProviderErrorRateLimit, true, "125.5", 125500 * time.Microsecond},
		{429, `{"detail":"Slow down"}`, model.ProviderErrorRateLimit, true, "0", 0},
		{429, `{"detail":"Slow down"}`, model.ProviderErrorRateLimit, true, "invalid", 3 * time.Second},
	} {
		t.Run(fmt.Sprintf("status=%d/retry-after-ms=%s", tc.status, tc.retryAfterMS), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "3")
				if tc.retryAfterMS != "" {
					w.Header().Set("Retry-After-Ms", tc.retryAfterMS)
				}
				w.Header().Set("X-TypeSafe-Request-ID", "failed-request")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			client, err := decisions.New(decisions.Config{Provider: "typesafe", Model: "jev-latest", BaseURL: server.URL + "/v1"})
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Evaluate(context.Background(), noulRequest())
			var failure *model.ProviderError
			if !errors.As(err, &failure) || failure.StatusCode != tc.status || failure.Kind != tc.kind || failure.Retryable != tc.retry || failure.RetryAfter != tc.delay {
				t.Fatalf("error = %v, metadata = %+v", err, failure)
			}
			if !errors.Is(err, model.ErrProviderFailure) || calls.Load() != 1 || result.Usage.RequestID != "failed-request" {
				t.Errorf("calls = %d, usage = %+v, error = %v", calls.Load(), result.Usage, err)
			}
		})
	}
}

func TestEvaluateInBandError(t *testing.T) {
	client := responseClient(t, `{"error":{"code":429,"message":"Slow down"},"usage":{"input_tokens":9,"cost":0.01}}`)
	result, err := client.Evaluate(context.Background(), noulRequest())
	var failure *model.ProviderError
	if !errors.As(err, &failure) || failure.StatusCode != 0 || failure.Kind != model.ProviderErrorRateLimit || !failure.Retryable {
		t.Fatalf("error = %v, metadata = %+v", err, failure)
	}
	if result.Answers != nil || result.Usage.Tokens.Known().InputTokens != 9 || result.Usage.Tokens.OutputTokens != nil || len(result.Usage.Costs) != 1 {
		t.Errorf("response = %+v", result)
	}
}

func TestEvaluateRejectsInvalidRequestsBeforeSending(t *testing.T) {
	var calls int
	client, err := decisions.New(decisions.Config{
		Provider: "test", Model: "m",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("unexpected request")
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		request decisions.Request
	}{
		{"missing state", decisions.Request{Questions: noulRequest().Questions}},
		{"scalar state", decisions.Request{State: true, Questions: noulRequest().Questions}},
		{"missing questions", decisions.Request{State: "text"}},
		{"unknown type", withQuestion(decisions.Question{Type: "future"})},
		{"invalid instructions", withQuestion(decisions.Question{Type: decisions.Noul, Instructions: 1})},
		{"invalid JSON", withQuestion(decisions.Question{Type: decisions.Noul, Instructions: jsontext.Value(`{`)})},
		{"unencodable content", withQuestion(decisions.Question{Type: decisions.Noul, Instructions: func() {}})},
		{"noul criteria", withQuestion(decisions.Question{Type: decisions.Noul, Criteria: map[string]string{"yes": "true"}})},
		{"choice criteria", withQuestion(decisions.Question{Type: decisions.Choice, Criteria: []string{"one"}})},
		{"empty choice", withQuestion(decisions.Question{Type: decisions.Choice, Criteria: map[string]string{}})},
		{"score criteria", withQuestion(decisions.Question{Type: decisions.Score, Criteria: map[string]string{"one": "low"}})},
		{"null score level", withQuestion(decisions.Question{Type: decisions.Score, Criteria: []any{nil}})},
		{"box criteria", withQuestion(decisions.Question{Type: decisions.Box, Criteria: []string{"one"}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := client.Evaluate(context.Background(), tc.request)
			if !errors.Is(err, model.ErrInvalidRequest) || !errors.Is(err, model.ErrRequestNotSent) || errors.Is(err, model.ErrProviderFailure) || result.Usage.Status != model.UsageUnavailable {
				t.Errorf("usage = %+v, error = %v", result.Usage, err)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("sent %d invalid requests", calls)
	}
}

func TestEvaluateResponseLimits(t *testing.T) {
	body := `{"model":"m","answers":{"q":{"type":"noul","noul":1}}}`
	for _, knownLength := range []bool{true, false} {
		for _, limit := range []int{len(body) - 1, len(body)} {
			t.Run(fmt.Sprintf("known=%v/limit=%d", knownLength, limit), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if knownLength {
						w.Header().Set("Content-Length", strconv.Itoa(len(body)))
					} else {
						w.(http.Flusher).Flush()
					}
					_, _ = io.WriteString(w, body)
				}))
				t.Cleanup(server.Close)
				client, err := decisions.New(decisions.Config{Provider: "test", Model: "m", BaseURL: server.URL, MaxResponseBytes: int64(limit)})
				if err != nil {
					t.Fatal(err)
				}
				result, err := client.Evaluate(context.Background(), noulRequest())
				if limit < len(body) {
					if !errors.Is(err, decisions.ErrOutputTooLarge) || errors.Is(err, model.ErrProviderFailure) || result.Answers != nil {
						t.Errorf("response = %+v, error = %v", result, err)
					}
				} else if err != nil || result.Answers["q"].Noul != 1 {
					t.Errorf("response = %+v, error = %v", result, err)
				}
			})
		}
	}
}

func TestEvaluateCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"model":`)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	client, err := decisions.New(decisions.Config{Provider: "test", Model: "m", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		select {
		case <-started:
			cancel()
		case <-ctx.Done():
		}
	}()
	result, err := client.Evaluate(ctx, noulRequest())
	if !errors.Is(err, context.Canceled) || errors.Is(err, model.ErrRequestNotSent) || result.Answers != nil {
		t.Fatalf("response = %+v, error = %v", result, err)
	}
	_, err = client.Evaluate(ctx, noulRequest())
	if !errors.Is(err, context.Canceled) || !errors.Is(err, model.ErrRequestNotSent) {
		t.Errorf("pre-canceled error = %v", err)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*decisions.Config)
	}{
		{"missing provider", func(c *decisions.Config) { c.Provider = " " }},
		{"missing model", func(c *decisions.Config) { c.Model = "" }},
		{"relative URL", func(c *decisions.Config) { c.BaseURL = "/v1" }},
		{"URL credentials", func(c *decisions.Config) { c.BaseURL = "https://secret@example.com/v1" }},
		{"URL query", func(c *decisions.Config) { c.BaseURL = "https://example.com/v1?key=secret" }},
		{"URL fragment", func(c *decisions.Config) { c.BaseURL = "https://example.com/v1#ignored" }},
		{"unknown profile", func(c *decisions.Config) { c.ResponseProfile = "unknown" }},
		{"negative limit", func(c *decisions.Config) { c.MaxResponseBytes = -1 }},
		{"ambiguous auth", func(c *decisions.Config) { c.APIKey = "key"; c.Authorizer = transport.BearerToken("other") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := decisions.Config{Provider: "test", Model: "m"}
			tc.change(&config)
			_, err := decisions.New(config)
			if !errors.Is(err, model.ErrInvalidRequest) {
				t.Errorf("error = %v", err)
			}
		})
	}
}

func noulRequest() decisions.Request {
	return withQuestion(decisions.Question{Type: decisions.Noul, Instructions: "Is it true?"})
}

func withQuestion(question decisions.Question) decisions.Request {
	return decisions.Request{State: "text", Questions: map[string]decisions.Question{"q": question}}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"X-Request-Id": {"request-1"}},
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)),
	}
}

func responseClient(t *testing.T, body string) *decisions.Client {
	t.Helper()
	client, err := decisions.New(decisions.Config{
		Provider: "test", Model: "m", ResponseProfile: model.ResponseProfileOpenRouter,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(body), nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func assertJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("JSON = %s, want %s", got, want)
	}
}
