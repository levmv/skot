package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// NormalizeToolSpecs validates named tools with object argument schemas and
// returns an owned copy.
func NormalizeToolSpecs(input []ToolSpec) ([]ToolSpec, error) {
	result := make([]ToolSpec, len(input))
	seen := make(map[string]struct{}, len(input))
	for index, spec := range input {
		normalized, err := normalizeToolSpec(spec)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[normalized.Name]; exists {
			return nil, fmt.Errorf("duplicate tool %q", normalized.Name)
		}
		seen[normalized.Name] = struct{}{}
		result[index] = normalized
	}
	return result, nil
}

func normalizeToolSpec(spec ToolSpec) (ToolSpec, error) {
	spec.Name = strings.TrimSpace(spec.Name)
	if spec.Name == "" {
		return ToolSpec{}, errors.New("tool name is required")
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(spec.InputSchema, &schema); err != nil || schema == nil {
		return ToolSpec{}, fmt.Errorf("tool %q input schema must be a JSON Schema object", spec.Name)
	}
	var schemaType string
	if err := json.Unmarshal(schema["type"], &schemaType); err != nil || schemaType != "object" {
		return ToolSpec{}, fmt.Errorf("tool %q input schema must have type object", spec.Name)
	}
	spec.InputSchema = append(json.RawMessage(nil), spec.InputSchema...)
	return spec, nil
}

// NormalizeToolArguments validates and re-encodes the single JSON object
// accepted by every model-facing tool. Empty input denotes {}.
func NormalizeToolArguments(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "{}"
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		return "", fmt.Errorf("invalid tool arguments: %w", err)
	}
	if object == nil {
		return "", errors.New("invalid tool arguments: expected one JSON object")
	}
	var normalized strings.Builder
	encoder := json.NewEncoder(&normalized)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(object); err != nil {
		return "", fmt.Errorf("normalize tool arguments: %w", err)
	}
	return strings.TrimSuffix(normalized.String(), "\n"), nil
}

// DecodeToolArguments decodes a tool argument object into target and rejects
// undeclared fields for struct targets.
func DecodeToolArguments(raw string, target any) error {
	normalized, err := NormalizeToolArguments(raw)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(normalized))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	return nil
}
