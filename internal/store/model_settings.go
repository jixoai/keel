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

// Tenant model settings: the stored models-document override of one tenant and
// its append-only audit history (change keel-model-settings). The document is
// opaque JSON here (the vocabulary lives in internal/modelcfg); the revision is
// the change identity clients compare and it never decreases, even across a
// reset — it is derived over both tables, so clearing an override advances it.

// Model settings history sources (model_settings_history.source).
const (
	// ModelSettingsSourceStored marks a revision that stored an override.
	ModelSettingsSourceStored = "stored"
	// ModelSettingsSourceDeployment marks a revision that cleared the override,
	// returning the tenant to its deployment document.
	ModelSettingsSourceDeployment = "deployment"
)

// ErrNoModelSettings is returned by DeleteModelSettings when the tenant has no
// stored override to clear. It tells the caller "already at the deployment
// document", which the HTTP layer may answer idempotently instead of failing.
var ErrNoModelSettings = errors.New("store: no model settings override")

// ErrModelSettingsConflict reports an expectedRevision that does not match the
// current revision: another write (or a reset) happened first. Callers turn it
// into 409 with the revision that won.
var ErrModelSettingsConflict = errors.New("store: model settings revision conflict")

// ModelSettingsConflictError is the typed form of ErrModelSettingsConflict. It
// carries the current revision so the rejection can tell the client what to
// write against next.
type ModelSettingsConflictError struct {
	// Current is the revision the store holds now.
	Current int64
}

func (e *ModelSettingsConflictError) Error() string {
	return fmt.Sprintf("store: model settings revision conflict: expected revision does not match (current revision is %d)", e.Current)
}

// Unwrap makes errors.Is(err, ErrModelSettingsConflict) hold.
func (e *ModelSettingsConflictError) Unwrap() error { return ErrModelSettingsConflict }

// GetModelSettings reads a tenant's stored models override. found=false means
// the tenant runs on its deployment document; in that case revision reports the
// lifecycle revision (the history head, 0 before anything was ever written) so
// a client's change identity never moves backwards after a reset.
//
// A tenant with no settings at all is not an error: the settings are keyed by
// the tenant the session names, independent of the tenants row.
func (s *Store) GetModelSettings(ctx context.Context, tenantID uuid.UUID) (json.RawMessage, int64, time.Time, bool, error) {
	var (
		raw       []byte
		revision  int64
		updatedAt time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT document, revision, updated_at FROM model_settings WHERE tenant_id = $1`,
		tenantID).Scan(&raw, &revision, &updatedAt)
	switch {
	case err == nil:
		return json.RawMessage(raw), revision, updatedAt, true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, 0, time.Time{}, false, fmt.Errorf("store: get model settings: %w", err)
	}
	var headRevision *int64
	var headAt *time.Time
	err = s.pool.QueryRow(ctx,
		`SELECT max(revision), max(created_at) FROM model_settings_history WHERE tenant_id = $1`,
		tenantID).Scan(&headRevision, &headAt)
	if err != nil {
		return nil, 0, time.Time{}, false, fmt.Errorf("store: get model settings history head: %w", err)
	}
	if headRevision != nil {
		revision = *headRevision
	}
	if headAt != nil {
		updatedAt = *headAt
	}
	return nil, revision, updatedAt, false, nil
}

// SetModelSettings stores a tenant's models override as the next revision and
// appends the matching history row in one transaction. revision is current+1,
// where current is derived over the override row and the history, so the value
// is monotonic across writes and resets.
//
// expectedRevision guards the write: 0 means "no expectation", a positive value
// must equal the current revision or the write is refused with
// *ModelSettingsConflictError. The check and the allocation happen under a
// per-tenant advisory lock, so two concurrent writers cannot both claim the
// same revision.
func (s *Store) SetModelSettings(ctx context.Context, tenantID uuid.UUID, expectedRevision int64, document json.RawMessage) (int64, error) {
	if len(document) == 0 || !json.Valid(document) {
		return 0, fmt.Errorf("store: model settings document is not valid JSON")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: set model settings begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockModelSettings(ctx, tx, tenantID); err != nil {
		return 0, err
	}
	current, err := modelSettingsRevision(ctx, tx, tenantID)
	if err != nil {
		return 0, err
	}
	if expectedRevision > 0 && expectedRevision != current {
		return current, &ModelSettingsConflictError{Current: current}
	}
	next := current + 1
	if _, err := tx.Exec(ctx,
		`INSERT INTO model_settings (tenant_id, revision, document, updated_at)
		 VALUES ($1, $2, $3::jsonb, now())
		 ON CONFLICT (tenant_id) DO UPDATE
		 SET revision = EXCLUDED.revision, document = EXCLUDED.document, updated_at = EXCLUDED.updated_at`,
		tenantID, next, string(document)); err != nil {
		return 0, fmt.Errorf("store: set model settings: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO model_settings_history (tenant_id, revision, document, source)
		 VALUES ($1, $2, $3::jsonb, $4)`,
		tenantID, next, string(document), ModelSettingsSourceStored); err != nil {
		return 0, fmt.Errorf("store: write model settings history: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: set model settings commit: %w", err)
	}
	return next, nil
}

