package session

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Keep flock for exclusion with older Skot executors. The additional OFD lock
// lets readers query ownership without taking a lock or relying on procfs inode
// identities and PID namespaces. Both locks live until this descriptor closes.
// See https://man7.org/linux/man-pages/man2/F_OFD_GETLK.2const.html.
func acquireStoreLock(file *os.File) error {
	if err := acquireFlock(file); err != nil {
		return err
	}
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: int16(unix.SEEK_SET)}
	if err := unix.FcntlFlock(file.Fd(), unix.F_OFD_SETLK, &lock); err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EACCES) {
			return errStoreWouldBlock
		}
		return fmt.Errorf("lock journal ownership: %w", err)
	}
	return nil
}

func journalLocked(file *os.File) (bool, error) {
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: int16(unix.SEEK_SET)}
	if err := unix.FcntlFlock(file.Fd(), unix.F_OFD_GETLK, &lock); err != nil {
		return false, fmt.Errorf("inspect journal ownership: %w", err)
	}
	return lock.Type != unix.F_UNLCK, nil
}
