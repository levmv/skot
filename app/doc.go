// Package app assembles and owns the concrete Skot application.
//
// It is UI-neutral but deliberately not a generic framework: it includes
// Skot's tools, sessions, settings, model selection, credentials, and sandbox
// policy. A terminal, graphical, or service frontend can drive Application
// without reconstructing those product decisions.
//
// The package is pre-v1. Its exported API may change when a second real
// consumer exposes a boundary that the current application does not model.
package app
