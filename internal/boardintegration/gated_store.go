package boardintegration

// Worker store gate (review P1-2). The board module's inbound/outbound workers
// lease rows from a syncstore.Store. Wrapping the store lets keel re-check the
// durable entry (enabled, same provider as this process, unchanged credential)
// immediately before every lease, including on a replica that did not receive
// the disable/rotation request. Already-leased work is stopped by the monitor's
// context cancellation and the provider gate.

import (
	"context"
	boardsync "github.com/jixoai/keel-board/sync"
	boardsyncstore "github.com/jixoai/keel-board/syncstore"
)

type gatedSyncStore struct {
	boardsyncstore.Store
	allowed func(ctx context.Context, key boardsync.Key) bool
}

func (g *gatedSyncStore) LeaseInbox(ctx context.Context, tenant string, key boardsync.Key, limit int, owner string) ([]boardsync.InboxItem, error) {
	if !g.allowed(ctx, key) {
		return nil, nil
	}
	return g.Store.LeaseInbox(ctx, tenant, key, limit, owner)
}

func (g *gatedSyncStore) LeaseOutbox(ctx context.Context, tenant string, keys []boardsync.Key, limit int, owner string) ([]boardsyncstore.Delivery, error) {
	kept := make([]boardsync.Key, 0, len(keys))
	for _, key := range keys {
		if g.allowed(ctx, key) {
			kept = append(kept, key)
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	return g.Store.LeaseOutbox(ctx, tenant, kept, limit, owner)
}

// The remaining Store methods are deliberately delegated unchanged: an ack
// must finish a lease this worker already owns, and management routes operate
// on their own synchronization. Only the two lease entry points are gated.

var _ boardsyncstore.Store = (*gatedSyncStore)(nil)
