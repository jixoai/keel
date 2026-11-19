package blob

import (
	"bytes"
	"context"
	"encoding/base64"
	"math/rand"
	"testing"

	"github.com/jixoai/keel/internal/store"
)

// TestStreamBase64OffsetsMatchFullSuffix is the resume contract: starting at
// any 4-multiple wire offset must produce exactly the suffix of the canonical
// base64 encoding. The cases cover 0, the 3-byte group boundary, chunk
// boundaries, the file end, and 1000 deterministic random offsets.
func TestStreamBase64OffsetsMatchFullSuffix(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, st)
	up := &Uploader{Store: st, CASDir: t.TempDir(), PGThreshold: 8 << 20}
	reader := NewReader(st, 4, 1)

	sizes := []int{
		0, 1, 2, 3, 4, 5,
		store.ChunkSize - 1,
		store.ChunkSize,
		store.ChunkSize + 1,
		2*store.ChunkSize + 2,
		200_000,
	}
	for _, size := range sizes {
		data := randomBytes(t, size)
		meta, err := up.Put(ctx, tenant, "application/octet-stream", bytes.NewReader(data))
		if err != nil {
			t.Fatalf("size %d: upload: %v", size, err)
		}
		full := base64.StdEncoding.EncodeToString(data)
		total := WireLen(int64(size))
		if int64(len(full)) != total {
			t.Fatalf("size %d: full length %d, WireLen %d", size, len(full), total)
		}

		offsets := map[int64]struct{}{0: {}}
		sourceOffsets := []int64{
			0, 3, 6,
			store.ChunkSize - 3, store.ChunkSize, store.ChunkSize + 3,
			2 * store.ChunkSize,
			int64(size) - int64(size)%3,
			int64(size),
		}
		for _, src := range sourceOffsets {
			if src < 0 || src%3 != 0 {
				continue
			}
			off := src / 3 * 4
			if off >= 0 && off <= total {
				offsets[off] = struct{}{}
			}
		}
		if total >= 4 {
			offsets[total-4] = struct{}{}
		}
		offsets[total] = struct{}{}

		for off := range offsets {
			var got bytes.Buffer
			n, err := reader.StreamBase64(ctx, tenant, meta.BlobID, meta, &got, off)
			if err != nil {
				t.Fatalf("size %d offset %d: %v", size, off, err)
			}
			want := full[off:]
			if got.String() != want {
				t.Fatalf("size %d offset %d: got %d bytes, want %d", size, off, got.Len(), len(want))
			}
			if n != int64(len(want)) {
				t.Fatalf("size %d offset %d: n=%d want %d", size, off, n, len(want))
			}
		}
	}

	// 1000 deterministic 4-multiple offsets against a body that crosses two
	// chunk boundaries. The blob lives in the local CAS so the property runs
	// against file offsets too (and stays cheap); the table above already covers
	// the pg_chunked branch at every boundary.
	casDir := t.TempDir()
	casUp := &Uploader{Store: st, CASDir: casDir, PGThreshold: 1}
	data := randomBytes(t, 200_003)
	meta, err := casUp.Put(ctx, tenant, "application/octet-stream", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("random-offset body: %v", err)
	}
	if meta.Storage != store.StorageLocalCAS {
		t.Fatalf("random-offset body storage = %d, want local CAS", meta.Storage)
	}
	reader.CASDir = casDir
	full := base64.StdEncoding.EncodeToString(data)
	total := WireLen(int64(len(data)))
	rng := rand.New(rand.NewSource(20261120))
	for i := 0; i < 1000; i++ {
		groups := total / 4
		off := rng.Int63n(groups+1) * 4
		var got bytes.Buffer
		if _, err := reader.StreamBase64(ctx, tenant, meta.BlobID, meta, &got, off); err != nil {
			t.Fatalf("random offset %d: %v", off, err)
		}
		if got.String() != full[off:] {
			t.Fatalf("random offset %d mismatch (%d vs %d bytes)", off, got.Len(), len(full[off:]))
		}
	}

	// Invalid offsets are refused before reading: not a group boundary, or past
	// the end.
	if _, err := reader.StreamBase64(ctx, tenant, meta.BlobID, meta, &bytes.Buffer{}, 1); err == nil {
		t.Fatal("offset 1 must be refused (not a multiple of 4)")
	}
	if _, err := reader.StreamBase64(ctx, tenant, meta.BlobID, meta, &bytes.Buffer{}, total+4); err == nil {
		t.Fatal("offset past the wire end must be refused")
	}
}

// FuzzBase64GroupBoundary states the arithmetic property the resume path relies
// on: every wire offset is a group boundary, so it maps to a source offset that
// is a multiple of 3 (and therefore the suffix is self-contained).
func FuzzBase64GroupBoundary(f *testing.F) {
	for _, size := range []int64{0, 1, 2, 3, 4, 5, 6, 100, 65_536 + 2} {
		f.Add(size)
	}
	f.Fuzz(func(t *testing.T, size int64) {
		if size < 0 || size > 4<<20 {
			return
		}
		total := WireLen(size)
		if total < 0 || total%4 != 0 {
			t.Fatalf("WireLen(%d) = %d is not a non-negative multiple of 4", size, total)
		}
		// total/4*3 is the smallest source offset whose encoding starts at the
		// wire end; it must be >= size (the suffix at the end is empty).
		if src := total / 4 * 3; src < size {
			t.Fatalf("WireLen(%d) ends inside the source at %d", size, src)
		}
	})
}
