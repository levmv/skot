package provider_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/levmv/skot/model"
	"github.com/levmv/skot/provider"
)

func TestUserImagesSurviveCallsAndReplay(t *testing.T) {
	first, second := contentTestImages(t)
	for _, api := range []string{"chat_completions", "responses", "anthropic_messages"} {
		t.Run(api, func(t *testing.T) {
			var bodies [][]byte
			selected := contentTestModel(t, api, "openai/test", func(request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				bodies = append(bodies, body)
			})
			var replay model.ReplayContext
			replay, err := replay.ForModel(selected.Info)
			if err != nil {
				t.Fatal(err)
			}
			mixed := model.Item{Kind: model.ItemUserText, Content: model.Content{
				{Kind: model.ContentPartText, Text: "Compare:"},
				{Kind: model.ContentPartImage, Image: &first},
				{Kind: model.ContentPartImage, Image: &second},
				{Kind: model.ContentPartText, Text: "What changed?"},
			}}
			history := []model.Item{mixed}
			response, err := selected.Backend.Complete(t.Context(), replay.PrepareRequest(selected.Backend, model.Request{Items: history}), nil)
			if err != nil {
				t.Fatal(err)
			}
			accepted, err := replay.AcceptResponse(response)
			if err != nil {
				t.Fatal(err)
			}
			history = append(history, accepted.Items...)
			// The host persists portable history, then adds an image-only question.
			saved, err := json.Marshal(history)
			if err != nil {
				t.Fatal(err)
			}
			var restored []model.Item
			if err := json.Unmarshal(saved, &restored); err != nil {
				t.Fatal(err)
			}
			restored = append(restored, model.Item{Kind: model.ItemUserText, Content: model.Content{{Kind: model.ContentPartImage, Image: &second}}})
			next, err := replay.ForModel(selected.Info)
			if err != nil || next.Epoch != replay.Epoch {
				t.Fatalf("continued replay = %+v, %v", next, err)
			}
			if _, err := selected.Backend.Complete(t.Context(), next.PrepareRequest(selected.Backend, model.Request{Items: restored}), nil); err != nil {
				t.Fatal(err)
			}
			if len(bodies) != 2 {
				t.Fatalf("generation attempts = %d", len(bodies))
			}
			textType := "text"
			if api == "responses" {
				textType = "input_text"
			}
			wantMixed := fmt.Sprintf(`[{"type":%q,"text":"Compare:"},%s,%s,{"type":%q,"text":"What changed?"}]`, textType, expectedImagePart(api, first), expectedImagePart(api, second), textType)
			for _, body := range bodies {
				messages := contentTestMessages(t, api, body)
				assertContentJSON(t, messages[0].Content, wantMixed)
			}
			messages := contentTestMessages(t, api, bodies[1])
			if len(messages) != 3 || messages[0].Role != "user" || messages[1].Role != "assistant" || messages[2].Role != "user" {
				t.Fatalf("replayed messages = %s", bodies[1])
			}
			assertContentJSON(t, messages[2].Content, "["+expectedImagePart(api, second)+"]")
		})
	}
}

func TestUserTextKeepsWireEncoding(t *testing.T) {
	for _, api := range []string{"chat_completions", "responses", "anthropic_messages"} {
		t.Run(api, func(t *testing.T) {
			var bodies [][]byte
			selected := contentTestModel(t, api, "openai/test", func(request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				bodies = append(bodies, body)
			})
			for _, item := range []model.Item{
				{Kind: model.ItemUserText, Text: "ordinary text"},
				{Kind: model.ItemUserText, Content: model.Content{{Kind: model.ContentPartText, Text: "ordinary "}, {Kind: model.ContentPartText, Text: "text"}}},
			} {
				if _, err := selected.Backend.Complete(t.Context(), model.Request{Items: []model.Item{item}}, nil); err != nil {
					t.Fatal(err)
				}
			}
			if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
				t.Fatalf("text encoding differs: %q", bodies)
			}
			want := `"ordinary text"`
			if api == "anthropic_messages" {
				want = `[{"type":"text","text":"ordinary text"}]`
			}
			assertContentJSON(t, contentTestMessages(t, api, bodies[0])[0].Content, want)
		})
	}
}

