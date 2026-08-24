package agent

import (
	"encoding/json"
	"slices"
	"testing"

	productlimits "github.com/levmv/skot/internal/limits"
)

func TestContentKeepsLegacyTextJSONAndRoundTripsImages(t *testing.T) {
	textJSON, err := json.Marshal(TextContent("ordinary result"))
	if err != nil {
		t.Fatal(err)
	}
	if string(textJSON) != `"ordinary result"` {
		t.Fatalf("text content JSON = %s", textJSON)
	}
	var legacy Content
	if err := json.Unmarshal([]byte(`"legacy journal result"`), &legacy); err != nil || legacy.Text() != "legacy journal result" || legacy.HasImage() {
		t.Fatalf("legacy content = %#v, error = %v", legacy, err)
	}

	original := ImageToolContent("screenshot", ImageContent{
		MediaType: "image/png", Data: []byte{1, 2, 3, 4}, Width: 12, Height: 8,
	})
	imageJSON, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var replayed Content
	if err := json.Unmarshal(imageJSON, &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.Text() != "screenshot" || !replayed.HasImage() || len(replayed) != 2 || replayed[1].Image.Width != 12 || !slices.Equal(replayed[1].Image.Data, original[1].Image.Data) {
		t.Fatalf("replayed content = %#v", replayed)
	}
}

func TestContentJSONRejectsUnsupportedImage(t *testing.T) {
	_, err := json.Marshal(Content{{Kind: ContentPartImage, Image: &ImageContent{
		MediaType: "image/svg+xml", Data: []byte("svg"), Width: 1, Height: 1,
	}}})
	if err == nil {
		t.Fatal("unsupported image content was serialized")
	}
}

func TestContentBoundsAggregateImageBytes(t *testing.T) {
	data := make([]byte, productlimits.MaxContentImageBytes/2+1)
	_, err := json.Marshal(Content{
		{Kind: ContentPartImage, Image: &ImageContent{MediaType: "image/png", Data: data, Width: 1, Height: 1}},
		{Kind: ContentPartImage, Image: &ImageContent{MediaType: "image/png", Data: data, Width: 1, Height: 1}},
	})
	if err == nil {
		t.Fatal("aggregate image payload was serialized")
	}
}
