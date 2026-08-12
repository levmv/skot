package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/session"
	workspacetools "github.com/levmv/skot/tools"
)

type openResources struct {
	processes    *workspacetools.ProcessManager
	session      *liveSession
	temporaryDir string
}

func (resources *openResources) fail(cause error) (*Application, error) {
	var cleanupErr error
	if resources.session != nil {
		cleanupErr = errors.Join(cleanupErr, resources.session.close())
	}
	if resources.processes != nil {
		cleanupErr = errors.Join(cleanupErr, resources.processes.Close())
	}
	if resources.temporaryDir != "" {
		cleanupErr = errors.Join(cleanupErr, os.RemoveAll(resources.temporaryDir))
	}
	return nil, errors.Join(cause, cleanupErr)
}

type openedSession struct {
	journal      *session.Store
	id           string
	managed      bool
	temporaryDir string
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
		opened.temporaryDir, err = os.MkdirTemp("", "sk-run-")
		if err == nil {
			opened.journal, err = session.OpenTransient(filepath.Join(opened.temporaryDir, "session.jsonl"))
		}
	}
	if err != nil {
		return opened, fmt.Errorf("open session: %w", err)
	}
	return opened, nil
}
