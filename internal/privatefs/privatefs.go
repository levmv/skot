// Package privatefs separates structural filesystem validation from optional
// permission repair and explicit trust checks for application configuration.
package privatefs

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// InspectDirectory validates an optional directory without changing it. A
// missing path is valid so callers can inspect state before deciding whether
// to create it.
func InspectDirectory(path, label string) error {
	return inspectDirectory(path, label, true)
}

// EnsureDirectory creates a directory and validates the resulting filesystem
// object rather than trusting MkdirAll alone. It does not change the mode of an
// existing directory.
func EnsureDirectory(path, label string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", label, err)
	}
	return inspectDirectory(path, label, false)
}

func inspectDirectory(path, label string, allowMissing bool) error {
	info, err := os.Lstat(path)
	if allowMissing && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s must be a directory", label)
	}
	return nil
}

// InspectRegularFile validates an optional regular file without changing it.
func InspectRegularFile(path, label string) error {
	return inspectRegularFile(path, label, true)
}

// RequireRegularFile is InspectRegularFile for state which must already exist.
func RequireRegularFile(path, label string) error {
	return inspectRegularFile(path, label, false)
}

func inspectRegularFile(path, label string, allowMissing bool) error {
	info, err := os.Lstat(path)
	if allowMissing && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", label)
	}
	return nil
}

// InspectTrustedDirectory validates an optional configuration root's structure
// and ownership (current user or root) without changing its permissions.
func InspectTrustedDirectory(path, label string) error {
	return inspectTrusted(path, label, true)
}

// InspectTrustedRegularFile validates an optional configuration or credential
// file's structure and ownership. Its permission bits do not prevent use.
func InspectTrustedRegularFile(path, label string) error {
	return inspectTrusted(path, label, false)
}

func inspectTrusted(path, label string, directory bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", label, err)
	}
	if directory && !info.IsDir() {
		return fmt.Errorf("%s must be a directory", label)
	}
	if !directory && !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", label)
	}
	return inspectOwnership(info, path, label)
}

// TryRestrictPermissions best-effort removes group/other access without
// following symlinks. It changes metadata only when path still identifies the
// inspected application-owned object. Failure is deliberately silent: refusing
// to use an otherwise valid object would not undo earlier exposure or restore
// trust in its contents.
func TryRestrictPermissions(path string) {
	inspected, err := os.Lstat(path)
	if err != nil {
		return
	}
	mode := inspected.Mode()
	if mode&os.ModeSymlink != 0 || (!inspected.IsDir() && !mode.IsRegular()) {
		return
	}
	if mode.Perm()&0o077 == 0 {
		return
	}
	// A replacement FIFO must not block before the identity check below.
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return
	}
	defer file.Close()
	current, err := file.Stat()
	if err != nil || !os.SameFile(inspected, current) {
		return
	}
	tryRestrictOpenFile(file, current)
}

// TryRestrictOpenFile is TryRestrictPermissions for an object which the caller
// has already opened safely.
func TryRestrictOpenFile(file *os.File) {
	info, err := file.Stat()
	if err != nil {
		return
	}
	tryRestrictOpenFile(file, info)
}

func tryRestrictOpenFile(file *os.File, info os.FileInfo) {
	if !safeToRestrict(info) {
		return
	}
	permissions := info.Mode().Perm()
	if permissions&0o077 == 0 {
		return
	}
	_ = file.Chmod(permissions &^ 0o077)
}
