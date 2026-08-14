package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/levmv/skot/agent"
)

type routeDiagnosticModel struct {
	agent.Model
	uri string
	api modelAPI
}

func (model routeDiagnosticModel) Complete(ctx context.Context, request agent.ModelRequest, emit func(agent.ModelStreamEvent)) (agent.ModelResponse, error) {
	response, err := model.Model.Complete(ctx, request, emit)
	if err == nil || !errors.Is(err, agent.ErrProviderFailure) || providerFailureHasIndependentExplanation(err) {
		return response, err
	}
	return response, fmt.Errorf(
		"%w; route %q is unverified, so the request may not match its %s protocol",
		err, model.uri, model.api,
	)
}

func providerFailureHasIndependentExplanation(err error) bool {
	var providerErr *agent.ProviderError
	if !errors.As(err, &providerErr) {
		return false
	}
	switch providerErr.Kind {
	case agent.ProviderErrorAuthentication, agent.ProviderErrorPermission, agent.ProviderErrorSubscription, agent.ProviderErrorQuota,
		agent.ProviderErrorRateLimit, agent.ProviderErrorUnavailable:
		return true
	default:
		return false
	}
}

func addRouteDiagnostics(model agent.Model, route resolvedModelRoute) agent.Model {
	if model == nil || route.Compatibility != modelCompatibilityUnverified {
		return model
	}
	return routeDiagnosticModel{Model: model, uri: route.URI, api: route.API}
}
