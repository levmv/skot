package app

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/state"
	"github.com/levmv/skot/model/chatcompletions"
	responsemodel "github.com/levmv/skot/model/responses"
)

const unknownModelContextWindow = 128 * 1024

type modelCompatibility string

const (
	modelCompatibilitySupported   modelCompatibility = "supported"
	modelCompatibilityUnverified  modelCompatibility = "unverified"
	modelCompatibilityUnsupported modelCompatibility = "unsupported"
)

// modelSpec is one reviewed local declaration. Nil/zero optional fields inherit
// the provider route defaults; it is deliberately not an exhaustive registry.
type modelSpec struct {
	URI              string
	Name             string
	API              modelAPI
	APIModel         string
	ContextWindow    int
	ReasoningEfforts []string
	ChatTraits       *chatcompletions.RouteTraits
	ResponsesTraits  *responsemodel.RouteTraits
	Compatibility    modelCompatibility
}

type modelRouteOverrides struct {
	BaseURL       string
	API           modelAPI
	ContextWindow int
}

type modelRouteEnrichment struct {
	ContextWindow          int
	ContextWindowEstimated bool
}

// resolvedModelRoute is the immutable, secret-free contract consumed by one
// adapter construction. Resolution is pure and never owns an HTTP client.
type resolvedModelRoute struct {
	URI                    string
	Provider               string
	Model                  string
	APIModel               string
	API                    modelAPI
	BaseURL                string
	Header                 http.Header
	Credentialless         bool
	CustomEndpoint         bool
	ContextWindow          int
	ContextWindowEstimated bool
	ReasoningEffort        string
	ReasoningEfforts       []string
	ChatTraits             chatcompletions.RouteTraits
	ResponsesTraits        responsemodel.RouteTraits
	Compatibility          modelCompatibility
	ProviderStateContract  agent.ProviderStateContract
}

