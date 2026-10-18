// Package images generates and edits images through dedicated image APIs.
// Each Generate call makes one HTTP request: Skot does not retry, fall back to
// another route, or split Count into separate requests. Applications keep image
// versions, choose references for the next edit, and account for usage, which
// is returned even when Generate fails.
package images

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/internal/modelhttp"
	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/transport"
)

// API selects the wire protocol and the meaning of reported usage and costs.
type API string

const (
	// OpenAIImages uses /images/generations and, with references, /images/edits.
	OpenAIImages API = "openai_images"
	// OpenRouterImages uses /images and reports USD costs.
	OpenRouterImages API = "openrouter_images"
)

var (
	// ErrNoImage reports a completed response that contains no image.
	ErrNoImage = errors.New("image response contains no image")
	// ErrOutputTooLarge reports a response above Config.MaxResponseBytes.
	ErrOutputTooLarge = errors.New("image response exceeds the local size limit")
)

const defaultMaxResponseBytes = 256 << 20

// Config describes one image route. API, Provider, and Model are required.
type Config struct {
	API API
	// Provider names the connection in errors. It does not select credentials,
	// endpoints, or provider-specific behavior.
	Provider string
	// Model is the exact upstream model ID.
	Model string
	// BaseURL includes the API version, such as https://api.openai.com/v1.
	// Empty selects the official endpoint for API.
	BaseURL string
	// APIKey is sent as a bearer token. Authorizer replaces it; specifying both
	// is an error. With neither, only Header can authorize requests.
	APIKey     string
	Authorizer transport.Authorizer
	// Nil uses Skot's transport, which waits up to five minutes for response
	// headers. Without a preview callback the provider may send them only once
	// the image is ready. Use [transport.NewHTTPClient] to increase
	// ResponseHeaderTimeout or customize the proxy and connection pool while
	// retaining model.ErrRequestNotSent classification. Bound the whole call
	// with its context.
	HTTPClient *http.Client
	// Header is copied and sent with every request; authorization runs after it.
	Header http.Header
	// MaxResponseBytes bounds the decompressed response body, including every
	// streamed preview. Zero selects 256 MiB.
	MaxResponseBytes int64
}

// Client is immutable and supports concurrent calls when its Authorizer and
// HTTPClient do.
type Client struct {
	api              API
	provider, model  string
	generate, edit   modelhttp.Route
	maxResponseBytes int64
}

// New prepares a client entirely from config, without network access,
// environment variables, or credential lookup. It copies Header.
func New(config Config) (*Client, error) {
	config.Provider = strings.TrimSpace(config.Provider)
	config.Model = strings.TrimSpace(config.Model)
	var defaultBaseURL, generatePath, editPath string
	switch config.API {
	case OpenAIImages:
		defaultBaseURL, generatePath, editPath = "https://api.openai.com/v1", "/images/generations", "/images/edits"
	case OpenRouterImages:
		defaultBaseURL, generatePath, editPath = "https://openrouter.ai/api/v1", "/images", "/images"
	case "":
		return nil, model.MarkInvalidRequest(errors.New("image API is required"))
	default:
		return nil, model.MarkInvalidRequest(fmt.Errorf("unsupported image API %q", config.API))
	}
	if config.Provider == "" || config.Model == "" {
		return nil, model.MarkInvalidRequest(errors.New("provider and model are required"))
	}
	if config.MaxResponseBytes < 0 {
		return nil, model.MarkInvalidRequest(errors.New("maximum response bytes cannot be negative"))
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = defaultMaxResponseBytes
	}
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil || endpoint.Hostname() == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || strings.Contains(baseURL, "#") {
		return nil, model.MarkInvalidRequest(errors.New("base URL must be an absolute HTTP or HTTPS URL without credentials, query, or fragment"))
	}
	authorizer := config.Authorizer
	if key := strings.TrimSpace(config.APIKey); key != "" {
		if authorizer != nil {
			return nil, model.MarkInvalidRequest(errors.New("supply either an API key or an authorizer, not both"))
		}
		authorizer = transport.BearerToken(key)
	}
	route := modelhttp.Route{
		Provider: config.Provider, Model: config.Model, Authorizer: authorizer,
		MaxErrorBodyBytes: int(min(config.MaxResponseBytes, limits.MaxModelCompletionBytes)),
	}
	route = modelhttp.NewRoute(route, config.HTTPClient, config.Header)
	client := &Client{
		api: config.API, provider: config.Provider, model: config.Model,
		generate: route, edit: route, maxResponseBytes: config.MaxResponseBytes,
	}
	client.generate.Endpoint = baseURL + generatePath
	client.edit.Endpoint = baseURL + editPath
	return client, nil
}

// Image holds an image file's original bytes. MediaType is detected from PNG,
// JPEG, GIF, and WebP data, otherwise taken from the provider's report, and may
// be empty. Width and Height are zero when unknown. Generate does not decode
// pixels or re-encode files; use imageinput.Prepare to turn a result into
// model.ImageContent for a conversation.
type Image struct {
	MediaType     string
	Data          []byte
	Width, Height int
}

