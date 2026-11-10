package provider

import "github.com/levmv/skot/internal/state"

// ProviderConfig adds or partially overrides a named provider and its models.
type ProviderConfig = state.ProviderConfig

// ModelConfig contains optional per-model overrides.
type ModelConfig = state.ModelConfig
