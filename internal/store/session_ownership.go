// Session ownership (change keel-bot-identity-access, A4.1).
//
// A session belongs to exactly one Bot and, through that Bot's Profile, to one
// Account (L10). created_by_account_id records the initiator (the human or
// trigger that asked), which is not necessarily the owner. The owner relation
// is written in the same transaction so "default private" is true from the
// first row: no other account has a relation until it is explicitly shared.
//
// The columns already exist (A1 added them nullable); this file closes the
// write contract without changing the schema.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SessionOwnership is the ownership tuple of one session.
type SessionOwnership struct {
	TenantID uuid.UUID
	// SessionID is the session row identity.
	SessionID uuid.UUID
	// BotID is the owning Bot handle.
	BotID string
	// AccountID is the Bot's owner account (derived from its Profile).
	AccountID uuid.UUID
	// CreatedByAccountID is the initiator. It may equal AccountID.
	CreatedByAccountID uuid.UUID
	// TeamBotID is the optional mission snapshot (a TeamBot start).
	TeamBotID string
}

// CreateOwnedSession creates a session with its ownership and owner relation.
//
// The Bot's archived state and owner account are re-checked here rather than
// trusted from the caller: a Bot archived between the request check and this
// write must refuse the new session, and a Bot may never be attributed to an
// account other than the one named by its Profile. Repeating the same ownership
// is idempotent; changing it for a live session is ErrConflict.
func (s *Store) CreateOwnedSession(ctx context.Context, ownership SessionOwnership) error {
	ownership.BotID = strings.TrimSpace(ownership.BotID)
	ownership.TeamBotID = strings.TrimSpace(ownership.TeamBotID)
	switch {
	case ownership.TenantID == uuid.Nil || ownership.SessionID == uuid.Nil:
		return invalidIdentity("create owned session", "tenant and session ids are required")
	case ownership.BotID == "":
		return invalidIdentity("create owned session", "bot id is required (every session belongs to a bot)")
	case ownership.AccountID == uuid.Nil:
		return invalidIdentity("create owned session", "account id is required")
	case ownership.CreatedByAccountID == uuid.Nil:
		return invalidIdentity("create owned session", "created-by account id is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return identityWriteError("create owned session", err)
	}
	defer tx.Rollback(ctx)
	if err := verifySessionOwnerTargets(ctx, tx, ownership); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO sessions
		    (tenant_id, session_id, bot_id, account_id, created_by_account_id, team_bot_id)
		 VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
		 ON CONFLICT (tenant_id, session_id) DO NOTHING`,
		ownership.TenantID, ownership.SessionID, ownership.BotID, ownership.AccountID,
		ownership.CreatedByAccountID, ownership.TeamBotID)
	if err != nil {
		return identityWriteError("create owned session", err)
	}
	if tag.RowsAffected() == 0 {
		existing, err := readSessionOwnership(ctx, tx, ownership.TenantID, ownership.SessionID)
		if err != nil {
			return err
		}
		switch {
		case existing.BotID == ownership.BotID && existing.AccountID == ownership.AccountID:
			// Idempotent repeat of the same ownership.
		case existing.BotID == "" && existing.AccountID == uuid.Nil && existing.CreatedByAccountID == uuid.Nil:
			// The session was created unowned by the pre-A4 path (POST
			// /v1/sessions without a bot, or a UI shell that creates first). Claim
			// it atomically; a concurrent claim loses the WHERE bot_id IS NULL race.
			claimed, err := tx.Exec(ctx,
				`UPDATE sessions
				    SET bot_id = $3, account_id = $4, created_by_account_id = $5,
				        team_bot_id = NULLIF($6, ''), updated_at = now()
				  WHERE tenant_id = $1 AND session_id = $2 AND bot_id IS NULL`,
				ownership.TenantID, ownership.SessionID, ownership.BotID,
				ownership.AccountID, ownership.CreatedByAccountID, ownership.TeamBotID)
			if err != nil {
				return identityWriteError("create owned session", err)
			}
			if claimed.RowsAffected() == 0 {
				return conflictIdentity("create owned session", "session %s was claimed concurrently", ownership.SessionID)
			}
		default:
			return conflictIdentity("create owned session",
				"session %s is already owned by bot %q account %s", ownership.SessionID, existing.BotID, existing.AccountID)
		}
	}
	// Default private: the owner relation grants the Bot's account alone.
	if _, err := tx.Exec(ctx,
		`INSERT INTO resource_relations
		    (tenant_id, resource_type, resource_id, relation, subject_type, subject_id, created_by)
		 VALUES ($1, 'session', $2, $3, $4, $5, $6)
		 ON CONFLICT (tenant_id, resource_type, resource_id, relation, subject_type, subject_id)
		 DO NOTHING`,
		ownership.TenantID, ownership.SessionID.String(), RelationOwner,
		RelationSubjectAccount, ownership.AccountID.String(), ownership.CreatedByAccountID); err != nil {
		return identityWriteError("create owned session", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return identityWriteError("create owned session", err)
	}
	return nil
}

// CreateAccountOwnedSession creates the pre-A4 session shape (no Bot) and
// writes the caller's owner relation in one transaction. It is the "create
// without botId" path: the session is private to the creating account, and the
// relation gate can authorize its single-resource routes.
//
// A session already owned by a Bot is a conflict (the create must name that
// Bot); an existing unowned session is claimed; repeating the same claim is
// idempotent. A different account can never take over an existing owner.
func (s *Store) CreateAccountOwnedSession(ctx context.Context, tenantID, sessionID, accountID uuid.UUID) error {
	if tenantID == uuid.Nil || sessionID == uuid.Nil || accountID == uuid.Nil {
		return invalidIdentity("create account session", "tenant, session and account ids are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return identityWriteError("create account session", err)
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx,
		`INSERT INTO sessions (tenant_id, session_id)
		 VALUES ($1, $2) ON CONFLICT (tenant_id, session_id) DO NOTHING`,
		tenantID, sessionID)
	if err != nil {
		return identityWriteError("create account session", err)
	}
	if tag.RowsAffected() == 0 {
		existing, err := readSessionOwnership(ctx, tx, tenantID, sessionID)
		if err != nil {
			return err
		}
		if existing.BotID != "" || existing.AccountID != uuid.Nil {
			return conflictIdentity("create account session",
				"session %s is already owned by bot %q account %s",
				sessionID, existing.BotID, existing.AccountID)
		}
	}
	var owner *uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT subject_id FROM resource_relations
		  WHERE tenant_id = $1 AND resource_type = 'session' AND resource_id = $2
		    AND relation = $3 AND subject_type = $4
		  LIMIT 1`,
		tenantID, sessionID.String(), RelationOwner, RelationSubjectAccount).Scan(&owner)
	switch {
	case err == nil:
		if *owner != accountID {
			return conflictIdentity("create account session",
				"session %s already has a different owner relation", sessionID)
		}
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx,
			`INSERT INTO resource_relations
			    (tenant_id, resource_type, resource_id, relation, subject_type, subject_id, created_by)
			 VALUES ($1, 'session', $2, $3, $4, $5::text, $5::uuid)
			 ON CONFLICT (tenant_id, resource_type, resource_id, relation, subject_type, subject_id)
			 DO NOTHING`,
			tenantID, sessionID.String(), RelationOwner,
			RelationSubjectAccount, accountID.String()); err != nil {
			return identityWriteError("create account session", err)
		}
	default:
		return identityWriteError("create account session", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return identityWriteError("create account session", err)
	}
	return nil
}

