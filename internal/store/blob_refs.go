package store

// Blob reference projection (change keel-ga §5 blob-lifecycle).
//
// ref_count is derived from the event log: one row in blob_refs per
// (blob, session, event_seq) plus a counter on blobs. The insert selects from
// blobs, so the same statement enforces the three admission rules the spec
// names in one place:
//
//   - the blob must exist in the *same tenant* as the event (the predicate is
//     on the composite key; a foreign blob id inserts nothing);
//   - the upload must be complete (an in-flight object is not a reference);
//   - the (blob, session, event_seq) conflict is a no-op, so a replayed event
//     cannot increment twice.
//
// The UPDATE only touches rows the INSERT actually created, which makes the
// pair atomic without a second round trip and without a read-then-write race.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// execer is the statement surface shared by the pool and an open transaction.
type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

const addRefSQL = `
WITH ins AS (
    INSERT INTO blob_refs (tenant_id, blob_id, session_id, event_seq)
    SELECT $1, $2, $3, $4
    FROM blobs b
    WHERE b.tenant_id = $1 AND b.blob_id = $2 AND b.complete
    ON CONFLICT DO NOTHING
    RETURNING blob_id
)
UPDATE blobs
SET ref_count = ref_count + 1
WHERE tenant_id = $1 AND blob_id IN (SELECT blob_id FROM ins)`

func addRef(ctx context.Context, q execer, tenantID, blobID, sessionID uuid.UUID, eventSeq int64) error {
	if _, err := q.Exec(ctx, addRefSQL, tenantID, blobID, sessionID, eventSeq); err != nil {
		return fmt.Errorf("store: add blob ref: %w", err)
	}
	return nil
}

// AddRefTx records one event's reference to a blob inside the caller's
// transaction. It is the production path: the kernel calls it between the event
// INSERT and COMMIT, so the reference and the event are one fact. Existing,
// complete, same-tenant blobs are counted; anything else is ignored.
func (s *Store) AddRefTx(ctx context.Context, tx pgx.Tx, tenantID, blobID, sessionID uuid.UUID, eventSeq int64) error {
	return addRef(ctx, tx, tenantID, blobID, sessionID, eventSeq)
}

// AddRef records a reference outside a surrounding transaction. It shares the
// admission SQL with AddRefTx; production uses AddRefTx (same-transaction) so a
// crash cannot commit an event without its refs.
func (s *Store) AddRef(ctx context.Context, tenantID, blobID, sessionID uuid.UUID, eventSeq int64) error {
	return addRef(ctx, s.pool, tenantID, blobID, sessionID, eventSeq)
}
