package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
)

type routeDiagnosticTestModel struct{ err error }

func (model routeDiagnosticTestModel) Info() agent.ModelInfo {
	return agent.ModelInfo{Backend: "test", Provider: "test", Model: "candidate"}
}

func (model routeDiagnosticTestModel) Complete(context.Context, agent.ModelRequest, func(agent.ModelStreamEvent)) (agent.ModelResponse, error) {
	return agent.ModelResponse{}, model.err
}

func TestUnverifiedRouteAddsProtocolContextOnlyAfterAProviderFailure(t *testing.T) {
	providerErr := agent.MarkProviderFailure(errors.New("upstream rejected field"))
	model := addRouteDiagnostics(routeDiagnosticTestModel{err: providerErr}, resolvedModelRoute{
		URI: "opencode-go/candidate", API: modelAPIResponses, Compatibility: modelCompatibilityUnverified,
	})
	_, err := model.Complete(context.Background(), agent.ModelRequest{}, nil)
	if !errors.Is(err, agent.ErrProviderFailure) ||
		!strings.Contains(err.Error(), `route "opencode-go/candidate" is unverified, so the request may not match its responses protocol`) {
		t.Fatalf("provider error = %v", err)
	}

	localErr := agent.MarkInvalidRequest(errors.New("invalid local request"))
	model = addRouteDiagnostics(routeDiagnosticTestModel{err: localErr}, resolvedModelRoute{
		URI: "opencode-go/candidate", API: modelAPIResponses, Compatibility: modelCompatibilityUnverified,
	})
	_, err = model.Complete(context.Background(), agent.ModelRequest{}, nil)
	if strings.Contains(err.Error(), `route "opencode-go/candidate" is unverified`) {
		t.Fatalf("local error received route diagnostic: %v", err)
	}

	authErr := &agent.ProviderError{
		Cause: agent.MarkProviderFailure(errors.New("credential rejected")),
		Kind:  agent.ProviderErrorAuthentication,
	}
	model = addRouteDiagnostics(routeDiagnosticTestModel{err: authErr}, resolvedModelRoute{
		URI: "opencode-go/candidate", API: modelAPIResponses, Compatibility: modelCompatibilityUnverified,
	})
	_, err = model.Complete(context.Background(), agent.ModelRequest{}, nil)
	if strings.Contains(err.Error(), `route "opencode-go/candidate" is unverified`) {
		t.Fatalf("authentication error received irrelevant route diagnostic: %v", err)
	}
}

func TestSupportedRouteDoesNotWrapItsBackend(t *testing.T) {
	inner := routeDiagnosticTestModel{err: agent.MarkProviderFailure(errors.New("service down"))}
	model := addRouteDiagnostics(inner, resolvedModelRoute{Compatibility: modelCompatibilitySupported})
	if _, wrapped := model.(routeDiagnosticModel); wrapped {
		t.Fatal("supported route was wrapped with an unverified-route diagnostic")
	}
}

func (model routeDiagnosticTestModel) ProjectModelItems(items []agent.Item) []agent.Item {
	return items
}
