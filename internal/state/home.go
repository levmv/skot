package state

import "github.com/levmv/skot/internal/privatefs"

// Home may be an existing directory selected by the user. Validate its trust
// boundary without implicitly changing an existing directory's permissions.
func inspectHome(path string) error {
	return privatefs.InspectTrustedDirectory(path, "Skot home")
}

func ensureHome(path string) error {
	if err := privatefs.EnsureDirectory(path, "Skot home"); err != nil {
		return err
	}
	return inspectHome(path)
}