func TestInvalidImageRequestsAreNotSent(t *testing.T) {
	first, _ := contentTestImages(t)
	imageContent := model.Content{{Kind: model.ContentPartImage, Image: &first}}
	large := first
	large.Data = make([]byte, (16<<20)/2+1)
	user := func(content model.Content) model.Item { return model.Item{Kind: model.ItemUserText, Content: content} }
	withToolResult := func(content model.Content) []model.Item {
		return []model.Item{
			{Kind: model.ItemToolCall, ResponseID: "response-1", ToolCall: &model.ToolCall{ID: "call-1", Name: "read", RawArguments: `{}`}},
			{Kind: model.ItemToolResult, ToolResult: &model.ToolResult{CallID: "call-1", Content: content}},
		}
	}
	var sixteen model.Content
	for range 16 {
		sixteen = append(sixteen, imageContent[0])
	}
	badDimensions := first
	badDimensions.Height = 0
	badMedia := first
	badMedia.MediaType = "image/svg+xml"
	for _, api := range []string{"chat_completions", "responses", "anthropic_messages"} {
		t.Run(api, func(t *testing.T) {
			calls := 0
			selected := contentTestModel(t, api, "openai/test", func(*http.Request) { calls++ })
			for _, test := range []struct {
				name     string
				items    []model.Item
				tooLarge bool
			}{
				{"ambiguous user", []model.Item{{Kind: model.ItemUserText, Text: "both", Content: imageContent}}, false},
				{"assistant content", []model.Item{{Kind: model.ItemAssistantText, ResponseID: "response-1", Content: imageContent}}, false},
				{"missing image", []model.Item{user(model.Content{{Kind: model.ContentPartImage}})}, false},
				{"unknown part", []model.Item{user(model.Content{{Kind: "file"}})}, false},
				{"image dimensions", []model.Item{user(model.Content{{Kind: model.ContentPartImage, Image: &badDimensions}})}, false},
				{"tool image type", withToolResult(model.Content{{Kind: model.ContentPartImage, Image: &badMedia}}), false},
				{"aggregate count", append([]model.Item{user(sixteen)}, withToolResult(imageContent)...), true},
				{"aggregate bytes", append([]model.Item{user(model.Content{{Kind: model.ContentPartImage, Image: &large}})}, withToolResult(model.Content{{Kind: model.ContentPartImage, Image: &large}})...), true},
			} {
				t.Run(test.name, func(t *testing.T) {
					_, err := selected.Backend.Complete(t.Context(), model.Request{Items: test.items}, nil)
					if !errors.Is(err, model.ErrInvalidRequest) || !errors.Is(err, model.ErrRequestNotSent) || errors.Is(err, model.ErrProviderFailure) || calls != 0 {
						t.Fatalf("calls=%d error=%v", calls, err)
					}
					if test.tooLarge && !errors.Is(err, model.ErrModelRequestTooLarge) {
						t.Fatalf("size failure lost classification: %v", err)
					}
				})
			}
			unsupported := contentTestModel(t, api, "deepseek/deepseek-v4-pro", func(*http.Request) { calls++ })
			if !unsupported.Info.ImageInputUnsupported {
				t.Fatal("expected a route known not to accept images")
			}
			for _, items := range [][]model.Item{{user(imageContent)}, withToolResult(imageContent)} {
				_, err := unsupported.Backend.Complete(t.Context(), model.Request{Items: items}, nil)
				if !errors.Is(err, model.ErrImageInputUnsupported) || !errors.Is(err, model.ErrInvalidRequest) || !errors.Is(err, model.ErrRequestNotSent) || calls != 0 {
					t.Fatalf("unsupported route: calls=%d error=%v", calls, err)
				}
			}
		})
	}
}

func contentTestImages(t *testing.T) (model.ImageContent, model.ImageContent) {
	t.Helper()
	source := image.NewGray(image.Rect(0, 0, 2, 1))
	var pngData, jpegData bytes.Buffer
	if err := png.Encode(&pngData, source); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jpegData, source, nil); err != nil {
		t.Fatal(err)
	}
	return model.ImageContent{MediaType: "image/png", Data: pngData.Bytes(), Width: 2, Height: 1},
		model.ImageContent{MediaType: "image/jpeg", Data: jpegData.Bytes(), Width: 2, Height: 1}
}

func contentTestModel(t *testing.T, api, uri string, receive func(*http.Request)) *provider.Model {
	t.Helper()
	stream := ""
	switch api {
	case "chat_completions":
		stream = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	case "responses":
		stream = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}]}}\n\n"
	case "anthropic_messages":
		stream = "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"OK\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		receive(request)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	})}
	selected, err := provider.Open(t.Context(), provider.Config{URI: uri, API: api, APIKey: "test-key", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	return selected
}

type contentTestMessage struct {
	Role    string         `json:"role"`
	Content jsontext.Value `json:"content"`
}

func contentTestMessages(t *testing.T, api string, body []byte) []contentTestMessage {
	t.Helper()
	var request struct {
		Messages []contentTestMessage `json:"messages"`
		Input    []contentTestMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if api == "responses" {
		return request.Input
	}
	return request.Messages
}

func expectedImagePart(api string, image model.ImageContent) string {
	data := base64.StdEncoding.EncodeToString(image.Data)
	url := "data:" + image.MediaType + ";base64," + data
	switch api {
	case "anthropic_messages":
		return fmt.Sprintf(`{"type":"image","source":{"type":"base64","media_type":%q,"data":%q}}`, image.MediaType, data)
	case "responses":
		return fmt.Sprintf(`{"type":"input_image","image_url":%q}`, url)
	default:
		return fmt.Sprintf(`{"type":"image_url","image_url":{"url":%q}}`, url)
	}
}

func assertContentJSON(t *testing.T, got jsontext.Value, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("content = %s, want %s", got, want)
	}
}