// CreateSessionLikeParent creates a child session (sub-agent spawn/fork) with
// the parent's owner: the child belongs to the same Bot/account as the parent,
// or to the account named by the parent's owner relation for a legacy
// account-owned session. The child's single-resource routes then authorize by
// the same relation as the parent.
//
// A parent with neither ownership columns nor an owner relation is refused: a
// child whose owner is unknowable must not exist (fail closed).
func (s *Store) CreateSessionLikeParent(ctx context.Context, tenantID, parentSessionID, childSessionID uuid.UUID) error {
	if tenantID == uuid.Nil || parentSessionID == uuid.Nil || childSessionID == uuid.Nil {
		return invalidIdentity("inherit session owner", "tenant, parent and child ids are required")
	}
	parent, err := s.GetSessionOwnership(ctx, tenantID, parentSessionID)
	if err != nil {
		return err
	}
	if parent.BotID != "" || parent.AccountID != uuid.Nil {
		inherited := parent
		inherited.SessionID = childSessionID
		if inherited.CreatedByAccountID == uuid.Nil {
			inherited.CreatedByAccountID = parent.AccountID
		}
		return s.CreateOwnedSession(ctx, inherited)
	}
	owner, found, err := s.SessionOwnerAccount(ctx, tenantID, parentSessionID)
	if err != nil {
		return err
	}
	if !found {
		return conflictIdentity("inherit session owner",
			"parent session %s has no owner relation to inherit", parentSessionID)
	}
	return s.CreateAccountOwnedSession(ctx, tenantID, childSessionID, owner)
}

