package modelconfig

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/levmv/skot/internal/codexauth"
)

type ProviderSpec struct {
	BaseURL        string
	Header         http.Header
	Credentialless bool
	DefaultAPI     API
	// PromptCache marks endpoints that expect the caller to place Anthropic
	// cache_control breakpoints. Compatible endpoints that cache on their own
	// leave it off so Skot does not compete for the few breakpoints allowed.
	PromptCache          bool
	RequireDeclaredModel bool
}

type API string

const (
	ChatCompletions   API = "chat_completions"
	Responses         API = "responses"
	AnthropicMessages API = "anthropic_messages"
)

var Providers = map[string]ProviderSpec{
	"deepseek": {BaseURL: "https://api.deepseek.com/v1", DefaultAPI: ChatCompletions},
	"anthropic": {
		BaseURL: "https://api.anthropic.com/v1", DefaultAPI: AnthropicMessages,
		PromptCache: true,
	},
	"openrouter": {
		BaseURL:    "https://openrouter.ai/api/v1",
		DefaultAPI: ChatCompletions,
		Header: http.Header{
			"HTTP-Referer": []string{"https://github.com/levmv/skot"},
			"X-Title":      []string{"Skot"},
		},
	},
	"openai":           {BaseURL: "https://api.openai.com/v1", DefaultAPI: ChatCompletions},
	codexauth.Provider: {BaseURL: codexauth.BaseURL, DefaultAPI: Responses},
	"opencode-go": {
		BaseURL: "https://opencode.ai/zen/go/v1", DefaultAPI: ChatCompletions,
		RequireDeclaredModel: true,
	},
	"ollama": {BaseURL: "http://localhost:11434/v1", Credentialless: true, DefaultAPI: ChatCompletions},
}

func ParseURI(value string) (string, string, error) {
	provider, model, ok := strings.Cut(strings.TrimSpace(value), "/")
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.TrimSpace(model)
	if !ok || provider == "" || model == "" {
		return "", "", fmt.Errorf("invalid model %q; expected provider/model", value)
	}
	return provider, model, nil
}

func Provider(provider string) (ProviderSpec, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	spec, exists := Providers[provider]
	if !exists {
		return ProviderSpec{}, fmt.Errorf("unsupported model provider %q", provider)
	}
	spec.Header = spec.Header.Clone()
	return spec, nil
}

func ParseAPI(value string) (API, error) {
	api := API(strings.ToLower(strings.TrimSpace(value)))
	if api == "" || KnownAPI(api) {
		return api, nil
	}
	return "", fmt.Errorf("unsupported model API %q; expected %s, %s, or %s", strings.TrimSpace(value),
		ChatCompletions, Responses, AnthropicMessages)
}

func KnownAPI(api API) bool {
	switch api {
	case ChatCompletions, Responses, AnthropicMessages:
		return true
	default:
		return false
	}
}

// APIFromBackendID recovers the protocol a built backend speaks. Backend
// identifiers are protocol-prefixed, which makes a session that already ran the
// authority on its own route.
func APIFromBackendID(backendID string) API {
	protocol, _, ok := strings.Cut(strings.TrimSpace(backendID), ".")
	if !ok {
		return ""
	}
	if api := API(protocol); KnownAPI(api) {
		return api
	}
	return ""
}
