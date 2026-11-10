package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Plugin inventory and lifecycle audit (change keel-plugin-core). The inventory
// is deployment-scoped: one plugins root, one assembled tool surface, so a
// plugin's state is not a per-tenant fact. The audit is tenant-scoped, because
// "who asked for this" is.
//
// The manifest and source documents are opaque JSON here (their vocabulary
// lives in internal/pluginpkg); the state string is the lifecycle state machine
// the manager owns. The store never interprets either.

// Plugin states as persisted. The lifecycle is
// installed -> enabled -> disabled -> uninstalled, plus failed (an enable that
// fell over) and invalid (the package on disk no longer validates).
const (
	PluginStateInstalled   = "installed"
	PluginStateEnabled     = "enabled"
	PluginStateDisabled    = "disabled"
	PluginStateFailed      = "failed"
	PluginStateInvalid     = "invalid"
	PluginStateUninstalled = "uninstalled"
)

// Plugin audit actions.
const (
	PluginActionInstall   = "install"
	PluginActionEnable    = "enable"
	PluginActionDisable   = "disable"
	PluginActionUninstall = "uninstall"
	PluginActionUpdate    = "update"
	PluginActionDiscover  = "discover"
)

// Plugin audit results.
const (
	PluginResultOK      = "ok"
	PluginResultFailed  = "failed"
	PluginResultRefused = "refused"
)

// PluginRecord is one row of the plugin inventory.
type PluginRecord struct {
	// ID is the plugin id (the manifest id, or the directory name for a package
	// whose manifest never parsed).
	ID string
	// Version is the manifest version.
	Version string
	// Kind is in-process|sidecar|webview.
	Kind string
	// State is the lifecycle state (PluginState*).
	State string
	// Bundled marks a plugin compiled into the binary.
	Bundled bool
	// Source is the provenance document ({"type":...,...}).
	Source json.RawMessage
	// Manifest is the raw manifest document.
	Manifest json.RawMessage
	// ContentHash is the package tree hash (empty for bundled plugins).
	ContentHash string
	// Path is the managed package directory (empty for bundled plugins).
	Path string
	// Diagnostics are the operator-facing reasons attached to the row.
	Diagnostics []string
	// InstalledAt, UpdatedAt and the state timestamps are UTC.
	InstalledAt time.Time
	UpdatedAt   time.Time
	EnabledAt   *time.Time
	DisabledAt  *time.Time
}

// PluginAuditEntry is one audit row.
type PluginAuditEntry struct {
	AuditID  int64           `json:"auditId"`
	TS       time.Time       `json:"ts"`
	TenantID *uuid.UUID      `json:"tenantId,omitempty"`
	Actor    string          `json:"actor"`
	PluginID string          `json:"pluginId"`
	Version  string          `json:"version,omitempty"`
	Action   string          `json:"action"`
	Result   string          `json:"result"`
	Detail   json.RawMessage `json:"detail,omitempty"`
}

// UpsertPlugin writes the inventory row, preserving installed_at (the age of an
// installation is not reset by a state change).
func (s *Store) UpsertPlugin(ctx context.Context, record PluginRecord) error {
	if record.ID == "" {
		return errors.New("store: plugin id is required")
	}
	diagnostics, err := json.Marshal(record.Diagnostics)
	if err != nil {
		return fmt.Errorf("store: marshal plugin diagnostics: %w", err)
	}
	source := record.Source
	if len(source) == 0 {
		source = json.RawMessage(`{}`)
	}
	manifest := record.Manifest
	if len(manifest) == 0 {
		manifest = json.RawMessage(`{}`)
	}
	installedAt := record.InstalledAt
	if installedAt.IsZero() {
		installedAt = time.Now().UTC()
	}
	updatedAt := record.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = installedAt
	}
	_, err = s.execRetry(ctx,
		`INSERT INTO plugins (id, version, kind, state, bundled, source, manifest, content_hash, path, diagnostics,
		                      installed_at, updated_at, enabled_at, disabled_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		 ON CONFLICT (id) DO UPDATE SET
		     version = EXCLUDED.version,
		     kind = EXCLUDED.kind,
		     state = EXCLUDED.state,
		     bundled = EXCLUDED.bundled,
		     source = EXCLUDED.source,
		     manifest = EXCLUDED.manifest,
		     content_hash = EXCLUDED.content_hash,
		     path = EXCLUDED.path,
		     diagnostics = EXCLUDED.diagnostics,
		     updated_at = EXCLUDED.updated_at,
		     enabled_at = EXCLUDED.enabled_at,
		     disabled_at = EXCLUDED.disabled_at`,
		record.ID, record.Version, record.Kind, record.State, record.Bundled, source, manifest,
		record.ContentHash, record.Path, diagnostics, installedAt, updatedAt, record.EnabledAt, record.DisabledAt)
	if err != nil {
		return fmt.Errorf("store: upsert plugin: %w", err)
	}
	return nil
}

