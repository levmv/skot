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
}

var modelProviderCatalog = map[string]providerSpec{
	"deepseek": {baseURL: "https://api.deepseek.com/v1"},
	"openrouter": {
		baseURL: "https://openrouter.ai/api/v1",
		header: http.Header{
			"HTTP-Referer": []string{"https://github.com/levmv/skot"},
			"X-Title":      []string{"Skot"},
		},
	},
	"openai": {baseURL: "https://api.openai.com/v1"},
	"ollama": {baseURL: "http://localhost:11434/v1", credentialless: true},
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
