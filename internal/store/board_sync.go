package store

// Board tenant configuration and blob translation (keel-board-v1, Phase 1).
//
// The board module owns the board domain and its own sync tables. This file
// owns only the keel-side configuration the board mount assembles from:
//
//   - board_tenant_sync_entries: one row per platform connection. secret_ref
//     is a role -> tenant-secret NAME map; no credential value is ever stored
//     here, and the list/read projections never decrypt.
//   - board_tenant_blob_keys: the board's key-addressed blob.Store translated
//     onto keel's (tenant, sha256)-addressed blob storage. The bytes live in
//     keel's own blobs/blob_chunks tables.
//
// Both tables are tenant rows and are registered in rlsTenantTables.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrBoardSyncEntryExists means a create named a key that already exists for
// the tenant. A create is never an implicit update.
var ErrBoardSyncEntryExists = errors.New("store: board sync entry already exists")

// ErrBoardSyncEntryMissing means an update named a key the tenant does not
// have.
var ErrBoardSyncEntryMissing = errors.New("store: board sync entry not found")

// ErrBoardSyncEntryConflict means an update's expected revision is stale. The
// caller re-reads and retries; a concurrent writer won.
var ErrBoardSyncEntryConflict = errors.New("store: board sync entry revision conflict")

// BoardSyncEntryRecord is one tenant's configured platform connection.
// SecretRef values are tenant secret names (the same grammar the settings
// surface stores), never values. Settings is opaque JSON: the board mount
// validates the platform vocabulary, so adding a setting never needs a
// migration here.
type BoardSyncEntryRecord struct {
	TenantID      uuid.UUID
	Key           string
	Platform      string
	SecretRef     map[string]string
	Settings      json.RawMessage
	Routes        map[string]string
	StartChannels []string
	Enabled       bool
	Revision      int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

const boardSyncEntryColumns = `tenant_id, key, platform, secret_ref, settings, routes,
	start_channels, enabled, revision, created_at, updated_at`

// ListBoardSyncEntries returns a tenant's entries in key order. A tenant with
// none is an empty slice, not an error.
func (s *Store) ListBoardSyncEntries(ctx context.Context, tenantID uuid.UUID) ([]BoardSyncEntryRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+boardSyncEntryColumns+` FROM board_tenant_sync_entries
		 WHERE tenant_id = $1 ORDER BY key`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list board sync entries: %w", err)
	}
	defer rows.Close()
	out := make([]BoardSyncEntryRecord, 0, 8)
	for rows.Next() {
		record, err := scanBoardSyncEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// GetBoardSyncEntry reads one entry. found=false is a missing key.
func (s *Store) GetBoardSyncEntry(ctx context.Context, tenantID uuid.UUID, key string) (BoardSyncEntryRecord, bool, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+boardSyncEntryColumns+` FROM board_tenant_sync_entries
		 WHERE tenant_id = $1 AND key = $2`, tenantID, key)
	record, err := scanBoardSyncEntry(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return BoardSyncEntryRecord{}, false, nil
	}
	if err != nil {
		return BoardSyncEntryRecord{}, false, err
	}
	return record, true, nil
}

// CreateBoardSyncEntry inserts a new entry. The stored revision starts at 1;
// an existing key is ErrBoardSyncEntryExists, never an overwrite.
func (s *Store) CreateBoardSyncEntry(ctx context.Context, record BoardSyncEntryRecord) (BoardSyncEntryRecord, error) {
	secretRef, err := json.Marshal(nonNilStringMap(record.SecretRef))
	if err != nil {
		return BoardSyncEntryRecord{}, fmt.Errorf("store: board sync secret ref: %w", err)
	}
	settings := record.Settings
	if len(settings) == 0 {
		settings = json.RawMessage("{}")
	}
	if !json.Valid(settings) {
		return BoardSyncEntryRecord{}, errors.New("store: board sync settings is not valid JSON")
	}
	routes, err := json.Marshal(nonNilStringMap(record.Routes))
	if err != nil {
		return BoardSyncEntryRecord{}, fmt.Errorf("store: board sync routes: %w", err)
	}
	channels := record.StartChannels
	if channels == nil {
		channels = []string{}
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO board_tenant_sync_entries
		 (tenant_id, key, platform, secret_ref, settings, routes, start_channels, enabled)
		 VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6::jsonb, $7, $8)
		 ON CONFLICT (tenant_id, key) DO NOTHING
		 RETURNING `+boardSyncEntryColumns,
		record.TenantID, record.Key, record.Platform, secretRef, settings, routes, channels, record.Enabled)
	created, err := scanBoardSyncEntry(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return BoardSyncEntryRecord{}, ErrBoardSyncEntryExists
	}
	if err != nil {
		return BoardSyncEntryRecord{}, err
	}
	return created, nil
}

// UpdateBoardSyncEntry replaces an entry's mutable fields when its revision
// still matches expectedRevision. A stale revision is ErrBoardSyncEntryConflict
// (a missing key is ErrBoardSyncEntryMissing), so two operators cannot silently
// overwrite each other.
func (s *Store) UpdateBoardSyncEntry(ctx context.Context, record BoardSyncEntryRecord, expectedRevision int64) (BoardSyncEntryRecord, error) {
	secretRef, err := json.Marshal(nonNilStringMap(record.SecretRef))
	if err != nil {
		return BoardSyncEntryRecord{}, fmt.Errorf("store: board sync secret ref: %w", err)
	}
	settings := record.Settings
	if len(settings) == 0 {
		settings = json.RawMessage("{}")
	}
	if !json.Valid(settings) {
		return BoardSyncEntryRecord{}, errors.New("store: board sync settings is not valid JSON")
	}
	routes, err := json.Marshal(nonNilStringMap(record.Routes))
	if err != nil {
		return BoardSyncEntryRecord{}, fmt.Errorf("store: board sync routes: %w", err)
	}
	channels := record.StartChannels
	if channels == nil {
		channels = []string{}
	}
	row := s.pool.QueryRow(ctx,
		`UPDATE board_tenant_sync_entries
		    SET platform = $3, secret_ref = $4::jsonb, settings = $5::jsonb,
		        routes = $6::jsonb, start_channels = $7, enabled = $8,
		        revision = revision + 1, updated_at = now()
		  WHERE tenant_id = $1 AND key = $2 AND revision = $9
		 RETURNING `+boardSyncEntryColumns,
		record.TenantID, record.Key, record.Platform, secretRef, settings, routes,
		channels, record.Enabled, expectedRevision)
	updated, err := scanBoardSyncEntry(row)
	if errors.Is(err, pgx.ErrNoRows) {
		exists := false
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM board_tenant_sync_entries WHERE tenant_id = $1 AND key = $2)`,
			record.TenantID, record.Key).Scan(&exists); err != nil {
			return BoardSyncEntryRecord{}, fmt.Errorf("store: board sync entry exists: %w", err)
		}
		if !exists {
			return BoardSyncEntryRecord{}, ErrBoardSyncEntryMissing
		}
		return BoardSyncEntryRecord{}, ErrBoardSyncEntryConflict
	}
	if err != nil {
		return BoardSyncEntryRecord{}, err
	}
	return updated, nil
}

