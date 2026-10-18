package images_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/levmv/skot/images"
	"github.com/levmv/skot/model"
)

func TestGenerateOpenAIImagesAndEdits(t *testing.T) {
	first, second := testPNG(t, 3, 2), testPNG(t, 5, 4)
	var requests []map[string]jsontext.Value
	var paths []string
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer secret" || request.Header.Get("Accept") != "application/json" {
			t.Errorf("authorization = %q, accept = %q", request.Header.Get("Authorization"), request.Header.Get("Accept"))
		}
		var body map[string]jsontext.Value
		if err := json.UnmarshalRead(request.Body, &body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests, paths = append(requests, body), append(paths, request.URL.Path)
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("X-Request-ID", "req_1")
		fmt.Fprintf(writer, `{"created":1,"data":[{"b64_json":%q}],"output_format":"png","usage":{"input_tokens":50,"input_tokens_details":{"text_tokens":10,"image_tokens":40},"output_tokens":200,"output_tokens_details":{"image_tokens":190,"text_tokens":10},"total_tokens":250}}`,
			base64.StdEncoding.EncodeToString(second))
	}))
	client := newClient(t, server, images.Config{API: images.OpenAIImages, BaseURL: "http://example.com/v1/", APIKey: "secret"})

	created, err := client.Generate(t.Context(), images.Request{Prompt: " Светлая комната "}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Images) != 1 || !bytes.Equal(created.Images[0].Data, second) ||
		created.Images[0].MediaType != "image/png" || created.Images[0].Width != 5 || created.Images[0].Height != 4 {
		t.Fatalf("images = %+v", created.Images)
	}
	tokens := created.Usage.Tokens
	if created.Usage.Status != model.UsageFinal || created.Usage.RequestID != "req_1" || tokens.Known() != (model.TokenCounts{
		InputTokens: 50, InputTextTokens: 10, InputImageTokens: 40,
		OutputTokens: 200, OutputTextTokens: 10, OutputImageTokens: 190, TotalTokens: 250,
	}) || !bytes.Contains(created.RawUsage, []byte(`"input_tokens_details"`)) {
		t.Fatalf("usage = %+v, raw = %s", created.Usage, created.RawUsage)
	}

	_, err = client.Generate(t.Context(), images.Request{
		Prompt:     "Сделай темнее",
		References: []images.Image{{MediaType: "image/png", Data: first}, {MediaType: "Image/JPEG", Data: []byte("jpeg")}},
		Count:      2, Size: "1536x1024", Quality: "high", Background: "opaque", Format: "jpeg", Compression: new(0),
		OpenAI: &images.OpenAIOptions{SafetyIdentifier: "user-hash"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if paths[0] != "/v1/images/generations" || paths[1] != "/v1/images/edits" {
		t.Fatalf("paths = %q", paths)
	}
	if got := encodeTest(t, requests[0]); got != `{"model":"gpt-image","n":1,"prompt":" Светлая комната "}` {
		t.Fatalf("generation request = %s", got)
	}
	want := fmt.Sprintf(`{"background":"opaque","images":[{"image_url":"data:image/png;base64,%s"},{"image_url":"data:image/jpeg;base64,anBlZw=="}],"model":"gpt-image","n":2,"output_compression":0,"output_format":"jpeg","prompt":"Сделай темнее","quality":"high","safety_identifier":"user-hash","size":"1536x1024"}`,
		base64.StdEncoding.EncodeToString(first))
	if got := encodeTest(t, requests[1]); got != want {
		t.Fatalf("edit request = %s", got)
	}
}

func TestGenerateOpenRouterImages(t *testing.T) {
	reference, result := testPNG(t, 2, 2), testPNG(t, 4, 3)
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)
	var received map[string]jsontext.Value
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/images" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if err := json.UnmarshalRead(request.Body, &received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(writer, `{"data":[{"b64_json":%q,"media_type":"image/jpeg"},{"b64_json":%q,"media_type":"image/svg+xml"}],"usage":{"prompt_tokens":0,"completion_tokens":4175,"total_tokens":4175,"cost":0.04}}`,
			base64.StdEncoding.EncodeToString(result), base64.StdEncoding.EncodeToString(svg))
	}))
	client := newClient(t, server, images.Config{API: images.OpenRouterImages, BaseURL: "http://example.com/api/v1"})

	response, err := client.Generate(t.Context(), images.Request{
		Prompt: "Векторный логотип", References: []images.Image{{MediaType: "image/png", Data: reference}}, Count: 2,
		OpenRouter: &images.OpenRouterOptions{AspectRatio: "16:9", Resolution: "2K", User: "user-hash"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"aspect_ratio":"16:9","input_references":[{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}],"model":"gpt-image","n":2,"prompt":"Векторный логотип","resolution":"2K","user":"user-hash"}`,
		base64.StdEncoding.EncodeToString(reference))
	if got := encodeTest(t, received); got != want {
		t.Fatalf("request = %s", got)
	}
	// Detected bytes win over the reported type; unrecognized files keep it.
	if len(response.Images) != 2 || response.Images[0].MediaType != "image/png" || response.Images[0].Width != 4 ||
		response.Images[1].MediaType != "image/svg+xml" || response.Images[1].Width != 0 || !bytes.Equal(response.Images[1].Data, svg) {
		t.Fatalf("images = %+v", response.Images)
	}
	if len(response.Usage.Costs) != 1 || response.Usage.Costs[0] != (model.ReportedCost{Kind: "account_charge", Amount: "0.04", Currency: "USD"}) ||
		response.Usage.Tokens.Known() != (model.TokenCounts{OutputTokens: 4175, TotalTokens: 4175}) || response.Usage.Tokens.OutputImageTokens != nil {
		t.Fatalf("usage = %+v", response.Usage)
	}
}

func TestGenerateKeepsAvailableResultsOnFailure(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(testPNG(t, 1, 1))
	const usage = `"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}`
	for _, test := range []struct {
		name, body  string
		status      int
		maxBytes    int64
		images      int
		target      error
		providerErr bool
		usage       bool
	}{
		{name: "malformed metadata", body: `{"created":"now","data":[{"b64_json":"` + good + `","media_type":5,"url":{}}],` + usage + `}`, images: 1, usage: true},
		{name: "corrupt entry", body: `{"data":[{"b64_json":"%%%"},7,{"b64_json":"` + good + `"}],` + usage + `}`, images: 1, target: model.ErrProviderFailure, usage: true},
		{name: "malformed data", body: `{"data":{"b64_json":"` + good + `"},` + usage + `}`, target: model.ErrProviderFailure, usage: true},
		{name: "URL only", body: `{"data":[{"url":"https://example.test/image.png"}],` + usage + `}`, target: model.ErrProviderFailure, usage: true},
		{name: "no image", body: `{"data":[],` + usage + `}`, target: images.ErrNoImage, usage: true},
		{name: "provider error", body: `{"data":[],"error":{"message":"blocked","code":"moderation_blocked"},` + usage + `}`, target: model.ErrProviderFailure, providerErr: true, usage: true},
		{name: "HTTP error", status: http.StatusInternalServerError, body: `{"error":{"message":"busy"}}`, target: model.ErrProviderFailure, providerErr: true},
		{name: "response too large", body: `{"data":[{"b64_json":"` + good + `"}],` + usage + `}`, maxBytes: 64, target: images.ErrOutputTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				writer.Header().Set("Content-Type", "application/json")
				writer.Header().Set("X-Request-ID", "req_1")
				writer.WriteHeader(max(test.status, http.StatusOK))
				io.WriteString(writer, test.body)
			}))
			client := newClient(t, server, images.Config{API: images.OpenAIImages, MaxResponseBytes: test.maxBytes})
			response, err := client.Generate(t.Context(), images.Request{Prompt: "room"}, nil)
			var providerErr *model.ProviderError
			if !errors.Is(err, test.target) || errors.As(err, &providerErr) != test.providerErr || errors.Is(err, model.ErrRequestNotSent) {
				t.Fatalf("error = %v", err)
			}
			// Only the local response limit is not a provider failure.
			if errors.Is(err, model.ErrProviderFailure) != (test.target != nil && test.target != images.ErrOutputTooLarge) {
				t.Fatalf("error = %v, provider failure = %t", err, errors.Is(err, model.ErrProviderFailure))
			}
			if len(response.Images) != test.images || requests.Load() != 1 || response.Usage.RequestID != "req_1" {
				t.Fatalf("images = %d, requests = %d, usage = %+v", len(response.Images), requests.Load(), response.Usage)
			}
			if test.usage != (response.Usage.Status == model.UsageFinal && response.Usage.Tokens.Known().TotalTokens == 12) {
				t.Fatalf("usage = %+v", response.Usage)
			}
		})
	}
}

