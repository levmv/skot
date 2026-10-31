package sessionnotify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/levmv/skot/internal/canonicalpath"
	"github.com/levmv/skot/internal/filelock"
)

const ioTimeout = 2 * time.Second

type frame struct {
	Version  int      `json:"version"`
	Instance string   `json:"instance"`
	IDs      []string `json:"ids,omitempty"`
	Rescan   bool     `json:"rescan,omitempty"`
	Owned    []string `json:"owned,omitempty"`
}

var publishers = struct {
	sync.Mutex
	nodes map[string]*node
}{nodes: make(map[string]*node)}

type node struct {
	mu       sync.Mutex
	home     string
	dir      string
	record   registration
	file     *os.File
	listener *net.UnixListener
	refs     int // guarded by publishers.Mutex
	owned    map[string]*Lease
	clients  map[*subscriber]struct{}
	closed   bool
	wg       sync.WaitGroup
}

type subscriber struct {
	conn  *net.UnixConn
	queue *pending
}

// Lease keeps one process/home node alive across a managed operation. Errors
// are diagnostic only: notifications cannot roll back a successful journal IO.
type Lease struct {
	node *node
	once sync.Once
	id   string
}

func Begin(home string) *Lease {
	home = canonicalpath.Resolve(home)
	publishers.Lock()
	defer publishers.Unlock()
	n := publishers.nodes[home]
	if n == nil {
		var err error
		n, err = newNode(home)
		if err != nil {
			slog.Warn("session notifications unavailable", "home", home, "error", err)
			return nil
		}
		publishers.nodes[home] = n
	}
	n.refs++
	return &Lease{node: n}
}

func (lease *Lease) Own(id string) {
	if lease == nil {
		return
	}
	lease.id = id
	n := lease.node
	n.mu.Lock()
	defer n.mu.Unlock()
	n.owned[id] = lease
	n.publishLocked(id)
}

func (lease *Lease) Change(id string) {
	if lease == nil {
		return
	}
	n := lease.node
	n.mu.Lock()
	defer n.mu.Unlock()
	n.publishLocked(id)
}

func (n *node) publishLocked(id string) {
	for client := range n.clients {
		client.queue.add(Changes{IDs: []string{id}})
	}
}

func (lease *Lease) Close() {
	if lease == nil {
		return
	}
	lease.once.Do(func() {
		n := lease.node
		n.mu.Lock()
		if lease.id != "" {
			if n.owned[lease.id] == lease {
				delete(n.owned, lease.id)
			}
			n.publishLocked(lease.id)
		}
		n.mu.Unlock()
		publishers.Lock()
		defer publishers.Unlock()
		n.refs--
		if n.refs == 0 {
			delete(publishers.nodes, n.home)
			if err := n.close(); err != nil {
				slog.Warn("session notification shutdown failed", "home", n.home, "error", err)
			}
		}
	})
}

