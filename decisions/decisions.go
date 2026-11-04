// Package decisions evaluates typed questions through the [TypeSafe System One API],
// including compatible models on [OpenRouter], independently of the Skot agent.
// Use it for classification, scoring, routing, and other decisions consumed by code.
// Create a client with [New], then call [Client.Evaluate]. Requests can combine
// Noul, Choice, Score, and Box questions over text, JSON structures, and images.
// Supported question types and media depend on the selected model.
//
// For TypeSafe directly, supply its model ID, API key, and a Provider label;
// the default BaseURL is https://api.typesafe.ai/v1. For OpenRouter, set BaseURL
// to https://openrouter.ai/api/v1 and ResponseProfile to
// [model.ResponseProfileOpenRouter] to read reported USD costs. Model IDs are
// sent as supplied. Other compatible servers can use their own BaseURL, including
// the API version; the client appends /systemone. Provider is a label for errors,
// not a model catalog or credential lookup.
//
// Each Evaluate call makes one request. Bound calls with a context deadline;
// applications choose their own thresholds, retries, and fallbacks. Check the
// error before using answers, and record Response.Usage even on failure: an
// evaluation can be billed even if its answers cannot be used. Missing token
// counts and costs remain unknown. Requests have a 32 MiB JSON limit; responses
// default to 16 MiB, adjustable through Config.MaxResponseBytes.
//
// [TypeSafe System One API]: https://docs.typesafe.ai/api/system-one
// [OpenRouter]: https://openrouter.ai/docs/guides/community/typesafe-sdk
package decisions

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/internal/modelhttp"
	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/transport"
)

// ErrOutputTooLarge reports a response above Config.MaxResponseBytes.
var ErrOutputTooLarge = errors.New("decision response exceeds the local size limit")

// Config describes one System One route. Provider and Model are required.
type Config struct {
	// Provider names the connection in errors. It does not select credentials,
	// endpoints, or provider-specific behavior.
	Provider string
	// Model is the exact upstream model ID, without catalog lookup.
	Model string
	// BaseURL includes the API version. Empty selects https://api.typesafe.ai/v1.
	// For OpenRouter use https://openrouter.ai/api/v1.
	BaseURL string
	// ResponseProfile selects monetary and error extensions. Use
	// model.ResponseProfileOpenRouter to read OpenRouter's reported USD costs.
	ResponseProfile model.ResponseProfile
	// APIKey is sent as a bearer token. Authorizer replaces it; supplying both
	// is an error. With neither, only Header can authorize requests.
	APIKey     string
	Authorizer transport.Authorizer
	// Nil uses Skot's HTTP transport. Bound the whole call with its context.
	HTTPClient *http.Client
	// Header is copied and sent with every request; authorization runs after it.
	Header http.Header
	// MaxResponseBytes bounds the decompressed response body. Zero selects 16 MiB.
	MaxResponseBytes int64
}

// Client is immutable and supports concurrent calls when its Authorizer and
// HTTPClient do.
type Client struct {
	route            modelhttp.Route
	profile          model.ResponseProfile
	maxResponseBytes int64
}

// New prepares a client without network access, environment variables, or
// credential lookup.
func New(config Config) (*Client, error) {
	config.Provider = strings.TrimSpace(config.Provider)
	config.Model = strings.TrimSpace(config.Model)
	if config.Provider == "" || config.Model == "" {
		return nil, model.MarkInvalidRequest(errors.New("provider and model are required"))
	}
	if config.ResponseProfile != "" && config.ResponseProfile != model.ResponseProfileOpenRouter {
		return nil, model.MarkInvalidRequest(fmt.Errorf("unsupported response profile %q", config.ResponseProfile))
	}
	if config.MaxResponseBytes < 0 || config.MaxResponseBytes == math.MaxInt64 {
		return nil, model.MarkInvalidRequest(errors.New("maximum response bytes must be nonnegative and less than MaxInt64"))
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = limits.MaxModelCompletionBytes
	}
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.typesafe.ai/v1"
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
	route := modelhttp.NewRoute(modelhttp.Route{
		Provider: config.Provider, Model: config.Model, Endpoint: baseURL + "/systemone",
		Authorizer: authorizer, MaxErrorBodyBytes: int(min(config.MaxResponseBytes, limits.MaxModelCompletionBytes)),
	}, config.HTTPClient, config.Header)
	return &Client{route: route, profile: config.ResponseProfile, maxResponseBytes: config.MaxResponseBytes}, nil
}

// Request asks named questions about a shared state. State, instructions, and
// criteria may contain JSON objects and arrays as well as text; Go structs,
// maps, slices, and jsontext.Value are accepted. The provider decides which
// question types and media the selected model supports.
type Request struct {
	// State must encode as a JSON string, object, or array.
	State     any
	Questions map[string]Question
	// Assets holds images keyed by placeholders used in State, Instructions,
	// or Criteria. Choose placeholders that cannot occur accidentally in text.
	// Images are sent as base64 JSON; multipart uploads are not used.
	Assets map[string]ImageAsset
}

type QuestionType string

const (
	Noul   QuestionType = "noul"
	Choice QuestionType = "choice"
	Score  QuestionType = "score"
	Box    QuestionType = "box"
)

