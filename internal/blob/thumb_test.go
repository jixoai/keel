package blob

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/store"
)

func syntheticImage(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x * 255 / maxInt(w-1, 1)),
				G: uint8(y * 255 / maxInt(h-1, 1)),
				B: uint8((x + y) % 256),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestThumbnailerGenerates320ForLargeImage is the spec's large-image scenario:
// a 4000px original stays downloadable byte-for-byte, and the generated
// thumbnail is ready, 320px on its long edge, and served from a normal blob row.
func TestThumbnailerGenerates320ForLargeImage(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, st)
	casDir := t.TempDir()
	reader := NewReader(st, 4, 1)
	generatorUploader := &Uploader{Store: st, CASDir: casDir, PGThreshold: 8 << 20}
	gen := NewThumbnailer(st, reader, generatorUploader, slog.New(slog.NewTextHandler(io.Discard, nil)))
	up := &Uploader{Store: st, CASDir: casDir, PGThreshold: 8 << 20, Thumbnails: gen}

	original := syntheticImage(t, 4000, 3000)
	meta, err := up.Put(ctx, tenant, "image/png", bytes.NewReader(original))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if meta.ThumbStatus != store.ThumbPending {
		t.Fatalf("thumb status after upload = %q, want pending", meta.ThumbStatus)
	}

	// Failure must not block the original: read it back before generating.
	var raw bytes.Buffer
	if _, err := reader.StreamRaw(ctx, tenant, meta.BlobID, meta, &raw, 0, 0); err != nil {
		t.Fatalf("read original: %v", err)
	}
	if !bytes.Equal(raw.Bytes(), original) {
		t.Fatal("original bytes changed")
	}

	job := store.ThumbJob{TenantID: tenant, BlobID: meta.BlobID, Mime: meta.Mime, Size: meta.Size}
	if err := gen.processOne(ctx, job); err != nil {
		t.Fatalf("generate: %v", err)
	}
	ready, err := st.BlobMetaOf(ctx, tenant, meta.BlobID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.ThumbStatus != store.ThumbReady || ready.ThumbBlobID == nil {
		t.Fatalf("after generation: status=%q thumb=%v, want ready", ready.ThumbStatus, ready.ThumbBlobID)
	}
	thumbMeta, err := st.BlobMetaOf(ctx, tenant, *ready.ThumbBlobID)
	if err != nil {
		t.Fatalf("thumbnail metadata: %v", err)
	}
	if !thumbMeta.Complete || thumbMeta.Mime != "image/png" || thumbMeta.RefCount != 0 {
		t.Fatalf("thumbnail row = %+v", thumbMeta)
	}
	if thumbMeta.ExpiresAt == nil {
		t.Fatal("thumbnail needs the orphan TTL safety net")
	}
	var thumbBytes bytes.Buffer
	if _, err := reader.StreamRaw(ctx, tenant, thumbMeta.BlobID, thumbMeta, &thumbBytes, 0, 0); err != nil {
		t.Fatalf("read thumbnail: %v", err)
	}
	thumb, format, err := image.Decode(&thumbBytes)
	if err != nil || format != "png" {
		t.Fatalf("decode thumbnail: format=%q err=%v", format, err)
	}
	bounds := thumb.Bounds()
	if bounds.Dx() != 320 || bounds.Dy() != 240 {
		t.Fatalf("thumbnail = %dx%d, want 320x240", bounds.Dx(), bounds.Dy())
	}

	// The original is still unchanged after generation.
	raw.Reset()
	if _, err := reader.StreamRaw(ctx, tenant, meta.BlobID, meta, &raw, 0, 0); err != nil {
		t.Fatalf("re-read original: %v", err)
	}
	if !bytes.Equal(raw.Bytes(), original) {
		t.Fatal("original bytes changed after thumbnail generation")
	}
}

// TestThumbnailerFailureDoesNotBlockAndNonImagesAreSkipped covers the contained
// failure path and the unsupported-MIME rule.
func TestThumbnailerFailureDoesNotBlockAndNonImagesAreSkipped(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, st)
	casDir := t.TempDir()
	reader := NewReader(st, 4, 1)
	gen := NewThumbnailer(st, reader, &Uploader{Store: st, CASDir: casDir}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	up := &Uploader{Store: st, CASDir: casDir, PGThreshold: 8 << 20, Thumbnails: gen}

	bad := []byte("this is not a PNG despite the content type")
	badMeta, err := up.Put(ctx, tenant, "image/png", bytes.NewReader(bad))
	if err != nil {
		t.Fatalf("bad image upload must succeed: %v", err)
	}
	if badMeta.ThumbStatus != store.ThumbPending {
		t.Fatalf("bad image status = %q, want pending", badMeta.ThumbStatus)
	}
	if err := gen.processOne(ctx, store.ThumbJob{TenantID: tenant, BlobID: badMeta.BlobID, Mime: badMeta.Mime, Size: badMeta.Size}); err == nil {
		t.Fatal("bad image should fail generation")
	}
	failed, err := st.BlobMetaOf(ctx, tenant, badMeta.BlobID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.ThumbStatus != store.ThumbFailed || failed.ThumbBlobID != nil {
		t.Fatalf("bad image after generation: status=%q thumb=%v", failed.ThumbStatus, failed.ThumbBlobID)
	}
	var raw bytes.Buffer
	if _, err := reader.StreamRaw(ctx, tenant, badMeta.BlobID, failed, &raw, 0, 0); err != nil || !bytes.Equal(raw.Bytes(), bad) {
		t.Fatalf("original after failed generation: len=%d err=%v", raw.Len(), err)
	}

	text, err := up.Put(ctx, tenant, "text/plain", bytes.NewReader([]byte("hello")))
	if err != nil {
		t.Fatal(err)
	}
	if text.ThumbStatus != store.ThumbNone {
		t.Fatalf("text blob thumb status = %q, want none", text.ThumbStatus)
	}
	jobs, err := st.PendingThumbnails(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.BlobID == text.BlobID {
			t.Fatal("non-image blob entered the thumbnail queue")
		}
	}
}

// TestThumbnailerSurvivesMissingSource makes a deleted source a failed (not a
// panicking) generation and proves SetThumbFailed is terminal.
func TestThumbnailerSurvivesMissingSource(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, st)
	casDir := t.TempDir()
	gen := NewThumbnailer(st, NewReader(st, 4, 1), &Uploader{Store: st, CASDir: casDir}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := gen.processOne(ctx, store.ThumbJob{TenantID: tenant, BlobID: uuid.New()})
	if err == nil || !errors.Is(err, store.ErrNoBlob) {
		t.Fatalf("missing source error = %v, want ErrNoBlob", err)
	}
}
