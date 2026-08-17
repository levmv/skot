package state

import "github.com/levmv/skot/internal/privatefs"

// Home may be an existing directory selected by the user. These helpers keep
// its structural checks shared without implicitly changing its permissions.
func inspectHome(path string) error {
	return privatefs.InspectDirectory(path, "Skot home")
}

func ensureHome(path string) error {
	return privatefs.EnsureDirectory(path, "Skot home")
}
