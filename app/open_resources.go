package app

import (
	"errors"
	"fmt"
	"strings"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/session"
	workspacetools "github.com/levmv/skot/tools"
)

type openResources struct {
	processes *workspacetools.ProcessManager
	children  *childSupervisor
	session   *liveSession
}

func (resources *openResources) fail(cause error) (*Application, error) {
	var cleanupErr error
	if resources.session != nil {
		cleanupErr = errors.Join(cleanupErr, resources.session.close())
	}
	if resources.children != nil {
		cleanupErr = errors.Join(cleanupErr, resources.children.Close())
	}
	if resources.processes != nil {
		cleanupErr = errors.Join(cleanupErr, resources.processes.Close())
	}
	return nil, errors.Join(cause, cleanupErr)
}

type openedSession struct {
	journal     *session.Store
	id          string
	managed     bool
	provisional bool
}

func openInitialSession(config Config, home, root string) (openedSession, error) {
	var opened openedSession
	journalPath := strings.TrimSpace(config.JournalPath)
	var err error
	switch {
	case journalPath != "":
		opened.journal, err = session.Open(journalPath)
	case config.Resume:
		var summary session.Summary
		summary, err = session.Resolve(home, root, config.ResumePrefix)
		if err == nil {
			opened.id = summary.ID
			opened.journal, err = session.OpenManaged(home, opened.id)
			opened.managed = err == nil
		} else {
			err = agent.MarkInvalidRequest(err)
		}
	case config.Interactive || config.SaveSession:
		opened.journal, opened.id, err = session.Create(home)
		opened.managed = err == nil
	default:
		opened.journal, opened.id, err = session.Create(home)
		opened.managed = err == nil
		opened.provisional = err == nil
	}
	if err != nil {
		return opened, fmt.Errorf("open session: %w", err)
	}
	return opened, nil
}
