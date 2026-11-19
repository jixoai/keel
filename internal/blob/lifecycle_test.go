package blob

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/store"
)

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("random bytes: %v", err)
	}
	return data
}

func newTenant(t *testing.T, st *store.Store) uuid.UUID {
	t.Helper()
	tenant := uuid.New()
	if err := st.CreateTenant(context.Background(), tenant, "blob-lifecycle"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	return tenant
}

func countChunks(t *testing.T, st *store.Store, tenant, blobID uuid.UUID) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM blob_chunks WHERE tenant_id = $1 AND blob_id = $2`,
		tenant, blobID).Scan(&n); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	return n
}

func expire(t *testing.T, st *store.Store, tenant, blobID uuid.UUID) {
	t.Helper()
	past := time.Now().Add(-time.Hour)
	ok, err := st.SetBlobExpiry(context.Background(), tenant, blobID, &past)
	if err != nil || !ok {
		t.Fatalf("expire blob: ok=%v err=%v", ok, err)
	}
}

// TestSweeperReapsExpiredBlobAndCAS is the spec's orphan-cleanup scenario with a
// real database and a real CAS file: metadata, chunks and the file all go, and a
// second pass is a harmless no-op.
func TestSweeperReapsExpiredBlobAndCAS(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, st)
	casDir := t.TempDir()
	up := &Uploader{Store: st, CASDir: casDir, PGThreshold: 1, MaxBytes: 8 << 20}
	data := randomBytes(t, 300_000)
	meta, err := up.Put(ctx, tenant, "image/png", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if meta.Storage != store.StorageLocalCAS {
		t.Fatalf("storage = %d, want local CAS", meta.Storage)
	}
	path, err := CASPath(casDir, meta.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("CAS file missing before sweep: %v", err)
	}
	expire(t, st, tenant, meta.BlobID)

	sw := &Sweeper{Store: st, CASDir: casDir, Batch: 50}
	if n, err := sw.Sweep(ctx); err != nil || n == 0 {
		t.Fatalf("sweep = (%d, %v), want at least this victim", n, err)
	}
	if _, err := st.BlobMetaOf(ctx, tenant, meta.BlobID); !errors.Is(err, store.ErrNoBlob) {
		t.Fatalf("metadata after sweep: %v, want ErrNoBlob", err)
	}
	if n := countChunks(t, st, tenant, meta.BlobID); n != 0 {
		t.Fatalf("chunks after sweep = %d, want 0", n)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("CAS file after sweep: %v, want not-exist", err)
	}

	// Idempotent repeat: no error, and the victim does not reappear.
	if _, err := sw.Sweep(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if _, err := st.BlobMetaOf(ctx, tenant, meta.BlobID); !errors.Is(err, store.ErrNoBlob) {
		t.Fatalf("metadata after second sweep: %v", err)
	}
}

// TestSweeperProtectsReferencedAndIncompleteBlobs covers the two MUST NOTs.
func TestSweeperProtectsReferencedAndIncompleteBlobs(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, st)
	casDir := t.TempDir()
	up := &Uploader{Store: st, CASDir: casDir, PGThreshold: 8 << 20}
	sw := &Sweeper{Store: st, CASDir: casDir, Batch: 50}

	referenced, err := up.Put(ctx, tenant, "image/png", bytes.NewReader(randomBytes(t, 200_000)))
	if err != nil {
		t.Fatal(err)
	}
	session := uuid.New()
	if err := st.AddRef(ctx, tenant, referenced.BlobID, session, 7); err != nil {
		t.Fatalf("add ref: %v", err)
	}
	expire(t, st, tenant, referenced.BlobID)
	if _, err := sw.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if meta, err := st.BlobMetaOf(ctx, tenant, referenced.BlobID); err != nil || meta.RefCount != 1 {
		t.Fatalf("referenced blob after sweep: meta=%+v err=%v", meta, err)
	}

	incompleteID := uuid.New()
	sum := make([]byte, 32)
	if _, err := rand.Read(sum); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateBlob(ctx, tenant, incompleteID, sum, 100, "image/png", store.StoragePGChunked); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendChunk(ctx, tenant, incompleteID, 0, randomBytes(t, 100)); err != nil {
		t.Fatal(err)
	}
	expire(t, st, tenant, incompleteID)
	if _, err := sw.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if meta, err := st.BlobMetaOf(ctx, tenant, incompleteID); err != nil || meta.Complete {
		t.Fatalf("incomplete blob after sweep: meta=%+v err=%v", meta, err)
	}
	if n := countChunks(t, st, tenant, incompleteID); n != 1 {
		t.Fatalf("incomplete blob chunks = %d, want 1 (never reap an unfinished upload)", n)
	}
}

// TestSweeperTenantIsolationAndContentSharing proves one tenant's reap does not
// delete another tenant's row, and a shared CAS digest survives until its last
// owner is gone.
func TestSweeperTenantIsolationAndContentSharing(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	casDir := t.TempDir()
	tenantA := newTenant(t, st)
	tenantB := newTenant(t, st)
	up := &Uploader{Store: st, CASDir: casDir, PGThreshold: 1, MaxBytes: 8 << 20}
	sw := &Sweeper{Store: st, CASDir: casDir, Batch: 50}

	data := randomBytes(t, 250_000)
	metaA, err := up.Put(ctx, tenantA, "image/png", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	metaB, err := up.Put(ctx, tenantB, "image/png", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if metaA.BlobID == metaB.BlobID || !bytes.Equal(metaA.SHA256, metaB.SHA256) {
		t.Fatal("fixture must be two tenant rows sharing one digest")
	}
	path, err := CASPath(casDir, metaA.SHA256)
	if err != nil {
		t.Fatal(err)
	}

	expire(t, st, tenantA, metaA.BlobID)
	if _, err := sw.Sweep(ctx); err != nil {
		t.Fatalf("sweep A: %v", err)
	}
	if _, err := st.BlobMetaOf(ctx, tenantA, metaA.BlobID); !errors.Is(err, store.ErrNoBlob) {
		t.Fatalf("tenant A blob after sweep: %v", err)
	}
	if _, err := st.BlobMetaOf(ctx, tenantB, metaB.BlobID); err != nil {
		t.Fatalf("tenant B blob was touched: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("shared CAS file removed while tenant B owns it: %v", err)
	}

	expire(t, st, tenantB, metaB.BlobID)
	if _, err := sw.Sweep(ctx); err != nil {
		t.Fatalf("sweep B: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("last owner's CAS file remains: %v", err)
	}
}

// TestSweeperRemovesThumbnailWithParent covers the internal reference: a ready
// thumbnail is not an independent TTL victim while its parent points at it, and
// it is deleted in the same transaction as the parent.
func TestSweeperRemovesThumbnailWithParent(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, st)
	casDir := t.TempDir()
	up := &Uploader{Store: st, CASDir: casDir, PGThreshold: 1, MaxBytes: 8 << 20}
	sw := &Sweeper{Store: st, CASDir: casDir, Batch: 50}

	parent, err := up.Put(ctx, tenant, "image/png", bytes.NewReader(randomBytes(t, 300_000)))
	if err != nil {
		t.Fatal(err)
	}
	thumb, err := up.Put(ctx, tenant, "image/png", bytes.NewReader(randomBytes(t, 20_000)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkThumbPending(ctx, tenant, parent.BlobID); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.SetThumbReady(ctx, tenant, parent.BlobID, thumb.BlobID); err != nil || !ok {
		t.Fatalf("set thumb ready: ok=%v err=%v", ok, err)
	}
	thumbPath, err := CASPath(casDir, thumb.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	parentPath, err := CASPath(casDir, parent.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	// Both rows look expired. The thumbnail's only blocker is the parent's
	// thumb_blob_id reference, and both must be reaped together.
	expire(t, st, tenant, thumb.BlobID)
	expire(t, st, tenant, parent.BlobID)

	if _, err := sw.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, id := range []uuid.UUID{parent.BlobID, thumb.BlobID} {
		if _, err := st.BlobMetaOf(ctx, tenant, id); !errors.Is(err, store.ErrNoBlob) {
			t.Fatalf("blob %s after sweep: %v", id, err)
		}
	}
	for _, path := range []string{parentPath, thumbPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("CAS file %s after sweep: %v", path, err)
		}
	}
}

// TestSweepOrphanCASIsScopedAndGraceful proves the crash-retry path touches only
// two-level hex CAS files, leaves fresh files alone, and never enters layerfs.
func TestSweepOrphanCASIsScopedAndGraceful(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	casDir := t.TempDir()
	sw := &Sweeper{Store: st, CASDir: casDir}

	old := time.Now().Add(-2 * time.Hour)
	orphan := func(name string) string {
		dir := filepath.Join(casDir, "ab", "cd")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	digestOld := make([]byte, 32)
	digestFresh := make([]byte, 32)
	if _, err := rand.Read(digestOld); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(digestFresh); err != nil {
		t.Fatal(err)
	}
	oldPath := orphan(hex.EncodeToString(digestOld))
	freshPath := orphan(hex.EncodeToString(digestFresh))
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}

	layerfsDir := filepath.Join(casDir, "layerfs", "objects")
	if err := os.MkdirAll(layerfsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	layerfsFile := filepath.Join(layerfsDir, "keep")
	if err := os.WriteFile(layerfsFile, []byte("layerfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(layerfsFile, old, old); err != nil {
		t.Fatal(err)
	}

	removed, err := sw.SweepOrphanCAS(ctx, time.Hour)
	if err != nil {
		t.Fatalf("orphan scan: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old orphan remains: %v", err)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Fatalf("fresh candidate must be outside the grace window: %v", err)
	}
	if _, err := os.Stat(layerfsFile); err != nil {
		t.Fatalf("layerfs file was touched: %v", err)
	}
}

// TestLifecycleLoopsStopOnContext proves both background loops are owned by the
// process context: cancel returns promptly instead of leaving a goroutine (and
// its ticker) behind on shutdown.
func TestLifecycleLoopsStopOnContext(t *testing.T) {
	st := testStore(t)
	casDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())

	sweeperDone := make(chan struct{})
	sweeper := &Sweeper{Store: st, CASDir: casDir, Interval: time.Hour, OrphanInterval: time.Hour}
	go func() { defer close(sweeperDone); sweeper.Run(ctx) }()

	generatorDone := make(chan struct{})
	generator := NewThumbnailer(st, NewReader(st, 2, 1), &Uploader{Store: st, CASDir: casDir}, nil)
	generator.Interval = time.Hour
	// The generator scans the shared queue at start. Cancel before launching it
	// so this test cannot race another package's pending fixture (the scan then
	// observes a cancelled context and returns without touching rows).
	genCtx, genCancel := context.WithCancel(context.Background())
	genCancel()
	go func() { defer close(generatorDone); generator.Run(genCtx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()
	for name, done := range map[string]chan struct{}{"sweeper": sweeperDone, "thumbnailer": generatorDone} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not stop on context cancel", name)
		}
	}
}
