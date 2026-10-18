package images

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	stdimage "image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"mime"
	"strings"
	"time"

	"github.com/levmv/skot/internal/modelhttp"
	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/transport"
	_ "golang.org/x/image/webp"
)

// field decodes one JSON value without failing the enclosing object, so a
// malformed field does not discard the images and usage beside it.
type field[T any] struct {
	value T
	err   error
}

func (field *field[T]) UnmarshalJSONFrom(decoder *jsontext.Decoder) error {
	raw, err := decoder.ReadValue()
	if err != nil {
		return err
	}
	field.err = json.Unmarshal(raw, &field.value)
	return nil
}

type responseFields struct {
	Usage jsontext.Value                          `json:"usage"`
	Error field[*modelhttp.ProviderErrorEnvelope] `json:"error"`
}

// Malformed optional metadata counts as absent.
type imageFields struct {
	Data      field[[]byte] `json:"b64_json"`
	MediaType field[string] `json:"media_type"`
	URL       field[string] `json:"url"`
}

type responseParser struct {
	client *Client
	usage  *modelhttp.UsageAccumulator
	result *Response
}

func (parser responseParser) readJSON(body io.Reader, contentLength int64) error {
	var buffer bytes.Buffer
	if contentLength > 0 && contentLength < math.MaxInt-bytes.MinRead {
		// Room for ReadFrom's final probe avoids growing a full buffer.
		buffer.Grow(int(contentLength) + bytes.MinRead)
	}
	if _, err := buffer.ReadFrom(body); err != nil {
		return fmt.Errorf("read %s image response: %w", parser.client.provider, err)
	}
	var payload struct {
		responseFields
		Data field[[]field[imageFields]] `json:"data"`
	}
	if err := json.Unmarshal(buffer.Bytes(), &payload); err != nil {
		return fmt.Errorf("decode %s image response: %w", parser.client.provider, err)
	}
	errs := []error{parser.observe(payload.responseFields, true)}
	if payload.Data.err != nil {
		// Entries are decoded separately; only a non-array value fails here.
		errs = append(errs, fmt.Errorf("%s image data is not an array: %w", parser.client.provider, payload.Data.err))
	}
	for index, entry := range payload.Data.value {
		var image Image
		err := entry.err
		if err == nil {
			image, err = decodeImage(entry.value)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s image %d: %w", parser.client.provider, index, err))
			continue
		}
		parser.result.Images = append(parser.result.Images, image)
	}
	return parser.finish(errs)
}

type streamEvent struct {
	responseFields
	imageFields
	Type field[string] `json:"type"`
	// Some routes put an error event's code and message at the top level.
	Code    any           `json:"code"`
	Message field[string] `json:"message"`
}

func (parser responseParser) readStream(ctx context.Context, body io.ReadCloser, idleTimeout time.Duration, onPreview func(Image)) error {
	provider := parser.client.provider
	stream := transport.OpenEventStream(ctx, body, transport.StreamOptions{
		IdleTimeout: idleTimeout, MaxEventBytes: int(min(parser.client.maxResponseBytes, math.MaxInt)),
	})
	defer stream.Close()
	for {
		received, err := stream.Next()
		if errors.Is(err, io.EOF) {
			if stream.SawDone() {
				return fmt.Errorf("%s image stream ended before the image completed", provider)
			}
			return fmt.Errorf("%s image stream ended before the image completed: %w", provider, io.ErrUnexpectedEOF)
		}
		if errors.Is(err, transport.ErrEventTooLarge) {
			return fmt.Errorf("%w: %s sent an event above %d bytes", ErrOutputTooLarge, provider, parser.client.maxResponseBytes)
		}
		if err != nil {
			return fmt.Errorf("read %s image stream: %w", provider, err)
		}
		var event streamEvent
		if err := json.Unmarshal(received.Data, &event); err != nil {
			return fmt.Errorf("decode %s image stream event: %w", provider, err)
		}
		kind := event.Type.value
		if kind == "" {
			kind = received.Name
		}
		if kind == "error" && event.Error.value == nil && event.Error.err == nil {
			event.Error.value = &modelhttp.ProviderErrorEnvelope{Message: event.Message.value, Code: event.Code}
		}
		// OpenAI names edit events image_edit.*; OpenRouter uses image_generation.*.
		operation, stage, _ := strings.Cut(kind, ".")
		if operation != "image_generation" && operation != "image_edit" {
			stage = ""
		}
		err = parser.observe(event.responseFields, stage == "completed")
		switch {
		case stage == "completed":
			// The final image completes the operation; [DONE] need not follow.
			image, imageErr := decodeImage(event.imageFields)
			if imageErr != nil {
				return errors.Join(err, fmt.Errorf("%s image: %w", provider, imageErr))
			}
			parser.result.Images = append(parser.result.Images, image)
			return err
		case err != nil:
			return err
		case stage == "partial_image" && onPreview != nil:
			// A preview is only a display aid; the final image may still arrive.
			if preview, err := decodeImage(event.imageFields); err == nil {
				onPreview(preview)
			}
		}
	}
}

