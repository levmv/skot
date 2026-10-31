package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/levmv/skot/agent"
	productlimits "github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/internal/privatefs"
	"github.com/levmv/skot/model"
	"golang.org/x/sys/unix"
)

type Snapshot struct {
	Summary Summary
	State   agent.State
}

func InspectAll(ctx context.Context, home string) ([]Summary, error) {
	return listSummaries(ctx, home, nil)
}

func Inspect(ctx context.Context, home, id string) (Snapshot, error) {
	snapshot := Snapshot{Summary: Summary{ID: id}}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	file, err := openManagedJournal(home, id, os.O_RDONLY)
	if err != nil {
		return snapshot, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return snapshot, err
	}
	snapshot.Summary.UpdatedAt = info.ModTime()
	snapshot.Summary.Locked, err = journalLocked(file)
	if err != nil {
		return snapshot, err
	}
	end, err := completePrefix(ctx, file, info.Size())
	if err != nil {
		return snapshot, err
	}
	records, err := readRecords(io.NewSectionReader(file, 0, end), productlimits.MaxJournalRecordBytes)
	if err != nil {
		return snapshot, err
	}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	snapshot.State, err = agent.Replay(records)
	if err != nil {
		return snapshot, err
	}
	if snapshot.State.SessionID != "" && snapshot.State.SessionID != id {
		return snapshot, errors.New("journal session ID does not match its directory")
	}
	snapshot.Summary.Workspace = canonicalWorkspace(snapshot.State.Workspace)
	for _, block := range snapshot.State.Blocks {
		for _, entry := range block.Entries {
			if entry.Item.Kind == model.ItemUserText {
				snapshot.Summary.Title = sessionTitle(agent.InputText(entry.Item))
				if snapshot.Summary.Title != "" {
					return snapshot, nil
				}
			}
		}
	}
	return snapshot, nil
}

// Read a finite prefix: appends after Stat cannot extend this snapshot, and a
// writer's unfinished final line is never parsed or repaired by an observer.
func completePrefix(ctx context.Context, file *os.File, size int64) (int64, error) {
	var buffer [32 * 1024]byte
	for end := size; end > 0; {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		start := max(0, end-int64(len(buffer)))
		part := buffer[:end-start]
		if _, err := file.ReadAt(part, start); err != nil {
			return 0, err
		}
		if index := bytes.LastIndexByte(part, '\n'); index >= 0 {
			return start + int64(index) + 1, nil
		}
		end = start
	}
	return 0, nil
}

func openManagedJournal(home, id string, flags int) (*os.File, error) {
	if !IsFullID(id) {
		return nil, fmt.Errorf("invalid session ID %q: a full managed session ID is required", id)
	}
	for _, dir := range []string{filepath.Join(home, "sessions"), filepath.Join(home, "sessions", id)} {
		if err := privatefs.InspectDirectory(dir, "session directory"); err != nil {
			return nil, err
		}
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.OpenFile(filepath.Join("sessions", id, journalName), flags|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("session journal must be a regular file")
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func sameJournal(file *os.File, path string) error {
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	current, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(opened, current) {
		return fmt.Errorf("session journal was replaced: %w", os.ErrNotExist)
	}
	return nil
}
