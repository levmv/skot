package model

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	productlimits "github.com/levmv/skot/internal/limits"
)

// Content is the provider-neutral, model-visible payload of a tool result.
// Text-only content is encoded as a JSON string; content containing images
// is encoded as an array of tagged parts.
type Content []ContentPart

type ContentPartKind string

const (
	ContentPartText  ContentPartKind = "text"
	ContentPartImage ContentPartKind = "image"
)

type ContentPart struct {
	Kind  ContentPartKind `json:"type"`
	Text  string          `json:"text,omitempty"`
	Image *ImageContent   `json:"image,omitzero"`
}

// ImageContent contains PNG or JPEG bytes and their dimensions.
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

// Text concatenates text parts in order, ignoring images.
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

// WithoutImages returns content with images replaced by marker text, preserving
// the order of surrounding text. A nil marker uses "[image omitted]".
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

// MarshalJSON validates content and encodes text as a string or mixed content
// as an array of parts.
func (content Content) MarshalJSON() ([]byte, error) {
	normalized, err := NormalizeContent(content)
	if err != nil {
		return nil, err
	}
	if !normalized.HasImage() {
		return json.Marshal(normalized.Text(), json.Deterministic(true))
	}
	type plain Content
	return json.Marshal(plain(normalized), json.Deterministic(true))
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
	normalized, err := NormalizeContent(Content(parts))
	if err != nil {
		return err
	}
	*content = normalized
	return nil
}

// NormalizeContent validates text and image parts and returns an owned copy.
func NormalizeContent(content Content) (Content, error) {
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
