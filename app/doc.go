// Package app assembles and owns the concrete Skot application.
//
// Application includes Skot's tools, sessions, settings, model selection,
// credentials, theme, and filesystem scope. Terminal, graphical, and service
// frontends can use the same application behavior. For model calls with
// application-managed history and execution, use package provider.
//
// ListSessions, ReadSession, and ReadSessionStatistics inspect saved sessions
// without opening an Application or interrupting their running owner. Statistics
// include recorded model usage and an estimate of the saved context.
package app
