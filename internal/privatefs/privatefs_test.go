package privatefs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTryRestrictPermissionsLeavesHardLinkedFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(dir, "shared.json")); err != nil {
		t.Fatal(err)
	}

	TryRestrictPermissions(path)

	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("hard-linked file mode = %v, %v", info, err)
	}
}
