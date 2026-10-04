package modelconfig

import (
	"fmt"
	"net/http"
	"strings"

	modelapi "github.com/levmv/skot/model"
	"github.com/levmv/skot/model/anthropic"
	"github.com/levmv/skot/model/chatcompletions"
	responsemodel "github.com/levmv/skot/model/responses"
)

const FallbackContextWindow = 128 * 1024

type Compatibility string

const (
	Supported   Compatibility = "supported"
	Unverified  Compatibility = "unverified"
	Unsupported Compatibility = "unsupported"
)

// Spec is one reviewed local declaration. Nil/zero optional fields inherit
// the provider route defaults; it is deliberately not an exhaustive registry.
type Spec struct {
	URI             string
	Name            string
	API             API
	APIModel        string
	ContextWindow   int
	MaxOutputTokens int
	// ImageInputUnsupported is a reviewed negative route fact. Its zero value
	// deliberately leaves image delivery optimistic for new and unknown models.
	ImageInputUnsupported  bool
	ReasoningEfforts       []string
	ChatTraits             *chatcompletions.RouteTraits
	ResponsesTraits        *responsemodel.RouteTraits
	DropMismatchedThinking bool
	// Compatibility overrides the supported default for a reviewed declaration.
	Compatibility Compatibility
}

type Overrides struct {
	BaseURL       string
	API           API
	ContextWindow int
}

// WithSelection adds metadata carried by one model selection. Process-wide
// overrides win because they are the more recent explicit instruction.
func (overrides Overrides) WithSelection(uri, api string, contextWindow int) Overrides {
	if overrides.API == "" {
		overrides.API = SelectionAPI(uri, api)
	}
	if overrides.ContextWindow == 0 {
		overrides.ContextWindow = SelectionContextWindow(uri, contextWindow)
	}
	return overrides
}

// SelectionAPI is the protocol a user attached to one undeclared route.
// It stops applying as soon as this build declares that route: a reviewed
// declaration owns the protocol, and the remembered guess is then discarded
// rather than competing with it.
func SelectionAPI(uri, api string) API {
	value := API(strings.ToLower(strings.TrimSpace(api)))
	if value == "" || !KnownAPI(value) {
		return ""
	}
	if _, declared := CatalogSpec(uri); declared {
		return ""
	}
	return value
}

// SelectionContextWindow is user-supplied metadata for one undeclared
// route. A later reviewed declaration replaces the remembered value.
func SelectionContextWindow(uri string, contextWindow int) int {
	if _, declared := CatalogSpec(uri); declared {
		return 0
	}
	return contextWindow
}

type Enrichment struct {
	ContextWindow          int
	ContextWindowEstimated bool
}

// Route is the immutable, secret-free contract consumed by one
// adapter construction. Resolution is pure and never owns an HTTP client.
type Route struct {
	URI                    string
	Provider               string
	Model                  string
	APIModel               string
	API                    API
	BaseURL                string
	Header                 http.Header
	Credentialless         bool
	CustomEndpoint         bool
	ImageInputUnsupported  bool
	ContextWindow          int
	ContextWindowEstimated bool
	MaxOutputTokens        int
	PromptCache            bool
	DropMismatchedThinking bool
	ReasoningEffort        string
	ReasoningEfforts       []string
	ChatTraits             chatcompletions.RouteTraits
	ResponsesTraits        responsemodel.RouteTraits
	Compatibility          Compatibility
	ProviderStateContract  modelapi.ProviderStateContract
}

