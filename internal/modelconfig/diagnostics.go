package modelconfig

import (
	"context"
	"errors"
	"fmt"

	"github.com/levmv/skot/model"
)

type diagnosticBackend struct {
	model.Backend
	uri string
	api API
}

func (backend diagnosticBackend) Complete(ctx context.Context, request model.Request, emit func(model.StreamEvent)) (model.Response, error) {
	response, err := backend.Backend.Complete(ctx, request, emit)
	if err == nil || !errors.Is(err, model.ErrProviderFailure) || providerFailureHasIndependentExplanation(err) {
		return response, err
	}
	return response, fmt.Errorf(
		"%w; route %q is unverified, so the request may not match its %s protocol",
		err, backend.uri, backend.api,
	)
}

func providerFailureHasIndependentExplanation(err error) bool {
	providerErr, ok := errors.AsType[*model.ProviderError](err)
	if !ok {
		return false
	}
	// Retrying these failures does not require changing the request protocol.
	// This includes explicit in-band generation errors with no more specific kind.
	if providerErr.Retryable {
		return true
	}
	switch providerErr.Kind {
	case model.ProviderErrorAuthentication, model.ProviderErrorPermission, model.ProviderErrorSubscription, model.ProviderErrorQuota,
		model.ProviderErrorRateLimit, model.ProviderErrorRequestTooLarge, model.ProviderErrorUnavailable:
		return true
	default:
		return false
	}
}

func addRouteDiagnostics(backend model.Backend, route Route) model.Backend {
	if backend == nil || route.Compatibility != Unverified {
		return backend
	}
	return diagnosticBackend{Backend: backend, uri: route.URI, api: route.API}
}
