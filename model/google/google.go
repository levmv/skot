// Package google provides an item-based Backend and a raw-response Observer
// for the Gemini GenerateContent protocol. The Backend requests one candidate;
// the Observer tracks any requested candidate count.
package google

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/internal/modelhttp"
	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/transport"
)

const ProviderStateContract model.ProviderStateContract = "gemini.signed_parts.v1"
const partsDataKind = "gemini.parts.v1"
const callIDKind = "gemini.function_call_id.v1"

type Config struct {
	Connection            *transport.Connection
	ImageInputUnsupported bool
	// ReasoningEffort maps to thinkingLevel. Empty keeps the default level;
	// available thought summaries are always requested.
	ReasoningEffort string
}

type Backend struct {
	connection                *transport.Connection
	provider, reasoningEffort string
	imageInputUnsupported     bool
}

func BackendID(provider string) string {
	return "gemini." + strings.TrimSpace(provider)
}

func New(config Config) (*Backend, error) {
	if config.Connection == nil || config.Connection.API() != "gemini" {
		return nil, model.MarkInvalidRequest(errors.New("a Gemini connection is required"))
	}
	effort := strings.ToLower(strings.TrimSpace(config.ReasoningEffort))
	switch effort {
	case "", "minimal", "low", "medium", "high":
	default:
		return nil, model.MarkInvalidRequest(fmt.Errorf("unsupported Gemini thinking level %q", effort))
	}
	return &Backend{connection: config.Connection, provider: config.Connection.Provider(), reasoningEffort: effort, imageInputUnsupported: config.ImageInputUnsupported}, nil
}

func (backend *Backend) ProjectModelItems(items []model.Item) []model.Item { return items }

type generateResponse struct {
	responseFields
	Candidates []struct {
		candidateFields
		Content content `json:"content"`
	} `json:"candidates"`
}