var modelCatalog = []modelSpec{
	{URI: "deepseek/deepseek-v4-flash", Name: "DeepSeek V4 Flash", ContextWindow: 1_000_000, Compatibility: modelCompatibilitySupported},
	{URI: "deepseek/deepseek-v4-pro", Name: "DeepSeek V4 Pro", ContextWindow: 1_000_000, Compatibility: modelCompatibilitySupported},
	{URI: "openrouter/free", Name: "OpenRouter Free", Compatibility: modelCompatibilitySupported},
	{URI: "openrouter/~x-ai/grok-latest", Name: "Grok Latest", Compatibility: modelCompatibilitySupported},
	{URI: "openrouter/~moonshotai/kimi-latest", Name: "Kimi Latest", Compatibility: modelCompatibilitySupported},
	{URI: "openrouter/~google/gemini-pro-latest", Name: "Gemini Pro Latest", Compatibility: modelCompatibilitySupported},
	// OpenCode Go routes are a local snapshot researched against the provider's
	// route table and Models.dev on 2026-08-13. Their protocol, state, tools,
	// reasoning choices, errors, and optional fields passed the live subscription
	// gateway baseline on 2026-08-14. Context limits follow the provider's
	// published route metadata.
	{
		URI: "opencode-go/gpt-5.6-luna", Name: "OpenCode Go · GPT 5.6 Luna", API: modelAPIResponses,
		ContextWindow:    922_000,
		ReasoningEfforts: []string{"", "none", "low", "medium", "high", "xhigh", "max"},
		ResponsesTraits:  &responsemodel.RouteTraits{ReasoningSummary: responsemodel.ReasoningSummaryAuto},
		Compatibility:    modelCompatibilitySupported,
	},
	{
		URI: "opencode-go/deepseek-v4-flash", Name: "OpenCode Go · DeepSeek V4 Flash", ContextWindow: 1_000_000,
		ReasoningEfforts: []string{"", "low", "high", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortTopLevel,
			ReasoningReplay: chatcompletions.ReasoningReplayToolTurns,
		},
		Compatibility: modelCompatibilitySupported,
	},
	{
		URI: "opencode-go/deepseek-v4-pro", Name: "OpenCode Go · DeepSeek V4 Pro", ContextWindow: 1_000_000,
		ReasoningEfforts: []string{"", "high", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortTopLevel,
			ReasoningReplay: chatcompletions.ReasoningReplayToolTurns,
		},
		Compatibility: modelCompatibilitySupported,
	},
	{
		URI: "opencode-go/kimi-k3", Name: "OpenCode Go · Kimi K3", ContextWindow: 1_048_576,
		ReasoningEfforts: []string{"", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortTopLevel,
			ReasoningReplay: chatcompletions.ReasoningReplayCurrentTurn,
		},
		Compatibility: modelCompatibilitySupported,
	},
	{
		URI: "opencode-go/glm-5.2", Name: "OpenCode Go · GLM-5.2", ContextWindow: 1_000_000,
		ReasoningEfforts: []string{"", "high", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortTopLevel,
			ReasoningReplay: chatcompletions.ReasoningReplayCurrentTurn,
		},
		Compatibility: modelCompatibilitySupported,
	},
	// These current OpenCode Go routes require Anthropic Messages. Recording
	// them prevents the provider's Chat Completions default from silently
	// selecting the wrong adapter, while keeping them out of runnable choices.
	{URI: "opencode-go/minimax-m3", Name: "OpenCode Go · MiniMax M3", API: modelAPIAnthropicMessages, Compatibility: modelCompatibilityUnsupported},
	{URI: "opencode-go/minimax-m2.7", Name: "OpenCode Go · MiniMax M2.7", API: modelAPIAnthropicMessages, Compatibility: modelCompatibilityUnsupported},
	{URI: "opencode-go/minimax-m2.5", Name: "OpenCode Go · MiniMax M2.5", API: modelAPIAnthropicMessages, Compatibility: modelCompatibilityUnsupported},
	{URI: "opencode-go/qwen3.8-max", Name: "OpenCode Go · Qwen3.8 Max", API: modelAPIAnthropicMessages, Compatibility: modelCompatibilityUnsupported},
	{URI: "opencode-go/qwen3.7-max", Name: "OpenCode Go · Qwen3.7 Max", API: modelAPIAnthropicMessages, Compatibility: modelCompatibilityUnsupported},
	{URI: "opencode-go/qwen3.7-plus", Name: "OpenCode Go · Qwen3.7 Plus", API: modelAPIAnthropicMessages, Compatibility: modelCompatibilityUnsupported},
	{URI: "opencode-go/qwen3.6-plus", Name: "OpenCode Go · Qwen3.6 Plus", API: modelAPIAnthropicMessages, Compatibility: modelCompatibilityUnsupported},
}