var Catalog = []Spec{
	// Subscription limits and efforts follow Codex model metadata, including
	// max_context_window, rather than the public OpenAI API catalog.
	{
		URI: "openai-codex/gpt-6-astra", Name: "ChatGPT · GPT 6 Astra", API: Responses,
		ContextWindow: 872_000, ReasoningEfforts: []string{"", "low", "medium", "high", "xhigh", "max", "ultra"},
	},
	{
		URI: "openai-codex/gpt-6-sol", Name: "ChatGPT · GPT 6 Sol", API: Responses,
		ContextWindow: 872_000, ReasoningEfforts: []string{"", "low", "medium", "high", "xhigh", "max", "ultra"},
	},
	{
		URI: "openai-codex/gpt-6-luna", Name: "ChatGPT · GPT 6 Luna", API: Responses,
		ContextWindow: 872_000, ReasoningEfforts: []string{"", "low", "medium", "high", "xhigh", "max"},
	},
	{
		URI: "openai-codex/gpt-5.6-sol", Name: "ChatGPT · GPT 5.6 Sol", API: Responses,
		ContextWindow: 872_000, ReasoningEfforts: []string{"", "low", "medium", "high", "xhigh", "max", "ultra"},
	},
	{
		URI: "openai-codex/gpt-5.6-terra", Name: "ChatGPT · GPT 5.6 Terra", API: Responses,
		ContextWindow: 872_000, ReasoningEfforts: []string{"", "low", "medium", "high", "xhigh", "max", "ultra"},
	},
	{
		URI: "openai-codex/gpt-5.6-luna", Name: "ChatGPT · GPT 5.6 Luna", API: Responses,
		ContextWindow: 872_000, ReasoningEfforts: []string{"", "low", "medium", "high", "xhigh", "max"},
	},
	{
		URI: "openai-codex/gpt-5.5", Name: "ChatGPT · GPT 5.5", API: Responses,
		ContextWindow: 272_000, ReasoningEfforts: []string{"", "low", "medium", "high", "xhigh"},
	},
	// Native Flash exposes off/low/high/max. The thinking switch expresses off;
	// enabled requests pair it with reasoning_effort.
	// https://api-docs.deepseek.com/guides/thinking_mode/
	{
		URI: "deepseek/deepseek-flash", Name: "DeepSeek V4.1 Flash", ContextWindow: 1_000_000,
		ReasoningEfforts: []string{"", "off", "low", "high", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortThinking,
			ReasoningReplay: chatcompletions.ReasoningReplayAllTurns,
		},
	},
	{
		URI: "deepseek/deepseek-v4-pro", Name: "DeepSeek V4 Pro", ContextWindow: 1_000_000,
		ImageInputUnsupported: true,
		ReasoningEfforts:      []string{"", "off", "high", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortThinking,
			ReasoningReplay: chatcompletions.ReasoningReplayAllTurns,
		},
	},
	{
		URI: "anthropic/claude-opus-5", Name: "Claude Opus 5", API: AnthropicMessages,
		ContextWindow: 1_000_000, MaxOutputTokens: 128_000, ReasoningEfforts: []string{""},
	},
	// Fable 5.1 binds thinking signatures to the preceding conversation context.
	// https://platform.claude.com/docs/en/build-with-claude/preserved-thinking
	{
		URI: "anthropic/claude-fable-5-1", Name: "Claude Fable 5.1", API: AnthropicMessages,
		ContextWindow: 1_000_000, MaxOutputTokens: 128_000, ReasoningEfforts: []string{""},
		DropMismatchedThinking: true, Compatibility: Unverified,
	},
	{URI: "openrouter/free", Name: "OpenRouter Free"},
	{URI: "openrouter/~x-ai/grok-latest", Name: "Grok Latest"},
	{URI: "openrouter/~moonshotai/kimi-latest", Name: "Kimi Latest"},
	{URI: "openrouter/~google/gemini-pro-latest", Name: "Gemini Pro Latest"},
	// Protocols: https://opencode.ai/docs/go/#endpoints
	// Limits and effort vocabularies: https://models.dev/api.json (opencode-go).
	{
		URI: "opencode-go/gpt-6-luna", Name: "OpenCode Go · GPT 6 Luna", API: Responses,
		ContextWindow:    922_000,
		ReasoningEfforts: []string{"", "none", "low", "medium", "high", "xhigh", "max"},
		ResponsesTraits:  &responsemodel.RouteTraits{ReasoningSummary: responsemodel.ReasoningSummaryAuto},
	},
	{
		URI: "opencode-go/gpt-5.6-luna", Name: "OpenCode Go · GPT 5.6 Luna", API: Responses,
		ContextWindow:    922_000,
		ReasoningEfforts: []string{"", "none", "low", "medium", "high", "xhigh", "max"},
		ResponsesTraits:  &responsemodel.RouteTraits{ReasoningSummary: responsemodel.ReasoningSummaryAuto},
	},
	{
		URI: "opencode-go/deepseek-v4.1-flash", Name: "OpenCode Go · DeepSeek V4.1 Flash", ContextWindow: 1_000_000,
		ReasoningEfforts: []string{"", "low", "high", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortTopLevel,
			ReasoningReplay: chatcompletions.ReasoningReplayAllTurns,
		},
	},
	{
		URI: "opencode-go/deepseek-v4-flash", Name: "OpenCode Go · DeepSeek V4 Flash", ContextWindow: 1_000_000,
		ImageInputUnsupported: true,
		ReasoningEfforts:      []string{"", "low", "high", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortTopLevel,
			ReasoningReplay: chatcompletions.ReasoningReplayAllTurns,
		},
	},
	{
		URI: "opencode-go/deepseek-v4-pro", Name: "OpenCode Go · DeepSeek V4 Pro", ContextWindow: 1_000_000,
		ImageInputUnsupported: true,
		ReasoningEfforts:      []string{"", "high", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortTopLevel,
			ReasoningReplay: chatcompletions.ReasoningReplayAllTurns,
		},
	},
	{
		URI: "opencode-go/deepseek-v4-flash-vision-exp", Name: "OpenCode Go · DeepSeek V4 Flash Vision Exp",
		ContextWindow: 1_000_000, ReasoningEfforts: []string{"", "off", "low", "high", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortThinking,
			ReasoningReplay: chatcompletions.ReasoningReplayAllTurns,
		},
	},
	{
		URI: "opencode-go/kimi-k3", Name: "OpenCode Go · Kimi K3", ContextWindow: 1_048_576,
		ReasoningEfforts: []string{"", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortTopLevel,
			ReasoningReplay: chatcompletions.ReasoningReplayCurrentTurn,
		},
	},
	{
		URI: "opencode-go/glm-5.2", Name: "OpenCode Go · GLM-5.2", ContextWindow: 1_000_000,
		ImageInputUnsupported: true,
		ReasoningEfforts:      []string{"", "high", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortTopLevel,
			ReasoningReplay: chatcompletions.ReasoningReplayCurrentTurn,
		},
	},
	{
		URI: "opencode-go/grok-4.7", Name: "OpenCode Go · Grok 4.7", API: Responses,
		ContextWindow: 500_000, ReasoningEfforts: []string{"", "low", "medium", "high", "xhigh"},
		ResponsesTraits: &responsemodel.RouteTraits{},
	},
	{
		URI: "opencode-go/grok-4.6", Name: "OpenCode Go · Grok 4.6", API: Responses,
		ContextWindow: 500_000, ReasoningEfforts: []string{"", "low", "medium", "high", "xhigh"},
		ResponsesTraits: &responsemodel.RouteTraits{},
	},
	{
		URI: "opencode-go/muse-spark-1.3-contributor", Name: "OpenCode Go · Muse Spark 1.3 Contributor", API: Responses,
		ContextWindow: 1_048_576, ReasoningEfforts: []string{"", "minimal", "low", "medium", "high", "xhigh"},
		ResponsesTraits: &responsemodel.RouteTraits{},
	},
	{
		URI: "opencode-go/muse-spark-1.2-contributor", Name: "OpenCode Go · Muse Spark 1.2 Contributor", API: Responses,
		ContextWindow: 1_048_576, ReasoningEfforts: []string{"", "minimal", "low", "medium", "high", "xhigh"},
		ResponsesTraits: &responsemodel.RouteTraits{},
	},
	{
		URI: "opencode-go/glm-5.3-flash", Name: "OpenCode Go · GLM-5.3-Flash", ContextWindow: 1_000_000,
		ReasoningEfforts: []string{"", "low", "high", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortTopLevel,
			ReasoningReplay: chatcompletions.ReasoningReplayCurrentTurn,
		},
	},
	{
		URI: "opencode-go/glm-5.3", Name: "OpenCode Go · GLM-5.3", ContextWindow: 1_000_000,
		ImageInputUnsupported: true,
		ReasoningEfforts:      []string{"", "low", "high", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortTopLevel,
			ReasoningReplay: chatcompletions.ReasoningReplayCurrentTurn,
		},
	},
	// These routes reason, but do not publish an optional effort vocabulary or
	// a reasoning replay contract for this endpoint.
	{
		URI: "opencode-go/glm-5.1", Name: "OpenCode Go · GLM-5.1", ContextWindow: 202_752,
		ImageInputUnsupported: true,
		ReasoningEfforts:      []string{""}, ChatTraits: &chatcompletions.RouteTraits{},
	},
	{
		URI: "opencode-go/kimi-k2.7-code", Name: "OpenCode Go · Kimi K2.7 Code", ContextWindow: 262_144,
		ReasoningEfforts: []string{""}, ChatTraits: &chatcompletions.RouteTraits{},
	},
	{
		URI: "opencode-go/kimi-k2.6", Name: "OpenCode Go · Kimi K2.6", ContextWindow: 262_144,
		ReasoningEfforts: []string{""}, ChatTraits: &chatcompletions.RouteTraits{},
	},
	{
		URI: "opencode-go/longcat-2.0", Name: "OpenCode Go · LongCat-2.0", ContextWindow: 1_000_000,
		ImageInputUnsupported: true,
		ReasoningEfforts:      []string{""},
		ChatTraits:            &chatcompletions.RouteTraits{},
	},
	{
		URI: "opencode-go/longcat-2.5-preview-free", Name: "OpenCode Go · LongCat 2.5 Preview Free", ContextWindow: 1_000_000,
		ReasoningEfforts: []string{""}, ChatTraits: &chatcompletions.RouteTraits{},
	},
	// MiMo thinks by default and expects reasoning_content back across turns.
	// https://mimo.mi.com/docs/en-US/usage-guide/passing-back-reasoning_content
	{
		URI: "opencode-go/mimo-v2.6-flash", Name: "OpenCode Go · MiMo V2.6 Flash", ContextWindow: 1_048_576,
		ReasoningEfforts: []string{""},
		ChatTraits:       &chatcompletions.RouteTraits{ReasoningReplay: chatcompletions.ReasoningReplayAllTurns},
	},
	{
		URI: "opencode-go/mimo-v2.6-pro", Name: "OpenCode Go · MiMo V2.6 Pro", ContextWindow: 1_048_576,
		ReasoningEfforts: []string{""},
		ChatTraits:       &chatcompletions.RouteTraits{ReasoningReplay: chatcompletions.ReasoningReplayAllTurns},
	},
	{
		URI: "opencode-go/mimo-v2.5", Name: "OpenCode Go · MiMo V2.5", ContextWindow: 1_000_000,
		ReasoningEfforts: []string{""},
		ChatTraits:       &chatcompletions.RouteTraits{ReasoningReplay: chatcompletions.ReasoningReplayAllTurns},
	},
	{
		URI: "opencode-go/mimo-v2.5-pro", Name: "OpenCode Go · MiMo V2.5 Pro", ContextWindow: 1_048_576,
		ReasoningEfforts: []string{""},
		ChatTraits:       &chatcompletions.RouteTraits{ReasoningReplay: chatcompletions.ReasoningReplayAllTurns},
	},
	{
		URI: "opencode-go/space-bunny-free", Name: "OpenCode Go · Space Bunny Free", ContextWindow: 524_288,
		ReasoningEfforts: []string{"", "low", "medium", "high", "xhigh", "max"},
		ChatTraits:       &chatcompletions.RouteTraits{ReasoningEffort: chatcompletions.ReasoningEffortTopLevel},
	},
	{
		URI: "opencode-go/hy4-preview", Name: "OpenCode Go · Hy4 preview", ContextWindow: 1_024_000,
		ImageInputUnsupported: true,
		ReasoningEfforts:      []string{"", "none", "high"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortTopLevel,
		},
	},
	{
		URI: "opencode-go/hy3", Name: "OpenCode Go · Hy3", ContextWindow: 192_000,
		ReasoningEfforts: []string{"", "none", "low", "high"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortTopLevel,
		},
	},
	{
		URI: "opencode-go/minimax-m3", Name: "OpenCode Go · MiniMax M3", API: AnthropicMessages,
		ContextWindow: 1_000_000, MaxOutputTokens: 131_072, ReasoningEfforts: []string{""},
	},
	{
		URI: "opencode-go/minimax-m2.7", Name: "OpenCode Go · MiniMax M2.7", API: AnthropicMessages,
		ContextWindow: 204_800, MaxOutputTokens: 131_072, ReasoningEfforts: []string{""},
	},
	{
		URI: "opencode-go/qwen3.8-max", Name: "OpenCode Go · Qwen3.8 Max", API: AnthropicMessages,
		ContextWindow: 1_000_000, MaxOutputTokens: 131_072, ReasoningEfforts: []string{""},
	},
	{
		URI: "opencode-go/qwen3.8-flash", Name: "OpenCode Go · Qwen3.8 Flash", API: AnthropicMessages,
		ContextWindow: 1_000_000, MaxOutputTokens: 131_072, ReasoningEfforts: []string{""},
	},
	{
		URI: "opencode-go/qwen3.7-max", Name: "OpenCode Go · Qwen3.7 Max", API: AnthropicMessages,
		ContextWindow: 1_000_000, MaxOutputTokens: 65_536, ReasoningEfforts: []string{""},
	},
	{
		URI: "opencode-go/qwen3.7-plus", Name: "OpenCode Go · Qwen3.7 Plus", API: AnthropicMessages,
		ContextWindow: 1_000_000, MaxOutputTokens: 65_536, ReasoningEfforts: []string{""},
	},
	{
		URI: "opencode-go/qwen3.6-plus", Name: "OpenCode Go · Qwen3.6 Plus", API: AnthropicMessages,
		ContextWindow: 1_000_000, MaxOutputTokens: 65_536, ReasoningEfforts: []string{""},
	},
}

