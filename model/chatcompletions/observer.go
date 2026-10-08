package chatcompletions

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"

	"github.com/levmv/skot/internal/modelhttp"
	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/transport"
)

// Observer tracks usage, identifiers, errors, and completion for n=1.
// Use one per attempt from one goroutine. Observe before writing to clients
// so usage survives failed writes.
type Observer struct {
	provider, model string
	usage           modelhttp.UsageAccumulator
	finishReason    string
}

// NewObserver starts observation for a non-nil Chat Completions connection.
func NewObserver(connection *transport.Connection) *Observer {
	return &Observer{provider: connection.Provider(), model: connection.APIModel()}
}

// ObserveHeader records x-request-id, falling back to request-id.
func (o *Observer) ObserveHeader(header http.Header) { o.usage.SetRequestID(header) }

// ObserveChunk inspects SSE data, excluding [DONE], without retaining deltas.
// Usage and identifiers are retained even when the chunk reports an error.
// Usage is final only if this chunk has no choices or choice 0 reports a nonempty
// finish_reason; otherwise it is partial. A later finish reason or [DONE] does
// not make it final.
func (o *Observer) ObserveChunk(data []byte) error { return o.decode(data, false) }

// ObserveResponse checks a complete JSON response for choice 0; finish_reason
// is optional. Present usage is final. The caller must bound the body read.
func (o *Observer) ObserveResponse(body []byte) error { return o.decode(body, true) }

func (o *Observer) decode(data []byte, response bool) error {
	var chunk struct {
		completionFields
		Choices []*choiceFields `json:"choices"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return model.MarkProviderFailure(fmt.Errorf("decode %s completion: %w", o.provider, err))
	}
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		return model.MarkProviderFailure(fmt.Errorf("%s completion is not a JSON object", o.provider))
	}
	var choice *choiceFields
	for _, candidate := range chunk.Choices {
		if candidate != nil && candidate.Index == 0 {
			choice = candidate
			break
		}
	}
	return o.observe(chunk.completionFields, choice, len(chunk.Choices) == 0, response)
}

// The backend already decoded deltas; pass its service fields without another
// JSON parse or allocating a second choices slice.
func (o *Observer) observeChunk(chunk streamChunk) error {
	var choice *choiceFields
	for i := range chunk.Choices {
		if chunk.Choices[i].Index == 0 {
			choice = &chunk.Choices[i].choiceFields
			break
		}
	}
	return o.observe(chunk.completionFields, choice, len(chunk.Choices) == 0, false)
}

func (o *Observer) observe(fields completionFields, choice *choiceFields, noChoices, response bool) error {
	if fields.ID != "" {
		o.usage.Snapshot.ResponseID = fields.ID
	}
	if fields.Model != "" {
		o.usage.Snapshot.Model = fields.Model
	}
	if fields.Provider != "" {
		o.usage.Snapshot.Provider = fields.Provider
	}
	finished := choice != nil && choice.FinishReason != "" && choice.FinishReason != "null"
	if finished {
		o.finishReason = choice.FinishReason
	}
	if err := o.usage.Observe(fields.Usage, "chat_completions", o.provider, response || noChoices || finished); err != nil {
		return model.MarkProviderFailure(err)
	}
	if fields.Error != nil {
		return modelhttp.NewProviderEnvelopeError(o.provider, o.model, fields.Error)
	}
	if choice != nil {
		if choice.Error != nil {
			return modelhttp.NewProviderEnvelopeError(o.provider, o.model, choice.Error)
		}
		if strings.EqualFold(strings.TrimSpace(choice.FinishReason), "error") {
			var err error = modelhttp.NewProviderError(modelhttp.ProviderErrorDetails{
				Provider: o.provider, Model: o.model,
				Message: `generation ended with finish_reason "error"`,
			})
			if native := strings.TrimSpace(choice.NativeFinishReason); native != "" {
				err = fmt.Errorf("%w (native_finish_reason %q)", err, native)
			}
			return err
		}
	}
	if response && choice == nil {
		return model.MarkProviderFailure(fmt.Errorf("%s completion has no choice 0", o.provider))
	}
	return nil
}

// End checks completion after SSE EOF: a finish reason or [DONE] is required.
// It does not promote partial usage to final. JSON uses ObserveResponse instead.
func (o *Observer) End(sawDone bool) error {
	if o.finishReason == "" && !sawDone {
		return model.MarkProviderFailure(fmt.Errorf("%s stream ended before a finish reason", o.provider))
	}
	return nil
}

// Usage returns the latest usage, including on errors. Missing reports are
// unavailable, not zero cost. A final report may still lack optional counters.
func (o *Observer) Usage() model.Usage {
	var response model.Response
	o.usage.Attach(&response)
	return response.Usage
}

// RawUsage returns an owned JSON object merging observed top-level usage fields.
// Snapshots replace fields rather than adding counters; nested objects are
// replaced whole. It returns nil when no usage object was reported.
func (o *Observer) RawUsage() jsontext.Value { return o.usage.RawUsage() }

// FinishReason returns choice 0's original reason, including unknown values.
func (o *Observer) FinishReason() string { return o.finishReason }

type completionFields struct {
	ID       string         `json:"id"`
	Model    string         `json:"model"`
	Provider string         `json:"provider"`
	Usage    jsontext.Value `json:"usage,omitzero"`
	Error    *apiError      `json:"error,omitzero"`
}

type choiceFields struct {
	Index              int       `json:"index"`
	FinishReason       string    `json:"finish_reason"`
	NativeFinishReason string    `json:"native_finish_reason"`
	Error              *apiError `json:"error,omitzero"`
}
