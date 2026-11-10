package state

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/levmv/skot/internal/privatefs"
)

// ProviderConfig overlays a built-in provider or defines a compatible service.
// Pointers distinguish omitted fields from explicit overrides.
type ProviderConfig struct {
	BaseURL   *string                `json:"base_url,omitzero"`
	API       *string                `json:"api,omitzero"`
	APIKeyEnv *string                `json:"api_key_env,omitzero"`
	Auth      *string                `json:"auth,omitzero"`
	Models    map[string]ModelConfig `json:"models,omitempty"`
}

// ModelConfig contains only explicitly configured model metadata.
type ModelConfig struct {
	Name             *string  `json:"name,omitzero"`
	API              *string  `json:"api,omitzero"`
	ContextWindow    *int     `json:"context_window,omitzero"`
	MaxOutputTokens  *int     `json:"max_output_tokens,omitzero"`
	ImageInput       *bool    `json:"image_input,omitzero"`
	ReasoningEfforts []string `json:"reasoning_efforts,omitzero"`
}

// ReadModelProviders reads user definitions from models.json. A missing file
// leaves the built-in catalog unchanged; invalid definitions are never ignored.
func ReadModelProviders(home string) (map[string]ProviderConfig, error) {
	if err := inspectHome(home); err != nil {
		return nil, err
	}
	path := filepath.Join(home, "models.json")
	if err := privatefs.InspectTrustedRegularFile(path, "model config file"); err != nil {
		return nil, err
	}
	privatefs.TryRestrictPermissions(path)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read model config %s: %w", path, err)
	}
	var document struct {
		Providers map[string]ProviderConfig `json:"providers,omitempty"`
	}
	if err := json.Unmarshal(raw, &document, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("decode model config %s: %w", path, err)
	}
	return document.Providers, nil
}
