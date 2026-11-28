// Package pg is the PostgreSQL adapter for the plugin platform storage
// contract (change keel-plugin-split). It owns the SQL for plugin_state,
// plugin_config and plugin_dependency. The install inventory row (`plugins`) is
// deliberately not written here: the core store and the harnessext plugin
// manager own it (store.UpsertPlugin), so two writers cannot overwrite each
// other's version, state and timestamps.
//
// The adapter is deliberately a separate package from internal/store: the
// contract package carries no driver, and this adapter can be built over any
// pgx pool the host already owns. SQLite is not implemented; the boundary is
// the pluginstore.Store interface.
package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jixoai/keel/internal/pluginstore"
)

// Store is the PostgreSQL implementation of pluginstore.Store.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

var _ pluginstore.Store = (*Store)(nil)

// New builds the adapter over an existing pool. The host has already applied
// the platform schema (internal/store/schema.sql).
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Store) GetState(ctx context.Context, tenantID uuid.UUID, pluginID, key string) (string, bool, error) {
	var value string
	err := s.pool.QueryRow(ctx, `
		SELECT value FROM plugin_state WHERE tenant_id = $1 AND plugin_id = $2 AND key = $3`,
		tenantID, pluginID, key).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("pluginstore: get state: %w", err)
	}
	return value, true, nil
}

func (s *Store) PutState(ctx context.Context, tenantID uuid.UUID, pluginID, key, value string) error {
	if pluginID == "" || key == "" {
		return errors.New("pluginstore: plugin id and state key are required")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO plugin_state (tenant_id, plugin_id, key, value, updated_at)
		VALUES ($1,$2,$3,$4,now())
		ON CONFLICT (tenant_id, plugin_id, key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		tenantID, pluginID, key, value)
	if err != nil {
		return fmt.Errorf("pluginstore: put state: %w", err)
	}
	return nil
}

func (s *Store) DeleteState(ctx context.Context, tenantID uuid.UUID, pluginID, key string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM plugin_state WHERE tenant_id = $1 AND plugin_id = $2 AND key = $3`,
		tenantID, pluginID, key)
	if err != nil {
		return false, fmt.Errorf("pluginstore: delete state: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Store) ListState(ctx context.Context, tenantID uuid.UUID, pluginID string) ([]pluginstore.StateEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT plugin_id, key, value, updated_at FROM plugin_state
		 WHERE tenant_id = $1 AND plugin_id = $2 ORDER BY key`, tenantID, pluginID)
	if err != nil {
		return nil, fmt.Errorf("pluginstore: list state: %w", err)
	}
	defer rows.Close()
	out := make([]pluginstore.StateEntry, 0)
	for rows.Next() {
		var entry pluginstore.StateEntry
		if err := rows.Scan(&entry.PluginID, &entry.Key, &entry.Value, &entry.UpdatedAt); err != nil {
			return nil, fmt.Errorf("pluginstore: scan state: %w", err)
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

func (s *Store) GetConfig(ctx context.Context, tenantID uuid.UUID, pluginID string) (pluginstore.Config, bool, error) {
	var config pluginstore.Config
	var document []byte
	err := s.pool.QueryRow(ctx, `
		SELECT plugin_id, document, updated_at FROM plugin_config
		 WHERE tenant_id = $1 AND plugin_id = $2`, tenantID, pluginID).Scan(
		&config.PluginID, &document, &config.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return pluginstore.Config{}, false, nil
	}
	if err != nil {
		return pluginstore.Config{}, false, fmt.Errorf("pluginstore: get config: %w", err)
	}
	config.Document = append(json.RawMessage(nil), document...)
	return config, true, nil
}

func (s *Store) PutConfig(ctx context.Context, tenantID uuid.UUID, pluginID string, document json.RawMessage) (pluginstore.Config, error) {
	if pluginID == "" {
		return pluginstore.Config{}, errors.New("pluginstore: plugin id is required")
	}
	if len(document) == 0 {
		document = json.RawMessage(`{}`)
	}
	var config pluginstore.Config
	var stored []byte
	err := s.pool.QueryRow(ctx, `
		INSERT INTO plugin_config (tenant_id, plugin_id, document, updated_at)
		VALUES ($1,$2,$3::jsonb,now())
		ON CONFLICT (tenant_id, plugin_id) DO UPDATE SET document = EXCLUDED.document, updated_at = now()
		RETURNING plugin_id, document, updated_at`, tenantID, pluginID, document).Scan(
		&config.PluginID, &stored, &config.UpdatedAt)
	if err != nil {
		return pluginstore.Config{}, fmt.Errorf("pluginstore: put config: %w", err)
	}
	config.Document = append(json.RawMessage(nil), stored...)
	return config, nil
}

func (s *Store) PutDependency(ctx context.Context, tenantID uuid.UUID, dependency pluginstore.Dependency) (pluginstore.Dependency, error) {
	if err := pluginstore.ValidateDependency(dependency); err != nil {
		return pluginstore.Dependency{}, err
	}
	if dependency.State == "" {
		dependency.State = pluginstore.DependencyPending
	}
	requires, err := json.Marshal(dependency.Requires)
	if err != nil {
		return pluginstore.Dependency{}, fmt.Errorf("pluginstore: encode requires: %w", err)
	}
	var storedRequires []byte
	var result pluginstore.Dependency
	err = s.pool.QueryRow(ctx, `
		INSERT INTO plugin_dependency (tenant_id, plugin_id, requires, state, reason, updated_at)
		VALUES ($1,$2,$3::jsonb,$4,$5,now())
		ON CONFLICT (tenant_id, plugin_id) DO UPDATE SET
		    requires = EXCLUDED.requires, state = EXCLUDED.state,
		    reason = EXCLUDED.reason, updated_at = now()
		RETURNING plugin_id, requires, state, reason, updated_at`,
		tenantID, dependency.PluginID, requires, dependency.State, dependency.Reason).Scan(
		&result.PluginID, &storedRequires, &result.State, &result.Reason, &result.UpdatedAt)
	if err != nil {
		return pluginstore.Dependency{}, fmt.Errorf("pluginstore: put dependency: %w", err)
	}
	if err := json.Unmarshal(storedRequires, &result.Requires); err != nil {
		return pluginstore.Dependency{}, fmt.Errorf("pluginstore: decode requires: %w", err)
	}
	return result, nil
}

func (s *Store) ListDependencies(ctx context.Context, tenantID uuid.UUID) ([]pluginstore.Dependency, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT plugin_id, requires, state, reason, updated_at FROM plugin_dependency
		 WHERE tenant_id = $1 ORDER BY plugin_id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("pluginstore: list dependencies: %w", err)
	}
	defer rows.Close()
	out := make([]pluginstore.Dependency, 0)
	for rows.Next() {
		var dependency pluginstore.Dependency
		var requires []byte
		if err := rows.Scan(&dependency.PluginID, &requires, &dependency.State, &dependency.Reason, &dependency.UpdatedAt); err != nil {
			return nil, fmt.Errorf("pluginstore: scan dependency: %w", err)
		}
		if err := json.Unmarshal(requires, &dependency.Requires); err != nil {
			return nil, fmt.Errorf("pluginstore: decode requires: %w", err)
		}
		out = append(out, dependency)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}
