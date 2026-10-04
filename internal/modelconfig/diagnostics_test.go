package modelconfig

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/levmv/skot/internal/modelhttp"
	modelapi "github.com/levmv/skot/model"
)

type routeDiagnosticTestModel struct{ err error }

func (model routeDiagnosticTestModel) Complete(context.Context, modelapi.Request, func(modelapi.StreamEvent)) (modelapi.Response, error) {
	return modelapi.Response{}, model.err
}

func TestRouteDiagnosticsExplainOnlyUnverifiedProtocolFailures(t *testing.T) {
	providerErr := modelapi.MarkProviderFailure(errors.New("upstream rejected field"))
	authErr := &modelapi.ProviderError{
		Cause: modelapi.MarkProviderFailure(errors.New("credential rejected")),
		Kind:  modelapi.ProviderErrorAuthentication,
	}
	for _, test := range []struct {
		name          string
		compatibility Compatibility
		cause         error
		wantContext   bool
	}{
		{"protocol failure", Unverified, providerErr, true},
		{"supported route", Supported, providerErr, false},
		{"local failure", Unverified, modelapi.MarkInvalidRequest(errors.New("invalid local request")), false},
		{"authentication failure", Unverified, authErr, false},
		{"generation failure", Unverified, modelhttp.NewProviderError(modelhttp.ProviderErrorDetails{
			Provider: "openrouter", Message: `generation ended with finish_reason "error"`,
		}), false},
		{"unknown completion reason", Unverified, modelhttp.UnsupportedCompletionReasonError("openrouter", "unknown"), true},
		{"invalid request", Unverified, modelhttp.NewProviderError(modelhttp.ProviderErrorDetails{
			Provider: "openrouter", Type: "invalid_request_error", Message: "unsupported field",
		}), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			route := Route{URI: "opencode-go/candidate", API: Responses, Compatibility: test.compatibility}
			model := addRouteDiagnostics(routeDiagnosticTestModel{err: test.cause}, route)
			_, err := model.Complete(t.Context(), modelapi.Request{}, nil)
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

func (model routeDiagnosticTestModel) ProjectModelItems(items []modelapi.Item) []modelapi.Item {
	return items
}
