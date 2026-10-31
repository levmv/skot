package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/levmv/skot/internal/filelock"
	"github.com/levmv/skot/internal/sessionnotify"
	"golang.org/x/sys/unix"
)

// LockJournal takes ownership without replaying, repairing, or creating a
// journal. The caller keeps the returned file open through all dependent cleanup.
func LockJournal(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("session journal must be a regular file")
	}
	if err == nil {
		err = filelock.Acquire(file)
		if errors.Is(err, filelock.ErrWouldBlock) {
			err = fmt.Errorf("%w: %s", ErrLocked, path)
		}
	}
	if err == nil {
		err = sameJournal(file, path)
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// Delete holds the parent's journal lock while its owner removes child and job
// state. It does not need a valid replay: corrupt journals remain deletable.
// Missing parent state still requires cleanup after an interrupted deletion.
func Delete(ctx context.Context, home, id string, cleanup func() error) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	// If only the journal is missing, create an empty lock target in the
	// existing directory. A concurrent creator/opener must own that same file
	// before it can write. Never recreate a missing session directory.
	file, err := openManagedJournal(home, id, os.O_RDWR|os.O_CREATE)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if errors.Is(err, os.ErrNotExist) {
		// A missing home has neither dependent state nor notification observers.
		// Do not create it just to announce an already-complete deletion.
		if _, homeErr := os.Stat(home); homeErr != nil {
			if errors.Is(homeErr, os.ErrNotExist) {
				return nil
			}
			return homeErr
		}
	}
	lease := sessionnotify.Begin(home)
	defer func() {
		if result == nil {
			lease.Change(id)
		}
		lease.Close()
	}()
	if file != nil {
		defer file.Close()
		if err := filelock.Acquire(file); err != nil {
			if errors.Is(err, filelock.ErrWouldBlock) {
				return fmt.Errorf("%w: %s", ErrLocked, id)
			}
			return err
		}
		if err := sameJournal(file, file.Name()); err != nil {
			return err
		}
		// Ownership must be released observably even if dependent cleanup
		// refuses deletion. The file's deferred Close runs before lease.Close.
		lease.Own(id)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if cleanup != nil {
		if err := cleanup(); err != nil {
			return err
		}
	}
	root, err := os.OpenRoot(home)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	return root.RemoveAll(filepath.Join("sessions", id))
}
