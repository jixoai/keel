package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ChunkSize is the fixed size of a blob chunk row. 64KiB is the sweet spot from
// the PG benchmark: big enough that a 1MB image is 16 rows, small enough that a
// streaming read never materialises more than one chunk per row.
const ChunkSize = 64 << 10

// Storage kinds.
const (
	StoragePGChunked int16 = 1
	StorageLocalCAS  int16 = 2
)

// Lane classes: small responses get the fast lane, large ones are shaped.
const (
	LaneSmall int16 = 1
	LaneLarge int16 = 2
)

// BlobMeta is the metadata a stream needs before reading any bytes.
type BlobMeta struct {
	TenantID  uuid.UUID
	BlobID    uuid.UUID
	SHA256    []byte
	Size      int64
	Mime      string
	Storage   int16
	Lane      int16
	Complete  bool
	Chunks    int32
	RefCount  int32
	CreatedAt time.Time
	ExpiresAt *time.Time
	// Thumbnail lifecycle (keel-ga §5): none | pending | ready | failed.
	// ThumbBlobID is set only when ThumbStatus == ThumbReady.
	ThumbStatus string
	ThumbBlobID *uuid.UUID
}

// Thumbnail status values. `none` means "this blob is not an image the
// thumbnailer handles" (or it predates the column), not "queued": only
// `pending` is claimed by the generator.
const (
	ThumbNone    = "none"
	ThumbPending = "pending"
	ThumbReady   = "ready"
	ThumbFailed  = "failed"
)

// ErrNoBlob is returned when a blob does not exist for the tenant.
var ErrNoBlob = errors.New("store: blob not found")

// ErrIncompleteBlob is returned when reading a blob whose upload never finished.
var ErrIncompleteBlob = errors.New("store: blob incomplete")

// CreateBlob reserves metadata for a blob. When the same tenant already holds
// the same sha256, the existing blob_id is returned with existing=true and no
// new rows are written (content dedupe).
func (s *Store) CreateBlob(ctx context.Context, tenantID uuid.UUID, blobID uuid.UUID, sum []byte, size int64, mime string, storage int16) (uuid.UUID, bool, error) {
	lane := LaneSmall
	if size >= 1<<20 {
		lane = LaneLarge
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO blobs (tenant_id, blob_id, sha256, size, mime, storage_kind, lane_class)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (tenant_id, sha256) DO NOTHING`,
		tenantID, blobID, sum, size, mime, storage, lane)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("store: create blob: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return blobID, false, nil
	}
	var existing uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT blob_id FROM blobs WHERE tenant_id = $1 AND sha256 = $2`, tenantID, sum).Scan(&existing); err != nil {
		return uuid.Nil, false, fmt.Errorf("store: read dedupe blob: %w", err)
	}
	return existing, true, nil
}