// DeleteBoardSyncEntry removes one configuration row. It is the rollback path
// when the board's local entry row could not be written; a missing row is not
// an error.
func (s *Store) DeleteBoardSyncEntry(ctx context.Context, tenantID uuid.UUID, key string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM board_tenant_sync_entries WHERE tenant_id = $1 AND key = $2`,
		tenantID, key)
	if err != nil {
		return false, fmt.Errorf("store: delete board sync entry: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetBoardSyncEntryEnabled flips only the enabled flag, leaving the credential
// and routing document untouched. It is the immediate-effect path a PATCH uses
// for a plain enable/disable.
func (s *Store) SetBoardSyncEntryEnabled(ctx context.Context, tenantID uuid.UUID, key string, enabled bool) (BoardSyncEntryRecord, error) {
	row := s.pool.QueryRow(ctx,
		`UPDATE board_tenant_sync_entries
		    SET enabled = $3, revision = revision + 1, updated_at = now()
		  WHERE tenant_id = $1 AND key = $2
		 RETURNING `+boardSyncEntryColumns, tenantID, key, enabled)
	record, err := scanBoardSyncEntry(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return BoardSyncEntryRecord{}, ErrBoardSyncEntryMissing
	}
	if err != nil {
		return BoardSyncEntryRecord{}, err
	}
	return record, nil
}

func scanBoardSyncEntry(row pgx.Row) (BoardSyncEntryRecord, error) {
	var (
		record    BoardSyncEntryRecord
		secretRef []byte
		settings  []byte
		routes    []byte
	)
	if err := row.Scan(&record.TenantID, &record.Key, &record.Platform, &secretRef,
		&settings, &routes, &record.StartChannels, &record.Enabled, &record.Revision,
		&record.CreatedAt, &record.UpdatedAt); err != nil {
		return BoardSyncEntryRecord{}, err
	}
	if len(secretRef) > 0 {
		if err := json.Unmarshal(secretRef, &record.SecretRef); err != nil {
			return BoardSyncEntryRecord{}, fmt.Errorf("store: decode board sync secret ref: %w", err)
		}
	}
	if len(settings) > 0 {
		record.Settings = json.RawMessage(settings)
	}
	if len(routes) > 0 {
		if err := json.Unmarshal(routes, &record.Routes); err != nil {
			return BoardSyncEntryRecord{}, fmt.Errorf("store: decode board sync routes: %w", err)
		}
	}
	if record.SecretRef == nil {
		record.SecretRef = map[string]string{}
	}
	if record.Routes == nil {
		record.Routes = map[string]string{}
	}
	if record.StartChannels == nil {
		record.StartChannels = []string{}
	}
	return record, nil
}

func nonNilStringMap(value map[string]string) map[string]string {
	if value == nil {
		return map[string]string{}
	}
	return value
}

// BoardBlobKey is one board attachment key's translation to a keel blob.
type BoardBlobKey struct {
	Key         string
	TenantID    uuid.UUID
	BlobID      uuid.UUID
	ContentType string
	Size        int64
}

// PutBoardBlobKey records key -> (tenant, blob). The board never overwrites a
// key; a retry that names the same blob is idempotent, a different blob for an
// existing key is refused.
func (s *Store) PutBoardBlobKey(ctx context.Context, record BoardBlobKey) error {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO board_tenant_blob_keys (key, tenant_id, blob_id, content_type, size)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (key) DO NOTHING`,
		record.Key, record.TenantID, record.BlobID, record.ContentType, record.Size)
	if err != nil {
		return fmt.Errorf("store: put board blob key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	existing, found, err := s.GetBoardBlobKey(ctx, record.Key)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("store: board blob key disappeared during insert")
	}
	if existing.BlobID != record.BlobID || existing.TenantID != record.TenantID {
		return errors.New("store: board blob key is already bound to another object")
	}
	return nil
}

