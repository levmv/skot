package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
)

type routeDiagnosticTestModel struct{ err error }

func (model routeDiagnosticTestModel) Complete(context.Context, agent.ModelRequest, func(agent.ModelStreamEvent)) (agent.ModelResponse, error) {
	return agent.ModelResponse{}, model.err
}

func TestRouteDiagnosticsExplainOnlyUnverifiedProtocolFailures(t *testing.T) {
	providerErr := agent.MarkProviderFailure(errors.New("upstream rejected field"))
	authErr := &agent.ProviderError{
		Cause: agent.MarkProviderFailure(errors.New("credential rejected")),
		Kind:  agent.ProviderErrorAuthentication,
	}
	for _, test := range []struct {
		name          string
		compatibility modelCompatibility
		cause         error
		wantContext   bool
	}{
		{"protocol failure", modelCompatibilityUnverified, providerErr, true},
		{"supported route", modelCompatibilitySupported, providerErr, false},
		{"local failure", modelCompatibilityUnverified, agent.MarkInvalidRequest(errors.New("invalid local request")), false},
		{"authentication failure", modelCompatibilityUnverified, authErr, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			route := resolvedModelRoute{URI: "opencode-go/candidate", API: modelAPIResponses, Compatibility: test.compatibility}
			model := addRouteDiagnostics(routeDiagnosticTestModel{err: test.cause}, route)
			_, err := model.Complete(t.Context(), agent.ModelRequest{}, nil)
			if !errors.Is(err, test.cause) {
				t.Fatalf("lost original error: %v", err)
			}
			if test.wantContext {
				if !strings.Contains(err.Error(), route.URI) || !strings.Contains(err.Error(), string(route.API)) {
					t.Fatalf("protocol failure lacks route context: %v", err)
				}
			} else if err.Error() != test.cause.Error() {
				t.Fatalf("unrelated failure received route diagnostic: %v", err)
			}
		})
	}
}

func (model routeDiagnosticTestModel) ProjectModelItems(items []agent.Item) []agent.Item {
	return items
}