func Resolve(uri, reasoningEffort string, overrides Overrides, enrichment Enrichment) (Route, error) {
	provider, model, err := ParseURI(uri)
	if err != nil {
		return Route{}, err
	}
	providerDescription, err := Provider(provider)
	if err != nil {
		return Route{}, err
	}
	if provider == "openai-codex" && (strings.TrimSpace(overrides.BaseURL) != "" || (overrides.API != "" && overrides.API != Responses)) {
		return Route{}, fmt.Errorf("openai-codex uses the ChatGPT Codex endpoint and Responses API; remove -base-url and any other -model-api override")
	}
	if overrides.ContextWindow < 0 {
		return Route{}, fmt.Errorf("model context window cannot be negative")
	}
	if enrichment.ContextWindow < 0 || (enrichment.ContextWindowEstimated && enrichment.ContextWindow == 0) {
		return Route{}, fmt.Errorf("invalid model context enrichment")
	}

	declaration, declared := CatalogSpec(uri)
	defaults := defaultModelSpec(provider)
	if providerDescription.RequireDeclaredModel && !declared && overrides.API == "" {
		return Route{}, &modelapi.APIRequiredError{URI: uri}
	}
	api := providerDescription.DefaultAPI
	if declaration.API != "" {
		api = declaration.API
	}
	declaredAPI := api
	compatibility := Unverified
	if declared {
		compatibility = Supported
		if declaration.Compatibility != "" {
			compatibility = declaration.Compatibility
		}
	}
	if overrides.API != "" {
		if !KnownAPI(overrides.API) {
			return Route{}, fmt.Errorf("unsupported model API %q", overrides.API)
		}
		// An explicit protocol makes compatibility unverified. Protocol-specific
		// route facts below are retained only when it still matches the reviewed
		// adapter.
		compatibility = Unverified
		api = overrides.API
	} else if compatibility == Unsupported {
		return Route{}, unsupportedModelRouteError(uri, api)
	}
	if !KnownAPI(api) {
		return Route{}, fmt.Errorf("unsupported model API %q", api)
	}

	usesReviewedProtocol := declared && api == declaredAPI
	reasoningEfforts := defaults.ReasoningEfforts
	if usesReviewedProtocol && declaration.ReasoningEfforts != nil {
		reasoningEfforts = declaration.ReasoningEfforts
	}
	if api == AnthropicMessages {
		// This adapter leaves reasoning effort at the provider default.
		reasoningEfforts = []string{DefaultReasoningEffort}
	}
	reasoningEfforts = append([]string(nil), reasoningEfforts...)
	reasoningEffort, err = normalizeReasoningEffortForRoute(uri, reasoningEffort, reasoningEfforts)
	if err != nil {
		return Route{}, err
	}

	traits := *defaults.ChatTraits
	if usesReviewedProtocol && declaration.ChatTraits != nil {
		traits = *declaration.ChatTraits
	}
	responsesTraits := *defaults.ResponsesTraits
	if usesReviewedProtocol && declaration.ResponsesTraits != nil {
		responsesTraits = *declaration.ResponsesTraits
	}
	baseURL := strings.TrimRight(strings.TrimSpace(providerDescription.BaseURL), "/")
	header := providerDescription.Header.Clone()
	customEndpoint := strings.TrimSpace(overrides.BaseURL) != ""
	if customEndpoint {
		baseURL = strings.TrimRight(strings.TrimSpace(overrides.BaseURL), "/")
		header = nil
		compatibility = Unverified
		// Cache extensions and cross-turn replay are route claims. A custom
		// compatible endpoint starts from the conservative generic behavior.
		traits.PromptCacheKey = false
		traits.ReasoningReplay = ""
		responsesTraits = responsemodel.RouteTraits{}
	}
	imageInputUnsupported := declaration.ImageInputUnsupported && !customEndpoint
	maxOutputTokens := 0
	if api == AnthropicMessages && !customEndpoint && declaration.API == AnthropicMessages {
		maxOutputTokens = declaration.MaxOutputTokens
	}
	// Placing cache breakpoints is a route claim like the traits above: a custom
	// compatible endpoint starts from the conservative generic behavior.
	promptCache := api == AnthropicMessages && !customEndpoint && providerDescription.PromptCache
	dropMismatchedThinking := api == AnthropicMessages && !customEndpoint &&
		usesReviewedProtocol && declaration.DropMismatchedThinking

	contextWindow, contextEstimated := 0, false
	switch {
	case overrides.ContextWindow > 0:
		contextWindow = overrides.ContextWindow
	case customEndpoint:
		contextWindow, contextEstimated = FallbackContextWindow, true
	case declaration.ContextWindow > 0:
		contextWindow = declaration.ContextWindow
	case enrichment.ContextWindow > 0:
		contextWindow = enrichment.ContextWindow
		contextEstimated = enrichment.ContextWindowEstimated
	default:
		contextWindow, contextEstimated = FallbackContextWindow, true
	}

	apiModel := strings.TrimSpace(declaration.APIModel)
	if apiModel == "" {
		apiModel = model
		if provider == "openrouter" {
			apiModel = canonicalOpenRouterModelID(model)
		}
	}
	stateContract := modelapi.ProviderStateContract("")
	switch api {
	case ChatCompletions:
		stateContract = traits.ProviderStateContract()
	case Responses:
		stateContract = responsesTraits.ProviderStateContract()
	case AnthropicMessages:
		stateContract = anthropic.ProviderStateContract
	}
	return Route{
		URI: provider + "/" + model, Provider: provider, Model: model, APIModel: apiModel, API: api,
		BaseURL: baseURL, Header: header, Credentialless: providerDescription.Credentialless,
		CustomEndpoint: customEndpoint, ImageInputUnsupported: imageInputUnsupported,
		ContextWindow: contextWindow, ContextWindowEstimated: contextEstimated,
		MaxOutputTokens: maxOutputTokens, PromptCache: promptCache,
		DropMismatchedThinking: dropMismatchedThinking,
		ReasoningEffort:        reasoningEffort, ReasoningEfforts: reasoningEfforts,
		ChatTraits: traits, ResponsesTraits: responsesTraits,
		Compatibility: compatibility, ProviderStateContract: stateContract,
	}, nil
}

