package sessionnotify

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/levmv/skot/internal/filelock"
	"github.com/levmv/skot/internal/privatefs"
	"golang.org/x/sys/unix"
)

const signalName = "signal"

type registration struct {
	Version  int    `json:"version"`
	Instance string `json:"instance"`
	Address  string `json:"address"`
}

func registry(home string) (string, error) {
	dir := filepath.Join(home, "session-notifications")
	if err := privatefs.EnsureDirectory(dir, "session notifications directory"); err != nil {
		return "", err
	}
	if err := privateDirectory(dir); err != nil {
		return "", err
	}
	file, err := openPrivateFile(filepath.Join(dir, signalName), os.O_CREATE|os.O_RDWR)
	if err != nil {
		return "", err
	}
	return dir, file.Close()
}

func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || !privateOwner(info) {
		return fmt.Errorf("notification directory must belong to this user and have private permissions: %s", path)
	}
	return nil
}

func privateOwner(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Uid) == uint64(os.Geteuid()) && info.Mode().Perm()&0o077 == 0
}

func openPrivateFile(path string, flags int) (*os.File, error) {
	file, err := os.OpenFile(path, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && info.Sys().(*syscall.Stat_t).Nlink == 0 {
		// Another collector or the owner may unlink the registration after
		// Open. This descriptor no longer denotes a discoverable node.
		err = fmt.Errorf("notification file disappeared: %w", os.ErrNotExist)
	}
	if err == nil && (!info.Mode().IsRegular() || !privateOwner(info) || info.Sys().(*syscall.Stat_t).Nlink != 1) {
		err = fmt.Errorf("notification file must be private, regular, and have one link: %s", path)
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func runtimeDirectory(id string) string { return filepath.Join("/tmp", "skot-"+id) }

func readRegistration(file *os.File, name string) (registration, error) {
	var record registration
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return record, err
	}
	if len(data) > 4096 {
		return record, errors.New("notification registration is too large")
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, fmt.Errorf("decode notification registration: %w", err)
	}
	if !validInstance(record.Instance) || name != record.Instance+".json" || record.Address != filepath.Join(runtimeDirectory(record.Instance), "s") {
		return record, errors.New("invalid notification registration identity or socket path")
	}
	return record, nil
}

func sameFile(file *os.File, path string) error {
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(opened, current) {
		return fmt.Errorf("notification registration was replaced: %w", os.ErrNotExist)
	}
	return nil
}

// inspectRegistration collects only after acquiring ownership of this exact
// inode. The lock remains held until cleanup has finished. A failed connection
// never grants permission to remove a live process's registration.
func inspectRegistration(dir, name string) (registration, bool, error) {
	if !registrationName(name) {
		return registration{}, false, errors.New("invalid notification registration name")
	}
	staging := strings.HasSuffix(name, ".tmp")
	if staging {
		// Publishers hold the permanent signal inode's lock across staging
		// creation and rename. Collectors cannot unlink the staging file in
		// the interval between its creation and its own lock acquisition.
		guard, err := openPrivateFile(filepath.Join(dir, signalName), os.O_RDWR)
		if err != nil {
			return registration{}, false, err
		}
		defer guard.Close()
		if err := filelock.Acquire(guard); errors.Is(err, filelock.ErrWouldBlock) {
			return registration{}, true, nil
		} else if err != nil {
			return registration{}, false, err
		}
	}
	path := filepath.Join(dir, name)
	file, err := openPrivateFile(path, os.O_RDWR)
	if errors.Is(err, os.ErrNotExist) {
		return registration{}, false, nil
	}
	if err != nil {
		return registration{}, false, err
	}
	defer file.Close()
	lockErr := filelock.Acquire(file)
	if lockErr != nil && !errors.Is(lockErr, filelock.ErrWouldBlock) {
		return registration{}, false, lockErr
	}
	if err := sameFile(file, path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return registration{}, false, nil
		}
		return registration{}, false, err
	}
	identity := name
	if staging {
		identity = strings.TrimSuffix(strings.TrimPrefix(name, "."), ".tmp") + ".json"
	}
	record, readErr := readRegistration(file, identity)
	if lockErr == nil {
		if readErr != nil && staging {
			// Interrupted staging can leave empty or partial JSON. The checked
			// filename still identifies its runtime directory; never use an
			// address from the incomplete contents as cleanup authority.
			record = registration{Instance: strings.TrimSuffix(identity, ".json")}
		}
		if readErr == nil || staging {
			if err := removeRuntime(record); err != nil {
				return record, false, err
			}
		}
		// A collector can erase a crashed, short-lived node before another
		// watcher discovers it. Preserve the same completion signal as a
		// graceful shutdown before making its registration disappear.
		if err := signalCompletion(dir); err != nil {
			return record, false, err
		}
		return record, false, os.Remove(path)
	}
	if readErr != nil {
		return record, true, readErr
	}
	if record.Version != protocolVersion {
		return record, true, fmt.Errorf("unsupported session notification protocol %d", record.Version)
	}
	if err := privateDirectory(runtimeDirectory(record.Instance)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return record, true, err
	}
	return record, true, nil
}

func registrationName(name string) bool {
	return strings.HasSuffix(name, ".json") && validInstance(strings.TrimSuffix(name, ".json")) ||
		strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp") && validInstance(strings.TrimSuffix(strings.TrimPrefix(name, "."), ".tmp"))
}

// Never recursively remove an address supplied by a registration. Only the
// socket and empty private directory derived from its unguessable ID are ours.
func removeRuntime(record registration) error {
	dir := runtimeDirectory(record.Instance)
	if err := privateDirectory(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat("s")
	if err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("notification socket was replaced")
		}
		err = root.Remove("s")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Remove(dir)
}

func signalCompletion(dir string) error {
	file, err := openPrivateFile(filepath.Join(dir, signalName), os.O_WRONLY)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteAt([]byte{1}, 0)
	return err
}
