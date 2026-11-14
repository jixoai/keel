package blob

// The image resolver's view of the blob store (keel-ga §3 provider-files).
//
// internal/provider defines the seam (provider.BlobSource) and knows nothing
// about chunks, CAS or PostgreSQL; this file supplies the implementation from the
// side that does. Three properties matter:
//
//   - Info reads metadata only: the resolver sizes and form-selects an image
//     before touching a byte.
//   - EncodeBase64 writes into a caller-sized buffer, so the inline path allocates
//     exactly one buffer of the encoded length (the resolver declares the cap).
//   - Open streams: an upload to a vendor never materialises the image, which is
//     what makes a 2GB Gemini file possible under a small memory ceiling.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// ImageSource adapts the store + streaming reader to provider.BlobSource. The
// workspace string is a tenant id (the blob tables are tenant-scoped).
type ImageSource struct {
	Store  *store.Store
	Reader *Reader
}

var _ provider.BlobSource = (*ImageSource)(nil)

// workspaceID parses the resolver's workspace scope. A malformed value is an
// error naming the field rather than a query against a zero tenant: a zero UUID
// would silently return "no such blob" for every image, hiding the wiring bug.
func workspaceID(workspace string) (uuid.UUID, error) {
	trimmed := strings.TrimSpace(workspace)
	if trimmed == "" {
		return uuid.Nil, errors.New("blob: image resolution needs a workspace (tenant) scope")
	}
	id, err := uuid.Parse(trimmed)
	if err != nil {
		return uuid.Nil, fmt.Errorf("blob: invalid workspace scope %q: %w", trimmed, err)
	}
	if id == uuid.Nil {
		return uuid.Nil, errors.New("blob: image resolution needs a non-zero workspace (tenant) scope")
	}
	return id, nil
}

// Info reads one blob's metadata for the workspace.
func (s *ImageSource) Info(ctx context.Context, workspace, blobID string) (provider.BlobInfo, error) {
	if s == nil || s.Store == nil {
		return provider.BlobInfo{}, errors.New("blob: image source is not configured")
	}
	tenant, id, err := resolveBlobID(workspace, blobID)
	if err != nil {
		return provider.BlobInfo{}, err
	}
	meta, err := s.Store.BlobMetaOf(ctx, tenant, id)
	if err != nil {
		return provider.BlobInfo{}, err
	}
	return provider.BlobInfo{
		BlobID: meta.BlobID.String(),
		SHA256: hexSHA256(meta.SHA256),
		Size:   meta.Size,
		Mime:   meta.Mime,
	}, nil
}

// Open streams the blob's raw bytes through a pipe: the reader's internal chunk
// buffer is reused between iterations, so the pipe (not a buffer of the whole
// image) is what the caller sees.
func (s *ImageSource) Open(ctx context.Context, workspace, blobID string) (io.ReadCloser, error) {
	if s == nil || s.Store == nil || s.Reader == nil {
		return nil, errors.New("blob: image source is not configured")
	}
	tenant, id, err := resolveBlobID(workspace, blobID)
	if err != nil {
		return nil, err
	}
	meta, err := s.Store.BlobMetaOf(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := s.Reader.StreamRaw(ctx, tenant, id, meta, pw, 0, 0)
		// CloseWithError(nil) is a clean EOF for the pipe reader.
		_ = pw.CloseWithError(err)
	}()
	return pr, nil
}

// EncodeBase64 fills dst with the blob's base64 form. dst must be exactly
// WireLen(size); a mismatch is a caller bug and is refused before any read, so a
// short buffer can never produce a truncated (silently corrupt) image.
func (s *ImageSource) EncodeBase64(ctx context.Context, workspace, blobID string, dst []byte) error {
	if s == nil || s.Store == nil || s.Reader == nil {
		return errors.New("blob: image source is not configured")
	}
	tenant, id, err := resolveBlobID(workspace, blobID)
	if err != nil {
		return err
	}
	meta, err := s.Store.BlobMetaOf(ctx, tenant, id)
	if err != nil {
		return err
	}
	want := WireLen(meta.Size)
	if int64(len(dst)) != want {
		return fmt.Errorf("blob: base64 buffer is %d bytes, blob %s needs %d", len(dst), id, want)
	}
	n, err := s.Reader.StreamBase64(ctx, tenant, id, meta, &sliceWriter{dst: dst}, 0)
	if err != nil {
		return err
	}
	if n != want {
		return fmt.Errorf("blob: base64 stream wrote %d of %d bytes for blob %s", n, want, id)
	}
	return nil
}

// resolveBlobID parses both halves of the reference.
func resolveBlobID(workspace, blobID string) (uuid.UUID, uuid.UUID, error) {
	tenant, err := workspaceID(workspace)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	id, err := uuid.Parse(strings.TrimSpace(blobID))
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("blob: invalid image blob id %q: %w", blobID, err)
	}
	return tenant, id, nil
}

// sliceWriter writes at successive offsets of a fixed buffer. blob.StreamBase64
// writes sequentially (chunk by chunk), so an offset cursor is exactly enough;
// an overflow is refused instead of allocating.
type sliceWriter struct {
	dst []byte
	off int
}

func (w *sliceWriter) Write(p []byte) (int, error) {
	if w.off+len(p) > len(w.dst) {
		return 0, fmt.Errorf("blob: base64 output overflows the %d-byte buffer", len(w.dst))
	}
	n := copy(w.dst[w.off:], p)
	w.off += n
	return n, nil
}

// hexSHA256 renders a stored digest for the cache key. A short/unreadable digest
// yields "" so the resolver falls back to the blob id in the key (two identical
// uploads are deduped by the store's own sha256 rule anyway).
func hexSHA256(sum []byte) string {
	if len(sum) != 32 {
		return ""
	}
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 64)
	for i, b := range sum {
		out[i*2] = hexDigits[b>>4]
		out[i*2+1] = hexDigits[b&0x0f]
	}
	return string(out)
}
