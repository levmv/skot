package sessionnotify

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func fallbackRuntimeDirectory(instance string) string {
	return filepath.Join("/tmp", "skot-"+instance)
}

func runtimeAddress(instance string) (string, error) {
	dir := fallbackRuntimeDirectory(instance)
	if base := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(base) {
		base = filepath.Clean(base)
		if err := inspectRuntimeBase(base); err != nil {
			return "", err
		}
		dir = filepath.Join(base, "skot", instance)
	}
	address := filepath.Join(dir, "s")
	// Use Darwin's stricter limit on both platforms. Never silently fall back
	// from an explicit runtime path: other mount namespaces may not share /tmp.
	if len(address) >= 104 {
		return "", fmt.Errorf("session notification socket path is too long: %s", address)
	}
	return address, nil
}

// runtimeBase validates the published layout without consulting this process's
// environment. An empty result denotes the private /tmp fallback layout.
func runtimeBase(record registration) (string, error) {
	address := record.Address
	if !validInstance(record.Instance) || !filepath.IsAbs(address) || filepath.Clean(address) != address ||
		len(address) >= 104 || strings.IndexByte(address, 0) >= 0 {
		return "", errors.New("invalid notification registration identity or socket path")
	}
	if address == filepath.Join(fallbackRuntimeDirectory(record.Instance), "s") {
		return "", nil
	}
	base := filepath.Dir(filepath.Dir(filepath.Dir(address)))
	if address != filepath.Join(base, "skot", record.Instance, "s") {
		return "", errors.New("invalid notification registration identity or socket path")
	}
	return base, nil
}

func inspectRuntimeBase(base string) error {
	info, err := os.Lstat(base)
	if err != nil {
		return fmt.Errorf("inspect XDG_RUNTIME_DIR %q: %w", base, err)
	}
	if !info.IsDir() || !privateOwner(info) || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("XDG_RUNTIME_DIR must belong to this user and have mode 0700: %s", base)
	}
	return nil
}

func runtimeParent(record registration, create bool) error {
	base, err := runtimeBase(record)
	if err != nil || base == "" {
		return err
	}
	if err := inspectRuntimeBase(base); err != nil {
		return err
	}
	dir := filepath.Join(base, "skot")
	if create {
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return privateDirectory(dir)
}

func inspectRuntime(record registration) error {
	if err := runtimeParent(record, false); err != nil {
		return err
	}
	return privateDirectory(filepath.Dir(record.Address))
}

// Only the socket and empty instance directory of a validated registration are
// cleanup authority. The shared runtime root and neighboring files are retained.
func removeRuntime(record registration) error {
	if err := inspectRuntime(record); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	dir := filepath.Dir(record.Address)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil {
		return err
	}
	named, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !os.SameFile(opened, named) {
		return errors.New("notification runtime directory was replaced")
	}
	info, err := root.Lstat("s")
	if err == nil {
		if info.Mode()&os.ModeSocket == 0 || !ownedByCurrentUser(info) {
			return errors.New("notification socket was replaced")
		}
		err = root.Remove("s")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Remove(dir)
}
