// Package agent implements Skot's journal-backed agent runtime.
//
// Runtime executes the model and tool loop, including retries, queued input,
// context management, and compaction. Supply a model backend, journal, and
// tools to New. For model calls with a caller-managed loop, use package provider.
//
// Runtime events are a live projection of mutations performed by Run, not a
// subscription to every journal append. Operations outside Run, including
// SwitchModel, Compact, shell runs, and reconciliation, are observed through
// State or Replay. A non-zero Event.Sequence gives the committed journal position
// associated with the event. Use State for a complete session snapshot.
package agent