// Request describes one generation, or an edit when References are present.
// Empty strings and nil fields leave a setting to the provider. Settings are
// sent unchanged; the provider decides which values and combinations the model
// accepts.
type Request struct {
	// Prompt is required and sent unchanged.
	Prompt string
	// References are sent in order. Each needs data and an image MediaType;
	// Width and Height may be zero. Pass a previous result unchanged to keep its
	// format and resolution. References keep the visible image, not the
	// provider's hidden context from the call that produced it.
	References []Image
	// Count is the number of images requested; zero requests one. The response
	// may contain a different number.
	Count int
	// Size, Quality, Background, and Format set size, quality, background, and
	// output_format.
	Size, Quality, Background, Format string
	// Compression sets output_compression, from 0 to 100.
	Compression *int
	// StreamIdleTimeout limits silence in a streamed response and requires a
	// preview callback. Zero disables the limit.
	StreamIdleTimeout time.Duration
	// OpenAI and OpenRouter are accepted only by their own API.
	OpenAI     *OpenAIOptions
	OpenRouter *OpenRouterOptions
}

type OpenAIOptions struct {
	// SafetyIdentifier is a stable identifier of the application's end user,
	// such as a hashed account ID. OpenAI uses it for abuse monitoring.
	SafetyIdentifier string
	// PartialImages sets partial_images and requires a preview callback. Nil
	// omits it, 0 disables previews, and 1 to 3 request that many. OpenAI bills
	// each partial image.
	PartialImages *int
}

type OpenRouterOptions struct {
	// AspectRatio and Resolution are sent unchanged; Size is not derived from
	// them.
	AspectRatio, Resolution string
	// User is a stable identifier of the application's end user.
	User string
}

// Response holds images in provider order and the latest reported usage.
// Inspect it even when Generate fails: images decoded before the failure and
// usage, including reported costs, are kept.
type Response struct {
	Images []Image
	// Usage is final once the provider reported usage for a completed response.
	// Costs holds reported amounts only; a missing cost is unknown, not zero.
	Usage model.Usage
	// RawUsage merges the reported usage fields, or is nil without a report.
	RawUsage jsontext.Value
}

// Generate makes one request. With a nil onPreview the response is a single
// JSON body. A non-nil onPreview requests a stream, which allows a Count of 0
// or 1, and receives each preview of the image synchronously, in order, before
// Generate returns. Previews never stand in for the final image, which is
// returned only in Response.Images; the client does not retain them and skips
// previews it cannot decode. An empty function consumes the stream without
// showing previews but does not stop the provider from generating them.
// Streaming support depends on the route, and Generate does not retry without
// it.
//
// Generate borrows request until it returns. Returned images and previews do
// not alias request or internal buffers; treat preview data as read-only.
//
// A nil error means at least one image. Errors keep their causes, including
// context cancellation. Invalid requests carry model.ErrInvalidRequest and
// model.ErrRequestNotSent, plus model.ErrModelRequestTooLarge for oversized
// ones. Provider, transport, and protocol failures carry
// model.ErrProviderFailure; errors reported by the provider are
// *model.ProviderError. A stream that ends before the image completes also
// carries io.ErrUnexpectedEOF at EOF, and a completed response without images
// ErrNoImage. ErrOutputTooLarge reports a response above
// Config.MaxResponseBytes; it is not a provider failure, since a higher limit
// or a smaller image avoids it.
//
// An error does not prove that the provider did not bill the generation: a
// decoding error or local limit can reject a response that was already paid
// for. Record Response.Usage after every call.
func (client *Client) Generate(ctx context.Context, request Request, onPreview func(Image)) (result Response, returnErr error) {
	var usage modelhttp.UsageAccumulator
	defer func() {
		result.Usage = usage.Snapshot
		if result.Usage.Status == "" {
			result.Usage.Status = model.UsageUnavailable
		}
		result.RawUsage = usage.RawUsage()
	}()
	defer func() {
		if !errors.Is(returnErr, model.ErrRequestNotSent) && !errors.Is(returnErr, ErrOutputTooLarge) {
			returnErr = model.MarkProviderFailure(returnErr)
		}
	}()
	stream := onPreview != nil
	body, err := client.buildRequest(request, stream)
	if err != nil {
		return result, model.MarkRequestNotSent(model.MarkInvalidRequest(err))
	}
	route := client.generate
	if len(request.References) != 0 {
		route = client.edit
	}
	header := http.Header{"Accept": {"application/json"}}
	if stream {
		header.Set("Accept", "text/event-stream")
	}
	response, err := modelhttp.Post(ctx, route, body, header)
	if response != nil {
		usage.SetRequestID(response.Header)
	}
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		return result, err
	}
	defer response.Body.Close()
	if response.ContentLength > client.maxResponseBytes {
		return result, fmt.Errorf("%w: %s sent %d bytes, limit is %d", ErrOutputTooLarge, client.provider, response.ContentLength, client.maxResponseBytes)
	}
	reader := &limitedBody{body: response.Body, remaining: client.maxResponseBytes, limit: client.maxResponseBytes}
	parser := responseParser{client: client, usage: &usage, result: &result}
	media, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if media == "text/event-stream" {
		return result, parser.readStream(ctx, reader, request.StreamIdleTimeout, onPreview)
	}
	return result, parser.readJSON(reader, response.ContentLength)
}
