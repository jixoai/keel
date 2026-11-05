package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/jixoai/fde2e-agent-runtime/internal/store"
)

// Errors returned by the uploader.
var (
	ErrTooLarge = errors.New("blob: upload too large")
)

// CASPath maps a sha256 digest to root/aa/bb/<hex>. Two directory levels keep
// any single directory under a few thousand entries.
func CASPath(root string, sha []byte) (string, error) {
	if len(sha) != sha256.Size {
		return "", fmt.Errorf("blob: bad sha256 length %d", len(sha))
	}
	h := hex.EncodeToString(sha)
	return filepath.Join(root, h[0:2], h[2:4], h), nil
}

// Uploader spools one upload to a temp file so memory stays constant, then
// either writes 64KiB chunks into PostgreSQL (small/normal images) or moves the
// file into the local CAS (large originals).
type Uploader struct {
	Store       *store.Store
	TempDir     string
	CASDir      string
	PGThreshold int64 // default 1MiB
	MaxBytes    int64 // default 64MiB
}

func (u *Uploader) applyDefaults() {
	if u.PGThreshold == 0 {
		u.PGThreshold = 1 << 20
	}
	if u.MaxBytes == 0 {
		u.MaxBytes = 64 << 20
	}
	if u.TempDir == "" {
		u.TempDir = os.TempDir()
	}
}

// Put consumes r (at most MaxBytes) and stores it for tenantID, returning the
// blob metadata. The digest is computed while spooling, so no second pass over
// the data is needed.
func (u *Uploader) Put(ctx context.Context, tenantID uuid.UUID, mime string, r io.Reader) (store.BlobMeta, error) {
	u.applyDefaults()
	tmp, err := os.CreateTemp(u.TempDir, "upload-*")
	if err != nil {
		return store.BlobMeta{}, fmt.Errorf("blob: temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	h := sha256.New()
	limited := io.LimitReader(r, u.MaxBytes+1)
	n, err := io.Copy(io.MultiWriter(tmp, h), limited)
	if err != nil {
		tmp.Close()
		return store.BlobMeta{}, fmt.Errorf("blob: spool: %w", err)
	}
	if n > u.MaxBytes {
		tmp.Close()
		return store.BlobMeta{}, ErrTooLarge
	}
	if err := tmp.Close(); err != nil {
		return store.BlobMeta{}, fmt.Errorf("blob: close temp: %w", err)
	}
	sum := h.Sum(nil)

	blobID := uuid.New()
	storage := store.StoragePGChunked
	if n > u.PGThreshold && u.CASDir != "" {
		storage = store.StorageLocalCAS
	}
	id, existing, err := u.Store.CreateBlob(ctx, tenantID, blobID, sum, n, mime, storage)
	if err != nil {
		return store.BlobMeta{}, err
	}
	if existing {
		return u.Store.BlobMetaOf(ctx, tenantID, id)
	}

	switch storage {
	case store.StorageLocalCAS:
		if err := u.moveIntoCAS(tmpPath, sum); err != nil {
			return store.BlobMeta{}, err
		}
	case store.StoragePGChunked:
		if err := u.writeChunks(ctx, tenantID, id, tmpPath); err != nil {
			return store.BlobMeta{}, err
		}
	}
	if err := u.Store.CompleteBlob(ctx, tenantID, id, int32((n+store.ChunkSize-1)/store.ChunkSize)); err != nil {
		return store.BlobMeta{}, err
	}
	return u.Store.BlobMetaOf(ctx, tenantID, id)
}

func (u *Uploader) writeChunks(ctx context.Context, tenantID, blobID uuid.UUID, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("blob: reopen temp: %w", err)
	}
	defer f.Close()
	buf := make([]byte, store.ChunkSize)
	var seq int32
	for {
		n, err := io.ReadFull(f, buf)
		if err == io.EOF {
			return nil
		}
		if err != nil && err != io.ErrUnexpectedEOF {
			return fmt.Errorf("blob: read temp: %w", err)
		}
		if n > 0 {
			if err := u.Store.AppendChunk(ctx, tenantID, blobID, seq, buf[:n]); err != nil {
				return err
			}
			seq++
		}
		if err == io.ErrUnexpectedEOF {
			return nil
		}
	}
}

func (u *Uploader) moveIntoCAS(tmpPath string, sum []byte) error {
	path, err := CASPath(u.CASDir, sum)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("blob: cas dir: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		return nil // another upload won the race; content is identical
	}
	if err := os.Rename(tmpPath, path); err != nil {
		// Cross-device rename: fall back to a copy.
		in, err2 := os.Open(tmpPath)
		if err2 != nil {
			return fmt.Errorf("blob: cas open: %w", err)
		}
		defer in.Close()
		out, err2 := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err2 != nil {
			if errors.Is(err2, os.ErrExist) {
				return nil
			}
			return fmt.Errorf("blob: cas create: %w", err2)
		}
		defer out.Close()
		if _, err2 := io.Copy(out, in); err2 != nil {
			return fmt.Errorf("blob: cas copy: %w", err2)
		}
		return nil
	}
	return nil
}
