package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	productlimits "github.com/levmv/skot/internal/limits"
)

// Content is the provider-neutral, model-visible payload of a tool result.
// Text-only content keeps its historical JSON string representation; content
// containing media is encoded as tagged parts. This lets existing journals
// replay without maintaining a second in-memory representation.
type Content []ContentPart

type ContentPartKind string

const (
	ContentPartText  ContentPartKind = "text"
	ContentPartImage ContentPartKind = "image"
)

type ContentPart struct {
	Kind  ContentPartKind `json:"type"`
	Text  string          `json:"text,omitempty"`
	Image *ImageContent   `json:"image,omitempty"`
}

// ImageContent contains request-ready bytes. The filesystem path and any
// source dimensions belong in an adjacent text part rather than this reusable
// provider-neutral value.
type ImageContent struct {
	MediaType string `json:"media_type"`
	Data      []byte `json:"data"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

const (
	maxContentParts   = 64
	maxContentImages  = 16
	maxImageDimension = 32_768
	maxImagePixels    = 100_000_000
)

func TextContent(text string) Content {
	return Content{{Kind: ContentPartText, Text: text}}
}

func ImageToolContent(text string, image ImageContent) Content {
	return Content{
		{Kind: ContentPartText, Text: text},
		{Kind: ContentPartImage, Image: &image},
	}
}

// Text concatenates text parts in order. Images deliberately contribute no
// synthetic prose: producers and projections add an explicit descriptive
// marker when the model or UI needs one.
func (content Content) Text() string {
	var text strings.Builder
	for _, part := range content {
		if part.Kind == ContentPartText {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}

func (content Content) HasImage() bool {
	for _, part := range content {
		if part.Kind == ContentPartImage && part.Image != nil {
			return true
		}
	}
	return false
}

func (content Content) Clone() Content {
	if content == nil {
		return nil
	}
	cloned := make(Content, len(content))
	for index, part := range content {
		cloned[index] = part
		if part.Image != nil {
			image := *part.Image
			image.Data = append([]byte(nil), part.Image.Data...)
			cloned[index].Image = &image
		}
	}
	return cloned
}

// cloneContentForProjection owns the part and image headers while sharing the
// immutable bytes established by normalizeContent. Request projection changes
// part structure and text only; copying image payloads on every context report
// and model request would add no isolation.
func cloneContentForProjection(content Content) Content {
	if content == nil {
		return nil
	}
	cloned := make(Content, len(content))
	for index, part := range content {
		cloned[index] = part
		if part.Image != nil {
			image := *part.Image
			cloned[index].Image = &image
		}
	}
	return cloned
}

// WithoutImages replaces every image with marker while preserving the order
// of surrounding text. It is used only for request projection; the journaled
// canonical content remains unchanged.
func (content Content) WithoutImages(marker func(ImageContent) string) Content {
	projected := make(Content, 0, len(content))
	for _, part := range content {
		if part.Kind != ContentPartImage || part.Image == nil {
			projected = append(projected, part)
			continue
		}
		text := "[image omitted]"
		if marker != nil {
			text = marker(*part.Image)
		}
		projected = append(projected, ContentPart{Kind: ContentPartText, Text: text})
	}
	return projected
}

// MarshalJSON preserves the schema-v3 string shape for text-only results. It
// also validates at the final journal serialization boundary, so an invalid
// value cannot be written in a form which replay would later reject. A tagged
// array is required only when part ordering or media carries semantics.
func (content Content) MarshalJSON() ([]byte, error) {
	normalized, err := normalizeContent(content)
	if err != nil {
		return nil, err
	}
	if !normalized.HasImage() {
		return json.Marshal(normalized.Text())
	}
	type plain Content
	return json.Marshal(plain(normalized))
}

func (content *Content) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return errors.New("empty content JSON")
	}
	if data[0] == '"' || bytes.Equal(data, []byte("null")) {
		var text string
		if !bytes.Equal(data, []byte("null")) {
			if err := json.Unmarshal(data, &text); err != nil {
				return err
			}
		}
		*content = TextContent(text)
		return nil
	}
	type plain Content
	var parts plain
	if err := json.Unmarshal(data, &parts); err != nil {
		return err
	}
	normalized, err := normalizeContent(Content(parts))
	if err != nil {
		return err
	}
	*content = normalized
	return nil
}

func normalizeContent(content Content) (Content, error) {
	if len(content) > maxContentParts {
		return nil, fmt.Errorf("content has %d parts, limit is %d", len(content), maxContentParts)
	}
	normalized := make(Content, len(content))
	images := 0
	imageBytes := 0
	for index, part := range content {
		switch part.Kind {
		case ContentPartText:
			if part.Image != nil {
				return nil, fmt.Errorf("content part %d has both text and image values", index)
			}
			normalized[index] = ContentPart{Kind: ContentPartText, Text: part.Text}
		case ContentPartImage:
			images++
			if images > maxContentImages {
				return nil, fmt.Errorf("content has %d images, limit is %d", images, maxContentImages)
			}
			if part.Image == nil || part.Text != "" {
				return nil, fmt.Errorf("content part %d has an invalid image value", index)
			}
			image := *part.Image
			image.MediaType = strings.ToLower(strings.TrimSpace(image.MediaType))
			switch image.MediaType {
			case "image/png", "image/jpeg":
			default:
				return nil, fmt.Errorf("content part %d has unsupported media type %q", index, image.MediaType)
			}
			if len(image.Data) == 0 || len(image.Data) > productlimits.MaxContentImageBytes {
				return nil, fmt.Errorf("content part %d image bytes are outside the 1..%d limit", index, productlimits.MaxContentImageBytes)
			}
			if imageBytes > productlimits.MaxContentImageBytes-len(image.Data) {
				return nil, fmt.Errorf("content image bytes exceed the %d-byte aggregate limit", productlimits.MaxContentImageBytes)
			}
			imageBytes += len(image.Data)
			if image.Width <= 0 || image.Height <= 0 || image.Width > maxImageDimension || image.Height > maxImageDimension || image.Width > maxImagePixels/image.Height {
				return nil, fmt.Errorf("content part %d has invalid image dimensions %dx%d", index, image.Width, image.Height)
			}
			image.Data = append([]byte(nil), image.Data...)
			normalized[index] = ContentPart{Kind: ContentPartImage, Image: &image}
		default:
			return nil, fmt.Errorf("content part %d has unsupported type %q", index, part.Kind)
		}
	}
	return normalized, nil
}
