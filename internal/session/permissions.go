package session

import (
	"fmt"
	"os"
)

func ensurePrivateDirectory(path, label string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", label, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s must be a directory", label)
	}
	if permissions := info.Mode().Perm(); permissions&0o077 != 0 {
		return fmt.Errorf("%s permissions %04o grant access to group or other users; expected 0700 or stricter", label, permissions)
	}
	return nil
}
