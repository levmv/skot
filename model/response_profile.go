package model

// ResponseProfile selects response extensions independently of a connection's
// name. The zero value uses the protocol's ordinary token and error fields.
type ResponseProfile string

const (
	// ResponseProfileOpenRouter reads OpenRouter's USD costs and interprets its
	// numeric in-band error codes as HTTP statuses for classification and retries.
	// An in-band error still has no transport HTTP status.
	ResponseProfileOpenRouter ResponseProfile = "openrouter"
)