// DeleteModelSettings clears a tenant's override, returning it to the
// deployment document, and appends the reset to the history in one transaction.
// The deleted override stays readable at the previous history revision; the new
// history row carries a NULL document and source 'deployment'. revision is
// current+1, so a client polling the settings sees the change.
//
// A tenant with no override is refused with ErrNoModelSettings (the caller may
// answer idempotently); expectedRevision guards exactly like SetModelSettings.
func (s *Store) DeleteModelSettings(ctx context.Context, tenantID uuid.UUID, expectedRevision int64) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: delete model settings begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockModelSettings(ctx, tx, tenantID); err != nil {
		return 0, err
	}
	var rowRevision int64
	err = tx.QueryRow(ctx,
		`SELECT revision FROM model_settings WHERE tenant_id = $1`, tenantID).Scan(&rowRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNoModelSettings
	}
	if err != nil {
		return 0, fmt.Errorf("store: read model settings: %w", err)
	}
	if expectedRevision > 0 && expectedRevision != rowRevision {
		return rowRevision, &ModelSettingsConflictError{Current: rowRevision}
	}
	current, err := modelSettingsRevision(ctx, tx, tenantID)
	if err != nil {
		return 0, err
	}
	next := current + 1
	if _, err := tx.Exec(ctx,
		`DELETE FROM model_settings WHERE tenant_id = $1`, tenantID); err != nil {
		return 0, fmt.Errorf("store: delete model settings: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO model_settings_history (tenant_id, revision, document, source)
		 VALUES ($1, $2, NULL, $3)`,
		tenantID, next, ModelSettingsSourceDeployment); err != nil {
		return 0, fmt.Errorf("store: write model settings history: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: delete model settings commit: %w", err)
	}
	return next, nil
}

// lockModelSettings serializes the read-derive-write of one tenant's settings
// across every connection and process. The lock is transaction-scoped, so it is
// released on commit or rollback.
func lockModelSettings(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('keel_model_settings'), hashtext($1::text))`,
		tenantID.String()); err != nil {
		return fmt.Errorf("store: lock model settings: %w", err)
	}
	return nil
}

// modelSettingsRevision derives the tenant's current revision: the override
// row's revision, or the history head when no override is stored. 0 means
// nothing was ever written.
func modelSettingsRevision(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (int64, error) {
	var current int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(GREATEST(
		     COALESCE((SELECT revision FROM model_settings WHERE tenant_id = $1), 0),
		     COALESCE((SELECT max(revision) FROM model_settings_history WHERE tenant_id = $1), 0)
		 ), 0)`, tenantID).Scan(&current); err != nil {
		return 0, fmt.Errorf("store: read model settings revision: %w", err)
	}
	return current, nil
}
