package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jixoai/keel/internal/sessionfork"
)

// RecordSessionFork writes the fork origin of a child session. It is called
// before the child's first input; the write is idempotent for the same fact
// (a retried fork with the same child id converges instead of failing).
//
// The columns are a reference: no parent event, manifest row or byte is copied.
// A child session that already exists for another parent is refused, so a
// deterministic child id can never be re-pointed at a second origin.
func (s *Store) RecordSessionFork(ctx context.Context, tenantID, sessionID uuid.UUID, fact sessionfork.Fact) error {
	if tenantID == uuid.Nil || sessionID == uuid.Nil {
		return errors.New("store: fork fact needs tenant and session")
	}
	if !fact.Valid() {
		return errors.New("store: fork fact needs a parent session and a non-negative origin")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE sessions
		   SET fork_parent_session_id = $3,
		       fork_origin_seq = $4,
		       fork_tree_hash = $5,
		       fork_config_hash = $6,
		       fork_tool_manifest_hash = $7,
		       fork_anchor_seq = $8,
		       updated_at = now()
		 WHERE tenant_id = $1 AND session_id = $2
		   AND (fork_parent_session_id IS NULL OR fork_parent_session_id = $3)`,
		tenantID, sessionID, fact.ParentSessionID, fact.OriginSeq, fact.TreeHash,
		fact.ConfigHash, fact.ToolManifestHash, fact.AnchorSeq)
	if err != nil {
		return fmt.Errorf("store: record session fork: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either the session is missing or it already belongs to another
		// origin. Both are refusals, never a silent re-point.
		var existing uuid.UUID
		err := s.pool.QueryRow(ctx,
			`SELECT COALESCE(fork_parent_session_id, '00000000-0000-0000-0000-000000000000')
			   FROM sessions WHERE tenant_id = $1 AND session_id = $2`,
			tenantID, sessionID).Scan(&existing)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSession
		}
		if err != nil {
			return fmt.Errorf("store: read session fork: %w", err)
		}
		return fmt.Errorf("store: session %s is already forked from %s, not %s", sessionID, existing, fact.ParentSessionID)
	}
	return nil
}

// SessionFork reads the fork origin of a session. found=false means the
// session exists but is not a fork; ErrNoSession means it does not exist.
func (s *Store) SessionFork(ctx context.Context, tenantID, sessionID uuid.UUID) (sessionfork.Fact, bool, error) {
	var (
		parent   *uuid.UUID
		origin   int64
		tree     *string
		config   *string
		manifest *string
		anchor   int64
	)
	err := s.pool.QueryRow(ctx, `
		SELECT fork_parent_session_id, fork_origin_seq, fork_tree_hash,
		       fork_config_hash, fork_tool_manifest_hash, fork_anchor_seq
		  FROM sessions WHERE tenant_id = $1 AND session_id = $2`,
		tenantID, sessionID).Scan(&parent, &origin, &tree, &config, &manifest, &anchor)
	if errors.Is(err, pgx.ErrNoRows) {
		return sessionfork.Fact{}, false, ErrNoSession
	}
	if err != nil {
		return sessionfork.Fact{}, false, fmt.Errorf("store: read session fork: %w", err)
	}
	if parent == nil {
		return sessionfork.Fact{}, false, nil
	}
	fact := sessionfork.Fact{
		ParentSessionID: *parent,
		OriginSeq:       origin,
		AnchorSeq:       anchor,
	}
	if tree != nil {
		fact.TreeHash = *tree
	}
	if config != nil {
		fact.ConfigHash = *config
	}
	if manifest != nil {
		fact.ToolManifestHash = *manifest
	}
	return fact, true, nil
}
