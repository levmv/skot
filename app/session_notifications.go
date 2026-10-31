package app

import (
	"context"

	"github.com/levmv/skot/internal/sessionnotify"
	"github.com/levmv/skot/internal/state"
)

// SessionChanges invalidates managed session snapshots. Changes may coalesce;
// they are not an execution log or a stream of token deltas.
type SessionChanges struct {
	// IDs contains full managed IDs whose state may have changed.
	IDs []string
	// Rescan asks the caller to reconcile the whole catalog, including removals.
	// When true, IDs can be ignored.
	Rescan bool
}

// WatchSessions observes managed sessions in a shared local Skot home until
// ctx is canceled or observation fails. An empty home uses Open's default.
// Callbacks run sequentially; the first requests Rescan after discovery is
// installed and existing live nodes have been connected. Changes continue to
// accumulate while callbacks read ListSessions or ReadSession.
//
// Notifications cover journal ownership, successful appends, release, and
// DeleteSession. Locked still means journal ownership, not generation or
// detached work. IDs can include empty drafts excluded by ListSessions; after
// a successful ReadSession, omit snapshots with an empty Summary.Title. Remove
// cached entries when ReadSession returns os.ErrNotExist.
// Old binaries and manual file edits do not publish changes.
// Processes must share access to the home and local Unix sockets. There is no
// background archive polling. Cancellation returns ctx.Err().
func WatchSessions(ctx context.Context, home string, onChange func(SessionChanges)) error {
	home, err := state.ResolveHome(home)
	if err != nil {
		return err
	}
	if onChange == nil {
		return sessionnotify.Watch(ctx, home, nil)
	}
	return sessionnotify.Watch(ctx, home, func(changes sessionnotify.Changes) {
		onChange(SessionChanges{IDs: changes.IDs, Rescan: changes.Rescan})
	})
}
