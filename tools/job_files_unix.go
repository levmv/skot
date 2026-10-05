//go:build darwin || linux

package tools

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/levmv/skot/internal/privatefs"
	"golang.org/x/sys/unix"
)

// Job paths originate in the canonical state home. Refuse symlinks introduced
// anywhere along them, then keep each operation inside the opened directory.
func openJobDirectory(path string, create bool) (*os.Root, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("job directory must be absolute: %s", path)
	}
	directory, err := os.Open("/")
	if err != nil {
		return nil, err
	}
	for part := range strings.SplitSeq(filepath.Clean(path), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		fd, err := unix.Openat(int(directory.Fd()), part, flags, 0)
		if errors.Is(err, os.ErrNotExist) && create {
			if err = unix.Mkdirat(int(directory.Fd()), part, 0o700); err == nil || errors.Is(err, os.ErrExist) {
				fd, err = unix.Openat(int(directory.Fd()), part, flags, 0)
			}
		}
		_ = directory.Close()
		if err != nil {
			return nil, &os.PathError{Op: "open job directory", Path: path, Err: err}
		}
		directory = os.NewFile(uintptr(fd), path)
	}
	defer directory.Close()
	// OpenRoot has no constructor from a descriptor. Verify its handle against
	// the directory opened without symlinks before using it for any I/O.
	root, err := os.OpenRoot(path + string(filepath.Separator) + ".")
	if err != nil {
		return nil, err
	}
	expected, statErr := directory.Stat()
	opened, err := root.Stat(".")
	if statErr != nil || err != nil || !os.SameFile(expected, opened) {
		_ = root.Close()
		return nil, fmt.Errorf("job directory changed while opening: %s", path)
	}
	if create {
		privatefs.TryRestrictOpenFile(directory)
	}
	return root, nil
}

func openJobFile(path string, flags int, mode os.FileMode) (*os.File, error) {
	directory, err := openJobDirectory(filepath.Dir(path), false)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	return directory.OpenFile(filepath.Base(path), flags|unix.O_NONBLOCK|unix.O_NOCTTY, mode)
}

func createJobTemp(directory *os.Root, name string) (*os.File, error) {
	return directory.OpenFile("."+name+"-"+rand.Text(), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
}

func removeJobDirectory(path string, recursive bool) error {
	directory, err := openJobDirectory(filepath.Dir(path), false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer directory.Close()
	if !recursive {
		return directory.Remove(filepath.Base(path))
	}
	return directory.RemoveAll(filepath.Base(path))
}