func resolveModelRoute(uri, reasoningEffort string, overrides modelRouteOverrides, enrichment modelRouteEnrichment) (resolvedModelRoute, error) {
	provider, model, err := parseModelURI(uri)
	if err != nil {
		return resolvedModelRoute{}, err
	}
	providerDescription, err := modelProviderSpec(provider)
	if err != nil {
		return resolvedModelRoute{}, err
	}
	if overrides.ContextWindow < 0 {
		return resolvedModelRoute{}, fmt.Errorf("model context window cannot be negative")
	}
	if enrichment.ContextWindow < 0 || (enrichment.ContextWindowEstimated && enrichment.ContextWindow == 0) {
		return resolvedModelRoute{}, fmt.Errorf("invalid model context enrichment")
	}

	declaration, declared := catalogModelSpec(uri)
	defaults := defaultModelSpec(provider)
	if providerDescription.requireDeclaredModel && !declared && overrides.API == "" {
		return resolvedModelRoute{}, fmt.Errorf(
			"model route %q has no reviewed protocol declaration; select a curated model or set -model-api explicitly",
			strings.TrimSpace(uri),
		)
	}
	api := providerDescription.defaultAPI
	if declaration.API != "" {
		api = declaration.API
	}
	compatibility := modelCompatibilityUnverified
	if declared && declaration.Compatibility != "" {
		compatibility = declaration.Compatibility
	}
	if overrides.API != "" {
		if !knownModelAPI(overrides.API) {
			return resolvedModelRoute{}, fmt.Errorf("unsupported model API %q", overrides.API)
		}
		// An explicit protocol bypasses the reviewed route declaration even when
		// it happens to select the same adapter today.
		compatibility = modelCompatibilityUnverified
		api = overrides.API
	} else if compatibility == modelCompatibilityUnsupported {
		return resolvedModelRoute{}, unsupportedModelRouteError(uri, api)
	}
	if !knownModelAPI(api) {
		return resolvedModelRoute{}, fmt.Errorf("unsupported model API %q", api)
	}

	reasoningEfforts := defaults.ReasoningEfforts
	if declaration.ReasoningEfforts != nil {
		reasoningEfforts = declaration.ReasoningEfforts
	}
	reasoningEfforts = append([]string(nil), reasoningEfforts...)
	reasoningEffort, err = normalizeReasoningEffortForRoute(uri, reasoningEffort, reasoningEfforts)
	if err != nil {
		return resolvedModelRoute{}, err
	}

	traits := *defaults.ChatTraits
	if declaration.ChatTraits != nil {
		traits = *declaration.ChatTraits
	}
	responsesTraits := *defaults.ResponsesTraits
	if declaration.ResponsesTraits != nil {
		responsesTraits = *declaration.ResponsesTraits
	}
	baseURL := strings.TrimRight(strings.TrimSpace(providerDescription.baseURL), "/")
	header := providerDescription.header.Clone()
	customEndpoint := strings.TrimSpace(overrides.BaseURL) != ""
	if customEndpoint {
		baseURL = strings.TrimRight(strings.TrimSpace(overrides.BaseURL), "/")
		header = nil
		compatibility = modelCompatibilityUnverified
		// Cache extensions and cross-turn replay are route claims. A custom
		// compatible endpoint starts from the conservative generic behavior.
		traits.PromptCacheKey = false
		traits.ReasoningReplay = ""
		responsesTraits = responsemodel.RouteTraits{}
	}

	contextWindow, contextEstimated := 0, false
	switch {
	case overrides.ContextWindow > 0:
		contextWindow = overrides.ContextWindow
	case customEndpoint:
		contextWindow, contextEstimated = unknownModelContextWindow, true
	case declaration.ContextWindow > 0:
		contextWindow = declaration.ContextWindow
	case enrichment.ContextWindow > 0:
		contextWindow = enrichment.ContextWindow
		contextEstimated = enrichment.ContextWindowEstimated
	default:
		contextWindow, contextEstimated = unknownModelContextWindow, true
	}

	apiModel := strings.TrimSpace(declaration.APIModel)
	if apiModel == "" {
		apiModel = model
		if provider == "openrouter" {
			apiModel = canonicalOpenRouterModelID(model)
		}
	}
	stateContract := agent.ProviderStateContract("")
	switch api {
	case modelAPIChatCompletions:
		stateContract = traits.ProviderStateContract()
	case modelAPIResponses:
		stateContract = responsesTraits.ProviderStateContract()
	}
	return resolvedModelRoute{
		URI: provider + "/" + model, Provider: provider, Model: model, APIModel: apiModel, API: api,
		BaseURL: baseURL, Header: header, Credentialless: providerDescription.credentialless,
		CustomEndpoint: customEndpoint, ContextWindow: contextWindow, ContextWindowEstimated: contextEstimated,
		ReasoningEffort: reasoningEffort, ReasoningEfforts: reasoningEfforts,
		ChatTraits: traits, ResponsesTraits: responsesTraits,
		Compatibility: compatibility, ProviderStateContract: stateContract,
	}, nil
}

func unsupportedModelRouteError(uri string, api modelAPI) error {
	if !implementedModelAPI(api) {
		return fmt.Errorf(
			"model route %q is unsupported: it requires model API %q, which is not implemented",
			strings.TrimSpace(uri), api,
		)
	}
	return fmt.Errorf("model route %q is unsupported by Skot", strings.TrimSpace(uri))
}

