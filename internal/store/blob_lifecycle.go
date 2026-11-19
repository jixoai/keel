package store

// Blob lifecycle persistence (change keel-ga §5 blob-lifecycle).
//
// Two derived-state machines live here, both driven from the durable tables:
//
//   - TTL reaping: ReleaseExpired claims unreferenced, expired, *complete*
//     blobs under row locks and deletes metadata -> chunks in one transaction,
//     returning the CAS identities the caller still has to unlink. The order is
//     the spec's: a crashed CAS unlink leaves an unreachable file (cleaned by the
//     orphan scan), never an object whose metadata claims bytes that are gone.
//   - Thumbnail lifecycle: none -> pending -> ready|failed. The generator only
//     claims pending rows, so a failed image is never retried in a storm; a
//     restarted process simply picks up whatever is still pending.
//
// Neither machine knows about tenants' policy: quota and retention policy decide
// *when* expires_at is set; these methods only enforce the invariant that a
// referenced or incomplete object is never removed.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ExpiredBlob is one reaped blob plus the CAS objects the sweeper must unlink.
// The row is already gone when this is returned; Thumb* describe the thumbnail
// blob that was removed from the same tenant in the same transaction.
type ExpiredBlob struct {
	TenantID uuid.UUID
	BlobID   uuid.UUID
	SHA256   []byte
	Storage  int16

	ThumbTenantID uuid.UUID
	ThumbBlobID   *uuid.UUID
	ThumbSHA256   []byte
	ThumbStorage  int16
}

// CASRef is one content-addressed object that may need unlinking. The same
// digest can back several blob rows (content dedupe), so callers must re-check
// CASBlobExists after metadata deletion before removing the file.
type CASRef struct {
	TenantID uuid.UUID
	SHA256   []byte
	Storage  int16
}

// CASRefs returns the parent and thumbnail CAS identities worth re-checking.
// Duplicates are left to the caller's per-sweep set: two references to the same
// digest are common (dedupe) and cheap to collapse.
func (e ExpiredBlob) CASRefs() []CASRef {
	out := []CASRef{{TenantID: e.TenantID, SHA256: e.SHA256, Storage: e.Storage}}
	if e.ThumbBlobID != nil && len(e.ThumbSHA256) > 0 {
		out = append(out, CASRef{TenantID: e.ThumbTenantID, SHA256: e.ThumbSHA256, Storage: e.ThumbStorage})
	}
	return out
}

