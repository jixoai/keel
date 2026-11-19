package fileplugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jixoai/keel/internal/layerfs"
)

// Manifest is the session-scoped path → hash projection. It is the only place
// paths exist; layerfs never sees them.
type Manifest interface {
	Get(ctx context.Context, tenantID, sessionID uuid.UUID, path string) (Entry, bool, error)
	List(ctx context.Context, tenantID, sessionID uuid.UUID) ([]Entry, error)
	Upsert(ctx context.Context, tenantID, sessionID uuid.UUID, entries []Entry) error
	Delete(ctx context.Context, tenantID, sessionID uuid.UUID, path string) error
	// PutSnapshot records the tree hash at an event sequence (fork O(1)).
	PutSnapshot(ctx context.Context, tenantID, sessionID uuid.UUID, seq int64, tree layerfs.Hash) error
	Snapshot(ctx context.Context, tenantID, sessionID uuid.UUID, seq int64) (layerfs.Hash, bool, error)
}

// MemoryManifest is the in-memory manifest (tests and a deployment without the
// migration applied). It enforces the same updated_seq monotonicity as the
// PostgreSQL projection.
type MemoryManifest struct {
	mu        sync.Mutex
	entries   map[string]Entry
	snapshots map[string]layerfs.Hash
}

// NewMemoryManifest builds an empty manifest.
func NewMemoryManifest() *MemoryManifest {
	return &MemoryManifest{entries: make(map[string]Entry), snapshots: make(map[string]layerfs.Hash)}
}

func memoryKey(tenantID, sessionID uuid.UUID, suffix string) string {
	return tenantID.String() + "/" + sessionID.String() + "/" + suffix
}

// Get implements Manifest.
func (m *MemoryManifest) Get(_ context.Context, tenantID, sessionID uuid.UUID, path string) (Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[memoryKey(tenantID, sessionID, path)]
	return entry, ok, nil
}

// List implements Manifest.
func (m *MemoryManifest) List(_ context.Context, tenantID, sessionID uuid.UUID) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := memoryKey(tenantID, sessionID, "")
	entries := make([]Entry, 0)
	for key, entry := range m.entries {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

// Upsert implements Manifest.
func (m *MemoryManifest) Upsert(_ context.Context, tenantID, sessionID uuid.UUID, entries []Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range entries {
		if entry.Path == "" || !entry.Hash.Valid() {
			return Errorf(CodeInvalid, "manifest entry %q is malformed", entry.Path)
		}
		key := memoryKey(tenantID, sessionID, entry.Path)
		if existing, ok := m.entries[key]; ok && existing.UpdatedSeq > entry.UpdatedSeq {
			continue
		}
		m.entries[key] = entry
	}
	return nil
}

// Delete implements Manifest.
func (m *MemoryManifest) Delete(_ context.Context, tenantID, sessionID uuid.UUID, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, memoryKey(tenantID, sessionID, path))
	return nil
}

// PutSnapshot implements Manifest.
func (m *MemoryManifest) PutSnapshot(_ context.Context, tenantID, sessionID uuid.UUID, seq int64, tree layerfs.Hash) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memoryKey(tenantID, sessionID, fmt.Sprintf("snapshot/%d", seq))
	if _, exists := m.snapshots[key]; !exists {
		m.snapshots[key] = tree
	}
	return nil
}

// Snapshot implements Manifest.
func (m *MemoryManifest) Snapshot(_ context.Context, tenantID, sessionID uuid.UUID, seq int64) (layerfs.Hash, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tree, ok := m.snapshots[memoryKey(tenantID, sessionID, fmt.Sprintf("snapshot/%d", seq))]
	return tree, ok, nil
}

// PostgresManifest is the durable projection. It expects the tables in
// SchemaDDL to exist; this package does not run migrations (the store's
// migration is the one owner of schema changes).
type PostgresManifest struct {
	pool *pgxpool.Pool
}

// NewPostgresManifest wraps a pgx pool.
func NewPostgresManifest(pool *pgxpool.Pool) (*PostgresManifest, error) {
	if pool == nil {
		return nil, errors.New("fileplugin: a pgx pool is required")
	}
	return &PostgresManifest{pool: pool}, nil
}

// Get implements Manifest.
func (p *PostgresManifest) Get(ctx context.Context, tenantID, sessionID uuid.UUID, path string) (Entry, bool, error) {
	var entry Entry
	var hash string
	err := p.pool.QueryRow(ctx, `
		SELECT path, hash, mode, size, updated_seq, COALESCE(last_task_id, '')
		FROM file_manifest
		WHERE tenant_id = $1 AND session_id = $2 AND path = $3`,
		tenantID, sessionID, path).
		Scan(&entry.Path, &hash, &entry.Mode, &entry.Size, &entry.UpdatedSeq, &entry.LastTaskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("fileplugin: read manifest: %w", err)
	}
	parsed, err := layerfs.ParseHash(hash)
	if err != nil {
		return Entry{}, false, err
	}
	entry.Hash = parsed
	return entry, true, nil
}

