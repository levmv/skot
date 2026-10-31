// Package sessionnotify implements the local, versioned session invalidation
// protocol. Only managed sessions participate; journal contents never travel
// over the socket.
package sessionnotify

import (
	"slices"
	"strings"
	"sync"
)

const (
	protocolVersion = 1
	maxPending      = 256
	maxFrameBytes   = 1024 * 1024
)

type Changes struct {
	IDs    []string
	Rescan bool
}

// pending bounds memory independently of callback and socket throughput.
// Overflow invalidates the whole catalog instead of losing a particular ID.
type pending struct {
	mu     sync.Mutex
	ids    map[string]struct{}
	rescan bool
	wake   chan struct{}
}

func newPending() *pending { return &pending{wake: make(chan struct{}, 1)} }

func (p *pending) add(changes Changes) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if changes.Rescan {
		p.rescan, p.ids = true, nil
	}
	if !p.rescan {
		for _, id := range changes.IDs {
			if p.ids == nil {
				p.ids = make(map[string]struct{})
			}
			p.ids[id] = struct{}{}
			if len(p.ids) > maxPending {
				p.rescan, p.ids = true, nil
				break
			}
		}
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *pending) take() Changes {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := Changes{Rescan: p.rescan}
	for id := range p.ids {
		result.IDs = append(result.IDs, id)
	}
	slices.Sort(result.IDs)
	p.ids, p.rescan = nil, false
	return result
}

func validID(id string) bool {
	return len(id) == len("session_")+32 && strings.HasPrefix(id, "session_") && validInstance(strings.TrimPrefix(id, "session_"))
}

func validInstance(id string) bool {
	return len(id) == 32 && strings.Trim(id, "0123456789abcdef") == ""
}
