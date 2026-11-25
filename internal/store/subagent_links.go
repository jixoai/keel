// Sub-agent spawn links (change keel-collab-subagent).
//
// The semantic link lives in the child's first user.message (mode,
// parentSessionId, depth) and is read by harnessext.DurableSubagentLink. The
// columns on sessions are its queryable projection — the same pattern the fork
// origin uses (fork_parent_session_id): the guarded Host needs direct-children
// counts and a restart-safe depth without replaying every child ledger.
//
// A fork writes nothing here: a fork is a ledger branch, not a delegation, and
// it does not consume sub-agent depth or width.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SubagentLinkRow is one projected spawn link.
type SubagentLinkRow struct {
	TenantID        uuid.UUID
	SessionID       uuid.UUID
	ParentSessionID uuid.UUID
	ModeID          string
	Depth           int
}

// MaxSubagentChildren bounds one listing of a session's direct children. The
// mode caps active children far below it; the bound exists so one corrupt or
// historical parent can never turn a read into an unbounded scan.
const MaxSubagentChildren = 1000

// RecordSubagentLink writes the projection before the child's first input.
// Writing the same link twice is idempotent; re-pointing a child at a different
// parent is refused (a deterministic child id must not move between origins).
func (s *Store) RecordSubagentLink(ctx context.Context, link SubagentLinkRow) error {
	if link.TenantID == uuid.Nil || link.SessionID == uuid.Nil || link.ParentSessionID == uuid.Nil {
		return errors.New("store: subagent link needs tenant, child and parent")
	}
	if link.Depth < 1 {
		return errors.New("store: subagent link needs a positive depth")
	}
	if link.ModeID == "" {
		return errors.New("store: subagent link needs a mode")
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (tenant_id, session_id, subagent_parent_session_id, subagent_depth, subagent_mode)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, session_id) DO UPDATE
		   SET subagent_parent_session_id = EXCLUDED.subagent_parent_session_id,
		       subagent_depth = EXCLUDED.subagent_depth,
		       subagent_mode = EXCLUDED.subagent_mode,
		       updated_at = now()
		 WHERE sessions.subagent_parent_session_id IS NULL
		    OR sessions.subagent_parent_session_id = EXCLUDED.subagent_parent_session_id`,
		link.TenantID, link.SessionID, link.ParentSessionID, link.Depth, link.ModeID)
	if err != nil {
		return fmt.Errorf("store: record subagent link: %w", err)
	}
	if tag.RowsAffected() == 0 {
		existing, found, readErr := s.SubagentLink(ctx, link.TenantID, link.SessionID)
		if readErr != nil {
			return readErr
		}
		if found && existing.ParentSessionID == link.ParentSessionID && existing.Depth == link.Depth && existing.ModeID == link.ModeID {
			return nil
		}
		return fmt.Errorf("store: session %s already belongs to another sub-agent origin", link.SessionID)
	}
	return nil
}

// SubagentLink reads one session's projected link. found=false covers both "a
// root session" and "no such session"; callers that need existence ask
// SessionInfo first.
func (s *Store) SubagentLink(ctx context.Context, tenantID, childID uuid.UUID) (SubagentLinkRow, bool, error) {
	if tenantID == uuid.Nil || childID == uuid.Nil {
		return SubagentLinkRow{}, false, nil
	}
	var (
		parent uuid.UUID
		depth  int
		mode   string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT subagent_parent_session_id, subagent_depth, subagent_mode
		  FROM sessions
		 WHERE tenant_id = $1 AND session_id = $2
		   AND subagent_parent_session_id IS NOT NULL`,
		tenantID, childID).Scan(&parent, &depth, &mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return SubagentLinkRow{}, false, nil
	}
	if err != nil {
		return SubagentLinkRow{}, false, fmt.Errorf("store: read subagent link: %w", err)
	}
	return SubagentLinkRow{
		TenantID: tenantID, SessionID: childID, ParentSessionID: parent,
		ModeID: mode, Depth: depth,
	}, true, nil
}

// SubagentChildren lists a session's direct children, oldest first. Archived
// children are excluded: they are deleted from the user's view and from the
// live tree.
func (s *Store) SubagentChildren(ctx context.Context, tenantID, parentSessionID uuid.UUID) ([]SubagentLinkRow, error) {
	if tenantID == uuid.Nil || parentSessionID == uuid.Nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT session_id, subagent_depth, subagent_mode
		  FROM sessions
		 WHERE tenant_id = $1 AND subagent_parent_session_id = $2
		   AND archived_at IS NULL
		 ORDER BY created_at, session_id
		 LIMIT $3`,
		tenantID, parentSessionID, MaxSubagentChildren)
	if err != nil {
		return nil, fmt.Errorf("store: list subagent children: %w", err)
	}
	defer rows.Close()
	out := make([]SubagentLinkRow, 0)
	for rows.Next() {
		var (
			childID uuid.UUID
			depth   int
			mode    string
		)
		if err := rows.Scan(&childID, &depth, &mode); err != nil {
			return nil, fmt.Errorf("store: scan subagent child: %w", err)
		}
		out = append(out, SubagentLinkRow{
			TenantID: tenantID, SessionID: childID, ParentSessionID: parentSessionID,
			ModeID: mode, Depth: depth,
		})
	}
	return out, rows.Err()
}

// CountActiveSubagentChildren counts the direct children of one session whose
// run is still live. "Active" is the run row's queued/pending/running state:
// it survives a restart (a resumed child is pending again) and a finished child
// does not occupy a width slot.
func (s *Store) CountActiveSubagentChildren(ctx context.Context, tenantID, parentSessionID uuid.UUID) (int, error) {
	return s.countActiveSubagent(ctx, tenantID,
		`AND s.subagent_parent_session_id = $2`, parentSessionID)
}

// CountActiveSubagentSessions counts the tenant's live sessions of one mode
// (the mode concurrency guard).
func (s *Store) CountActiveSubagentSessions(ctx context.Context, tenantID uuid.UUID, modeID string) (int, error) {
	return s.countActiveSubagent(ctx, tenantID,
		`AND s.subagent_mode = $2`, modeID)
}

func (s *Store) countActiveSubagent(ctx context.Context, tenantID uuid.UUID, filter string, arg any) (int, error) {
	if s == nil {
		return 0, nil
	}
	var count int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM sessions s
		  JOIN runs r ON r.tenant_id = s.tenant_id AND r.session_id = s.session_id
		 WHERE s.tenant_id = $1
		   AND s.archived_at IS NULL
		   AND r.status IN ('queued', 'pending', 'running') `+filter,
		tenantID, arg).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: count active subagent sessions: %w", err)
	}
	return count, nil
}

