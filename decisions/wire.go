package decisions

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/levmv/skot/internal/modelhttp"
)

type wireRequest struct {
	Model     string                  `json:"model"`
	State     jsontext.Value          `json:"state"`
	Questions map[string]wireQuestion `json:"questions"`
	Assets    map[string]wireAsset    `json:"assets,omitempty"`
}

type wireQuestion struct {
	Type         QuestionType   `json:"type"`
	Instructions jsontext.Value `json:"instructions,omitzero"`
	Criteria     jsontext.Value `json:"criteria,omitzero"`
}

type wireAsset struct {
	Type   string `json:"type"`
	Format string `json:"format"`
	Data   []byte `json:"data_base64"`
}

func buildRequest(model string, request Request) (wireRequest, error) {
	wire := wireRequest{Model: model, Questions: make(map[string]wireQuestion, len(request.Questions))}
	var err error
	wire.State, err = modelhttp.MarshalRequestJSON(request.State)
	if err != nil {
		return wire, fmt.Errorf("encode state: %w", err)
	}
	if !isContent(wire.State, false) {
		return wire, errors.New("state must be a JSON string, object, or array")
	}
	if len(request.Questions) == 0 {
		return wire, errors.New("at least one question is required")
	}
	for name, question := range request.Questions {
		encoded, err := encodeQuestion(question)
		if err != nil {
			return wire, fmt.Errorf("question %q: %w", name, err)
		}
		wire.Questions[name] = encoded
	}
	if len(request.Assets) > 0 {
		wire.Assets = make(map[string]wireAsset, len(request.Assets))
	}
	for name, asset := range request.Assets {
		if name == "" || len(asset.Data) == 0 {
			return wire, errors.New("image assets require a nonempty placeholder and image data")
		}
		switch asset.Format {
		case "png", "jpg", "jpeg":
		default:
			return wire, fmt.Errorf("asset %q: image format must be png, jpg, or jpeg", name)
		}
		wire.Assets[name] = wireAsset{Type: "image", Format: asset.Format, Data: asset.Data}
	}
	return wire, nil
}

func encodeQuestion(question Question) (wireQuestion, error) {
	wire := wireQuestion{Type: question.Type}
	var err error
	if question.Instructions != nil {
		wire.Instructions, err = modelhttp.MarshalRequestJSON(question.Instructions)
		if err != nil {
			return wire, fmt.Errorf("encode instructions: %w", err)
		}
		if !isContent(wire.Instructions, true) {
			return wire, errors.New("instructions must be a JSON string, object, array, or null")
		}
	}
	if question.Criteria != nil {
		wire.Criteria, err = modelhttp.MarshalRequestJSON(question.Criteria)
		if err != nil {
			return wire, fmt.Errorf("encode criteria: %w", err)
		}
	}
	switch wire.Type {
	case Noul, Choice:
		if wire.Type == Noul && absent(wire.Criteria) {
			break
		}
		var criteria map[string]jsontext.Value
		if wire.Criteria.Kind() != '{' || json.Unmarshal(wire.Criteria, &criteria) != nil {
			return wire, errors.New("criteria must be a JSON object")
		}
		if wire.Type == Choice && len(criteria) == 0 {
			return wire, errors.New("choice requires at least one option")
		}
		for key, value := range criteria {
			if wire.Type == Noul && key != "true" && key != "false" {
				return wire, errors.New("noul criteria only accepts true and false keys")
			}
			if !isContent(value, true) {
				return wire, fmt.Errorf("criterion %q must be a JSON string, object, array, or null", key)
			}
		}
	case Score:
		var criteria []jsontext.Value
		if wire.Criteria.Kind() != '[' || json.Unmarshal(wire.Criteria, &criteria) != nil || len(criteria) == 0 {
			return wire, errors.New("score criteria must be a nonempty JSON array")
		}
		for index, value := range criteria {
			if !isContent(value, false) {
				return wire, fmt.Errorf("score level %d must be a JSON string, object, or array", index)
			}
		}
	case Box:
		if !absent(wire.Criteria) {
			return wire, errors.New("box questions do not accept criteria")
		}
	default:
		return wire, fmt.Errorf("unsupported question type %q", wire.Type)
	}
	return wire, nil
}

func isContent(raw jsontext.Value, nullable bool) bool {
	switch raw.Kind() {
	case '"', '{', '[':
		return true
	case 'n':
		return nullable
	default:
		return false
	}
}

func absent(raw jsontext.Value) bool { return len(raw) == 0 || raw.Kind() == 'n' }

func hasBoxQuestion(questions map[string]wireQuestion) bool {
	for _, question := range questions {
		if question.Type == Box {
			return true
		}
	}
	return false
}
