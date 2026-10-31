package app

import (
	"fmt"
	"os"
	"path/filepath"
)

// Permission bits alone do not establish whether the user trusts a shared
// group or a mount's permission mapping. Report them once during startup.
func homePermissionNotices(home string, interactive bool) []string {
	var notices []string
	names := []string{"", "config.json", "store.bin", "tools.json"}
	if interactive {
		names = append(names, "interactive.json")
	}
	for _, name := range names {
		path := home
		if name != "" {
			path = filepath.Join(home, name)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0o022 == 0 {
			continue
		}
		notices = append(notices, fmt.Sprintf(
			"Skot state at %q has group or other write permission; use a private location if it is shared with untrusted users", path,
		))
	}
	return notices
}