func (backend *Backend) Complete(ctx context.Context, request model.Request, emit func(model.StreamEvent)) (result model.Response, returnErr error) {
	observer := NewObserver(backend.connection, 1)
	defer func() { result.Usage = observer.Usage() }()
	started := false
	defer func() {
		if started && !errors.Is(returnErr, model.ErrRequestNotSent) {
			returnErr = model.MarkProviderFailure(returnErr)
		} else {
			returnErr = model.MarkRequestNotSent(returnErr)
		}
	}()
	wire, err := backend.buildRequest(request)
	if err != nil {
		return result, model.MarkInvalidRequest(err)
	}
	body, err := modelhttp.MarshalRequestJSON(wire)
	if err != nil {
		return result, model.MarkInvalidRequest(err)
	}
	if len(body) > limits.MaxModelRequestBytes {
		return result, model.MarkInvalidRequest(model.ErrModelRequestTooLarge)
	}
	header := http.Header{"Accept": {"text/event-stream"}}
	if request.SessionID != "" {
		header.Set("X-Session-ID", request.SessionID)
	}
	started = true
	response, err := backend.connection.Post(ctx, body, header)
	if response != nil {
		observer.ObserveHeader(response.Header)
	}
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		return result, err
	}
	defer response.Body.Close()
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != "text/event-stream" {
		return result, fmt.Errorf("expected an SSE response from %s", backend.provider)
	}
	stream := transport.OpenEventStream(ctx, response.Body, transport.StreamOptions{IdleTimeout: request.StreamIdleTimeout})
	defer stream.Close()
	var parts []part
	var finish string
	var completionErr error
	remaining := limits.MaxModelCompletionBytes
	for {
		event, err := stream.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, transport.ErrDrainComplete) {
			if err := observer.End(nil); err != nil {
				return result, err
			}
			break
		}
		if errors.Is(err, transport.ErrEventTooLarge) || len(event.Data) > remaining {
			finish = model.StopReasonOutputLimit
			break
		}
		if err != nil {
			return result, observer.End(err)
		}
		remaining -= len(event.Data)
		var chunk generateResponse
		if err := json.Unmarshal(event.Data, &chunk); err != nil {
			return result, fmt.Errorf("decode %s response: %w", backend.provider, err)
		}
		if err := observer.observeChunk(chunk); err != nil {
			return result, err
		}
		wasFinished := finish != "" || completionErr != nil
		if observer.BlockReason() != "" {
			finish = "content_filter"
		}
		if len(chunk.Candidates) > 1 {
			return result, fmt.Errorf("%s returned more than one candidate", backend.provider)
		}
		for _, candidate := range chunk.Candidates {
			if candidate.Content.Role != "" && candidate.Content.Role != "model" {
				return result, fmt.Errorf("%s returned a non-model candidate", backend.provider)
			}
			if wasFinished && (len(candidate.Content.Parts) != 0 || candidate.FinishReason != "") {
				return result, fmt.Errorf("%s returned content after completion", backend.provider)
			}
			if finished(candidate.FinishReason) {
				finish, completionErr = normalizeFinish(backend.provider, candidate.FinishReason)
			}
			if completionErr != nil {
				continue
			}
			if _, err := semanticItems(candidate.Content.Parts, model.IsIncompleteStopReason(finish)); err != nil {
				return result, fmt.Errorf("%s response: %w", backend.provider, err)
			}
			parts = append(parts, candidate.Content.Parts...)
			if emit != nil {
				for _, value := range candidate.Content.Parts {
					if value.Text == nil || *value.Text == "" {
						continue
					}
					kind := model.EventTextDelta
					if value.Thought {
						kind = model.EventReasoningSummaryDelta
					}
					emit(model.StreamEvent{Kind: kind, Text: *value.Text})
				}
			}
		}
		if finish != "" || completionErr != nil {
			if err := stream.BeginDrain(transport.DrainOptions{MinComments: 2, QuietPeriod: 5 * time.Second, MaxDuration: 45 * time.Second}); err != nil {
				return result, err
			}
		}
	}
	if completionErr != nil {
		return result, completionErr
	}
	incomplete := model.IsIncompleteStopReason(finish)
	items, err := semanticItems(parts, incomplete)
	if err != nil {
		return result, fmt.Errorf("%s response: %w", backend.provider, err)
	}
	if len(items) == 0 && !incomplete {
		return result, fmt.Errorf("%s completed without model output", backend.provider)
	}
	if !incomplete {
		for _, value := range parts {
			if value.ThoughtSignature != "" {
				state, err := json.Marshal(parts, json.Deterministic(true))
				if err != nil {
					return result, err
				}
				items = append([]model.Item{{Kind: model.ItemReasoning, ProviderData: []model.ProviderData{{Kind: partsDataKind, Data: state}}}}, items...)
				break
			}
		}
		for _, item := range items {
			if item.Kind == model.ItemToolCall {
				finish = "tool_calls"
				break
			}
		}
	}
	return model.Response{Items: items, StopReason: finish}, nil
}

func normalizeFinish(provider, value string) (string, error) {
	switch value {
	case "STOP":
		return "stop", nil
	case "MAX_TOKENS":
		return "max_tokens", nil
	case "CONTINUATION":
		// Returning partial output does not automatically spend another request
		// to follow the provider's continuation token.
		return "incomplete", nil
	case "SAFETY", "RECITATION", "LANGUAGE", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION", "ESCALATION":
		return "content_filter", nil
	case "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL", "MALFORMED_RESPONSE":
		// Generation failed; a fresh attempt can succeed with the same input.
		return "", &model.ProviderError{Cause: fmt.Errorf("%s generation failed: %s", provider, value), Code: value, Retryable: true}
	case "MISSING_THOUGHT_SIGNATURE":
		return "", &model.ProviderError{Cause: fmt.Errorf("%s rejected history with a missing thought signature", provider), Code: value, Kind: model.ProviderErrorRequest}
	case "PUP_LIMITED_DISABLED":
		return "", &model.ProviderError{Cause: fmt.Errorf("%s account is limited or disabled", provider), Code: value, Kind: model.ProviderErrorPermission}
	case "OTHER", "IMAGE_OTHER", "NO_IMAGE", "TOO_MANY_TOOL_CALLS":
		return "", &model.ProviderError{Cause: fmt.Errorf("%s generation failed: %s", provider, value), Code: value}
	default:
		return "", modelhttp.UnsupportedCompletionReasonError(provider, value)
	}
}