func unsupportedModelRouteError(uri string, api API) error {
	if !KnownAPI(api) {
		return fmt.Errorf(
			"model route %q is unsupported: it requires model API %q, which is not implemented",
			strings.TrimSpace(uri), api,
		)
	}
	return fmt.Errorf("model route %q is unsupported by Skot", strings.TrimSpace(uri))
}

func defaultModelSpec(provider string) Spec {
	traits := chatcompletions.RouteTraits{
		ReasoningEffort: chatcompletions.ReasoningEffortTopLevel,
		ReasoningReplay: chatcompletions.ReasoningReplayCurrentTurn,
	}
	// Undeclared routes expose only the conservative default/high vocabulary;
	// route-specific values require a reviewed declaration.
	efforts := []string{DefaultReasoningEffort, "high"}
	switch provider {
	case "deepseek":
		traits.ReasoningReplay = chatcompletions.ReasoningReplayAllTurns
	case "openrouter":
		traits.ReasoningEffort = chatcompletions.ReasoningEffortNested
	case "openai":
		traits.PromptCacheKey = true
	case "ollama":
		efforts = []string{DefaultReasoningEffort}
		traits.ReasoningEffort = ""
	}
	responsesTraits := responsemodel.RouteTraits{}
	if provider == "openai-codex" {
		responsesTraits = responsemodel.RouteTraits{
			ReasoningSummary:   responsemodel.ReasoningSummaryAuto,
			EncryptedReasoning: true, PromptCacheKey: true, RequireInstructions: true,
		}
	}
	return Spec{ReasoningEfforts: efforts, ChatTraits: &traits, ResponsesTraits: &responsesTraits}
}

