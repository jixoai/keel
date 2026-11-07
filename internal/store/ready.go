// Readiness, queue-depth, and event-observer seams. Readiness is a deployment
// contract: the instance must only receive traffic when PostgreSQL is reachable
// and the schema it expects has been applied.

package store

import (
	"context"
	"fmt"
	"strings"
)

// EventObserver receives the kind of every successfully committed event. It is
// called after the transaction commits, from the appending goroutine. The
// observer must be cheap and must not block (metrics counters only).
type EventObserver func(kind string)

// SetObserver installs the event observer. It is expected at boot; nil disables
// notifications.
func (s *Store) SetObserver(o EventObserver) {
	if o == nil {
		s.observer.Store(nil)
		return
	}
	s.observer.Store(&o)
}

func (s *Store) notifyEvent(kind string) {
	if o := s.observer.Load(); o != nil {
		(*o)(kind)
	}
}

// requiredTables is the schema contract checked by CheckReady. It covers every
// table the HTTP/worker paths touch, including the migration-added columns and
// the handoff audit tables (internal/handoff schema 0001).
var requiredTables = []string{
	"tenants", "sessions", "events", "checkpoints", "submissions",
	"tasks", "blobs", "blob_chunks", "blob_refs", "runs",
	"usage_ledger", "mcp_oauth_pending", "mcp_oauth_credentials",
	"runtime_instances", "handoffs",
}

// CheckReady verifies that PostgreSQL is reachable and the runtime schema is
// present. It is bounded by ctx (the HTTP layer applies a short timeout).
func (s *Store) CheckReady(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("store: ready: ping: %w", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT t.name FROM unnest($1::text[]) AS t(name)
		 WHERE to_regclass('public.' || t.name) IS NULL`, requiredTables)
	if err != nil {
		return fmt.Errorf("store: ready: schema probe: %w", err)
	}
	defer rows.Close()
	var missing []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("store: ready: scan schema probe: %w", err)
		}
		missing = append(missing, name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: ready: schema probe: %w", err)
	}
	if len(missing) > 0 {
		return fmt.Errorf("store: ready: missing tables: %s", strings.Join(missing, ", "))
	}
	// Columns added by later migrations are the difference between "the first
	// schema was applied" and "this binary's schema was applied". The contract
	// covers every column cmd/runtime adds after store.Migrate:
	//   - kernel 0001: sessions.owner/owner_epoch/lease_until, runs.owner/owner_epoch;
	//   - kernel 0003: runs.loadout_id/loadout_revision;
	//   - kernel 0004: runs.pending_input, runs.read_watermark;
	//   - kernel 0005: runs.loadout_generation, runs.loadout_owner_epoch;
	//   - store base schema: the external-session marks on sessions.
	// A database missing any of them must not report ready (an edge/mirror read
	// would silently see pre-fencing, pre-lease or pre-external rows).
	var migrated bool
	if err := s.pool.QueryRow(ctx,
		`SELECT
		   EXISTS (SELECT 1 FROM information_schema.columns
		           WHERE table_schema = current_schema() AND table_name = 'events' AND column_name = 'dedupe_key')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'runs' AND column_name = 'status')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'tasks' AND column_name = 'status')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'sessions' AND column_name = 'owner')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'sessions' AND column_name = 'owner_epoch')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'sessions' AND column_name = 'lease_until')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'sessions' AND column_name = 'state_owner')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'sessions' AND column_name = 'external_backend')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'sessions' AND column_name = 'external_ref')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'sessions' AND column_name = 'external_status')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'sessions' AND column_name = 'external_recovery')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'sessions' AND column_name = 'external_updated_at')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'sessions' AND column_name = 'metering')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'runs' AND column_name = 'owner')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'runs' AND column_name = 'owner_epoch')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'runs' AND column_name = 'loadout_id')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'runs' AND column_name = 'loadout_revision')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'runs' AND column_name = 'pending_input')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'runs' AND column_name = 'read_watermark')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'runs' AND column_name = 'loadout_generation')
		   AND EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'runs' AND column_name = 'loadout_owner_epoch')`).
		Scan(&migrated); err != nil {
		return fmt.Errorf("store: ready: migration probe: %w", err)
	}
	if !migrated {
		return fmt.Errorf("store: ready: schema is older than this binary (run migrations)")
	}
	return nil
}

// CountPendingRuns reports the durable run queue depth. Pending means
// "claimable now"; a run whose lease expired is still running from the table's
// point of view until a worker reclaims it, and shows up under
// keel_leases_expired_total instead.
func (s *Store) CountPendingRuns(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM runs WHERE status = 'pending'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count pending runs: %w", err)
	}
	return n, nil
}