func TestGenerateClassifiesFailuresAfterSending(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	client := newClient(t, server, images.Config{API: images.OpenAIImages})
	_, err := client.Generate(t.Context(), images.Request{Prompt: "room"}, nil)
	if !errors.Is(err, model.ErrProviderFailure) || errors.Is(err, model.ErrRequestNotSent) {
		t.Fatalf("error = %v", err)
	}
}

func TestGenerateStreamsPreviews(t *testing.T) {
	previews := [][]byte{testPNG(t, 1, 1), testPNG(t, 2, 1)}
	final := testPNG(t, 8, 6)
	var received map[string]jsontext.Value
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/images/edits" || request.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("path = %q, accept = %q", request.URL.Path, request.Header.Get("Accept"))
		}
		if err := json.UnmarshalRead(request.Body, &received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		// An undecodable preview is skipped without ending the stream.
		io.WriteString(writer, "data: {\"type\":\"image_edit.partial_image\",\"b64_json\":\"%%%\"}\n\n")
		for index, preview := range previews {
			fmt.Fprintf(writer, "event: image_edit.partial_image\ndata: {\"type\":\"image_edit.partial_image\",\"partial_image_index\":%d,\"b64_json\":%q}\n\n",
				index, base64.StdEncoding.EncodeToString(preview))
		}
		fmt.Fprintf(writer, "data: {\"type\":\"image_edit.completed\",\"b64_json\":%q,\"usage\":{\"input_tokens\":3,\"output_tokens\":4,\"total_tokens\":7}}\n\n",
			base64.StdEncoding.EncodeToString(final))
		writer.(http.Flusher).Flush()
		// The completed event ends the call without waiting for [DONE].
		<-request.Context().Done()
	}))
	client := newClient(t, server, images.Config{API: images.OpenAIImages})
	var got []images.Image
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	response, err := client.Generate(ctx, images.Request{
		Prompt: "darker", References: []images.Image{{MediaType: "image/png", Data: previews[0]}},
		OpenAI: &images.OpenAIOptions{PartialImages: new(2)},
	}, func(preview images.Image) { got = append(got, preview) })
	if err != nil {
		t.Fatal(err)
	}
	if string(received["stream"]) != "true" || string(received["partial_images"]) != "2" {
		t.Fatalf("request = %v", received)
	}
	if len(got) != 2 || !bytes.Equal(got[0].Data, previews[0]) || got[1].Width != 2 {
		t.Fatalf("previews = %+v", got)
	}
	if len(response.Images) != 1 || !bytes.Equal(response.Images[0].Data, final) || response.Images[0].Width != 8 ||
		response.Usage.Status != model.UsageFinal || response.Usage.Tokens.Known().TotalTokens != 7 {
		t.Fatalf("response = %+v", response)
	}
}

