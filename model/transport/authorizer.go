package transport

import (
	"context"
	"net/http"
)

// Authorizer applies provider-owned authorization to a request. Implementations
// may refresh credentials and must be safe for concurrent use.
type Authorizer interface {
	Authorize(context.Context, *http.Request) error
}

// AuthorizerFunc adapts a function to Authorizer.
type AuthorizerFunc func(context.Context, *http.Request) error

func (fn AuthorizerFunc) Authorize(ctx context.Context, request *http.Request) error {
	return fn(ctx, request)
}

// BearerToken sets the Authorization header to Bearer followed by token.
func BearerToken(token string) Authorizer {
	return HeaderToken("Authorization", "Bearer "+token)
}

// HeaderToken sets a credential header, such as x-api-key for Anthropic.
func HeaderToken(name, token string) Authorizer {
	return AuthorizerFunc(func(_ context.Context, request *http.Request) error {
		request.Header.Set(name, token)
		return nil
	})
}
