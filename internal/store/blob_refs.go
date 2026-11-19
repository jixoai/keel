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
//
// Concurrent sweep direction: the FK makes "reference first, then sweep"
// safe (the sweeper's FOR UPDATE cannot overtake a reference that already
// inserted). The opposite interleaving is a real race: the sweeper may hold the
// row lock and commit a delete while this INSERT is waiting on the FK's
// KEY SHARE check, which then fails with SQLSTATE 23503. That outcome is not a
// commit failure — the blob was already expired and unreferenced when the event
// chose it — so it is treated as "no reference recorded" (the event still
// commits, ref_count stays unchanged). The sweeper is the winner of the race;
// the event keeps its payload verbatim and remains a valid "unknown blob id"
// for every later reader.

import (
	"context"
	"errors"
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

// errBlobRefGone means the referenced blob row was deleted after the INSERT
// selected it (the FK's KEY SHARE check lost the race to a TTL sweep). It is a
// control-flow sentinel, never surfaced to callers.
var errBlobRefGone = errors.New("store: referenced blob disappeared")

func addRef(ctx context.Context, q execer, tenantID, blobID, sessionID uuid.UUID, eventSeq int64) error {
	if _, err := q.Exec(ctx, addRefSQL, tenantID, blobID, sessionID, eventSeq); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			// The FK check lost to a concurrent TTL sweep. The blob is already
			// gone, so there is nothing to reference; the event must not be
			// rejected for choosing an expired id. Same semantics as any other
			// unknown blob id: tolerate it and record zero refs.
			return errBlobRefGone
		}
		return fmt.Errorf("store: add blob ref: %w", err)
	}
	return nil
}

// AddRefTx records one event's reference to a blob inside the caller's
// transaction. It is the production path: the kernel calls it between the event
// INSERT and COMMIT, so the reference and the event are one fact. Existing,
// complete, same-tenant blobs are counted; anything else is ignored.
//
// A 23503 does not merely need to be ignored: in PostgreSQL an error aborts the
// whole transaction, so the event append would still fail at COMMIT. The work
// therefore runs inside a savepoint; a lost race rolls back to that savepoint
// (leaving the event transaction healthy) and reports success with zero refs.
func (s *Store) AddRefTx(ctx context.Context, tx pgx.Tx, tenantID, blobID, sessionID uuid.UUID, eventSeq int64) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: add blob ref savepoint: %w", err)
	}
	if err := addRef(ctx, sp, tenantID, blobID, sessionID, eventSeq); err != nil {
		// Roll back to the savepoint in both cases: on a sentinel this clears the
		// aborted subtransaction, on a real error it keeps the outer transaction
		// usable until the caller decides to fail it.
		if rbErr := sp.Rollback(ctx); rbErr != nil {
			return fmt.Errorf("%v (rollback savepoint: %w)", err, rbErr)
		}
		if errors.Is(err, errBlobRefGone) {
			return nil
		}
		return err
	}
	if err := sp.Commit(ctx); err != nil {
		return fmt.Errorf("store: release blob ref savepoint: %w", err)
	}
	return nil
}

// AddRef records a reference outside a surrounding transaction. It shares the
// admission SQL with AddRefTx; production uses AddRefTx (same-transaction) so a
// crash cannot commit an event without its refs. A lost FK race is a nil no-op:
// the implicit statement transaction is discarded, nothing to clean up.
func (s *Store) AddRef(ctx context.Context, tenantID, blobID, sessionID uuid.UUID, eventSeq int64) error {
	err := addRef(ctx, s.pool, tenantID, blobID, sessionID, eventSeq)
	if errors.Is(err, errBlobRefGone) {
		return nil
	}
	return err
}