func TestGenerateStreamFailures(t *testing.T) {
	preview := "data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"" + base64.StdEncoding.EncodeToString(testPNG(t, 1, 1)) + "\"}\n\n"
	for _, test := range []struct {
		name, body  string
		idle        time.Duration
		target      error
		providerErr bool
		usage       bool
	}{
		{name: "corrupt final image", body: preview + `data: {"type":"image_generation.completed","b64_json":"%%%","usage":{"prompt_tokens":3,"completion_tokens":4,"cost":0.04}}` + "\n\n", target: model.ErrProviderFailure, usage: true},
		{name: "EOF after preview", body: preview, target: io.ErrUnexpectedEOF},
		{name: "DONE after preview", body: preview + "data: [DONE]\n\n", target: model.ErrProviderFailure},
		{name: "error event", body: preview + `data: {"type":"error","error":{"message":"failed","code":"server_error"}}` + "\n\n", target: model.ErrProviderFailure, providerErr: true},
		{name: "idle", body: preview, idle: 20 * time.Millisecond, target: model.ErrModelStreamIdle},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(writer, test.body)
				if test.idle != 0 {
					writer.(http.Flusher).Flush()
					<-request.Context().Done()
				}
			}))
			client := newClient(t, server, images.Config{API: images.OpenRouterImages})
			previews := 0
			response, err := client.Generate(t.Context(), images.Request{Prompt: "room", StreamIdleTimeout: test.idle}, func(images.Image) { previews++ })
			var providerErr *model.ProviderError
			if !errors.Is(err, test.target) || !errors.Is(err, model.ErrProviderFailure) || errors.As(err, &providerErr) != test.providerErr {
				t.Fatalf("error = %v", err)
			}
			if test.name == "DONE after preview" && errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("[DONE] reported as EOF: %v", err)
			}
			// A preview never becomes the result.
			if previews != 1 || len(response.Images) != 0 {
				t.Fatalf("previews = %d, images = %d", previews, len(response.Images))
			}
			if test.usage != (response.Usage.Status == model.UsageFinal && len(response.Usage.Costs) == 1) {
				t.Fatalf("usage = %+v", response.Usage)
			}
		})
	}
}