// CreateSpawnedSession creates the child session of a delegation. When botID
// names a member Bot, the child belongs to that Bot and its Profile's Account:
// the member runs with its own Account permissions and Harness, while
// created_by keeps the original initiator. A bindingless spawn (empty botID)
// keeps the pre-delegation behavior and inherits the parent's ownership.
//
// The team mission snapshot rides along: a member session inside a TeamBot run
// still records the TeamBot it was started for.
func (s *Store) CreateSpawnedSession(ctx context.Context, tenantID, parentSessionID, childSessionID uuid.UUID, botID string) error {
	botID = strings.TrimSpace(botID)
	if botID == "" {
		return s.CreateSessionLikeParent(ctx, tenantID, parentSessionID, childSessionID)
	}
	if tenantID == uuid.Nil || parentSessionID == uuid.Nil || childSessionID == uuid.Nil {
		return invalidIdentity("create spawned session", "tenant, parent and child ids are required")
	}
	parent, err := s.GetSessionOwnership(ctx, tenantID, parentSessionID)
	if err != nil {
		return err
	}
	bot, err := s.GetBot(ctx, tenantID, botID)
	if err != nil {
		return err
	}
	profile, err := s.GetProfile(ctx, tenantID, bot.ProfileID)
	if err != nil {
		return err
	}
	createdBy := parent.CreatedByAccountID
	if createdBy == uuid.Nil {
		createdBy = parent.AccountID
	}
	if err := s.CreateOwnedSession(ctx, SessionOwnership{
		TenantID: tenantID, SessionID: childSessionID,
		BotID: botID, AccountID: profile.AccountID,
		CreatedByAccountID: createdBy, TeamBotID: parent.TeamBotID,
	}); err != nil {
		return err
	}
	// The delegating account observes the child it spawned (the run page's
	// sub-agent DAG reads child sessions through session.read). The member stays
	// the owner; this is an explicit parent -> child viewer relation, not a
	// second owner.
	if parent.AccountID != uuid.Nil && parent.AccountID != profile.AccountID {
		if _, err := s.PutResourceRelation(ctx, ResourceRelation{
			TenantID: tenantID, ResourceType: "session", ResourceID: childSessionID.String(),
			Relation: RelationViewer, SubjectType: RelationSubjectAccount,
			SubjectID: parent.AccountID.String(), CreatedBy: createdBy,
		}); err != nil {
			return err
		}
	}
	return nil
}

// SessionOwnerAccount returns the account named by a session's owner relation.
// found=false is a session with no account owner tuple.
func (s *Store) SessionOwnerAccount(ctx context.Context, tenantID, sessionID uuid.UUID) (uuid.UUID, bool, error) {
	if tenantID == uuid.Nil || sessionID == uuid.Nil {
		return uuid.Nil, false, invalidIdentity("session owner account", "tenant and session ids are required")
	}
	var raw string
	err := s.pool.QueryRow(ctx,
		`SELECT subject_id FROM resource_relations
		  WHERE tenant_id = $1 AND resource_type = 'session' AND resource_id = $2
		    AND relation = $3 AND subject_type = $4
		  LIMIT 1`,
		tenantID, sessionID.String(), RelationOwner, RelationSubjectAccount).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, identityWriteError("session owner account", err)
	}
	accountID, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, false, identityWriteError("session owner account", err)
	}
	return accountID, true, nil
}

