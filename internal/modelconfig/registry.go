package modelconfig

import (
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/levmv/skot/internal/state"
)

// Registry owns immutable user overlays. A nil registry uses the built-ins.
// Never mutate the package catalogs to register application-owned providers.
type Registry struct {
	providers map[string]state.ProviderConfig
}

func NewRegistry(layers ...map[string]state.ProviderConfig) (*Registry, error) {
	registry := &Registry{providers: make(map[string]state.ProviderConfig)}
	for _, layer := range layers {
		seen := make(map[string]bool)
		for name, patch := range layer {
			name = strings.ToLower(strings.TrimSpace(name))
			if !validProviderName(name) || seen[name] {
				return nil, fmt.Errorf("invalid or duplicate provider name %q", name)
			}
			seen[name] = true
			if credential, ok := credentialProviderSpec(name); ok && credential.Capabilities&CredentialModel == 0 {
				return nil, fmt.Errorf("provider name %q belongs to a tool service", name)
			}
			base := registry.providers[name]
			if patch.BaseURL != nil {
				base.BaseURL = copyConfigValue(patch.BaseURL)
			}
			if patch.API != nil {
				base.API = copyConfigValue(patch.API)
			}
			if patch.APIKeyEnv != nil {
				base.APIKeyEnv = copyConfigValue(patch.APIKeyEnv)
			}
			if patch.Auth != nil {
				base.Auth = copyConfigValue(patch.Auth)
			}
			if base.Models == nil {
				base.Models = make(map[string]state.ModelConfig)
			}
			modelKeys := make(map[string]bool)
			for id, model := range patch.Models {
				if id == "" || strings.TrimSpace(id) != id {
					return nil, fmt.Errorf("provider %q has an invalid model ID %q", name, id)
				}
				if _, known := CatalogSpec(name + "/" + id); known {
					_, id, _ = strings.Cut(canonicalCatalogURI(name+"/"+id), "/")
				}
				if modelKeys[id] {
					return nil, fmt.Errorf("provider %q repeats model %q through an alias", name, id)
				}
				modelKeys[id] = true
				base.Models[id] = mergeModelConfig(base.Models[id], model)
			}
			registry.providers[name] = base
		}
	}
	for name, definition := range registry.providers {
		if definition.API != nil {
			api, err := ParseAPI(*definition.API)
			if err != nil || api == "" {
				return nil, fmt.Errorf("provider %q has an invalid api %q", name, *definition.API)
			}
		}
		if definition.BaseURL != nil {
			endpoint, err := url.Parse(*definition.BaseURL)
			if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
				return nil, fmt.Errorf("provider %q requires an HTTP(S) base_url without credentials, query, or fragment", name)
			}
		}
		if definition.APIKeyEnv != nil && *definition.APIKeyEnv != "" && !validEnvironmentName(*definition.APIKeyEnv) {
			return nil, fmt.Errorf("provider %q has an invalid api_key_env", name)
		}
		if definition.Auth != nil && *definition.Auth != "none" && *definition.Auth != "api_key" {
			return nil, fmt.Errorf("provider %q auth must be api_key or none", name)
		}
		if name == "openai-codex" && (definition.BaseURL != nil || definition.API != nil || definition.APIKeyEnv != nil || definition.Auth != nil) {
			return nil, fmt.Errorf("openai-codex uses its fixed OAuth connection; only model metadata may be configured")
		}
		spec, err := registry.Provider(name)
		if err != nil {
			return nil, err
		}
		if spec.BaseURL == "" || !KnownAPI(spec.DefaultAPI) {
			return nil, fmt.Errorf("provider %q requires base_url and api", name)
		}
		for id, model := range definition.Models {
			if model.API != nil {
				api, err := ParseAPI(*model.API)
				if err != nil || api == "" {
					return nil, fmt.Errorf("model %q has an invalid api", name+"/"+id)
				}
				if name == "openai-codex" && api != Responses {
					return nil, fmt.Errorf("openai-codex models require the Responses API")
				}
			}
			if model.ContextWindow != nil && *model.ContextWindow <= 0 {
				return nil, fmt.Errorf("model %q context_window must be positive", name+"/"+id)
			}
			if model.MaxOutputTokens != nil && *model.MaxOutputTokens <= 0 {
				return nil, fmt.Errorf("model %q max_output_tokens must be positive", name+"/"+id)
			}
			if _, err := Resolve(name+"/"+id, "", Overrides{Registry: registry}, Enrichment{}); err != nil {
				return nil, fmt.Errorf("configure model %q: %w", name+"/"+id, err)
			}
		}
	}
	if len(registry.providers) == 0 {
		return nil, nil
	}
	return registry, nil
}

func copyConfigValue[T any](value *T) *T { result := *value; return &result }