// AppendChunk writes one chunk. Chunks must be written in order; a retry of the
// same seq is idempotent.
func (s *Store) AppendChunk(ctx context.Context, tenantID, blobID uuid.UUID, seq int32, data []byte) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO blob_chunks (tenant_id, blob_id, seq, bytes)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (tenant_id, blob_id, seq) DO NOTHING`,
		tenantID, blobID, seq, data)
	if err != nil {
		return fmt.Errorf("store: append chunk %d: %w", seq, err)
	}
	return nil
}

// CompleteBlob marks an upload finished and records the blob's byte usage
// (spec: tenancy-quotas / 计量事件: 图片字节、存储字节).
//
// Both facts are objective quantities of the stored object, so they are
// recorded at the moment the object becomes readable rather than when a request
// claimed to be sending one. The dedupe key is the blob id: a retried
// completion records nothing new, and a content-deduped upload (same sha256,
// same blob id) counts once.
//
// There is no price column anywhere in this path: prices live in contracts.
//
// Two statements in one transaction rather than one data-modifying CTE: a CTE
// that both updates `blobs` and inserts into `tenant_usage` can deadlock under
// concurrent uploads (the insert's unique-index probe and the update's row lock
// are taken in an order PostgreSQL does not fix for you — observed as a 40P01
// under parallel test packages). Updating first, then reading the row, gives a
// single deterministic lock order per blob.
func (s *Store) CompleteBlob(ctx context.Context, tenantID, blobID uuid.UUID, chunks int32) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: complete blob: %w", err)
	}
	defer tx.Rollback(ctx)

	var (
		size int64
		mime string
	)
	err = tx.QueryRow(ctx,
		`UPDATE blobs SET complete = true
		  WHERE tenant_id = $1 AND blob_id = $2
		  RETURNING size, mime`, tenantID, blobID).Scan(&size, &mime)
	if errors.Is(err, pgx.ErrNoRows) {
		// A blob that does not exist records nothing: metering follows the
		// object, never a request that claimed to finish one.
		return fmt.Errorf("store: complete blob: %w", ErrNoBlob)
	}
	if err != nil {
		return fmt.Errorf("store: complete blob: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO tenant_usage (tenant_id, kind, quantity, source, blob_id, dedupe_key)
		 VALUES ($1, 'storage_bytes', $2, 'upload', $3, $4)
		 ON CONFLICT (tenant_id, kind, dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING`,
		tenantID, size, blobID, blobID.String()); err != nil {
		return fmt.Errorf("store: record storage usage: %w", err)
	}
	if strings.HasPrefix(mime, "image/") {
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenant_usage (tenant_id, kind, quantity, source, blob_id, dedupe_key)
			 VALUES ($1, 'image_bytes', $2, 'upload', $3, $4)
			 ON CONFLICT (tenant_id, kind, dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING`,
			tenantID, size, blobID, blobID.String()); err != nil {
			return fmt.Errorf("store: record image usage: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: complete blob: %w", err)
	}
	return nil
}

// BlobMetaOf reads blob metadata.
func (s *Store) BlobMetaOf(ctx context.Context, tenantID, blobID uuid.UUID) (BlobMeta, error) {
	var m BlobMeta
	err := s.pool.QueryRow(ctx,
		`SELECT tenant_id, blob_id, sha256, size, mime, storage_kind, lane_class,
		        complete, ref_count, created_at, expires_at, thumb_status, thumb_blob_id,
		        COALESCE((SELECT max(seq) + 1 FROM blob_chunks c
		                  WHERE c.tenant_id = b.tenant_id AND c.blob_id = b.blob_id), 0)
		 FROM blobs b WHERE tenant_id = $1 AND blob_id = $2`, tenantID, blobID).
		Scan(&m.TenantID, &m.BlobID, &m.SHA256, &m.Size, &m.Mime, &m.Storage, &m.Lane,
			&m.Complete, &m.RefCount, &m.CreatedAt, &m.ExpiresAt, &m.ThumbStatus, &m.ThumbBlobID, &m.Chunks)
	if errors.Is(err, pgx.ErrNoRows) {
		return BlobMeta{}, ErrNoBlob
	}
	if err != nil {
		return BlobMeta{}, fmt.Errorf("store: blob meta: %w", err)
	}
	return m, nil
}

// ChunkBatch is one keyset-page read: the bytes of chunks [fromSeq, fromSeq+len).
type ChunkBatch struct {
	From int32
	Data []byte // concatenated chunk bytes
}

// ReadChunkBatch reads up to n chunks starting at fromSeq (inclusive) into dst
// (dst[:0] is used as the accumulator, so a pooled buffer can be reused). The
// bytes are copied out of pgx's buffers before the connection is released, so
// the caller may write them slowly without pinning a backend.
func (s *Store) ReadChunkBatch(ctx context.Context, tenantID, blobID uuid.UUID, fromSeq int32, n int, dst []byte) (ChunkBatch, error) {
	if n <= 0 || n > 16 {
		n = 1
	}
	rows, err := s.Pool().Query(ctx,
		`SELECT bytes FROM blob_chunks
		 WHERE tenant_id = $1 AND blob_id = $2 AND seq >= $3
		 ORDER BY seq LIMIT $4`, tenantID, blobID, fromSeq, n)
	if err != nil {
		return ChunkBatch{}, fmt.Errorf("store: read chunks: %w", err)
	}
	defer rows.Close()
	out := dst[:0]
	var got int32
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return ChunkBatch{}, fmt.Errorf("store: scan chunk: %w", err)
		}
		out = append(out, b...)
		got++
	}
	if err := rows.Err(); err != nil {
		return ChunkBatch{}, err
	}
	return ChunkBatch{From: fromSeq, Data: out}, nil
}

// ReadChunkAt reads a single chunk (for random access / small previews).
func (s *Store) ReadChunkAt(ctx context.Context, tenantID, blobID uuid.UUID, seq int32) ([]byte, error) {
	var b []byte
	err := s.Pool().QueryRow(ctx,
		`SELECT bytes FROM blob_chunks WHERE tenant_id = $1 AND blob_id = $2 AND seq = $3`,
		tenantID, blobID, seq).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoBlob
	}
	if err != nil {
		return nil, fmt.Errorf("store: read chunk: %w", err)
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

// HashReader wraps an io.Reader and computes sha256 as it is consumed. It is
// used by the upload path so the digest is known without a second pass.
type HashReader struct {
	R io.Reader
	H hash.Hash
	N int64
}

// NewHashReader returns a sha256-tracking reader.
func NewHashReader(r io.Reader) *HashReader {
	return &HashReader{R: r, H: sha256.New()}
}

func (h *HashReader) Read(p []byte) (int, error) {
	n, err := h.R.Read(p)
	if n > 0 {
		h.H.Write(p[:n])
		h.N += int64(n)
	}
	return n, err
}

// Sum returns the digest.
func (h *HashReader) Sum() []byte { return h.H.Sum(nil) }