func CatalogSpec(uri string) (Spec, bool) {
	normalized := strings.ToLower(strings.TrimSpace(uri))
	switch normalized {
	case "deepseek/deepseek-v4-flash", "deepseek/deepseek-v4-flash-vision-exp":
		// DeepSeek serves these retired model IDs with V4.1 Flash. Preserve saved
		// selections while applying the current model's image and reasoning support.
		// https://api-docs.deepseek.com/quick_start/pricing/
		normalized = "deepseek/deepseek-flash"
	case "opencode-go/deepseek-flash":
		// Go still accepts the original ID used by saved selections.
		normalized = "opencode-go/deepseek-v4.1-flash"
	}
	for _, spec := range Catalog {
		if normalized == strings.ToLower(spec.URI) {
			spec.URI = strings.TrimSpace(uri)
			spec.ReasoningEfforts = append([]string(nil), spec.ReasoningEfforts...)
			if spec.ChatTraits != nil {
				traits := *spec.ChatTraits
				spec.ChatTraits = &traits
			}
			if spec.ResponsesTraits != nil {
				traits := *spec.ResponsesTraits
				spec.ResponsesTraits = &traits
			}
			return spec, true
		}
	}
	return Spec{}, false
}

func canonicalOpenRouterModelID(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if strings.EqualFold(modelID, "free") {
		return "openrouter/free"
	}
	return modelID
}
