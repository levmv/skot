//go:build darwin || linux

package privatefs

import (
	"os"
	"syscall"
)

func safeToRestrict(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Uid) != uint64(os.Geteuid()) {
		return false
	}
	// A regular file with another hard link may also be a user-managed file
	// outside the application directory. Do not change that inode implicitly.
	return !info.Mode().IsRegular() || stat.Nlink == 1
}
