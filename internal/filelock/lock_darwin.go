package filelock

import (
	"os"

	"golang.org/x/sys/unix"
)

func Acquire(file *os.File) error {
	return acquireFlock(file)
}

// Darwin's flock and fcntl locks share the vnode's lock list. F_GETLK queries
// it without acquiring ownership, including flock locks held by this process.
func Locked(file *os.File) (bool, error) {
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: int16(unix.SEEK_SET)}
	if err := unix.FcntlFlock(file.Fd(), unix.F_GETLK, &lock); err != nil {
		return false, err
	}
	return lock.Type != unix.F_UNLCK, nil
}
