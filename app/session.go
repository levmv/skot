package app

import (
	"github.com/levmv/skot/agent"
)

// liveSession owns one session-bound runtime and its persistence lifetime.
// Application services such as settings, tools, credentials, and process
// policy remain outside it and may eventually be shared by several sessions.
type liveSession struct {
	id          string
	runtime     *agent.Runtime
	journal     sessionJournal
	managed     bool
	provisional bool
	memory      bool
}

type sessionJournal interface {
	agent.Journal
	HasUserTurn() bool
	TailRepaired() bool
	Close() error
	ClosePruningEmpty() error
	CloseDiscarding() error
}

func newLiveSession(id string, runtime *agent.Runtime, journal sessionJournal, managed bool) *liveSession {
	return &liveSession{id: id, runtime: runtime, journal: journal, managed: managed}
}

func (current *liveSession) close() error {
	if current == nil || current.journal == nil {
		return nil
	}
	if current.provisional {
		return current.journal.CloseDiscarding()
	}
	if current.managed {
		return current.journal.ClosePruningEmpty()
	}
	return current.journal.Close()
}
