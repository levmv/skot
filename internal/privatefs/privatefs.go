// Package privatefs separates structural filesystem validation from optional
// permission repair. A missing or broader-than-usual mode may be harmless
// historical state, while a symlink or the wrong object type changes what an
// operation would act on and remains an error.
package privatefs

import (
	"errors"
	"fmt"
	"os"
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
	file, err := os.Open(path)
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