// GetPlugin reads one inventory row. found=false means the id is unknown.
func (s *Store) GetPlugin(ctx context.Context, id string) (PluginRecord, bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+pluginColumns+` FROM plugins WHERE id = $1`, id)
	record, err := scanPlugin(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return PluginRecord{}, false, nil
	}
	if err != nil {
		return PluginRecord{}, false, fmt.Errorf("store: get plugin: %w", err)
	}
	return record, true, nil
}

// ListPlugins returns the whole inventory, ordered by id.
func (s *Store) ListPlugins(ctx context.Context) ([]PluginRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+pluginColumns+` FROM plugins ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list plugins: %w", err)
	}
	defer rows.Close()
	out := make([]PluginRecord, 0)
	for rows.Next() {
		record, err := scanPlugin(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan plugin: %w", err)
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// DeletePlugin removes an inventory row. found=false means it was already gone.
func (s *Store) DeletePlugin(ctx context.Context, id string) (bool, error) {
	tag, err := s.execRetry(ctx, `DELETE FROM plugins WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("store: delete plugin: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

const pluginColumns = `id, version, kind, state, bundled, source, manifest, content_hash, path, diagnostics,
	installed_at, updated_at, enabled_at, disabled_at`

// rowScanner is the shared surface of pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanPlugin(row rowScanner) (PluginRecord, error) {
	var (
		record      PluginRecord
		source      []byte
		manifest    []byte
		diagnostics []byte
	)
	if err := row.Scan(&record.ID, &record.Version, &record.Kind, &record.State, &record.Bundled, &source, &manifest,
		&record.ContentHash, &record.Path, &diagnostics, &record.InstalledAt, &record.UpdatedAt,
		&record.EnabledAt, &record.DisabledAt); err != nil {
		return PluginRecord{}, err
	}
	record.Source = json.RawMessage(source)
	record.Manifest = json.RawMessage(manifest)
	if len(diagnostics) > 0 {
		if err := json.Unmarshal(diagnostics, &record.Diagnostics); err != nil {
			return PluginRecord{}, fmt.Errorf("decode diagnostics: %w", err)
		}
	}
	return record, nil
}

// AppendPluginAudit writes one audit row and returns it with its cursor.
func (s *Store) AppendPluginAudit(ctx context.Context, entry PluginAuditEntry) (PluginAuditEntry, error) {
	if entry.PluginID == "" {
		return PluginAuditEntry{}, errors.New("store: audit plugin id is required")
	}
	if entry.Action == "" || entry.Result == "" {
		return PluginAuditEntry{}, errors.New("store: audit action and result are required")
	}
	if entry.TS.IsZero() {
		entry.TS = time.Now().UTC()
	}
	detail := entry.Detail
	if len(detail) == 0 {
		detail = json.RawMessage(`{}`)
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO plugin_audit (ts, tenant_id, actor, plugin_id, version, action, result, detail)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING audit_id`,
		entry.TS, entry.TenantID, entry.Actor, entry.PluginID, entry.Version, entry.Action, entry.Result, detail)
	if err := row.Scan(&entry.AuditID); err != nil {
		return PluginAuditEntry{}, fmt.Errorf("store: append plugin audit: %w", err)
	}
	entry.Detail = detail
	return entry, nil
}

// ListPluginAudit returns audit rows in chronological order (audit_id >
// after), optionally narrowed to one plugin and scoped to a tenant. A nil
// tenant reads the deployment-wide trail; the product API always passes the
// caller's tenant.
func (s *Store) ListPluginAudit(ctx context.Context, tenantID *uuid.UUID, pluginID string, after int64, limit int) ([]PluginAuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT audit_id, ts, tenant_id, actor, plugin_id, version, action, result, detail
		 FROM plugin_audit
		 WHERE audit_id > $1
		   AND ($2::uuid IS NULL OR tenant_id IS NULL OR tenant_id = $2)
		   AND ($3 = '' OR plugin_id = $3)
		 ORDER BY audit_id
		 LIMIT $4`,
		after, tenantID, pluginID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list plugin audit: %w", err)
	}
	defer rows.Close()
	out := make([]PluginAuditEntry, 0, limit)
	for rows.Next() {
		var (
			entry  PluginAuditEntry
			detail []byte
		)
		if err := rows.Scan(&entry.AuditID, &entry.TS, &entry.TenantID, &entry.Actor, &entry.PluginID,
			&entry.Version, &entry.Action, &entry.Result, &detail); err != nil {
			return nil, fmt.Errorf("store: scan plugin audit: %w", err)
		}
		entry.Detail = json.RawMessage(detail)
		out = append(out, entry)
	}
	return out, rows.Err()
}