// Question describes one decision. See https://docs.typesafe.ai/primitives for
// question design and the meaning of each answer.
type Question struct {
	Type QuestionType
	// Instructions may be text, a JSON object or array, or nil.
	Instructions any
	// Criteria is an object of named options for Choice, an ordered array of
	// levels for Score, or an optional object with "true" and "false" keys for
	// Noul. Descriptions may be text, objects, or arrays. Choice and Noul also
	// allow null descriptions. Box does not use Criteria.
	Criteria any
}

// ImageAsset holds the original image bytes. Format is png, jpg, or jpeg.
// Evaluate sends TypeSafe-Danger-Zone-Images: true when assets or Box questions
// are present. Model support and image limits are documented by the provider.
type ImageAsset struct {
	Format string
	Data   []byte
}

// Answer contains the value selected by Type. Other value fields are unused.
type Answer struct {
	Type   QuestionType
	Noul   float64
	Choice string
	Score  float64
	Box    *BoxCoordinates
	// Confidence is distinct from the answer's probability. It is present for
	// Choice and Score, and may be nil for Box and Noul.
	Confidence    *float64
	Probabilities map[string]float64
	// Legend preserves the provider's JSON descriptions of Score levels.
	Legend map[string]jsontext.Value
}

// BoxCoordinates gives image edges as fractions of height and width, from 0 to 1.
type BoxCoordinates struct {
	Top, Bottom, Left, Right float64
}

type Response struct {
	// Answers has one entry per question on success, and is nil on failure.
	Answers map[string]Answer
	// Usage includes the reported model, response ID, provider, and costs when
	// available. Inspect it even when Evaluate fails. An absent cost is unknown.
	Usage model.Usage
	// RawUsage preserves the usage fields used to produce Usage.
	RawUsage jsontext.Value
}

// Evaluate posts to /systemone once, without streaming or automatic retries.
// Requests are limited to 32 MiB of JSON. Invalid requests carry
// model.ErrInvalidRequest and model.ErrRequestNotSent. Transport and protocol
// failures carry model.ErrProviderFailure; HTTP errors are *model.ProviderError.
// ErrOutputTooLarge reports a local response limit, not a provider failure.
// Context cancellation is preserved through errors.Is.
func (client *Client) Evaluate(ctx context.Context, request Request) (result Response, returnErr error) {
	var usage modelhttp.UsageAccumulator
	defer func() {
		result.Usage = usage.Snapshot
		if result.Usage.Status == "" {
			result.Usage.Status = model.UsageUnavailable
		}
		result.RawUsage = usage.RawUsage()
		if returnErr != nil {
			result.Answers = nil
		}
		if !errors.Is(returnErr, model.ErrRequestNotSent) && !errors.Is(returnErr, ErrOutputTooLarge) {
			returnErr = model.MarkProviderFailure(returnErr)
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, model.MarkRequestNotSent(err)
	}
	wire, err := buildRequest(client.route.Model, request)
	if err != nil {
		return result, model.MarkRequestNotSent(model.MarkInvalidRequest(err))
	}
	body, err := modelhttp.MarshalRequestJSON(wire)
	if err != nil {
		return result, model.MarkRequestNotSent(model.MarkInvalidRequest(fmt.Errorf("encode decision request: %w", err)))
	}
	const maxRequestBytes = 32 << 20
	if len(body) > maxRequestBytes {
		return result, model.MarkRequestNotSent(model.MarkInvalidRequest(fmt.Errorf("%w: decision request exceeds 32 MiB", model.ErrModelRequestTooLarge)))
	}
	header := http.Header{"Accept": {"application/json"}}
	if len(wire.Assets) != 0 || hasBoxQuestion(wire.Questions) {
		header.Set("TypeSafe-Danger-Zone-Images", "true")
	}
	response, err := modelhttp.Post(ctx, client.route, body, header)
	if response != nil {
		usage.SetRequestID(response.Header)
		if usage.Snapshot.RequestID == "" {
			usage.Snapshot.RequestID = response.Header.Get("X-TypeSafe-Request-ID")
		}
		defer response.Body.Close()
	}
	if err != nil {
		var failure *model.ProviderError
		if response != nil && errors.As(err, &failure) && failure.StatusCode == response.StatusCode {
			// TypeSafe gives retry-after-ms precedence over Retry-After. Post
			// creates this HTTP error per call, so its metadata is ours to update.
			// https://github.com/typesafe-ai/typesafe-sdk-python/blob/main/src/typesafe_sdk/_core/errors.py
			milliseconds, parseErr := strconv.ParseFloat(strings.TrimSpace(response.Header.Get("Retry-After-Ms")), 64)
			nanoseconds := milliseconds * float64(time.Millisecond)
			if parseErr == nil && nanoseconds >= 0 && nanoseconds < float64(math.MaxInt64) {
				failure.RetryAfter = time.Duration(nanoseconds)
			}
		}
		return result, err
	}
	if response.ContentLength > client.maxResponseBytes {
		return result, ErrOutputTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, client.maxResponseBytes+1))
	if int64(len(data)) > client.maxResponseBytes {
		return result, ErrOutputTooLarge
	}
	if err != nil {
		return result, fmt.Errorf("read %s decision response: %w", client.route.Provider, err)
	}
	result.Answers, err = client.decodeResponse(data, wire.Questions, &usage)
	return result, err
}
