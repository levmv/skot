// Package model defines provider-neutral requests, responses, usage, and
// conversation items. ReplayContext prepares caller-owned history for a selected
// backend and accepts responses without executing tools or storing a journal.
// Use provider.Open to select a configured provider, or construct a protocol
// backend directly. Retries, fallback, and persistence belong to the caller.
package model