// GetSessionOwnership reads a session's ownership. A missing or foreign session
// is ErrNotFound.
func (s *Store) GetSessionOwnership(ctx context.Context, tenantID, sessionID uuid.UUID) (SessionOwnership, error) {
	if tenantID == uuid.Nil || sessionID == uuid.Nil {
		return SessionOwnership{}, invalidIdentity("get session ownership", "tenant and session ids are required")
	}
	return readSessionOwnership(ctx, s.pool, tenantID, sessionID)
}

type ownershipQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func readSessionOwnership(ctx context.Context, q ownershipQuerier, tenantID, sessionID uuid.UUID) (SessionOwnership, error) {
	var (
		ownership SessionOwnership
		botID     *string
		accountID *uuid.UUID
		createdBy *uuid.UUID
		teamBotID *string
	)
	err := q.QueryRow(ctx,
		`SELECT tenant_id, session_id, bot_id, account_id, created_by_account_id, team_bot_id
		   FROM sessions WHERE tenant_id = $1 AND session_id = $2`,
		tenantID, sessionID).Scan(&ownership.TenantID, &ownership.SessionID,
		&botID, &accountID, &createdBy, &teamBotID)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionOwnership{}, identityWriteError("get session ownership", err)
	}
	if err != nil {
		return SessionOwnership{}, identityWriteError("get session ownership", err)
	}
	if botID != nil {
		ownership.BotID = *botID
	}
	if accountID != nil {
		ownership.AccountID = *accountID
	}
	if createdBy != nil {
		ownership.CreatedByAccountID = *createdBy
	}
	if teamBotID != nil {
		ownership.TeamBotID = *teamBotID
	}
	return ownership, nil
}

// verifySessionOwnerTargets checks the referenced Bot (and optional TeamBot) in
// the same transaction as the write.
func verifySessionOwnerTargets(ctx context.Context, tx pgx.Tx, ownership SessionOwnership) error {
	var (
		archived  bool
		ownerAcct uuid.UUID
	)
	err := tx.QueryRow(ctx,
		`SELECT b.archived, p.account_id
		   FROM bots b
		   JOIN profiles p ON p.tenant_id = b.tenant_id AND p.profile_id = b.profile_id
		  WHERE b.tenant_id = $1 AND b.bot_id = $2`,
		ownership.TenantID, ownership.BotID).Scan(&archived, &ownerAcct)
	if errors.Is(err, pgx.ErrNoRows) {
		return identityWriteError("create owned session", err)
	}
	if err != nil {
		return identityWriteError("create owned session", err)
	}
	if archived {
		return fmt.Errorf("%w: bot %q is archived and refuses new sessions", ErrConflict, ownership.BotID)
	}
	if ownerAcct != ownership.AccountID {
		return conflictIdentity("create owned session",
			"bot %q belongs to account %s, not %s", ownership.BotID, ownerAcct, ownership.AccountID)
	}
	if ownership.TeamBotID != "" {
		var teamArchived bool
		err := tx.QueryRow(ctx,
			`SELECT archived FROM team_bots WHERE tenant_id = $1 AND team_bot_id = $2`,
			ownership.TenantID, ownership.TeamBotID).Scan(&teamArchived)
		if errors.Is(err, pgx.ErrNoRows) {
			return identityWriteError("create owned session", err)
		}
		if err != nil {
			return identityWriteError("create owned session", err)
		}
		if teamArchived {
			return fmt.Errorf("%w: team bot %q is archived and refuses new sessions", ErrConflict, ownership.TeamBotID)
		}
	}
	return nil
}
