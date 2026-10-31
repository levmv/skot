package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/levmv/skot/internal/privatefs"
	"github.com/levmv/skot/internal/session"
	"github.com/levmv/skot/internal/state"
	"github.com/levmv/skot/tools"
)

// ErrSessionBusy identifies a session whose journal or remaining work is owned
// by another executor. Use errors.Is; a previous Locked observation is advisory.
var ErrSessionBusy = session.ErrLocked

// ListSessions lists managed sessions across all workspaces without opening an
// Application. Empty drafts and child sessions are excluded. Unreadable session
// metadata sets that entry's Error; a failure to read the catalog itself returns
// an error. The rest of each journal is validated only when read or resumed.
// An empty home uses the same default as Open. Missing homes return an empty list.
func ListSessions(ctx context.Context, home string) ([]SessionSummary, error) {
	home, err := state.ResolveHome(home)
	if err != nil {
		return nil, err
	}
	entries, err := session.InspectAll(ctx, home)
	if err != nil {
		return nil, err
	}
	result := make([]SessionSummary, 0, len(entries))
	for _, entry := range entries {
		result = append(result, sessionSummary(entry))
	}
	return result, nil
}

// ReadSession reads the last complete journal prefix for a full managed ID,
// including while its owner is running. It neither repairs the tail nor marks
// unfinished work interrupted. State.LastSequence identifies the saved revision;
// token deltas and child-agent histories are not part of this snapshot.
func ReadSession(ctx context.Context, home, id string) (SessionSnapshot, error) {
	home, err := state.ResolveHome(home)
	if err != nil {
		return SessionSnapshot{}, err
	}
	snapshot, err := session.Inspect(ctx, home, id)
	return SessionSnapshot{Summary: sessionSummary(snapshot.Summary), State: snapshot.State}, err
}

// DeleteSession deletes a managed session and its child-agent and durable-job
// state. It returns ErrSessionBusy for owned journals or active workers and never
// stops them. Workspace files are untouched. Repeating a successful delete is
// harmless. Corrupt or incomplete journal contents do not prevent deletion;
// unobservable job ownership or unconfirmed job completion does. Only full IDs
// are accepted.
func DeleteSession(ctx context.Context, home, id string) error {
	home, err := state.ResolveHome(home)
	if err != nil {
		return err
	}
	return session.Delete(ctx, home, id, func() error {
		children, ids, err := lockSessionChildren(ctx, home, id)
		if err != nil {
			return err
		}
		defer func() {
			for _, child := range children {
				_ = child.Close()
			}
		}()
		if err := tools.RemoveSessionJobs(ctx, home, append(ids, id)...); err != nil {
			if errors.Is(err, tools.ErrSessionJobsActive) {
				return fmt.Errorf("%w: %w", ErrSessionBusy, err)
			}
			return err
		}
		root, err := os.OpenRoot(home)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		defer root.Close()
		return root.RemoveAll(filepath.Join("agents", id))
	})
}

func lockSessionChildren(ctx context.Context, home, parentID string) ([]*os.File, []string, error) {
	parent := filepath.Join(home, "agents", parentID)
	for _, dir := range []string{filepath.Join(home, "agents"), parent} {
		if err := privatefs.InspectDirectory(dir, "child agent directory"); err != nil {
			return nil, nil, err
		}
	}
	entries, err := os.ReadDir(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var locks []*os.File
	var ids []string
	fail := func(cause error) ([]*os.File, []string, error) {
		for _, lock := range locks {
			_ = lock.Close()
		}
		return nil, nil, cause
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if !entry.IsDir() {
			return fail(fmt.Errorf("invalid child agent directory %q", entry.Name()))
		}
		dir := filepath.Join(parent, entry.Name())
		// Children are published by renaming their staging directory before
		// they can run. A crash may leave an empty or partial staging entry.
		if strings.HasPrefix(entry.Name(), ".agent_") && strings.HasSuffix(entry.Name(), ".tmp") {
			file, err := session.LockJournal(filepath.Join(dir, "events.jsonl"))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fail(err)
			}
			if file != nil {
				locks = append(locks, file)
			}
			continue
		}
		metadata, err := readChildMetadata(filepath.Join(dir, "metadata.json"))
		if err != nil {
			return fail(err)
		}
		if metadata.Version != childMetadataVersion || metadata.AgentID != entry.Name() ||
			!validChildAgentID(metadata.AgentID) || metadata.ParentSessionID != parentID || !validChildSessionID(metadata.SessionID) {
			return fail(errors.New("invalid child session ownership"))
		}
		file, err := session.LockJournal(filepath.Join(dir, "events.jsonl"))
		if err != nil {
			return fail(err)
		}
		locks = append(locks, file)
		ids = append(ids, metadata.SessionID)
	}
	return locks, ids, nil
}

func sessionSummary(entry session.Summary) SessionSummary {
	return SessionSummary{
		ID: entry.ID, Workspace: entry.Workspace, Title: entry.Title,
		UpdatedAt: entry.UpdatedAt, Locked: entry.Locked, Error: entry.Error,
	}
}
