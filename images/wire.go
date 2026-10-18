package images

import (
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"strings"

	"github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/internal/modelhttp"
	"github.com/levmv/skot/model"
)

type wireRequest struct {
	Model             string `json:"model"`
	Prompt            string `json:"prompt"`
	N                 int    `json:"n"`
	Size              string `json:"size,omitempty"`
	Quality           string `json:"quality,omitempty"`
	Background        string `json:"background,omitempty"`
	OutputFormat      string `json:"output_format,omitempty"`
	OutputCompression *int   `json:"output_compression,omitzero"`
	Stream            bool   `json:"stream,omitzero"`

	Images           []openAIImage `json:"images,omitempty"`
	SafetyIdentifier string        `json:"safety_identifier,omitempty"`
	PartialImages    *int          `json:"partial_images,omitzero"`

	InputReferences []openRouterReference `json:"input_references,omitempty"`
	AspectRatio     string                `json:"aspect_ratio,omitempty"`
	Resolution      string                `json:"resolution,omitempty"`
	User            string                `json:"user,omitempty"`
}

type openAIImage struct {
	ImageURL string `json:"image_url"`
}

type openRouterReference struct {
	Type     string `json:"type"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

func (client *Client) buildRequest(request Request, stream bool) ([]byte, error) {
	if strings.TrimSpace(request.Prompt) == "" {
		return nil, errors.New("image prompt is required")
	}
	if request.Count < 0 {
		return nil, errors.New("image count cannot be negative")
	}
	if stream && request.Count > 1 {
		return nil, errors.New("streamed image responses support one image")
	}
	if request.Compression != nil && (*request.Compression < 0 || *request.Compression > 100) {
		return nil, errors.New("image compression must be between 0 and 100")
	}
	if request.StreamIdleTimeout < 0 {
		return nil, errors.New("stream idle timeout cannot be negative")
	}
	if request.StreamIdleTimeout != 0 && !stream {
		return nil, errors.New("stream idle timeout requires a preview callback")
	}
	if (request.OpenAI != nil && client.api != OpenAIImages) || (request.OpenRouter != nil && client.api != OpenRouterImages) {
		return nil, errors.New("image options do not match the selected image API")
	}
	wire := wireRequest{
		Model: client.model, Prompt: request.Prompt, N: max(1, request.Count),
		Size: request.Size, Quality: request.Quality, Background: request.Background,
		OutputFormat: request.Format, OutputCompression: request.Compression, Stream: stream,
	}
	if options := request.OpenAI; options != nil {
		if partial := options.PartialImages; partial != nil {
			if !stream {
				return nil, errors.New("partial images require a preview callback")
			}
			if *partial < 0 || *partial > 3 {
				return nil, errors.New("partial images must be between 0 and 3")
			}
		}
		wire.SafetyIdentifier, wire.PartialImages = options.SafetyIdentifier, options.PartialImages
	}
	if options := request.OpenRouter; options != nil {
		wire.AspectRatio, wire.Resolution, wire.User = options.AspectRatio, options.Resolution, options.User
	}
	// Check the encoded size before allocating data URLs for large references.
	size := len(request.Prompt)
	mediaTypes := make([]string, len(request.References))
	for index, reference := range request.References {
		mediaType, _, err := mime.ParseMediaType(reference.MediaType)
		if err != nil || !strings.HasPrefix(mediaType, "image/") {
			return nil, fmt.Errorf("reference %d has no image media type", index)
		}
		if len(reference.Data) == 0 {
			return nil, fmt.Errorf("reference %d has no image bytes", index)
		}
		if reference.Width < 0 || reference.Height < 0 {
			return nil, fmt.Errorf("reference %d has negative dimensions", index)
		}
		mediaTypes[index] = mediaType
		size += len(mediaType) + base64.StdEncoding.EncodedLen(len(reference.Data))
		if size > limits.MaxModelRequestBytes {
			return nil, requestTooLarge(size)
		}
	}
	for index, reference := range request.References {
		dataURL := "data:" + mediaTypes[index] + ";base64," + base64.StdEncoding.EncodeToString(reference.Data)
		switch client.api {
		case OpenAIImages:
			wire.Images = append(wire.Images, openAIImage{ImageURL: dataURL})
		case OpenRouterImages:
			reference := openRouterReference{Type: "image_url"}
			reference.ImageURL.URL = dataURL
			wire.InputReferences = append(wire.InputReferences, reference)
		}
	}
	body, err := modelhttp.MarshalRequestJSON(wire)
	if err != nil {
		return nil, fmt.Errorf("encode image request: %w", err)
	}
	if len(body) > limits.MaxModelRequestBytes {
		return nil, requestTooLarge(len(body))
	}
	return body, nil
}

func requestTooLarge(size int) error {
	return fmt.Errorf("%w: image request is at least %d bytes, limit is %d", model.ErrModelRequestTooLarge, size, limits.MaxModelRequestBytes)
}
