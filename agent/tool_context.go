package agent

import (
	"context"
	"strings"
)

type toolSessionContextKey struct{}

// WithToolSessionID associates a model-requested tool call with its session.
// Process tools use it to keep managed jobs and completion events from crossing
// sessions, including jobs recovered after a restart.
func WithToolSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, toolSessionContextKey{}, strings.TrimSpace(sessionID))
}

func ToolSessionID(ctx context.Context) string {
	sessionID, _ := ctx.Value(toolSessionContextKey{}).(string)
	return sessionID
}
