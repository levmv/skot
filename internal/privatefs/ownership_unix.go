//go:build darwin || linux

package privatefs

import (
	"fmt"
	"os"
	"syscall"
)

func inspectOwnership(info os.FileInfo, path, label string) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot inspect owner of %s %q", label, path)
	}
	// Administrator-provisioned configuration is trusted too: root already
	// controls the current user's state, and such files need not be writable.
	if stat.Uid != 0 && uint64(stat.Uid) != uint64(os.Geteuid()) {
		return fmt.Errorf("%s %q is owned by uid %d; expected the current user or root", label, path, stat.Uid)
	}
	return nil
}

func safeToRestrict(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Uid) != uint64(os.Geteuid()) {
		return false
	}
	// A regular file with another hard link may also be a user-managed file
	// outside the application directory. Do not change that inode implicitly.
	return !info.Mode().IsRegular() || stat.Nlink == 1
}
