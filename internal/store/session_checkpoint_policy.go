package store

// Per-session checkpoint policy overrides (change keel-checkpoint). The stored
// document is opaque JSON: internal/compaction owns its schema and the server
// always narrows it against the tenant policy before use. This file only keeps
// the durable row and its monotonic revision, so a policy write is a plain
// upsert and a reader can detect a concurrent update.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SessionCheckpointPolicy is one stored override row. Policy is the raw JSON
// document; Revision increases on every accepted write (it is an update counter,
// not a compare-and-swap token).
type SessionCheckpointPolicy struct {
	Policy   json.RawMessage
	Revision int64
}

// GetSessionCheckpointPolicy reads a session's override. found=false with a nil
// error means the session exists but has no override (the tenant policy
// applies). ErrNoSession means the tenant/session pair does not exist.
func (s *Store) GetSessionCheckpointPolicy(ctx context.Context, tenantID, sessionID uuid.UUID) (SessionCheckpointPolicy, bool, error) {
	var (
		raw []byte
		rev int64
	)
	err := s.pool.QueryRow(ctx,
		`SELECT policy, revision FROM session_checkpoint_policies
		 WHERE tenant_id = $1 AND session_id = $2`,
		tenantID, sessionID).Scan(&raw, &rev)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM sessions WHERE tenant_id = $1 AND session_id = $2)`,
			tenantID, sessionID).Scan(&exists); err != nil {
			return SessionCheckpointPolicy{}, false, fmt.Errorf("store: session probe: %w", err)
		}
		if !exists {
			return SessionCheckpointPolicy{}, false, ErrNoSession
		}
		return SessionCheckpointPolicy{}, false, nil
	}
	if err != nil {
		return SessionCheckpointPolicy{}, false, fmt.Errorf("store: get session checkpoint policy: %w", err)
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	return SessionCheckpointPolicy{Policy: json.RawMessage(raw), Revision: rev}, true, nil
}

// DeleteSessionCheckpointPolicy removes a session's override. Clearing the
// override is a delete, not an empty document: a row that no longer narrows
// anything must not keep a revision or be mistaken for a stored policy. A
// missing (or foreign-tenant) session answers ErrNoSession; a session with no
// override is a no-op.
func (s *Store) DeleteSessionCheckpointPolicy(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM session_checkpoint_policies
		 WHERE tenant_id = $1 AND session_id = $2`,
		tenantID, sessionID)
	if err != nil {
		return fmt.Errorf("store: delete session checkpoint policy: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM sessions WHERE tenant_id = $1 AND session_id = $2)`,
			tenantID, sessionID).Scan(&exists); err != nil {
			return fmt.Errorf("store: session probe: %w", err)
		}
		if !exists {
			return ErrNoSession
		}
	}
	return nil
}

// SetSessionCheckpointPolicy upserts a session's override and bumps its
// revision. A missing (or foreign-tenant) session answers ErrNoSession: the
// guarded INSERT cannot create an override for a session that does not exist.
func (s *Store) SetSessionCheckpointPolicy(ctx context.Context, tenantID, sessionID uuid.UUID, document json.RawMessage) (int64, error) {
	if len(document) == 0 {
		document = json.RawMessage("{}")
	}
	if !json.Valid(document) {
		return 0, fmt.Errorf("store: session checkpoint policy is not valid JSON")
	}
	var revision int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO session_checkpoint_policies (tenant_id, session_id, policy)
		 SELECT $1, $2, $3::jsonb
		 WHERE EXISTS (SELECT 1 FROM sessions WHERE tenant_id = $1 AND session_id = $2)
		 ON CONFLICT (tenant_id, session_id) DO UPDATE
		 SET policy = EXCLUDED.policy,
		     revision = session_checkpoint_policies.revision + 1,
		     updated_at = now()
		 RETURNING revision`,
		tenantID, sessionID, string(document)).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNoSession
	}
	if err != nil {
		return 0, fmt.Errorf("store: set session checkpoint policy: %w", err)
	}
	return revision, nil
}