// SessionInfo is the session row projection the collaboration SessionIndex
// needs (existence, owning Bot, state and the soft-delete mark).
type SessionInfo struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	BotID     string
	State     string
	Archived  bool
}

// GetSessionInfo reads one session row. ErrNoSession covers both a missing row
// and a row of another tenant.
func (s *Store) GetSessionInfo(ctx context.Context, tenantID, sessionID uuid.UUID) (SessionInfo, error) {
	if tenantID == uuid.Nil || sessionID == uuid.Nil {
		return SessionInfo{}, ErrNoSession
	}
	var (
		botID    string
		state    string
		archived bool
	)
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(bot_id, ''), COALESCE(state, 'active'), archived_at IS NOT NULL
		  FROM sessions
		 WHERE tenant_id = $1 AND session_id = $2`,
		tenantID, sessionID).Scan(&botID, &state, &archived)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionInfo{}, ErrNoSession
	}
	if err != nil {
		return SessionInfo{}, fmt.Errorf("store: read session info: %w", err)
	}
	return SessionInfo{TenantID: tenantID, SessionID: sessionID, BotID: botID, State: state, Archived: archived}, nil
}

// SetSessionState records the collaboration lifecycle on the session row
// (active/done/cancelled). It is the durable counterpart of the Host's
// SessionIndex.SetState: a cancelled session is no longer an active parent.
func (s *Store) SetSessionState(ctx context.Context, tenantID, sessionID uuid.UUID, state string) error {
	if tenantID == uuid.Nil || sessionID == uuid.Nil {
		return ErrNoSession
	}
	switch state {
	case "active", "done", "cancelled":
	default:
		return fmt.Errorf("store: session state %q is not in the closed vocabulary", state)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE sessions SET state = $3, updated_at = now()
		  WHERE tenant_id = $1 AND session_id = $2`,
		tenantID, sessionID, state)
	if err != nil {
		return fmt.Errorf("store: set session state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoSession
	}
	return nil
}

// SubagentTreeRow is one node of the durable sub-agent tree projection.
type SubagentTreeRow struct {
	SessionID       uuid.UUID
	ParentSessionID uuid.UUID
	ModeID          string
	Depth           int
	BotID           string
	RunStatus       string
	CreatedAt       time.Time
}

// SubagentTree reads the spawned descendants of one session from the durable
// projection. It is the restart-safe tree the registry cannot provide: the
// in-process SubagentRunner index is cold after a restart while these rows
// survive. Forks are absent by construction (they never write the columns), so
// the tree is the delegation tree only.
func (s *Store) SubagentTree(ctx context.Context, tenantID, rootSessionID uuid.UUID) ([]SubagentTreeRow, error) {
	if tenantID == uuid.Nil || rootSessionID == uuid.Nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		WITH RECURSIVE tree AS (
		    SELECT session_id, subagent_parent_session_id, subagent_depth, subagent_mode,
		           COALESCE(bot_id, '') AS bot_id, archived_at, created_at, 0 AS level
		      FROM sessions
		     WHERE tenant_id = $1 AND session_id = $2
		    UNION ALL
		    SELECT c.session_id, c.subagent_parent_session_id, c.subagent_depth, c.subagent_mode,
		           COALESCE(c.bot_id, '') AS bot_id, c.archived_at, c.created_at, t.level + 1
		      FROM sessions c
		      JOIN tree t ON c.tenant_id = $1 AND c.subagent_parent_session_id = t.session_id
		     WHERE t.level < 64
		)
		SELECT t.session_id, t.subagent_parent_session_id, COALESCE(t.subagent_depth, 0),
		       COALESCE(t.subagent_mode, ''), t.bot_id, COALESCE(r.status, ''), t.created_at
		  FROM tree t
		  LEFT JOIN runs r ON r.tenant_id = $1 AND r.session_id = t.session_id
		 WHERE t.level > 0 AND t.archived_at IS NULL
		 ORDER BY t.subagent_depth, t.created_at, t.session_id
		 LIMIT $3`,
		tenantID, rootSessionID, MaxSubagentChildren)
	if err != nil {
		return nil, fmt.Errorf("store: read subagent tree: %w", err)
	}
	defer rows.Close()
	out := make([]SubagentTreeRow, 0)
	for rows.Next() {
		var (
			row      SubagentTreeRow
			parent   *uuid.UUID
			depth    int
			mode     string
			botID    string
			runState string
		)
		if err := rows.Scan(&row.SessionID, &parent, &depth, &mode, &botID, &runState, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan subagent tree: %w", err)
		}
		if parent != nil {
			row.ParentSessionID = *parent
		}
		row.Depth = depth
		row.ModeID = mode
		row.BotID = botID
		row.RunStatus = runState
		out = append(out, row)
	}
	return out, rows.Err()
}
