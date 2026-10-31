package sessionnotify

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/levmv/skot/internal/filelock"
	"golang.org/x/sys/unix"
)

type connection struct {
	record registration
	conn   net.Conn
}

type disconnected struct {
	connection *connection
	owned      []string
	err        error
}

type observer struct {
	ctx     context.Context
	home    string
	dir     string
	queue   *pending
	refresh chan struct{}
	lost    chan disconnected
	errors  chan error
	wg      sync.WaitGroup
	active  map[string]*connection // only the event loop accesses connections
	mu      sync.Mutex
	owners  map[string][]string // readers publish ownership for crash recovery
}

// Watch installs discovery before enumerating nodes. IO workers keep draining
// sockets and filesystem events even while the caller is reading its snapshot.
func Watch(ctx context.Context, home string, onChange func(Changes)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onChange == nil {
		return errors.New("session change callback is required")
	}
	dir, err := registry(home)
	if err != nil {
		return err
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()
	if err := watcher.Add(dir); err != nil {
		return err
	}
	// Explicitly watch the permanent inode: a kqueue directory watch alone
	// cannot report a registration that is created and removed between reads.
	if err := watcher.Add(filepath.Join(dir, signalName)); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	o := &observer{ctx: ctx, home: home, dir: dir, queue: newPending(), refresh: make(chan struct{}, 1),
		lost: make(chan disconnected), errors: make(chan error, 1), active: make(map[string]*connection), owners: make(map[string][]string)}
	defer func() {
		cancel()
		for _, current := range o.active {
			_ = current.conn.Close()
		}
		o.wg.Wait()
	}()
	o.wg.Go(func() { o.filesystem(watcher) })
	if err := o.discover(); err != nil {
		return err
	}
	// The first callback always subsumes every change collected during setup.
	o.queue.add(Changes{Rescan: true})
	initial := o.queue.take()
	o.wg.Go(func() {
		onChange(initial)
		for {
			select {
			case <-ctx.Done():
				return
			case <-o.queue.wake:
				changes := o.queue.take()
				if changes.Rescan || len(changes.IDs) != 0 {
					onChange(changes)
				}
			}
		}
	})
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-o.errors:
			return err
		case <-o.refresh:
			if err := o.discover(); err != nil {
				return err
			}
		case lost := <-o.lost:
			if err := o.reconnect(lost); err != nil {
				return err
			}
		}
	}
}

func (o *observer) fail(err error) {
	select {
	case o.errors <- err:
	default:
	}
}

func (o *observer) requestDiscovery() {
	select {
	case o.refresh <- struct{}{}:
	default:
	}
}

func (o *observer) filesystem(watcher *fsnotify.Watcher) {
	for {
		select {
		case <-o.ctx.Done():
			return
		case err, ok := <-watcher.Errors:
			if !ok {
				o.fail(errors.New("session discovery stopped"))
				return
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				o.queue.add(Changes{Rescan: true})
				o.requestDiscovery()
			} else {
				o.fail(fmt.Errorf("session discovery: %w", err))
				return
			}
		case event, ok := <-watcher.Events:
			if !ok {
				o.fail(errors.New("session discovery stopped"))
				return
			}
			if event.Name == o.dir || event.Name == filepath.Join(o.dir, signalName) {
				if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
					o.fail(errors.New("session discovery directory or signal file was removed"))
					return
				}
				if event.Name == filepath.Join(o.dir, signalName) && event.Has(fsnotify.Write) {
					o.queue.add(Changes{Rescan: true})
					o.requestDiscovery()
				}
			}
			if registrationName(filepath.Base(event.Name)) {
				o.requestDiscovery()
			}
		}
	}
}

func (o *observer) discover() error {
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return fmt.Errorf("discover session notification nodes: %w", err)
	}
	for _, entry := range entries {
		if err := o.ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		if !registrationName(name) || o.active[strings.TrimSuffix(name, ".json")] != nil {
			continue
		}
		record, alive, err := inspectRegistration(o.dir, name)
		if err != nil {
			return err
		}
		if !alive {
			o.queue.add(Changes{Rescan: true})
			continue
		}
		if strings.HasSuffix(name, ".tmp") {
			continue
		}
		if err := o.connect(record); err != nil {
			// A node may shut down after the lock check but before dialing.
			// Recheck ownership before either collecting or reporting failure.
			if err := o.reconnect(disconnected{connection: &connection{record: record}, err: err}); err != nil {
				return err
			}
		}
	}
	return nil
}

