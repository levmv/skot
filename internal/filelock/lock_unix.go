//go:build darwin || linux

package filelock

import (
	"errors"
	"os"
	"syscall"
)

var ErrWouldBlock = errors.New("file lock would block")

func acquireFlock(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return ErrWouldBlock
	}
	return err
}