func defaultModelSpec(provider string) modelSpec {
	traits := chatcompletions.RouteTraits{
		ReasoningEffort: chatcompletions.ReasoningEffortTopLevel,
		ReasoningReplay: chatcompletions.ReasoningReplayCurrentTurn,
	}
	efforts := []string{defaultReasoningEffort, "high"}
	switch provider {
	case "deepseek":
		traits.ReasoningReplay = chatcompletions.ReasoningReplayToolTurns
	case "openrouter":
		traits.ReasoningEffort = chatcompletions.ReasoningEffortNested
	case "openai":
		traits.PromptCacheKey = true
	case "ollama":
		efforts = []string{defaultReasoningEffort}
		traits.ReasoningEffort = ""
	}
	responsesTraits := responsemodel.RouteTraits{}
	return modelSpec{ReasoningEfforts: efforts, ChatTraits: &traits, ResponsesTraits: &responsesTraits}
}

func catalogModelSpec(uri string) (modelSpec, bool) {
	normalized := strings.ToLower(strings.TrimSpace(uri))
	for _, spec := range modelCatalog {
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
	return modelSpec{}, false
}

func canonicalOpenRouterModelID(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if strings.EqualFold(modelID, "free") {
		return "openrouter/free"
	}
	return modelID
}

func knownModelURIs(store *state.Store, current string) []string {
	models := make([]string, 0, len(modelCatalog)+4)
	seen := make(map[string]struct{}, cap(models))
	add := func(uri string) {
		uri = strings.TrimSpace(uri)
		key := strings.ToLower(uri)
		if key == "" {
			return
		}
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		models = append(models, uri)
	}
	add(current)
	if store != nil {
		if settings, err := store.Settings(); err == nil {
			add(settings.Model)
			for _, uri := range settings.RecentModels {
				add(uri)
			}
		}
	}
	for _, spec := range modelCatalog {
		add(spec.URI)
	}
	return models
}

func modelChoices(store *state.Store, current string, overrides modelRouteOverrides) []ModelChoice {
	choices := make([]ModelChoice, 0, len(modelCatalog)+4)
	for _, uri := range knownModelURIs(store, current) {
		declaration, declared := catalogModelSpec(uri)
		route, err := resolveModelRoute(uri, "", overrides, modelRouteEnrichment{})
		if err != nil {
			api := declaration.API
			if api == "" {
				if provider, _, parseErr := parseModelURI(uri); parseErr == nil {
					if providerSpec, providerErr := modelProviderSpec(provider); providerErr == nil {
						api = providerSpec.defaultAPI
					}
				}
			}
			compatibility := modelCompatibility("")
			if declared && declaration.Compatibility != "" {
				compatibility = declaration.Compatibility
			}
			contextEstimated := declaration.ContextWindow <= 0
			choices = append(choices, ModelChoice{
				URI: uri, Name: declaration.Name, Protocol: string(api), Compatibility: string(compatibility),
				ContextWindow: declaration.ContextWindow, ContextWindowEstimated: contextEstimated,
				ReasoningEfforts: append([]string(nil), declaration.ReasoningEfforts...),
				Unavailable:      true, UnavailableReason: err.Error(),
			})
			continue
		}
		if !implementedModelAPI(route.API) {
			choices = append(choices, ModelChoice{
				URI: uri, Name: declaration.Name, Protocol: string(route.API), Compatibility: string(route.Compatibility),
				ContextWindow: route.ContextWindow, ContextWindowEstimated: route.ContextWindowEstimated,
				ReasoningEfforts: append([]string(nil), route.ReasoningEfforts...), Unavailable: true,
				UnavailableReason: fmt.Sprintf("model API %q is not implemented", route.API),
			})
			continue
		}
		choices = append(choices, ModelChoice{
			URI: uri, Name: declaration.Name, Protocol: string(route.API), Compatibility: string(route.Compatibility),
			ContextWindow: route.ContextWindow, ContextWindowEstimated: route.ContextWindowEstimated,
			ReasoningEfforts: append([]string(nil), route.ReasoningEfforts...),
		})
	}
	return choices
}
