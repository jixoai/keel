package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// waitForLockWait blocks until a statement containing needle is waiting on a
// lock. It makes the interleaving below deterministic without sleeping on a
// guess: AddRef must have reached its FK check before the sweeper commits.
func waitForLockWait(t *testing.T, st *Store, needle string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting bool
		err := st.Pool().QueryRow(context.Background(),
			`SELECT EXISTS(
			   SELECT 1 FROM pg_stat_activity
			   WHERE datname = current_database()
			     AND wait_event_type = 'Lock'
			     AND query LIKE $1)`, "%"+needle+"%").Scan(&waiting)
		if err == nil && waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("statement %q never waited on a lock (err=%v)", needle, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAddRefToleratesConcurrentSweepDelete reproduces the P1 interleaving: the
// sweeper holds FOR UPDATE on the candidate, AddRef's INSERT is waiting on the
// FK's KEY SHARE check, the sweeper deletes and commits, and the FK check then
// raises 23503. AddRef must answer nil — the event that referenced the expired
// blob still commits with zero refs.
func TestAddRefToleratesConcurrentSweepDelete(t *testing.T) {
	if os.Getenv("KEEL_TEST_DSN") == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	st := readyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "blob-ref-race"); err != nil {
		t.Fatal(err)
	}
	blobID := uuid.New()
	sum := sha256.Sum256(blobID[:])
	if _, _, err := st.CreateBlob(ctx, tenant, blobID, sum[:], 64, "image/png", StoragePGChunked); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendChunk(ctx, tenant, blobID, 0, []byte("expired")); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteBlob(ctx, tenant, blobID, 1); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if ok, err := st.SetBlobExpiry(ctx, tenant, blobID, &past); err != nil || !ok {
		t.Fatalf("expire: ok=%v err=%v", ok, err)
	}

	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// The test intentionally holds the row while another statement waits. The
	// pool's 3s idle-in-transaction guard would kill this transaction under a
	// loaded full-suite run before the interleaving is observed.
	if _, err := tx.Exec(ctx, "SET LOCAL idle_in_transaction_session_timeout = 0"); err != nil {
		t.Fatalf("relax idle timeout: %v", err)
	}
	var locked uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT blob_id FROM blobs WHERE tenant_id = $1 AND blob_id = $2 FOR UPDATE`,
		tenant, blobID).Scan(&locked); err != nil {
		t.Fatalf("lock candidate: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- st.AddRef(ctx, tenant, blobID, uuid.New(), 1)
	}()
	waitForLockWait(t, st, "INSERT INTO blob_refs")

	// ReleaseExpired's transaction order: refs -> metadata -> chunks.
	for _, sql := range []string{
		`DELETE FROM blob_refs WHERE tenant_id = $1 AND blob_id = $2`,
		`DELETE FROM blobs WHERE tenant_id = $1 AND blob_id = $2`,
		`DELETE FROM blob_chunks WHERE tenant_id = $1 AND blob_id = $2`,
	} {
		if _, err := tx.Exec(ctx, sql, tenant, blobID); err != nil {
			t.Fatalf("sweeper delete: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("sweeper commit: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AddRef after concurrent sweep = %v, want nil (FK race is tolerated)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AddRef never returned after the sweeper committed")
	}

	var refs int
	if err := st.Pool().QueryRow(ctx,
		`SELECT count(*) FROM blob_refs WHERE tenant_id = $1 AND blob_id = $2`,
		tenant, blobID).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if refs != 0 {
		t.Fatalf("blob_refs rows = %d, want 0", refs)
	}
	if _, err := st.BlobMetaOf(ctx, tenant, blobID); !errors.Is(err, ErrNoBlob) {
		t.Fatalf("blob after sweep: %v, want ErrNoBlob", err)
	}

	// A later AddRef for the same (now missing) blob is still a nil no-op.
	if err := st.AddRef(ctx, tenant, blobID, uuid.New(), 2); err != nil {
		t.Fatalf("AddRef after delete = %v, want nil", err)
	}
}