func readFrame(scanner *bufio.Scanner, instance string) (frame, error) {
	var f frame
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return f, err
		}
		return f, errors.New("session notification connection closed")
	}
	if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
		return f, fmt.Errorf("decode session notification: %w", err)
	}
	if f.Version != protocolVersion || f.Instance != instance {
		return f, errors.New("session notification protocol or instance mismatch")
	}
	for _, ids := range [][]string{f.IDs, f.Owned} {
		for _, id := range ids {
			if !validID(id) {
				return f, errors.New("invalid managed ID in session notification")
			}
		}
	}
	return f, nil
}

func (o *observer) connect(record registration) error {
	ctx, cancel := context.WithTimeout(o.ctx, ioTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", record.Address)
	if err != nil {
		return err
	}
	// The deadline covers the initial snapshot, not an idle subscription.
	deadline, _ := ctx.Deadline()
	_ = conn.SetReadDeadline(deadline)
	stop := context.AfterFunc(o.ctx, func() { _ = conn.Close() })
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), maxFrameBytes)
	initial, err := readFrame(scanner, record.Instance)
	if err != nil {
		stop()
		_ = conn.Close()
		return err
	}
	_ = conn.SetReadDeadline(time.Time{})
	current := &connection{record: record, conn: conn}
	o.active[record.Instance] = current
	o.receive(record.Instance, initial)
	o.wg.Go(func() {
		defer stop()
		defer conn.Close()
		owned := initial.Owned
		for {
			f, err := readFrame(scanner, record.Instance)
			if err != nil {
				select {
				case o.lost <- disconnected{connection: current, owned: owned, err: err}:
				case <-o.ctx.Done():
				}
				return
			}
			owned = f.Owned
			o.receive(record.Instance, f)
		}
	})
	return nil
}

func (o *observer) receive(instance string, f frame) {
	o.mu.Lock()
	o.owners[instance] = slices.Clone(f.Owned)
	o.mu.Unlock()
	o.queue.add(Changes{IDs: f.IDs, Rescan: f.Rescan})
}

func (o *observer) reconnect(lost disconnected) error {
	id := lost.connection.record.Instance
	if current := o.active[id]; current != nil && current != lost.connection {
		return nil
	}
	delete(o.active, id)
	o.mu.Lock()
	delete(o.owners, id)
	o.mu.Unlock()
	// A broken stream has unknown history, even if reconnection succeeds.
	o.queue.add(Changes{Rescan: true})
	for attempt := range 4 {
		record, alive, err := inspectRegistration(o.dir, id+".json")
		if err != nil {
			return err
		}
		if !alive {
			if len(lost.owned) != 0 {
				o.wg.Go(func() { o.awaitRelease(id, lost.owned) })
			}
			return nil
		}
		if err := o.connect(record); err == nil {
			// The earlier Rescan may have completed before changes made during
			// the gap. The new ownership snapshot omits released sessions, so
			// reconcile again only after the replacement stream is installed.
			o.queue.add(Changes{Rescan: true})
			return nil
		} else {
			lost.err = err
		}
		if err := pause(o.ctx, time.Duration(attempt+1)*25*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("cannot restore session notifications from live node %s: %w", id, lost.err)
}

// Socket EOF can precede kernel release of journal descriptors. Probe only the
// affected ownership locks after that event, stopping on release or a new owner.
// There is no archive traversal and no recurring timer in a stable subscription.
func (o *observer) awaitRelease(previous string, ids []string) {
	remaining := slices.Clone(ids)
	deadline := time.Now().Add(ioTimeout)
	for len(remaining) != 0 {
		next := make([]string, 0, len(remaining))
		for _, id := range remaining {
			if o.newOwner(previous, id) {
				continue
			}
			locked, err := managedLocked(o.home, id)
			if err != nil {
				o.fail(err)
				return
			}
			if locked {
				next = append(next, id)
			}
		}
		o.queue.add(Changes{IDs: slices.Clone(remaining)})
		remaining = next
		if len(remaining) == 0 {
			return
		}
		if time.Now().After(deadline) {
			o.fail(fmt.Errorf("cannot confirm session ownership after node %s disconnected", previous))
			return
		}
		if err := pause(o.ctx, 20*time.Millisecond); err != nil {
			return
		}
	}
}

func (o *observer) newOwner(previous, id string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for instance, ids := range o.owners {
		if instance != previous && slices.Contains(ids, id) {
			return true
		}
	}
	return false
}

func managedLocked(home, id string) (bool, error) {
	root, err := os.OpenRoot(home)
	if err != nil {
		return false, err
	}
	defer root.Close()
	file, err := root.OpenFile(filepath.Join("sessions", id, "events.jsonl"), os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	return filelock.Locked(file)
}

func pause(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
