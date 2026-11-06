package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/fde2e-keel/internal/store"
)

// testStore opens the store named by FDE2E_TEST_DSN, skipping when unset so a
// plain `go test ./...` works without a database.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("FDE2E_TEST_DSN")
	if dsn == "" {
		t.Skip("FDE2E_TEST_DSN not set")
	}
	s, err := store.Open(context.Background(), store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + 7)
	}
	return b
}

func TestUploadAndBase64Stream(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "test"); err != nil {
		t.Fatal(err)
	}
	up := &Uploader{Store: st, CASDir: t.TempDir(), PGThreshold: 8 << 20}
	data := pattern(1<<20 + 7) // crosses many chunk boundaries with a remainder
	meta, err := up.Put(ctx, tenant, "image/png", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", meta.Size, len(data))
	}
	if !meta.Complete {
		t.Fatal("blob not marked complete")
	}
	want := base64.StdEncoding.EncodeToString(data)

	r := NewReader(st, 4, 1)
	var out bytes.Buffer
	if _, err := r.StreamBase64(ctx, tenant, meta.BlobID, meta, &out, 0); err != nil {
		t.Fatal(err)
	}
	if out.String() != want {
		t.Fatalf("base64 mismatch: got %d bytes, want %d", out.Len(), len(want))
	}

	// Resume at a wire offset that is a multiple of 4 (3 source bytes).
	for _, off := range []int64{4, 4 * 333, 4 * 100000} {
		out.Reset()
		if _, err := r.StreamBase64(ctx, tenant, meta.BlobID, meta, &out, off); err != nil {
			t.Fatal(err)
		}
		if out.String() != want[off:] {
			t.Fatalf("resume at %d mismatch: got %d, want %d", off, out.Len(), len(want)-int(off))
		}
	}
}

func TestUploadDedupes(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "dedupe"); err != nil {
		t.Fatal(err)
	}
	up := &Uploader{Store: st, CASDir: t.TempDir(), PGThreshold: 8 << 20}
	data := pattern(1000)
	a, err := up.Put(ctx, tenant, "image/jpeg", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	b, err := up.Put(ctx, tenant, "image/jpeg", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if a.BlobID != b.BlobID {
		t.Fatalf("dedupe failed: %s != %s", a.BlobID, b.BlobID)
	}
	sum := sha256.Sum256(data)
	if !bytes.Equal(a.SHA256, sum[:]) {
		t.Fatal("sha256 mismatch")
	}
}

func TestStreamRawRange(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "raw"); err != nil {
		t.Fatal(err)
	}
	up := &Uploader{Store: st, CASDir: t.TempDir(), PGThreshold: 8 << 20}
	data := pattern(300000)
	meta, err := up.Put(ctx, tenant, "application/octet-stream", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	r := NewReader(st, 4, 1)
	var out bytes.Buffer
	if _, err := r.StreamRaw(ctx, tenant, meta.BlobID, meta, &out, 65530, 100); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data[65530:65630]) {
		t.Fatal("range mismatch across a chunk boundary")
	}
}

func TestCASPathLayout(t *testing.T) {
	sum := sha256.Sum256([]byte("x"))
	p, err := CASPath("/cas", sum[:])
	if err != nil {
		t.Fatal(err)
	}
	if len(p) < len("/cas/aa/bb/")+64 {
		t.Fatalf("unexpected path %q", p)
	}
}

func TestCASUploadAndBase64Stream(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "cas"); err != nil {
		t.Fatal(err)
	}
	casDir := t.TempDir()
	up := &Uploader{Store: st, CASDir: casDir, PGThreshold: 1 << 20}
	data := pattern(1<<20 + 3)
	meta, err := up.Put(ctx, tenant, "image/png", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Storage != store.StorageLocalCAS {
		t.Fatalf("storage = %d, want CAS", meta.Storage)
	}
	r := NewReader(st, 4, 1)
	r.CASDir = casDir
	var out bytes.Buffer
	if _, err := r.StreamBase64(ctx, tenant, meta.BlobID, meta, &out, 0); err != nil {
		t.Fatalf("stream: %v (storage=%d cas=%q size=%d)", err, meta.Storage, r.CASDir, meta.Size)
	}
	if want := base64.StdEncoding.EncodeToString(data); out.String() != want {
		t.Fatalf("mismatch: got %d want %d", out.Len(), len(want))
	}
}
