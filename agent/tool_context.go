package agent

import (
	"context"
	"slices"
	"strings"
)

type toolSessionContextKey struct{}

type toolReadOnlyPathsContextKey struct{}

// WithToolReadOnlyPaths supplies canonical resource directories authorized by
// the host. Tools still enforce protected paths and write restrictions.
func WithToolReadOnlyPaths(ctx context.Context, paths []string) context.Context {
	return context.WithValue(ctx, toolReadOnlyPathsContextKey{}, slices.Clone(paths))
}

func ToolReadOnlyPaths(ctx context.Context) []string {
	paths, _ := ctx.Value(toolReadOnlyPathsContextKey{}).([]string)
	return slices.Clone(paths)
}

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
