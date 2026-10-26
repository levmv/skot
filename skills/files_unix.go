//go:build unix

package skills

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openSource(path string) (*os.Root, error) {
	return os.OpenRoot(path)
}

func openManifest(root *os.Root) (*os.File, error) {
	// Root confines symlinks to this package. Nonblocking open also prevents
	// a manifest replaced with a FIFO from hanging before the regular-file check.
	file, err := root.OpenFile("SKILL.md", os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("SKILL.md must be a regular file")
	}
	if info.Size() > MaxDocumentBytes {
		_ = file.Close()
		return nil, fmt.Errorf("SKILL.md exceeds %d bytes", MaxDocumentBytes)
	}
	return file, nil
}