func mergeModelConfig(base, patch state.ModelConfig) state.ModelConfig {
	if patch.Name != nil {
		base.Name = copyConfigValue(patch.Name)
	}
	if patch.API != nil {
		base.API = copyConfigValue(patch.API)
	}
	if patch.ContextWindow != nil {
		base.ContextWindow = copyConfigValue(patch.ContextWindow)
	}
	if patch.MaxOutputTokens != nil {
		base.MaxOutputTokens = copyConfigValue(patch.MaxOutputTokens)
	}
	if patch.ImageInput != nil {
		base.ImageInput = copyConfigValue(patch.ImageInput)
	}
	if patch.ReasoningEfforts != nil {
		base.ReasoningEfforts = slices.Clone(patch.ReasoningEfforts)
	}
	return base
}

func validProviderName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return name != "." && name != ".."
}

func validEnvironmentName(name string) bool {
	for i, c := range name {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return name != ""
}

func (registry *Registry) definition(name string) (state.ProviderConfig, bool) {
	if registry == nil {
		return state.ProviderConfig{}, false
	}
	definition, ok := registry.providers[name]
	return definition, ok
}

func (registry *Registry) Provider(name string) (ProviderSpec, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	spec, err := Provider(name)
	definition, configured := registry.definition(name)
	if !configured {
		return spec, err
	}
	if err != nil {
		spec = ProviderSpec{CustomEndpoint: true}
	}
	if definition.BaseURL != nil {
		baseURL := strings.TrimRight(strings.TrimSpace(*definition.BaseURL), "/")
		if baseURL != strings.TrimRight(spec.BaseURL, "/") || len(spec.APIBaseURLs) != 0 {
			spec.CustomEndpoint = true
			spec.Header = nil
			spec.RequestEncoding = ""
			spec.PromptCache = false
		}
		spec.BaseURL = baseURL
		spec.APIBaseURLs = nil
	}
	if definition.API != nil {
		spec.DefaultAPI = API(strings.ToLower(strings.TrimSpace(*definition.API)))
	}
	if definition.Auth != nil {
		spec.AuthNone = *definition.Auth == "none"
		spec.Credentialless = spec.AuthNone
	}
	return spec, nil
}

func (registry *Registry) modelConfig(uri string) (state.ModelConfig, bool) {
	name, id, err := ParseURI(uri)
	if err != nil {
		return state.ModelConfig{}, false
	}
	definition, _ := registry.definition(name)
	if _, known := CatalogSpec(uri); known {
		_, id, _ = strings.Cut(canonicalCatalogURI(uri), "/")
	}
	model, ok := definition.Models[id]
	return model, ok
}

func (registry *Registry) configuredAPI(uri string) API {
	name, _, _ := ParseURI(uri)
	definition, _ := registry.definition(name)
	api := ""
	if definition.API != nil {
		api = *definition.API
	}
	if model, _ := registry.modelConfig(uri); model.API != nil {
		api = *model.API
	}
	return API(strings.ToLower(strings.TrimSpace(api)))
}

func (registry *Registry) CatalogSpec(uri string) (Spec, bool) {
	spec, known := CatalogSpec(uri)
	model, configured := registry.modelConfig(uri)
	if !known && !configured {
		return spec, false
	}
	spec.URI = strings.TrimSpace(uri)
	if api := registry.configuredAPI(uri); api != "" {
		spec.API = api
	}
	if model.Name != nil {
		spec.Name = *model.Name
	}
	if model.ContextWindow != nil {
		spec.ContextWindow = *model.ContextWindow
	}
	if model.MaxOutputTokens != nil {
		spec.MaxOutputTokens = *model.MaxOutputTokens
	}
	if model.ImageInput != nil {
		spec.ImageInputUnsupported = !*model.ImageInput
	}
	if model.ReasoningEfforts != nil {
		spec.ReasoningEfforts = []string{DefaultReasoningEffort}
		for _, effort := range model.ReasoningEfforts {
			effort = strings.ToLower(strings.TrimSpace(effort))
			if effort == "default" {
				effort = ""
			}
			if !slices.Contains(spec.ReasoningEfforts, effort) {
				spec.ReasoningEfforts = append(spec.ReasoningEfforts, effort)
			}
		}
	}
	return spec, true
}

func (registry *Registry) Models() []Spec {
	models := make([]Spec, 0, len(Catalog))
	seen := make(map[string]bool)
	for _, native := range Catalog {
		spec, _ := registry.CatalogSpec(native.URI)
		models = append(models, spec)
		seen[native.URI] = true
	}
	if registry != nil {
		var extra []string
		for name, definition := range registry.providers {
			for id := range definition.Models {
				uri := name + "/" + id
				if !seen[uri] {
					extra = append(extra, uri)
				}
			}
		}
		sort.Strings(extra)
		for _, uri := range extra {
			spec, _ := registry.CatalogSpec(uri)
			models = append(models, spec)
		}
	}
	return models
}

func (registry *Registry) SelectionAPI(uri, api string) API {
	if registry.configuredAPI(uri) != "" {
		return ""
	}
	return SelectionAPI(uri, api)
}

func (registry *Registry) SelectionContextWindow(uri string, window int) int {
	if model, _ := registry.modelConfig(uri); model.ContextWindow != nil {
		return 0
	}
	return SelectionContextWindow(uri, window)
}
