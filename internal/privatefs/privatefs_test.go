package privatefs

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestOwnershipChecksAllowSharedPermissionsWithoutRepair(t *testing.T) {
	for _, mode := range []os.FileMode{0o770, 0o707} {
		for _, directory := range []bool{false, true} {
			path := filepath.Join(t.TempDir(), "state")
			if directory {
				if err := os.Mkdir(path, mode); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte("{}\n"), mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			inspect := InspectTrustedRegularFile
			if directory {
				inspect = InspectTrustedDirectory
			}
			if err := inspect(path, "state"); err != nil {
				t.Fatalf("mode %04o, directory=%v: %v", mode, directory, err)
			}
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != mode {
				t.Fatalf("inspection changed mode: %v, %v", info, err)
			}
		}
	}
}

func TestOwnershipTrustsCurrentUserAndRoot(t *testing.T) {
	path := t.TempDir()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Change only the stat snapshot: exercising another owner's metadata must
	// not require privileges or change ownership on the filesystem.
	stat := info.Sys().(*syscall.Stat_t)
	current := uint32(os.Geteuid())
	for _, test := range []struct {
		owner uint32
		allow bool
	}{
		{current, true},
		{0, true},
		{max(1, current+1), false},
	} {
		stat.Uid = test.owner
		err := inspectOwnership(info, path, "state directory")
		if (err == nil) != test.allow {
			t.Fatalf("owner %d: %v", test.owner, err)
		}
		if !test.allow && !strings.Contains(err.Error(), "owned by uid") {
			t.Fatalf("unhelpful ownership error: %v", err)
		}
	}
}

func TestStateAllowsAdministratorOwnedPaths(t *testing.T) {
	if err := InspectTrustedDirectory("/", "state directory"); err != nil {
		t.Fatalf("administrator-owned directory: %v", err)
	}
	if err := InspectTrustedRegularFile("/etc/passwd", "state file"); err != nil {
		t.Fatalf("administrator-owned file: %v", err)
	}
}

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
