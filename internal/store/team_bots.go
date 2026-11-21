// TeamBot persistence (change keel-bot-identity-access, A1).
//
// A team_bots row is Mission + Roster. It has no account/profile/model columns
// because a TeamBot is not a permission subject (L12); the roster is a JSON
// document owned by internal/botteam, and revision/content_hash are its change
// identity and optimistic guard.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TeamBotRecord is one stored TeamBot row.
type TeamBotRecord struct {
	TenantID uuid.UUID
	// ID is the stable team handle (team_bots.team_bot_id).
	ID string
	// Name is the editable display name.
	Name string
	// Mission is the team objective injected into member/captain sessions.
	Mission string
	// Roster is the JSON array of bot references.
	Roster json.RawMessage
	// Revision is the monotonic content revision derived by botteam.Seal.
	Revision int64
	// ContentHash is the content hash derived by botteam.Seal.
	ContentHash string
	// Archived marks a retired team.
	Archived  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateTeamBot inserts a TeamBot. A duplicate handle is ErrConflict.
func (s *Store) CreateTeamBot(ctx context.Context, record TeamBotRecord) (TeamBotRecord, error) {
	if err := validateTeamBotRecord(record); err != nil {
		return TeamBotRecord{}, err
	}
	created, err := scanTeamBotRow(s.pool.QueryRow(ctx,
		`INSERT INTO team_bots
		    (tenant_id, team_bot_id, name, mission, roster, revision, content_hash, archived)
		 VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8)
		 RETURNING tenant_id, team_bot_id, name, mission, roster, revision,
		           content_hash, archived, created_at, updated_at`,
		record.TenantID, record.ID, record.Name, record.Mission, string(record.Roster),
		record.Revision, record.ContentHash, record.Archived))
	if err != nil {
		return TeamBotRecord{}, identityWriteError("create team bot", err)
	}
	return created, nil
}

// CreateTeamBotWithOwner inserts a TeamBot and its owner relation in one
// transaction. The owner is the creating account; TeamBot is a resource, never
// a permission subject (L12).
func (s *Store) CreateTeamBotWithOwner(ctx context.Context, record TeamBotRecord, ownerAccountID, createdBy uuid.UUID) (TeamBotRecord, error) {
	if err := validateTeamBotRecord(record); err != nil {
		return TeamBotRecord{}, err
	}
	if ownerAccountID == uuid.Nil || createdBy == uuid.Nil {
		return TeamBotRecord{}, invalidIdentity("create team bot", "owner and creator accounts are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TeamBotRecord{}, identityWriteError("create team bot", err)
	}
	defer tx.Rollback(ctx)
	created, err := scanTeamBotRow(tx.QueryRow(ctx,
		`INSERT INTO team_bots
		    (tenant_id, team_bot_id, name, mission, roster, revision, content_hash, archived)
		 VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8)
		 RETURNING tenant_id, team_bot_id, name, mission, roster, revision,
		           content_hash, archived, created_at, updated_at`,
		record.TenantID, record.ID, record.Name, record.Mission, string(record.Roster),
		record.Revision, record.ContentHash, record.Archived))
	if err != nil {
		return TeamBotRecord{}, identityWriteError("create team bot", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO resource_relations
		    (tenant_id, resource_type, resource_id, relation, subject_type, subject_id, created_by)
		 VALUES ($1, 'team_bot', $2, $3, $4, $5, $6)
		 ON CONFLICT (tenant_id, resource_type, resource_id, relation, subject_type, subject_id)
		 DO NOTHING`,
		record.TenantID, created.ID, RelationOwner, RelationSubjectAccount,
		ownerAccountID.String(), createdBy); err != nil {
		return TeamBotRecord{}, identityWriteError("create team bot owner relation", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamBotRecord{}, identityWriteError("create team bot", err)
	}
	return created, nil
}

// GetTeamBot reads one TeamBot. Another tenant's team is ErrNotFound.
func (s *Store) GetTeamBot(ctx context.Context, tenantID uuid.UUID, teamBotID string) (TeamBotRecord, error) {
	teamBotID = strings.TrimSpace(teamBotID)
	if tenantID == uuid.Nil || teamBotID == "" {
		return TeamBotRecord{}, invalidIdentity("get team bot", "tenant and team bot ids are required")
	}
	record, err := scanTeamBotRow(s.pool.QueryRow(ctx,
		`SELECT tenant_id, team_bot_id, name, mission, roster, revision,
		        content_hash, archived, created_at, updated_at
		   FROM team_bots WHERE tenant_id = $1 AND team_bot_id = $2`, tenantID, teamBotID))
	if err != nil {
		return TeamBotRecord{}, identityWriteError("get team bot", err)
	}
	return record, nil
}

// ListTeamBots returns up to limit TeamBots ordered by handle. Archived teams
// are excluded unless includeArchived is true.
func (s *Store) ListTeamBots(ctx context.Context, tenantID uuid.UUID, includeArchived bool, limit int) ([]TeamBotRecord, error) {
	if tenantID == uuid.Nil {
		return nil, invalidIdentity("list team bots", "tenant id is required")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, team_bot_id, name, mission, roster, revision,
		        content_hash, archived, created_at, updated_at
		   FROM team_bots
		  WHERE tenant_id = $1 AND ($2 OR NOT archived)
		  ORDER BY team_bot_id
		  LIMIT $3`, tenantID, includeArchived, identityListLimit(limit))
	if err != nil {
		return nil, identityWriteError("list team bots", err)
	}
	defer rows.Close()
	out := make([]TeamBotRecord, 0, 8)
	for rows.Next() {
		record, err := scanTeamBotRow(rows)
		if err != nil {
			return nil, identityWriteError("scan team bot", err)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("list team bots", err)
	}
	return out, nil
}

// UpdateTeamBot replaces the content, guarded by expectedRevision. A row whose
// revision moved is ErrConflict; a missing/foreign row is ErrNotFound. An
// archived team can be updated but not unarchived by this call.
func (s *Store) UpdateTeamBot(ctx context.Context, record TeamBotRecord, expectedRevision int64) (TeamBotRecord, error) {
	if err := validateTeamBotRecord(record); err != nil {
		return TeamBotRecord{}, err
	}
	updated, err := scanTeamBotRow(s.pool.QueryRow(ctx,
		`UPDATE team_bots
		    SET name = $3, mission = $4, roster = $5::jsonb,
		        revision = $6, content_hash = $7, archived = $8, updated_at = now()
		  WHERE tenant_id = $1 AND team_bot_id = $2 AND revision = $9
		    AND ($8 OR NOT archived)
		  RETURNING tenant_id, team_bot_id, name, mission, roster, revision,
		            content_hash, archived, created_at, updated_at`,
		record.TenantID, record.ID, record.Name, record.Mission, string(record.Roster),
		record.Revision, record.ContentHash, record.Archived, expectedRevision))
	if err == nil {
		return updated, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TeamBotRecord{}, identityWriteError("update team bot", err)
	}
	if _, existsErr := s.GetTeamBot(ctx, record.TenantID, record.ID); existsErr != nil {
		return TeamBotRecord{}, existsErr
	}
	return TeamBotRecord{}, conflictIdentity("update team bot", "revision moved (expected %d)", expectedRevision)
}

// ArchiveTeamBot marks a TeamBot archived. It is idempotent. Archiving is a
// lifecycle write, not a content edit: the revision and content hash do not
// move (the hash covers content only).
func (s *Store) ArchiveTeamBot(ctx context.Context, tenantID uuid.UUID, teamBotID string) error {
	teamBotID = strings.TrimSpace(teamBotID)
	if tenantID == uuid.Nil || teamBotID == "" {
		return invalidIdentity("archive team bot", "tenant and team bot ids are required")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE team_bots SET archived = true, updated_at = now()
		  WHERE tenant_id = $1 AND team_bot_id = $2`, tenantID, teamBotID)
	if err != nil {
		return identityWriteError("archive team bot", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	if _, err := s.GetTeamBot(ctx, tenantID, teamBotID); err != nil {
		return err
	}
	return nil
}

func validateTeamBotRecord(record TeamBotRecord) error {
	switch {
	case record.TenantID == uuid.Nil:
		return invalidIdentity("team bot", "tenant id is required")
	case strings.TrimSpace(record.ID) == "":
		return invalidIdentity("team bot", "team bot id is required")
	case strings.TrimSpace(record.Name) == "":
		return invalidIdentity("team bot", "name is required")
	case strings.TrimSpace(record.Mission) == "":
		return invalidIdentity("team bot", "mission is required")
	case len(record.Roster) == 0 || !json.Valid(record.Roster):
		return invalidIdentity("team bot", "roster is not valid JSON")
	case record.Revision < 0:
		return invalidIdentity("team bot", "revision must not be negative")
	}
	return nil
}

func scanTeamBotRow(row rowScanner) (TeamBotRecord, error) {
	var (
		record TeamBotRecord
		roster []byte
	)
	err := row.Scan(&record.TenantID, &record.ID, &record.Name, &record.Mission,
		&roster, &record.Revision, &record.ContentHash, &record.Archived,
		&record.CreatedAt, &record.UpdatedAt)
	record.Roster = json.RawMessage(roster)
	return record, err
}
