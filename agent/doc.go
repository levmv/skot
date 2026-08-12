// Package agent implements Skot's journal-backed, session-aware agent runtime.
//
// The package is intentionally opinionated rather than a general agent
// framework. It owns the append-only conversation model, replay invariants,
// model and tool lifecycle, cancellation, queued input, context management,
// compaction, and typed runtime events. Applications assemble concrete models,
// journals, tools, user interfaces, settings, authentication, and isolation.
//
// This is a pre-v1 API. It is public so another Skot-shaped application can
// exercise the boundary, not because every exported declaration is already a
// long-term compatibility promise.
//
// Runtime events are a live projection of mutations performed by Run, not a
// subscription to every journal append. Operations outside Run, including
// SwitchModel, Compact, shell runs, and reconciliation, are observed through
// State or Replay. A non-zero Event.Sequence is a committed watermark for the
// fact carried by that event, not a second complete State fold.
package agent