func newNode(home string) (_ *node, err error) {
	dir, err := registry(home)
	if err != nil {
		return nil, err
	}
	// Collect only on concrete node/discovery activity, never on a timer.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if registrationName(entry.Name()) {
			if _, _, err := inspectRegistration(dir, entry.Name()); err != nil {
				return nil, err
			}
		}
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(random[:])
	record := registration{Version: protocolVersion, Instance: id, Address: filepath.Join(runtimeDirectory(id), "s")}
	// Use the stricter Darwin limit on both supported platforms. TMPDIR and
	// home may exceed sockaddr_un's capacity, including in ordinary tests.
	if len(record.Address) >= 104 {
		return nil, errors.New("session notification socket path is too long")
	}
	guard, err := openPrivateFile(filepath.Join(dir, signalName), os.O_RDWR)
	if err != nil {
		return nil, err
	}
	defer guard.Close()
	deadline := time.Now().Add(ioTimeout)
	for {
		err := filelock.Acquire(guard)
		if err == nil {
			break
		}
		if !errors.Is(err, filelock.ErrWouldBlock) || time.Now().After(deadline) {
			return nil, fmt.Errorf("lock notification publication: %w", err)
		}
		_ = pause(context.Background(), 5*time.Millisecond)
	}
	path := filepath.Join(dir, "."+id+".tmp")
	file, err := openPrivateFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(path)
			_ = file.Close()
		}
	}()
	if err = filelock.Acquire(file); err != nil {
		return nil, err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if _, err = file.Write(data); err != nil {
		return nil, err
	}
	// Persist staging metadata before allocating runtime resources. A process
	// killed during socket setup leaves a registration the collector can own.
	if err := os.Mkdir(runtimeDirectory(id), 0o700); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(runtimeDirectory(id))
		}
	}()
	if err := privateDirectory(runtimeDirectory(id)); err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: record.Address, Net: "unix"})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = listener.Close()
		}
	}()
	if err = os.Rename(path, filepath.Join(dir, id+".json")); err != nil {
		return nil, err
	}
	n := &node{home: home, dir: dir, record: record, file: file, listener: listener,
		owned: make(map[string]*Lease), clients: make(map[*subscriber]struct{})}
	n.wg.Go(n.accept)
	return n, nil
}

func (n *node) snapshotLocked(changes Changes) frame {
	f := frame{Version: protocolVersion, Instance: n.record.Instance, IDs: changes.IDs, Rescan: changes.Rescan}
	for id := range n.owned {
		f.Owned = append(f.Owned, id)
	}
	slices.Sort(f.Owned)
	return f
}

func (n *node) accept() {
	for {
		conn, err := n.listener.AcceptUnix()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				slog.Warn("session notification listener failed", "error", err)
			}
			return
		}
		client := &subscriber{conn: conn, queue: newPending()}
		n.mu.Lock()
		if n.closed {
			n.mu.Unlock()
			_ = conn.Close()
			continue
		}
		// Registration and the initial ownership snapshot share the publication
		// mutex, so every later change is queued after this snapshot.
		n.clients[client] = struct{}{}
		initial := n.snapshotLocked(Changes{})
		initial.IDs = slices.Clone(initial.Owned)
		n.wg.Go(func() { n.serve(client, initial) })
		n.mu.Unlock()
	}
}

func (n *node) serve(client *subscriber, initial frame) {
	done := make(chan struct{})
	go func() {
		var unexpected [1]byte
		_, _ = client.conn.Read(unexpected[:])
		close(done)
	}()
	defer func() {
		_ = client.conn.Close()
		<-done
		n.mu.Lock()
		delete(n.clients, client)
		n.mu.Unlock()
	}()
	f := initial
	for {
		data, err := json.Marshal(f)
		if err != nil || len(data) >= maxFrameBytes {
			slog.Warn("session notification frame exceeds protocol limits", "error", err)
			return
		}
		_ = client.conn.SetWriteDeadline(time.Now().Add(ioTimeout))
		if _, err := client.conn.Write(append(data, '\n')); err != nil {
			return
		}
		for {
			select {
			case <-done:
				return
			case <-client.queue.wake:
			}
			n.mu.Lock()
			changes := client.queue.take()
			f = n.snapshotLocked(changes)
			n.mu.Unlock()
			if changes.Rescan || len(changes.IDs) != 0 {
				break
			}
		}
	}
}

func (n *node) close() error {
	// This write follows every journal close/cleanup, and precedes removal of
	// the registration. It survives coalesced discovery events on kqueue.
	err := signalCompletion(n.dir)
	n.mu.Lock()
	n.closed = true
	for client := range n.clients {
		_ = client.conn.Close()
	}
	n.mu.Unlock()
	err = errors.Join(err, n.listener.Close())
	n.wg.Wait()
	err = errors.Join(err, removeRuntime(n.record))
	err = errors.Join(err, os.Remove(filepath.Join(n.dir, n.record.Instance+".json")))
	err = errors.Join(err, n.file.Close())
	if err != nil {
		return fmt.Errorf("close session notification node: %w", err)
	}
	return nil
}