// GetBoardBlobKey reads one translation row.
func (s *Store) GetBoardBlobKey(ctx context.Context, key string) (BoardBlobKey, bool, error) {
	var record BoardBlobKey
	err := s.pool.QueryRow(ctx,
		`SELECT key, tenant_id, blob_id, content_type, size
		   FROM board_tenant_blob_keys WHERE key = $1`, key).
		Scan(&record.Key, &record.TenantID, &record.BlobID, &record.ContentType, &record.Size)
	if errors.Is(err, pgx.ErrNoRows) {
		return BoardBlobKey{}, false, nil
	}
	if err != nil {
		return BoardBlobKey{}, false, fmt.Errorf("store: get board blob key: %w", err)
	}
	return record, true, nil
}

// DeleteBoardBlobKey removes one translation row and reports whether it
// existed. The bytes remain in keel's blob store for the normal lifecycle
// (dedupe/GC); no board path relies on physical deletion timing.
func (s *Store) DeleteBoardBlobKey(ctx context.Context, key string) (BoardBlobKey, bool, error) {
	var record BoardBlobKey
	err := s.pool.QueryRow(ctx,
		`DELETE FROM board_tenant_blob_keys WHERE key = $1
		 RETURNING key, tenant_id, blob_id, content_type, size`, key).
		Scan(&record.Key, &record.TenantID, &record.BlobID, &record.ContentType, &record.Size)
	if errors.Is(err, pgx.ErrNoRows) {
		return BoardBlobKey{}, false, nil
	}
	if err != nil {
		return BoardBlobKey{}, false, fmt.Errorf("store: delete board blob key: %w", err)
	}
	return record, true, nil
}

// BoardBlobKeyReferenced reports whether another board key still names the
// blob; the adapter uses it to decide whether deleting a key is the last one.
func (s *Store) BoardBlobKeyReferenced(ctx context.Context, tenantID, blobID uuid.UUID) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM board_tenant_blob_keys WHERE tenant_id = $1 AND blob_id = $2)`,
		tenantID, blobID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: board blob key reference: %w", err)
	}
	return exists, nil
}