// ReleaseExpired reaps up to limit unreferenced, expired, complete blobs.
//
// Selection and deletion happen in one transaction:
//
//  1. SELECT ... FOR UPDATE SKIP LOCKED takes the candidates (SKIP LOCKED lets
//     several processes sweep concurrently without waiting on each other);
//  2. blob_refs -> blobs -> blob_chunks are deleted in that order, so the
//     metadata disappears before the bytes it describes;
//  3. COMMIT. A failure at any point rolls the whole batch back, and the next
//     sweep sees the same candidates again.
//
// A blob whose row is still pointed at by another row's thumb_blob_id is not a
// candidate: that reference is what keeps a ready thumbnail alive. A blob still
// inside its upload (complete=false) or with ref_count>0 is never a candidate,
// no matter how old it is.
//
// The returned rows are the CAS unlink work; a second sweep finds no candidates
// and returns an empty slice, which is the idempotent-repeat property.
func (s *Store) ReleaseExpired(ctx context.Context, limit int) ([]ExpiredBlob, error) {
	if limit <= 0 {
		limit = 100
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: release expired: %w", err)
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx,
		`SELECT b.tenant_id, b.blob_id, b.sha256, b.storage_kind, b.thumb_blob_id
		 FROM blobs b
		 WHERE b.ref_count = 0
		   AND b.complete
		   AND b.expires_at IS NOT NULL
		   AND b.expires_at < now()
		   AND NOT EXISTS (
		       SELECT 1 FROM blobs p
		       WHERE p.tenant_id = b.tenant_id
		         AND p.thumb_blob_id = b.blob_id
		         AND p.blob_id <> b.blob_id)
		 ORDER BY b.expires_at
		 LIMIT $1
		 FOR UPDATE OF b SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: release expired: select: %w", err)
	}
	var victims []ExpiredBlob
	for rows.Next() {
		var v ExpiredBlob
		if err := rows.Scan(&v.TenantID, &v.BlobID, &v.SHA256, &v.Storage, &v.ThumbBlobID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: release expired: scan: %w", err)
		}
		victims = append(victims, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: release expired: %w", err)
	}
	if len(victims) == 0 {
		return nil, tx.Commit(ctx)
	}

	// Resolve thumbnail metadata before deleting either row: the CAS unlink
	// needs the digest and storage kind, and after the metadata delete they are
	// gone.
	for i := range victims {
		v := &victims[i]
		if v.ThumbBlobID == nil {
			continue
		}
		var (
			tenant uuid.UUID
			blobID uuid.UUID
			sum    []byte
			kind   int16
		)
		err := tx.QueryRow(ctx,
			`SELECT tenant_id, blob_id, sha256, storage_kind
			 FROM blobs WHERE tenant_id = $1 AND blob_id = $2`,
			v.TenantID, *v.ThumbBlobID).Scan(&tenant, &blobID, &sum, &kind)
		if errors.Is(err, pgx.ErrNoRows) {
			// The thumbnail row is already gone (a concurrent sweep or a failed
			// generation cleanup). The parent can still be reaped.
			v.ThumbBlobID = nil
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("store: release expired: thumbnail: %w", err)
		}
		v.ThumbTenantID = tenant
		v.ThumbSHA256 = sum
		v.ThumbStorage = kind
	}

	// Deduplicate the (tenant, blob) delete list: a thumbnail may itself be a
	// candidate in the same batch, or two parents may share one thumbnail id.
	type blobKey struct {
		tenant uuid.UUID
		blob   uuid.UUID
	}
	keys := make(map[blobKey]struct{}, len(victims)*2)
	for _, v := range victims {
		keys[blobKey{v.TenantID, v.BlobID}] = struct{}{}
		if v.ThumbBlobID != nil {
			keys[blobKey{v.ThumbTenantID, *v.ThumbBlobID}] = struct{}{}
		}
	}
	for k := range keys {
		for _, sql := range []string{
			`DELETE FROM blob_refs WHERE tenant_id = $1 AND blob_id = $2`,
			`DELETE FROM blobs WHERE tenant_id = $1 AND blob_id = $2`,
			`DELETE FROM blob_chunks WHERE tenant_id = $1 AND blob_id = $2`,
		} {
			if _, err := tx.Exec(ctx, sql, k.tenant, k.blob); err != nil {
				return nil, fmt.Errorf("store: release expired: delete: %w", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: release expired: commit: %w", err)
	}
	return victims, nil
}

// SetBlobExpiry sets (or clears, with nil) the expiry mark. Retention policy is
// the caller's decision; a nil mark means "never expires", which is what
// ordinary uploads carry until a policy says otherwise.
func (s *Store) SetBlobExpiry(ctx context.Context, tenantID, blobID uuid.UUID, at *time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE blobs SET expires_at = $3 WHERE tenant_id = $1 AND blob_id = $2`,
		tenantID, blobID, at)
	if err != nil {
		return false, fmt.Errorf("store: set blob expiry: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// CASBlobExists reports whether any blob row still names this content digest on
// the local CAS. The CAS directory is shared by every tenant (content dedupe is
// per tenant but the file is not), so the check deliberately has no tenant
// predicate: unlinking a digest another tenant still owns would break that
// tenant's object.
func (s *Store) CASBlobExists(ctx context.Context, sha []byte) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM blobs WHERE sha256 = $1 AND storage_kind = $2)`,
		sha, StorageLocalCAS).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: cas blob exists: %w", err)
	}
	return exists, nil
}

// ThumbJob is one queued thumbnail.
type ThumbJob struct {
	TenantID uuid.UUID
	BlobID   uuid.UUID
	Mime     string
	Size     int64
}

// MarkThumbPending queues an uploaded image for thumbnail generation. It is a
// no-op for non-images, for blobs whose thumbnail already exists (ready) or has
// permanently failed, and for an unfinished upload. queueing is idempotent.
func (s *Store) MarkThumbPending(ctx context.Context, tenantID, blobID uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE blobs
		 SET thumb_status = 'pending', thumb_updated_at = now()
		 WHERE tenant_id = $1 AND blob_id = $2
		   AND complete
		   AND thumb_status = 'none'
		   AND lower(split_part(mime, ';', 1)) IN ('image/jpeg', 'image/png', 'image/gif')`,
		tenantID, blobID)
	if err != nil {
		return false, fmt.Errorf("store: mark thumb pending: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// PendingThumbnails returns queued thumbnails in upload order. The partial index
// keeps this proportional to the queue. Multiple processes may see the same row
// (there is no claim mark): generation is deterministic and the ready update is
// conditional, so duplicate work is wasted CPU, never two different thumbs.
func (s *Store) PendingThumbnails(ctx context.Context, limit int) ([]ThumbJob, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, blob_id, mime, size
		 FROM blobs
		 WHERE thumb_status = 'pending'
		   AND complete
		   AND lower(split_part(mime, ';', 1)) IN ('image/jpeg', 'image/png', 'image/gif')
		 ORDER BY created_at
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: pending thumbnails: %w", err)
	}
	defer rows.Close()
	var out []ThumbJob
	for rows.Next() {
		var job ThumbJob
		if err := rows.Scan(&job.TenantID, &job.BlobID, &job.Mime, &job.Size); err != nil {
			return nil, fmt.Errorf("store: pending thumbnails: scan: %w", err)
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// SetThumbReady records the thumbnail object. It only applies while the source
// is still pending, so a late duplicate generator cannot overwrite a terminal
// state. false means the row moved on (deleted, failed, or already ready).
func (s *Store) SetThumbReady(ctx context.Context, tenantID, blobID, thumbBlobID uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE blobs
		 SET thumb_status = 'ready', thumb_blob_id = $3, thumb_updated_at = now()
		 WHERE tenant_id = $1 AND blob_id = $2 AND thumb_status = 'pending'`,
		tenantID, blobID, thumbBlobID)
	if err != nil {
		return false, fmt.Errorf("store: set thumb ready: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SetThumbFailed marks a permanent generation failure. It only applies while
// pending: failed is terminal by design, so the periodic scan cannot turn a bad
// image into a retry storm.
func (s *Store) SetThumbFailed(ctx context.Context, tenantID, blobID uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE blobs
		 SET thumb_status = 'failed', thumb_updated_at = now()
		 WHERE tenant_id = $1 AND blob_id = $2 AND thumb_status = 'pending'`,
		tenantID, blobID)
	if err != nil {
		return false, fmt.Errorf("store: set thumb failed: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
