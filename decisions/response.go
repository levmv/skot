package decisions

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/levmv/skot/internal/modelhttp"
)

func (client *Client) decodeResponse(data []byte, questions map[string]wireQuestion, usage *modelhttp.UsageAccumulator) (map[string]Answer, error) {
	// Decode the envelope before its fields so a malformed answer cannot erase
	// usage from an evaluation that may already have been billed.
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("decode %s decision response: %w", client.route.Provider, err)
	}
	usageErr := usage.Observe(fields["usage"], "systemone", client.profile, true)
	var metadataErr error
	for name, target := range map[string]*string{
		"model": &usage.Snapshot.Model, "id": &usage.Snapshot.ResponseID, "provider": &usage.Snapshot.Provider,
	} {
		if raw := fields[name]; !absent(raw) {
			metadataErr = errors.Join(metadataErr, json.Unmarshal(raw, target))
		}
	}
	if raw := fields["error"]; !absent(raw) {
		var envelope modelhttp.ProviderErrorEnvelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, errors.Join(fmt.Errorf("decode decision error: %w", err), usageErr, metadataErr)
		}
		return nil, errors.Join(modelhttp.NewProviderEnvelopeError(client.route.Provider, client.route.Model, client.profile, &envelope), usageErr, metadataErr)
	}
	if usage.Snapshot.Model == "" {
		metadataErr = errors.Join(metadataErr, errors.New("decision response is missing its model"))
	}
	answers, answerErr := decodeAnswers(fields["answers"], questions)
	if err := errors.Join(answerErr, usageErr, metadataErr); err != nil {
		return nil, fmt.Errorf("%s decision response: %w", client.route.Provider, err)
	}
	return answers, nil
}

// Required scalars use pointers on the wire: null or absent must not become a
// plausible zero-valued decision. Only a complete, matching answer set escapes.
type wireAnswer struct {
	Type          QuestionType              `json:"type"`
	Noul          *float64                  `json:"noul"`
	Choice        *string                   `json:"choice"`
	Score         *float64                  `json:"score"`
	Confidence    *float64                  `json:"confidence"`
	Probabilities map[string]*float64       `json:"probabilities"`
	Legend        map[string]jsontext.Value `json:"legend"`
	Box           *struct {
		Top    *float64 `json:"top"`
		Bottom *float64 `json:"bottom"`
		Left   *float64 `json:"left"`
		Right  *float64 `json:"right"`
	} `json:"box"`
}

func decodeAnswers(raw jsontext.Value, questions map[string]wireQuestion) (map[string]Answer, error) {
	var fields map[string]jsontext.Value
	if absent(raw) {
		return nil, errors.New("missing answers")
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("decode answers: %w", err)
	}
	if len(fields) != len(questions) {
		return nil, errors.New("answers do not match the requested questions")
	}
	answers := make(map[string]Answer, len(questions))
	for name, question := range questions {
		answer, err := decodeAnswer(fields[name], question)
		if err != nil {
			return nil, fmt.Errorf("answer %q: %w", name, err)
		}
		answers[name] = answer
	}
	return answers, nil
}

func decodeAnswer(raw jsontext.Value, question wireQuestion) (Answer, error) {
	var wire wireAnswer
	var answer Answer
	if absent(raw) {
		return answer, errors.New("missing answer")
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return answer, err
	}
	if wire.Type != question.Type {
		return answer, fmt.Errorf("type %q does not match question type %q", wire.Type, question.Type)
	}
	answer.Type, answer.Confidence = wire.Type, wire.Confidence
	if wire.Confidence != nil && !numberInRange(wire.Confidence, 0, 1) {
		return answer, errors.New("confidence must be between 0 and 1")
	}
	if (wire.Type == Choice || wire.Type == Score) && wire.Confidence == nil {
		return answer, errors.New("missing confidence")
	}
	var options map[string]jsontext.Value
	switch wire.Type {
	case Noul:
		if !numberInRange(wire.Noul, 0, 1) {
			return answer, errors.New("noul must be a number between 0 and 1")
		}
		answer.Noul = *wire.Noul
	case Choice:
		// Criteria was validated before sending the request.
		_ = json.Unmarshal(question.Criteria, &options)
		if wire.Choice == nil {
			return answer, errors.New("missing choice")
		}
		if _, ok := options[*wire.Choice]; !ok {
			return answer, fmt.Errorf("choice %q was not offered", *wire.Choice)
		}
		answer.Choice = *wire.Choice
	case Score:
		var levels []jsontext.Value
		_ = json.Unmarshal(question.Criteria, &levels)
		if !numberInRange(wire.Score, 0, float64(len(levels)-1)) {
			return answer, errors.New("score must be a number within the requested rubric")
		}
		answer.Score = *wire.Score
		options = make(map[string]jsontext.Value, len(levels))
		for index, level := range levels {
			options[strconv.Itoa(index)] = level
		}
		if len(wire.Legend) != len(options) {
			return answer, errors.New("legend does not match the requested score levels")
		}
		for key := range options {
			if !isContent(wire.Legend[key], false) {
				return answer, fmt.Errorf("missing or invalid legend for score level %s", key)
			}
		}
		answer.Legend = wire.Legend
	case Box:
		if wire.Box == nil || !numberInRange(wire.Box.Top, 0, 1) || !numberInRange(wire.Box.Bottom, 0, 1) ||
			!numberInRange(wire.Box.Left, 0, 1) || !numberInRange(wire.Box.Right, 0, 1) {
			return answer, errors.New("box requires top, bottom, left, and right coordinates between 0 and 1")
		}
		answer.Box = &BoxCoordinates{Top: *wire.Box.Top, Bottom: *wire.Box.Bottom, Left: *wire.Box.Left, Right: *wire.Box.Right}
	}
	if options != nil {
		if len(wire.Probabilities) != len(options) {
			return answer, errors.New("probabilities do not match the requested criteria")
		}
		answer.Probabilities = make(map[string]float64, len(options))
		for key := range options {
			probability := wire.Probabilities[key]
			if !numberInRange(probability, 0, 1) {
				return answer, fmt.Errorf("probability for %q must be a number between 0 and 1", key)
			}
			answer.Probabilities[key] = *probability
		}
	}
	return answer, nil
}

func numberInRange(value *float64, lower, upper float64) bool {
	return value != nil && !math.IsNaN(*value) && *value >= lower && *value <= upper
}
