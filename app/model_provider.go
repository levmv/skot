package app

import (
	"fmt"
	"net/http"
	"strings"
)

type providerSpec struct {
	baseURL              string
	header               http.Header
	credentialless       bool
	defaultAPI           modelAPI
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
	return "", fmt.Errorf("unsupported model API %q", strings.TrimSpace(value))
}

func knownModelAPI(api modelAPI) bool {
	switch api {
	case modelAPIChatCompletions, modelAPIResponses, modelAPIAnthropicMessages:
		return true
	default:
		return false
	}
}

func implementedModelAPI(api modelAPI) bool {
	return api == modelAPIChatCompletions || api == modelAPIResponses
}
