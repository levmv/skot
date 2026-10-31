package sessionnotify

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRuntimeBase(t *testing.T) string {
	t.Helper()
	// Socket paths must fit on Darwin too, regardless of the test's TMPDIR.
	dir, err := os.MkdirTemp("/tmp", "skot-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestRuntimeAddressUsesFallbackForEmptyOrRelativeSetting(t *testing.T) {
	const instance = "0123456789abcdef0123456789abcdef"
	for _, setting := range []string{"", "relative/runtime"} {
		t.Setenv("XDG_RUNTIME_DIR", setting)
		address, err := runtimeAddress(instance)
		if want := filepath.Join(fallbackRuntimeDirectory(instance), "s"); err != nil || address != want {
			t.Fatalf("setting %q: address=%q, err=%v, want %s", setting, address, err, want)
		}
	}
}

func TestRuntimeAddressRejectsExplicitInvalidSetting(t *testing.T) {
	for _, setting := range []string{"missing", "shared", "long"} {
		t.Run(setting, func(t *testing.T) {
			base := testRuntimeBase(t)
			switch setting {
			case "missing":
				base = filepath.Join(base, "missing")
			case "shared":
				if err := os.Chmod(base, 0o770); err != nil {
					t.Fatal(err)
				}
			case "long":
				base = filepath.Join(base, strings.Repeat("long", 24))
				if err := os.Mkdir(base, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("XDG_RUNTIME_DIR", base)
			if _, err := runtimeAddress("0123456789abcdef0123456789abcdef"); err == nil {
				t.Fatal("invalid explicit runtime accepted or silently replaced by /tmp")
			}
		})
	}
}

func TestCollectionUsesPublishedRuntimeForStaging(t *testing.T) {
	base := testRuntimeBase(t)
	t.Setenv("XDG_RUNTIME_DIR", base)
	n, err := newNode(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer n.close()
	keep := filepath.Join(base, "skot", "keep")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := "." + n.record.Instance + ".tmp"
	if err := os.Rename(filepath.Join(n.dir, n.record.Instance+".json"), filepath.Join(n.dir, name)); err != nil {
		t.Fatal(err)
	}
	// A crash before publication leaves a complete staging record and socket,
	// without an owner. The collector may run with a different environment.
	_ = n.listener.Close()
	n.wg.Wait()
	_ = n.file.Close()
	t.Setenv("XDG_RUNTIME_DIR", testRuntimeBase(t))
	if _, live, err := inspectRegistration(n.dir, name); err != nil || live {
		t.Fatalf("collection: live=%v, %v", live, err)
	}
	for _, removed := range []string{filepath.Join(n.dir, name), filepath.Dir(n.record.Address)} {
		if _, err := os.Lstat(removed); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("runtime or registration remains at %s: %v", removed, err)
		}
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "keep" {
		t.Fatalf("collection changed neighboring state: %q, %v", data, err)
	}
}

func TestRuntimeCleanupPreservesReplacedSocket(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", testRuntimeBase(t))
	n, err := newNode(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.close() })
	if err := os.Remove(n.record.Address); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.record.Address, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := n.close(); err == nil {
		t.Fatal("cleanup accepted replacement")
	}
	if data, err := os.ReadFile(n.record.Address); err != nil || string(data) != "keep" {
		t.Fatalf("cleanup changed replacement: %q, %v", data, err)
	}
}
