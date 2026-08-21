package app

import (
	"fmt"
	"net/http"
	"strings"
)

type providerSpec struct {
	baseURL        string
	header         http.Header
	credentialless bool
	defaultAPI     modelAPI
	// promptCache marks endpoints that expect the caller to place Anthropic
	// cache_control breakpoints. Compatible endpoints that cache on their own
	// leave it off so Skot does not compete for the few breakpoints allowed.
	promptCache          bool
	requireDeclaredModel bool
}

type modelAPI string

const (
	modelAPIChatCompletions   modelAPI = "chat_completions"
	modelAPIResponses         modelAPI = "responses"
	modelAPIAnthropicMessages modelAPI = "anthropic_messages"
)

var modelProviderCatalog = map[string]providerSpec{
	"deepseek": {baseURL: "https://api.deepseek.com/v1", defaultAPI: modelAPIChatCompletions},
	"anthropic": {
		baseURL: "https://api.anthropic.com/v1", defaultAPI: modelAPIAnthropicMessages,
		promptCache: true,
	},
	"openrouter": {
		baseURL:    "https://openrouter.ai/api/v1",
		defaultAPI: modelAPIChatCompletions,
		header: http.Header{
			"HTTP-Referer": []string{"https://github.com/levmv/skot"},
			"X-Title":      []string{"Skot"},
		},
	},
	"openai": {baseURL: "https://api.openai.com/v1", defaultAPI: modelAPIChatCompletions},
	"opencode-go": {
		baseURL: "https://opencode.ai/zen/go/v1", defaultAPI: modelAPIChatCompletions,
		requireDeclaredModel: true,
	},
	"ollama": {baseURL: "http://localhost:11434/v1", credentialless: true, defaultAPI: modelAPIChatCompletions},
}

func parseModelURI(value string) (string, string, error) {
	provider, model, ok := strings.Cut(strings.TrimSpace(value), "/")
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.TrimSpace(model)
	if !ok || provider == "" || model == "" {
		return "", "", fmt.Errorf("invalid model %q; expected provider/model", value)
	}
	return provider, model, nil
}

func modelProviderSpec(provider string) (providerSpec, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	spec, exists := modelProviderCatalog[provider]
	if !exists {
		return providerSpec{}, fmt.Errorf("unsupported model provider %q", provider)
	}
	spec.header = spec.header.Clone()
	return spec, nil
}

func parseModelAPI(value string) (modelAPI, error) {
	api := modelAPI(strings.ToLower(strings.TrimSpace(value)))
	if api == "" || knownModelAPI(api) {
		return api, nil
	}
	return "", fmt.Errorf("unsupported model API %q; expected %s, %s, or %s", strings.TrimSpace(value),
		modelAPIChatCompletions, modelAPIResponses, modelAPIAnthropicMessages)
}

func knownModelAPI(api modelAPI) bool {
	switch api {
	case modelAPIChatCompletions, modelAPIResponses, modelAPIAnthropicMessages:
		return true
	default:
		return false
	}
}

// modelAPIFromBackendID recovers the protocol a built backend speaks. Backend
// identifiers are protocol-prefixed, which makes a session that already ran the
// authority on its own route.
func modelAPIFromBackendID(backendID string) modelAPI {
	protocol, _, ok := strings.Cut(strings.TrimSpace(backendID), ".")
	if !ok {
		return ""
	}
	if api := modelAPI(protocol); knownModelAPI(api) {
		return api
	}
	return ""
}

func implementedModelAPI(api modelAPI) bool {
	switch api {
	case modelAPIChatCompletions, modelAPIResponses, modelAPIAnthropicMessages:
		return true
	default:
		return false
	}
}
