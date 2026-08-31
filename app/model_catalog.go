package app

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/state"
	"github.com/levmv/skot/model/anthropic"
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
	URI             string
	Name            string
	API             modelAPI
	APIModel        string
	ContextWindow   int
	MaxOutputTokens int
	// ImageInputUnsupported is a reviewed negative route fact. Its zero value
	// deliberately leaves image delivery optimistic for new and unknown models.
	ImageInputUnsupported bool
	ReasoningEfforts      []string
	ChatTraits            *chatcompletions.RouteTraits
	ResponsesTraits       *responsemodel.RouteTraits
	// Compatibility overrides the supported default for a reviewed declaration.
	Compatibility modelCompatibility
}

type modelRouteOverrides struct {
	BaseURL       string
	API           modelAPI
	ContextWindow int
}

// withSelection adds the protocol carried by one model selection. The
// process-wide override wins: it is the more recent explicit instruction, and
// the picker must describe the route the backend will build.
func (overrides modelRouteOverrides) withSelection(uri, api string) modelRouteOverrides {
	if overrides.API == "" {
		overrides.API = selectionModelAPI(uri, api)
	}
	return overrides
}

// selectionModelAPI is the protocol a user attached to one undeclared route.
// It stops applying as soon as this build declares that route: a reviewed
// declaration owns the protocol, and the remembered guess is then discarded
// rather than competing with it.
func selectionModelAPI(uri, api string) modelAPI {
	value := modelAPI(strings.ToLower(strings.TrimSpace(api)))
	if value == "" || !knownModelAPI(value) {
		return ""
	}
	if _, declared := catalogModelSpec(uri); declared {
		return ""
	}
	return value
}

// ModelAPIRequiredError reports a route which a mixed-protocol gateway serves
// and this build does not declare. Its protocol cannot be inferred from the
// provider, so a frontend must obtain one before the route can be selected.
type ModelAPIRequiredError struct {
	URI string
}

func (failure *ModelAPIRequiredError) Error() string {
	return fmt.Sprintf(
		"model %q is not available in Skot's current model list; choose another model or specify this model's API with -model-api",
		strings.TrimSpace(failure.URI),
	)
}

// IsModelAPIRequired reports a selection which only needs a protocol to become
// usable, so a frontend can offer that choice instead of the error.
func IsModelAPIRequired(err error) bool {
	_, ok := errors.AsType[*ModelAPIRequiredError](err)
	return ok
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
	ImageInputUnsupported  bool
	ContextWindow          int
	ContextWindowEstimated bool
	MaxOutputTokens        int
	PromptCache            bool
	ReasoningEffort        string
	ReasoningEfforts       []string
	ChatTraits             chatcompletions.RouteTraits
	ResponsesTraits        responsemodel.RouteTraits
	Compatibility          modelCompatibility
	ProviderStateContract  agent.ProviderStateContract
}

