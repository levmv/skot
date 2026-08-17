//go:build darwin || linux

package state

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/levmv/skot/internal/privatefs"
	"golang.org/x/sys/unix"
)

func acquireInteractiveLock(path string, timeout time.Duration) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CLOEXEC|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open interactive state lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	fail := func(err error) (*os.File, error) {
		_ = file.Close()
		return nil, err
	}
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return fail(fmt.Errorf("inspect interactive state lock: %w", err))
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG {
		return fail(errors.New("interactive state lock must be a regular file"))
	}
	privatefs.TryRestrictOpenFile(file)
	deadline := time.Now().Add(timeout)
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fail(fmt.Errorf("lock interactive state: %w", err))
		}
		if !time.Now().Before(deadline) {
			return fail(fmt.Errorf("lock interactive state: timed out after %s", timeout))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func releaseInteractiveLock(file *os.File) error {
	if file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}
