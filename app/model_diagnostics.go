package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/levmv/skot/agent"
)

type routeDiagnosticBackend struct {
	agent.Backend
	uri string
	api modelAPI
}

func (backend routeDiagnosticBackend) Complete(ctx context.Context, request agent.ModelRequest, emit func(agent.ModelStreamEvent)) (agent.ModelResponse, error) {
	response, err := backend.Backend.Complete(ctx, request, emit)
	if err == nil || !errors.Is(err, agent.ErrProviderFailure) || providerFailureHasIndependentExplanation(err) {
		return response, err
	}
	return response, fmt.Errorf(
		"%w; route %q is unverified, so the request may not match its %s protocol",
		err, backend.uri, backend.api,
	)
}

func providerFailureHasIndependentExplanation(err error) bool {
	providerErr, ok := errors.AsType[*agent.ProviderError](err)
	if !ok {
		return false
	}
	switch providerErr.Kind {
	case agent.ProviderErrorAuthentication, agent.ProviderErrorPermission, agent.ProviderErrorSubscription, agent.ProviderErrorQuota,
		agent.ProviderErrorRateLimit, agent.ProviderErrorRequestTooLarge, agent.ProviderErrorUnavailable:
		return true
	default:
		return false
	}
}

func addRouteDiagnostics(backend agent.Backend, route resolvedModelRoute) agent.Backend {
	if backend == nil || route.Compatibility != modelCompatibilityUnverified {
		return backend
	}
	return routeDiagnosticBackend{Backend: backend, uri: route.URI, api: route.API}
}