var modelCatalog = []modelSpec{
	// Native DeepSeek V4 exposes off/high/max. The thinking switch expresses off;
	// enabled requests pair it with reasoning_effort. Low/medium collapse to high.
	{
		URI: "deepseek/deepseek-v4-flash", Name: "DeepSeek V4 Flash", ContextWindow: 1_000_000,
		ImageInputUnsupported: true,
		ReasoningEfforts:      []string{"", "off", "high", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortThinking,
			ReasoningReplay: chatcompletions.ReasoningReplayToolTurns,
		},
	},
	{
		URI: "deepseek/deepseek-v4-pro", Name: "DeepSeek V4 Pro", ContextWindow: 1_000_000,
		ImageInputUnsupported: true,
		ReasoningEfforts:      []string{"", "off", "high", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortThinking,
			ReasoningReplay: chatcompletions.ReasoningReplayToolTurns,
		},
	},
	// The Messages adapter sends no thinking controls, so this route declares no
	// reasoning vocabulary even though the model reasons by default.
	{
		URI: "anthropic/claude-opus-5", Name: "Claude Opus 5", API: modelAPIAnthropicMessages,
		ContextWindow: 1_000_000, MaxOutputTokens: 128_000, ReasoningEfforts: []string{""},
	},
	{URI: "openrouter/free", Name: "OpenRouter Free"},
	{URI: "openrouter/~x-ai/grok-latest", Name: "Grok Latest"},
	{URI: "openrouter/~moonshotai/kimi-latest", Name: "Kimi Latest"},
	{URI: "openrouter/~google/gemini-pro-latest", Name: "Gemini Pro Latest"},
	{
		URI: "opencode-go/gpt-5.6-luna", Name: "OpenCode Go · GPT 5.6 Luna", API: modelAPIResponses,
		ContextWindow:    922_000,
		ReasoningEfforts: []string{"", "none", "low", "medium", "high", "xhigh", "max"},
		ResponsesTraits:  &responsemodel.RouteTraits{ReasoningSummary: responsemodel.ReasoningSummaryAuto},
	},
	{
		URI: "opencode-go/deepseek-v4-flash", Name: "OpenCode Go · DeepSeek V4 Flash", ContextWindow: 1_000_000,
		ImageInputUnsupported: true,
		ReasoningEfforts:      []string{"", "low", "high", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortTopLevel,
			ReasoningReplay: chatcompletions.ReasoningReplayToolTurns,
		},
	},
	{
		URI: "opencode-go/deepseek-v4-pro", Name: "OpenCode Go · DeepSeek V4 Pro", ContextWindow: 1_000_000,
		ImageInputUnsupported: true,
		ReasoningEfforts:      []string{"", "high", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortTopLevel,
			ReasoningReplay: chatcompletions.ReasoningReplayToolTurns,
		},
	},
	{
		URI: "opencode-go/deepseek-v4-flash-vision-exp", Name: "OpenCode Go · DeepSeek V4 Flash Vision Exp",
		ContextWindow: 1_000_000, ReasoningEfforts: []string{"", "off", "low", "high", "max"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortThinking,
			ReasoningReplay: chatcompletions.ReasoningReplayToolTurns,
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
		URI: "opencode-go/grok-4.6", Name: "OpenCode Go · Grok 4.6", API: modelAPIResponses,
		ContextWindow: 500_000, ReasoningEfforts: []string{"", "low", "medium", "high", "xhigh"},
		ResponsesTraits: &responsemodel.RouteTraits{},
	},
	{
		URI: "opencode-go/muse-spark-1.2-contributor", Name: "OpenCode Go · Muse Spark 1.2 Contributor", API: modelAPIResponses,
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
		URI: "opencode-go/mimo-v2.5", Name: "OpenCode Go · MiMo V2.5", ContextWindow: 1_000_000,
		ReasoningEfforts: []string{""}, ChatTraits: &chatcompletions.RouteTraits{},
	},
	{
		URI: "opencode-go/mimo-v2.5-pro", Name: "OpenCode Go · MiMo V2.5 Pro", ContextWindow: 1_048_576,
		ReasoningEfforts: []string{""}, ChatTraits: &chatcompletions.RouteTraits{},
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
		URI: "opencode-go/hy3", Name: "OpenCode Go · Hy3", ContextWindow: 256_000,
		ReasoningEfforts: []string{"", "none", "low", "high"},
		ChatTraits: &chatcompletions.RouteTraits{
			ReasoningEffort: chatcompletions.ReasoningEffortTopLevel,
		},
	},
	{
		URI: "opencode-go/minimax-m3", Name: "OpenCode Go · MiniMax M3", API: modelAPIAnthropicMessages,
		ContextWindow: 1_000_000, MaxOutputTokens: 131_072, ReasoningEfforts: []string{""},
	},
	{
		URI: "opencode-go/minimax-m2.7", Name: "OpenCode Go · MiniMax M2.7", API: modelAPIAnthropicMessages,
		ContextWindow: 204_800, MaxOutputTokens: 131_072, ReasoningEfforts: []string{""},
	},
	{
		URI: "opencode-go/qwen3.8-max", Name: "OpenCode Go · Qwen3.8 Max", API: modelAPIAnthropicMessages,
		ContextWindow: 1_000_000, MaxOutputTokens: 131_072, ReasoningEfforts: []string{""},
	},
	{
		URI: "opencode-go/qwen3.8-flash", Name: "OpenCode Go · Qwen3.8 Flash", API: modelAPIAnthropicMessages,
		ContextWindow: 1_000_000, MaxOutputTokens: 131_072, ReasoningEfforts: []string{""},
	},
	{
		URI: "opencode-go/qwen3.7-max", Name: "OpenCode Go · Qwen3.7 Max", API: modelAPIAnthropicMessages,
		ContextWindow: 1_000_000, MaxOutputTokens: 65_536, ReasoningEfforts: []string{""},
	},
	{
		URI: "opencode-go/qwen3.7-plus", Name: "OpenCode Go · Qwen3.7 Plus", API: modelAPIAnthropicMessages,
		ContextWindow: 1_000_000, MaxOutputTokens: 65_536, ReasoningEfforts: []string{""},
	},
	{
		URI: "opencode-go/qwen3.6-plus", Name: "OpenCode Go · Qwen3.6 Plus", API: modelAPIAnthropicMessages,
		ContextWindow: 1_000_000, MaxOutputTokens: 65_536, ReasoningEfforts: []string{""},
	},
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
		return resolvedModelRoute{}, &ModelAPIRequiredError{URI: uri}
	}
	api := providerDescription.defaultAPI
	if declaration.API != "" {
		api = declaration.API
	}
	declaredAPI := api
	compatibility := modelCompatibilityUnverified
	if declared {
		compatibility = modelCompatibilitySupported
		if declaration.Compatibility != "" {
			compatibility = declaration.Compatibility
		}
	}
	if overrides.API != "" {
		if !knownModelAPI(overrides.API) {
			return resolvedModelRoute{}, fmt.Errorf("unsupported model API %q", overrides.API)
		}
		// An explicit protocol makes compatibility unverified. Protocol-specific
		// route facts below are retained only when it still matches the reviewed
		// adapter.
		compatibility = modelCompatibilityUnverified
		api = overrides.API
	} else if compatibility == modelCompatibilityUnsupported {
		return resolvedModelRoute{}, unsupportedModelRouteError(uri, api)
	}
	if !knownModelAPI(api) {
		return resolvedModelRoute{}, fmt.Errorf("unsupported model API %q", api)
	}

	usesReviewedProtocol := declared && api == declaredAPI
	reasoningEfforts := defaults.ReasoningEfforts
	if usesReviewedProtocol && declaration.ReasoningEfforts != nil {
		reasoningEfforts = declaration.ReasoningEfforts
	}
	if api == modelAPIAnthropicMessages {
		// This adapter does not yet send optional thinking controls.
		reasoningEfforts = []string{defaultReasoningEffort}
	}
	reasoningEfforts = append([]string(nil), reasoningEfforts...)
	reasoningEffort, err = normalizeReasoningEffortForRoute(uri, reasoningEffort, reasoningEfforts)
	if err != nil {
		return resolvedModelRoute{}, err
	}

	traits := *defaults.ChatTraits
	if usesReviewedProtocol && declaration.ChatTraits != nil {
		traits = *declaration.ChatTraits
	}
	responsesTraits := *defaults.ResponsesTraits
	if usesReviewedProtocol && declaration.ResponsesTraits != nil {
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
	imageInputUnsupported := declaration.ImageInputUnsupported && !customEndpoint
	maxOutputTokens := 0
	if api == modelAPIAnthropicMessages && !customEndpoint && declaration.API == modelAPIAnthropicMessages {
		maxOutputTokens = declaration.MaxOutputTokens
	}
	// Placing cache breakpoints is a route claim like the traits above: a custom
	// compatible endpoint starts from the conservative generic behavior.
	promptCache := api == modelAPIAnthropicMessages && !customEndpoint && providerDescription.promptCache

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
	case modelAPIAnthropicMessages:
		stateContract = anthropic.ProviderStateContract
	}
	return resolvedModelRoute{
		URI: provider + "/" + model, Provider: provider, Model: model, APIModel: apiModel, API: api,
		BaseURL: baseURL, Header: header, Credentialless: providerDescription.credentialless,
		CustomEndpoint: customEndpoint, ImageInputUnsupported: imageInputUnsupported,
		ContextWindow: contextWindow, ContextWindowEstimated: contextEstimated,
		MaxOutputTokens: maxOutputTokens, PromptCache: promptCache,
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
	// Undeclared routes expose only the conservative default/high vocabulary;
	// route-specific values require a reviewed declaration.
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

// modelSelection is one known route together with the protocol its last
// deliberate selection carried. An empty API means the route describes itself.
type modelSelection struct {
	URI string
	API string
}

func knownModelSelections(store *state.InteractiveStore, current, currentAPI string) []modelSelection {
	var stored []modelSelection
	if store != nil {
		if settings, err := store.Settings(); err == nil {
			stored = append(stored, modelSelection{URI: settings.Workspace.Model, API: settings.Workspace.ModelAPI})
			for _, selection := range settings.ModelHistory {
				stored = append(stored, modelSelection{URI: selection.Model, API: selection.ModelAPI})
			}
		}
	}
	selections := make([]modelSelection, 0, len(modelCatalog)+len(stored)+1)
	seen := make(map[string]struct{}, cap(selections))
	add := func(uri, api string) {
		uri = strings.TrimSpace(uri)
		key := strings.ToLower(uri)
		if key == "" {
			return
		}
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		selections = append(selections, modelSelection{URI: uri, API: api})
	}
	add(current, currentAPI)
	for _, selection := range stored {
		add(selection.URI, selection.API)
	}
	for _, spec := range modelCatalog {
		add(spec.URI, "")
	}
	return selections
}

func modelChoices(store *state.InteractiveStore, current, currentAPI string, overrides modelRouteOverrides) []ModelChoice {
	choices := make([]ModelChoice, 0, len(modelCatalog)+4)
	for _, selection := range knownModelSelections(store, current, currentAPI) {
		uri := selection.URI
		declaration, _ := catalogModelSpec(uri)
		selected := overrides.withSelection(uri, selection.API)
		explicitProtocol := selected.API != "" && overrides.API == ""
		route, err := resolveModelRoute(uri, "", selected, modelRouteEnrichment{})
		if err != nil {
			api := declaration.API
			if api == "" {
				if provider, _, parseErr := parseModelURI(uri); parseErr == nil {
					if providerSpec, providerErr := modelProviderSpec(provider); providerErr == nil {
						api = providerSpec.defaultAPI
					}
				}
			}
			contextEstimated := declaration.ContextWindow <= 0
			choices = append(choices, ModelChoice{
				URI: uri, Name: declaration.Name, Protocol: string(api),
				ContextWindow: declaration.ContextWindow, ContextWindowEstimated: contextEstimated,
				ReasoningEfforts: append([]string(nil), declaration.ReasoningEfforts...),
				Unavailable:      true, UnavailableReason: err.Error(),
			})
			continue
		}
		if !implementedModelAPI(route.API) {
			choices = append(choices, ModelChoice{
				URI: uri, Name: declaration.Name, Protocol: string(route.API),
				ContextWindow: route.ContextWindow, ContextWindowEstimated: route.ContextWindowEstimated,
				ReasoningEfforts: append([]string(nil), route.ReasoningEfforts...), Unavailable: true,
				UnavailableReason: fmt.Sprintf("model API %q is not implemented", route.API),
			})
			continue
		}
		choices = append(choices, ModelChoice{
			URI: uri, Name: declaration.Name, Protocol: string(route.API),
			ProtocolExplicit: explicitProtocol,
			ContextWindow:    route.ContextWindow, ContextWindowEstimated: route.ContextWindowEstimated,
			ReasoningEfforts: append([]string(nil), route.ReasoningEfforts...),
		})
	}
	return choices
}