// List implements Manifest.
func (p *PostgresManifest) List(ctx context.Context, tenantID, sessionID uuid.UUID) ([]Entry, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT path, hash, mode, size, updated_seq, COALESCE(last_task_id, '')
		FROM file_manifest
		WHERE tenant_id = $1 AND session_id = $2
		ORDER BY path`, tenantID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("fileplugin: list manifest: %w", err)
	}
	defer rows.Close()
	entries := make([]Entry, 0)
	for rows.Next() {
		var entry Entry
		var hash string
		if err := rows.Scan(&entry.Path, &hash, &entry.Mode, &entry.Size, &entry.UpdatedSeq, &entry.LastTaskID); err != nil {
			return nil, fmt.Errorf("fileplugin: scan manifest: %w", err)
		}
		parsed, err := layerfs.ParseHash(hash)
		if err != nil {
			return nil, err
		}
		entry.Hash = parsed
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// Upsert implements Manifest. The updated_seq guard makes an out-of-order
// capture (or a replayed event) unable to overwrite a newer projection.
func (p *PostgresManifest) Upsert(ctx context.Context, tenantID, sessionID uuid.UUID, entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("fileplugin: manifest upsert: %w", err)
	}
	defer tx.Rollback(ctx)
	const statement = `
		INSERT INTO file_manifest (tenant_id, session_id, path, hash, mode, size, updated_seq, last_task_id, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), now())
		ON CONFLICT (tenant_id, session_id, path) DO UPDATE
		SET hash = EXCLUDED.hash,
		    mode = EXCLUDED.mode,
		    size = EXCLUDED.size,
		    updated_seq = EXCLUDED.updated_seq,
		    last_task_id = EXCLUDED.last_task_id,
		    updated_at = now()
		WHERE file_manifest.updated_seq <= EXCLUDED.updated_seq`
	batch := &pgx.Batch{}
	for _, entry := range entries {
		if entry.Path == "" || !entry.Hash.Valid() {
			return Errorf(CodeInvalid, "manifest entry %q is malformed", entry.Path)
		}
		batch.Queue(statement, tenantID, sessionID, entry.Path, entry.Hash.String(), entry.Mode, entry.Size, entry.UpdatedSeq, entry.LastTaskID)
		if batch.Len() >= 500 {
			if err := tx.SendBatch(ctx, batch).Close(); err != nil {
				return fmt.Errorf("fileplugin: manifest upsert: %w", err)
			}
			batch = &pgx.Batch{}
		}
	}
	if batch.Len() > 0 {
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("fileplugin: manifest upsert: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("fileplugin: manifest upsert: %w", err)
	}
	return nil
}

// Delete implements Manifest.
func (p *PostgresManifest) Delete(ctx context.Context, tenantID, sessionID uuid.UUID, path string) error {
	if _, err := p.pool.Exec(ctx, `DELETE FROM file_manifest WHERE tenant_id = $1 AND session_id = $2 AND path = $3`,
		tenantID, sessionID, path); err != nil {
		return fmt.Errorf("fileplugin: delete manifest: %w", err)
	}
	return nil
}

// PutSnapshot implements Manifest.
func (p *PostgresManifest) PutSnapshot(ctx context.Context, tenantID, sessionID uuid.UUID, seq int64, tree layerfs.Hash) error {
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO session_snapshot (tenant_id, session_id, seq, tree_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, session_id, seq) DO NOTHING`,
		tenantID, sessionID, seq, tree.String()); err != nil {
		return fmt.Errorf("fileplugin: put snapshot: %w", err)
	}
	return nil
}

// Snapshot implements Manifest.
func (p *PostgresManifest) Snapshot(ctx context.Context, tenantID, sessionID uuid.UUID, seq int64) (layerfs.Hash, bool, error) {
	var raw string
	err := p.pool.QueryRow(ctx, `
		SELECT tree_hash FROM session_snapshot
		WHERE tenant_id = $1 AND session_id = $2 AND seq = $3`,
		tenantID, sessionID, seq).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("fileplugin: read snapshot: %w", err)
	}
	tree, err := layerfs.ParseHash(raw)
	if err != nil {
		return "", false, err
	}
	return tree, true, nil
}

// SchemaDDL is the exact DDL the file plugin needs. It is intentionally not
// executed here: internal/store/schema.sql is the single migration owner. The
// main integration must add it there (and add both tables to the RLS table list
// in internal/store/rls.go, which then applies the standard ENABLE/FORCE/policy
// shape).
const SchemaDDL = `
CREATE TABLE IF NOT EXISTS file_manifest (
    tenant_id    uuid NOT NULL,
    session_id   uuid NOT NULL,
    path         text NOT NULL,
    hash         text NOT NULL,
    mode         integer NOT NULL DEFAULT 420,
    size         bigint NOT NULL DEFAULT 0,
    updated_seq  bigint NOT NULL,
    last_task_id text,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, session_id, path)
);

CREATE INDEX IF NOT EXISTS file_manifest_by_hash
    ON file_manifest (tenant_id, hash);

CREATE TABLE IF NOT EXISTS session_snapshot (
    tenant_id  uuid NOT NULL,
    session_id uuid NOT NULL,
    seq        bigint NOT NULL,
    tree_hash  text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, session_id, seq)
);
`