// observe records usage before reporting a provider error.
func (parser responseParser) observe(fields responseFields, final bool) error {
	protocol, profile := "openai_images", model.ResponseProfile("")
	if parser.client.api == OpenRouterImages {
		// OpenRouter reports Chat Completions counters and its USD cost.
		protocol, profile = "chat_completions", model.ResponseProfileOpenRouter
	}
	usageErr := parser.usage.Observe(fields.Usage, protocol, profile, final)
	if usageErr != nil {
		usageErr = fmt.Errorf("%s image usage: %w", parser.client.provider, usageErr)
	}
	switch {
	case fields.Error.err != nil:
		return errors.Join(fmt.Errorf("decode %s error: %w", parser.client.provider, fields.Error.err), usageErr)
	case fields.Error.value != nil:
		return errors.Join(modelhttp.NewProviderEnvelopeError(parser.client.provider, parser.client.model, profile, fields.Error.value), usageErr)
	}
	return usageErr
}

// finish reports a provider error ahead of the absence of images.
func (parser responseParser) finish(errs []error) error {
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if len(parser.result.Images) == 0 {
		return fmt.Errorf("%s: %w", parser.client.provider, ErrNoImage)
	}
	return nil
}

func decodeImage(fields imageFields) (Image, error) {
	if fields.Data.err != nil {
		return Image{}, fields.Data.err
	}
	data := fields.Data.value
	if len(data) == 0 {
		if fields.URL.value != "" {
			return Image{}, errors.New("image is available only by URL, which is not supported")
		}
		return Image{}, errors.New("image has no data")
	}
	result := Image{Data: data}
	if mediaType, _, err := mime.ParseMediaType(fields.MediaType.value); err == nil && strings.HasPrefix(mediaType, "image/") {
		result.MediaType = mediaType
	}
	// Header detection does not decode pixels. Formats registered by other
	// packages are left to the provider's report.
	if config, format, err := stdimage.DecodeConfig(bytes.NewReader(data)); err == nil {
		switch format {
		case "png", "jpeg", "gif", "webp":
			result.MediaType = "image/" + format
			result.Width, result.Height = config.Width, config.Height
		}
	}
	return result, nil
}

// limitedBody fails a read once the body exceeds limit bytes.
type limitedBody struct {
	body             io.ReadCloser
	remaining, limit int64
}

func (body *limitedBody) Read(p []byte) (int, error) {
	if body.remaining < 0 {
		return 0, body.err()
	}
	if int64(len(p))-1 > body.remaining {
		p = p[:body.remaining+1]
	}
	n, err := body.body.Read(p)
	body.remaining -= int64(n)
	if body.remaining < 0 {
		return n - 1, body.err()
	}
	return n, err
}

func (body *limitedBody) err() error {
	return fmt.Errorf("%w: response exceeds %d bytes", ErrOutputTooLarge, body.limit)
}

func (body *limitedBody) Close() error { return body.body.Close() }
