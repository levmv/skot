package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/levmv/skot/internal/canonicalpath"
)

// ErrSessionJobsActive means a worker still owns durable state being removed.
var ErrSessionJobsActive = errors.New("session has active background jobs")

// RemoveSessionJobs removes durable state for the given sessions only after
// checking every worker. It never adopts or stops work. The caller must exclude
// new launches for all sessionIDs until this operation returns, e.g. by owning
// their journals. Every job must have a terminal result and an absent worker;
// an unobservable worker or an unconfirmed command outcome prevents removal.
func RemoveSessionJobs(ctx context.Context, stateHome string, sessionIDs ...string) error {
	stateHome = canonicalpath.Resolve(stateHome)
	var paths []string
	for _, id := range sessionIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.TrimSpace(id) == "" {
			return errors.New("session ID is required")
		}
		path := sessionJobHome(filepath.Join(stateHome, "jobs"), id)
		if err := inspectRemovableSessionJobs(ctx, path, id); err != nil {
			return err
		}
		paths = append(paths, path)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Preflight all sessions before changing any state. Once cleanup starts,
	// finish it even if the caller's context is subsequently cancelled.
	for _, path := range paths {
		if err := removeJobDirectory(path, true); err != nil {
			return err
		}
	}
	return nil
}

func inspectRemovableSessionJobs(ctx context.Context, path, sessionID string) error {
	directory, err := openJobDirectory(path, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer directory.Close()
	entries, err := fs.ReadDir(directory.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() {
			return fmt.Errorf("invalid job directory %q", entry.Name())
		}
		jobDir := filepath.Join(path, entry.Name())
		metadata, err := loadJobMetadata(jobDir)
		if err != nil {
			return err
		}
		if err := validateJobSessionOwnership(metadata, sessionID); err != nil {
			return err
		}
		// A terminal result can precede worker exit. Wait for the control
		// reader to disappear too, so cleanup cannot race a worker's last I/O.
		live, err := probeJobControl(jobControlPath(jobDir))
		if err != nil {
			return fmt.Errorf("inspect job %s: %w", metadata.JobID, err)
		}
		if live {
			return fmt.Errorf("%w: %s", ErrSessionJobsActive, metadata.JobID)
		}
		// Losing the worker does not prove that its payload exited: commands
		// run in their own process session and can survive a worker crash.
		// A conforming worker publishes its result before closing the FIFO,
		// so read the result after observing that it no longer owns control.
		if _, terminal, err := readJobTerminalResult(jobDir, metadata.JobID); err != nil {
			return fmt.Errorf("inspect job %s result: %w", metadata.JobID, err)
		} else if !terminal {
			return fmt.Errorf("job %s has no terminal result; its command may still be running", metadata.JobID)
		}
	}
	return nil
}
