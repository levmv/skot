//go:build darwin || linux

package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/levmv/skot/internal/privatefs"
	"golang.org/x/sys/unix"
)

func acquireInteractiveLock(path string, timeout time.Duration) (*os.File, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	file, err := acquireStateLock(ctx, path, "interactive state")
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("lock interactive state: timed out after %s", timeout)
	}
	return file, err
}

func acquireStateLock(ctx context.Context, path, label string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CLOEXEC|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s lock: %w", label, err)
	}
	file := os.NewFile(uintptr(fd), path)
	fail := func(err error) (*os.File, error) {
		_ = file.Close()
		return nil, err
	}
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return fail(fmt.Errorf("inspect %s lock: %w", label, err))
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG {
		return fail(fmt.Errorf("%s lock must be a regular file", label))
	}
	privatefs.TryRestrictOpenFile(file)
	for {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fail(fmt.Errorf("lock %s: %w", label, err))
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func releaseStateLock(file *os.File) error {
	if file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}
