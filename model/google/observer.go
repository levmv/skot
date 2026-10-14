package google

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"slices"
	"time"

	"github.com/levmv/skot/internal/modelhttp"
	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/transport"
)

// Observer tracks GenerateContent metadata without inspecting or retaining
// candidate content. Use one per attempt from one goroutine. Observe before
// writing to clients so usage survives failed writes.
type Observer struct {
	provider, model string
	candidateCount  int
	finishReasons   map[int]string
	blockReason     string
	usage           modelhttp.UsageAccumulator
	retryAfter      time.Duration
	failure         error
}

// NewObserver starts observation for a non-nil Gemini connection. candidateCount
// is generationConfig.candidateCount from the request; <= 0 uses its default, 1.
// It is needed to detect streams that omit an expected candidate entirely.
func NewObserver(connection *transport.Connection, candidateCount int) *Observer {
	return &Observer{
		provider: connection.Provider(), model: connection.APIModel(),
		candidateCount: max(1, candidateCount), finishReasons: make(map[int]string),
	}
}

// ObserveHeader records request identifiers and Retry-After for in-band errors.
func (o *Observer) ObserveHeader(header http.Header) {
	o.usage.SetRequestID(header)
	o.retryAfter = modelhttp.ParseRetryAfter(header.Get("Retry-After"), time.Now())
}

// ObserveChunk inspects one SSE data payload. Present usage is final only when
// all expected candidates have finished or the prompt is blocked. Later finish
// events without usage do not promote an earlier partial snapshot to final.
func (o *Observer) ObserveChunk(data []byte) error { return o.decode(data, false) }

// ObserveResponse inspects a complete JSON response and requires completion of
// all expected candidates or a prompt block. Present usage is final, including
// on provider errors. The caller must bound the body read.
func (o *Observer) ObserveResponse(body []byte) error { return o.decode(body, true) }

func (o *Observer) decode(data []byte, response bool) error {
	var chunk struct {
		responseFields
		Candidates []candidateFields `json:"candidates"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return o.fail(fmt.Errorf("decode %s response: %w", o.provider, err))
	}
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		return o.fail(fmt.Errorf("%s response is not a JSON object", o.provider))
	}
	return o.observe(chunk.responseFields, slices.Values(chunk.Candidates), response)
}

// The adapter has already decoded content; share the metadata processing
// without parsing the response again or copying its candidates.
func (o *Observer) observeChunk(chunk generateResponse) error {
	return o.observe(chunk.responseFields, func(yield func(candidateFields) bool) {
		for _, candidate := range chunk.Candidates {
			if !yield(candidate.candidateFields) {
				return
			}
		}
	}, false)
}

func (o *Observer) observe(fields responseFields, candidates iter.Seq[candidateFields], response bool) error {
	if fields.ResponseID != "" {
		o.usage.Snapshot.ResponseID = fields.ResponseID
	}
	if fields.ModelVersion != "" {
		o.usage.Snapshot.Model = fields.ModelVersion
	}
	if reason := fields.PromptFeedback.BlockReason; reason != "" && reason != "BLOCK_REASON_UNSPECIFIED" {
		o.blockReason = reason
	}
	var candidateErr error
	for candidate := range candidates {
		if candidate.Index < 0 || candidate.Index >= o.candidateCount {
			candidateErr = fmt.Errorf("%s response has unexpected candidate index %d (requested %d)", o.provider, candidate.Index, o.candidateCount)
			break
		}
		previous := o.finishReasons[candidate.Index]
		if finished(previous) {
			if finished(candidate.FinishReason) && candidate.FinishReason != previous {
				candidateErr = fmt.Errorf("%s candidate %d changed its finish reason", o.provider, candidate.Index)
				break
			}
		} else {
			o.finishReasons[candidate.Index] = candidate.FinishReason
		}
	}
	usageErr := o.usage.Observe(fields.Usage, "gemini", o.provider, response || o.Complete())
	var providerErr error
	if fields.Error != nil {
		providerErr = modelhttp.NewProviderError(modelhttp.ProviderErrorDetails{
			Provider: o.provider, Model: o.model,
			StatusCode: fields.Error.Code, Message: fields.Error.Message, Type: fields.Error.Status,
			RetryAfter: max(o.retryAfter, modelhttp.GoogleRetryDelay(fields.Error.Details)),
		})
	}
	if err := errors.Join(providerErr, candidateErr, usageErr); err != nil {
		return o.fail(err)
	}
	if response {
		return o.End(nil)
	}
	return nil
}

// Complete reports a prompt block or finish reasons for every expected
// candidate. It does not imply success: inspect BlockReason, FinishReason, and
// observation/End errors. Unknown finish reasons are preserved as terminal.
// Completion does not end SSE reading; later events may contain usage or errors.
func (o *Observer) Complete() bool {
	if o.blockReason != "" {
		return true
	}
	if len(o.finishReasons) != o.candidateCount {
		return false
	}
	for _, reason := range o.finishReasons {
		if !finished(reason) {
			return false
		}
	}
	return true
}

// End checks completion when reading ends. Pass nil or io.EOF for normal EOF;
// cancellation and other read errors remain errors even after Complete is true.
// A caller that deliberately stops draining after completion may pass nil.
// End never promotes usage to final or treats a finish reason as success.
func (o *Observer) End(readErr error) error {
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return o.fail(readErr)
	}
	if o.failure != nil {
		return o.failure
	}
	if !o.Complete() {
		return o.fail(fmt.Errorf("%s response ended before all %d candidates finished: %w", o.provider, o.candidateCount, io.ErrUnexpectedEOF))
	}
	return nil
}

func (o *Observer) fail(err error) error {
	err = model.MarkProviderFailure(err)
	if o.failure == nil {
		o.failure = err
	}
	return err
}

// Usage returns the latest valid report, including on errors. Missing reports
// are unavailable, not zero cost. A final report may lack optional counters.
func (o *Observer) Usage() model.Usage {
	var response model.Response
	o.usage.Attach(&response)
	return response.Usage
}

// RawUsage returns an owned JSON object merging observed top-level usage fields.
// Snapshots replace counters rather than adding them; nested objects are
// replaced whole. It returns nil when no usage object was reported.
func (o *Observer) RawUsage() jsontext.Value { return o.usage.RawUsage() }

// FinishReason returns a candidate's original reason, including unknown values.
// An empty value or FINISH_REASON_UNSPECIFIED does not indicate completion.
func (o *Observer) FinishReason(index int) string { return o.finishReasons[index] }

// BlockReason returns the prompt's blocking reason, or empty when not blocked.
func (o *Observer) BlockReason() string { return o.blockReason }

func finished(reason string) bool { return reason != "" && reason != "FINISH_REASON_UNSPECIFIED" }

type responseFields struct {
	Usage          jsontext.Value `json:"usageMetadata"`
	ResponseID     string         `json:"responseId"`
	ModelVersion   string         `json:"modelVersion"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	Error *struct {
		Code    int            `json:"code"`
		Message string         `json:"message"`
		Status  string         `json:"status"`
		Details jsontext.Value `json:"details"`
	} `json:"error"`
}

type candidateFields struct {
	Index        int    `json:"index"`
	FinishReason string `json:"finishReason"`
}
