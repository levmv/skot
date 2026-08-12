//go:build darwin || linux

package session

import (
	"errors"
	"os"
	"syscall"
)

var errStoreWouldBlock = errors.New("journal lock would block")

func acquireStoreLock(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errStoreWouldBlock
	}
	return err
}