func TestGenerateRejectsInvalidRequestsBeforeSending(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("invalid request was sent")
	}))
	client := newClient(t, server, images.Config{API: images.OpenAIImages})
	preview := func(images.Image) {}
	for _, test := range []struct {
		name    string
		request images.Request
		preview func(images.Image)
	}{
		{name: "blank prompt", request: images.Request{Prompt: " \n"}},
		{name: "negative count", request: images.Request{Prompt: "x", Count: -1}},
		{name: "streamed count", request: images.Request{Prompt: "x", Count: 2}, preview: preview},
		{name: "compression", request: images.Request{Prompt: "x", Compression: new(101)}},
		{name: "idle without stream", request: images.Request{Prompt: "x", StreamIdleTimeout: time.Second}},
		{name: "partial without stream", request: images.Request{Prompt: "x", OpenAI: &images.OpenAIOptions{PartialImages: new(1)}}},
		{name: "partial range", request: images.Request{Prompt: "x", OpenAI: &images.OpenAIOptions{PartialImages: new(4)}}, preview: preview},
		{name: "other API options", request: images.Request{Prompt: "x", OpenRouter: &images.OpenRouterOptions{}}},
		{name: "reference media type", request: images.Request{Prompt: "x", References: []images.Image{{MediaType: "application/pdf", Data: []byte("x")}}}},
		{name: "empty reference", request: images.Request{Prompt: "x", References: []images.Image{{MediaType: "image/png"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response, err := client.Generate(t.Context(), test.request, test.preview)
			if !errors.Is(err, model.ErrInvalidRequest) || !errors.Is(err, model.ErrRequestNotSent) || response.Usage.Status != model.UsageUnavailable {
				t.Fatalf("response = %+v, error = %v", response, err)
			}
		})
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	for _, config := range []images.Config{
		{Provider: "openai", Model: "gpt-image"},
		{API: "dall_e", Provider: "openai", Model: "gpt-image"},
		{API: images.OpenAIImages, Model: "gpt-image"},
		{API: images.OpenAIImages, Provider: "openai"},
		{API: images.OpenAIImages, Provider: "openai", Model: "gpt-image", APIKey: "key", Authorizer: noAuthorizer{}},
		{API: images.OpenAIImages, Provider: "openai", Model: "gpt-image", BaseURL: "https://api.openai.com/v1?key=secret"},
		{API: images.OpenAIImages, Provider: "openai", Model: "gpt-image", MaxResponseBytes: -1},
	} {
		if _, err := images.New(config); !errors.Is(err, model.ErrInvalidRequest) {
			t.Errorf("New(%+v) error = %v", config, err)
		}
	}
}

type noAuthorizer struct{}

func (noAuthorizer) Authorize(context.Context, *http.Request) error { return nil }

func newClient(t *testing.T, server *httptest.Server, config images.Config) *images.Client {
	t.Helper()
	config.Provider, config.Model, config.HTTPClient = "test", "gpt-image", server.Client()
	if config.BaseURL == "" {
		config.BaseURL = "http://example.com/v1"
	}
	client, err := images.New(config)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewGray(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func encodeTest(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}
