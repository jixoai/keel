// Bot persistence (change keel-bot-identity-access, A1).
//
// A bots row is tenant + a text handle + the columns the list/guard queries
// need + two JSON documents (default_model, harness). The documents are owned
// by internal/botteam; this layer only proves they are JSON and never imports
// the domain package (the architecture guard keeps that boundary).
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

// BotRecord is one stored bot row.
type BotRecord struct {
	TenantID uuid.UUID
	// ID is the stable bot handle (bots.bot_id).
	ID string
	// Name is the editable display name.
	Name string
	// ProfileID is the Profile that supplies Account and Role.
	ProfileID uuid.UUID
	// ModelKind is the bot's model working mode (text/image/decision/...).
	ModelKind string
	// DefaultModel is the provider/model JSON document.
	DefaultModel json.RawMessage
	// Harness is the tools + context JSON document.
	Harness json.RawMessage
	// Archived marks a retired bot; the row stays for history and its sessions.
	Archived  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateBot inserts a bot. A duplicate handle is ErrConflict, and so is a
// missing Profile (the composite FK is tenant-consistent). created=false is not
// an outcome: a create that does not insert is always an error.
func (s *Store) CreateBot(ctx context.Context, record BotRecord) (BotRecord, error) {
	if err := validateBotRecord(record); err != nil {
		return BotRecord{}, err
	}
	created, err := scanBotRow(s.pool.QueryRow(ctx,
		`INSERT INTO bots
		    (tenant_id, bot_id, name, profile_id, model_kind, default_model, harness, archived)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb, $8)
		 RETURNING tenant_id, bot_id, name, profile_id, model_kind, default_model,
		           harness, archived, created_at, updated_at`,
		record.TenantID, record.ID, record.Name, record.ProfileID, record.ModelKind,
		string(record.DefaultModel), string(record.Harness), record.Archived))
	if err != nil {
		return BotRecord{}, identityWriteError("create bot", err)
	}
	return created, nil
}

// CreateBotWithOwner inserts a Bot and its owner relation in one transaction
// (A4.2a §7). The owner account is the one named by the Bot's Profile; the
// relation is what single-resource Bot routes authorize against. A failure
// leaves neither row behind, so no Bot can exist without an owner.
func (s *Store) CreateBotWithOwner(ctx context.Context, record BotRecord, ownerAccountID, createdBy uuid.UUID) (BotRecord, error) {
	if err := validateBotRecord(record); err != nil {
		return BotRecord{}, err
	}
	if ownerAccountID == uuid.Nil || createdBy == uuid.Nil {
		return BotRecord{}, invalidIdentity("create bot", "owner and creator accounts are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return BotRecord{}, identityWriteError("create bot", err)
	}
	defer tx.Rollback(ctx)
	// The handler decided ownerAccountID from an earlier read. Re-read the
	// profile under a share lock so a concurrent profile rebind either commits
	// before this check (we see it and refuse) or waits until this transaction
	// ends (then it moves the new bot too).
	profileAccount, err := lockProfileAccount(ctx, tx, record.TenantID, record.ProfileID)
	if err != nil {
		return BotRecord{}, err
	}
	if profileAccount != ownerAccountID {
		return BotRecord{}, conflictIdentity("create bot", "profile owner changed concurrently")
	}
	created, err := scanBotRow(tx.QueryRow(ctx,
		`INSERT INTO bots
		    (tenant_id, bot_id, name, profile_id, model_kind, default_model, harness, archived)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb, $8)
		 RETURNING tenant_id, bot_id, name, profile_id, model_kind, default_model,
		           harness, archived, created_at, updated_at`,
		record.TenantID, record.ID, record.Name, record.ProfileID, record.ModelKind,
		string(record.DefaultModel), string(record.Harness), record.Archived))
	if err != nil {
		return BotRecord{}, identityWriteError("create bot", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO resource_relations
		    (tenant_id, resource_type, resource_id, relation, subject_type, subject_id, created_by)
		 VALUES ($1, 'bot', $2, $3, $4, $5, $6)
		 ON CONFLICT (tenant_id, resource_type, resource_id, relation, subject_type, subject_id)
		 DO NOTHING`,
		record.TenantID, created.ID, RelationOwner, RelationSubjectAccount,
		ownerAccountID.String(), createdBy); err != nil {
		return BotRecord{}, identityWriteError("create bot owner relation", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return BotRecord{}, identityWriteError("create bot", err)
	}
	return created, nil
}

// UpdateBotWithOwner replaces a Bot's editable content and moves its owner
// relation in the same transaction (Profile rebind). newOwner/oldOwner are
// zero when the Profile did not change. A stale CAS or a relation failure rolls
// the whole write back, so the Bot never reports a new owner it does not have.
func (s *Store) UpdateBotWithOwner(ctx context.Context, record BotRecord, expectedArchived bool, newOwner, oldOwner, actor uuid.UUID) (BotRecord, error) {
	if err := validateBotRecord(record); err != nil {
		return BotRecord{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return BotRecord{}, identityWriteError("update bot", err)
	}
	defer tx.Rollback(ctx)
	if newOwner != uuid.Nil {
		// Same lock/check as CreateBotWithOwner: the new owner came from an
		// earlier profile read, and a concurrent rebind must not leave this Bot
		// pointing at an account its profile no longer names.
		profileAccount, err := lockProfileAccount(ctx, tx, record.TenantID, record.ProfileID)
		if err != nil {
			return BotRecord{}, err
		}
		if profileAccount != newOwner {
			return BotRecord{}, conflictIdentity("update bot", "profile owner changed concurrently")
		}
	}
	updated, err := scanBotRow(tx.QueryRow(ctx,
		`UPDATE bots
		    SET name = $3, profile_id = $4, model_kind = $5,
		        default_model = $6::jsonb, harness = $7::jsonb,
		        archived = $8, updated_at = now()
		  WHERE tenant_id = $1 AND bot_id = $2 AND archived = $9
		    AND ($8 OR NOT archived)
		  RETURNING tenant_id, bot_id, name, profile_id, model_kind, default_model,
		            harness, archived, created_at, updated_at`,
		record.TenantID, record.ID, record.Name, record.ProfileID, record.ModelKind,
		string(record.DefaultModel), string(record.Harness), record.Archived, expectedArchived))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Distinguish "no such bot" from "the lifecycle bit moved first".
			if _, existsErr := s.GetBot(ctx, record.TenantID, record.ID); existsErr != nil {
				return BotRecord{}, existsErr
			}
			return BotRecord{}, conflictIdentity("update bot", "lifecycle changed concurrently (archived=%v)", expectedArchived)
		}
		return BotRecord{}, identityWriteError("update bot", err)
	}
	if newOwner != uuid.Nil && newOwner != oldOwner {
		if _, err := tx.Exec(ctx,
			`INSERT INTO resource_relations
			    (tenant_id, resource_type, resource_id, relation, subject_type, subject_id, created_by)
			 VALUES ($1, 'bot', $2, $3, $4, $5, $6)
			 ON CONFLICT (tenant_id, resource_type, resource_id, relation, subject_type, subject_id)
			 DO NOTHING`,
			record.TenantID, updated.ID, RelationOwner, RelationSubjectAccount,
			newOwner.String(), actor); err != nil {
			return BotRecord{}, identityWriteError("update bot owner relation", err)
		}
		if oldOwner != uuid.Nil {
			if _, err := tx.Exec(ctx,
				`DELETE FROM resource_relations
				  WHERE tenant_id = $1 AND resource_type = 'bot' AND resource_id = $2
				    AND relation = $3 AND subject_type = $4 AND subject_id = $5`,
				record.TenantID, updated.ID, RelationOwner, RelationSubjectAccount,
				oldOwner.String()); err != nil {
				return BotRecord{}, identityWriteError("delete stale bot owner relation", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return BotRecord{}, identityWriteError("update bot", err)
	}
	return updated, nil
}

// GetBot reads one bot. Another tenant's bot is ErrNotFound.
func (s *Store) GetBot(ctx context.Context, tenantID uuid.UUID, botID string) (BotRecord, error) {
	botID = strings.TrimSpace(botID)
	if tenantID == uuid.Nil || botID == "" {
		return BotRecord{}, invalidIdentity("get bot", "tenant and bot ids are required")
	}
	record, err := scanBotRow(s.pool.QueryRow(ctx,
		`SELECT tenant_id, bot_id, name, profile_id, model_kind, default_model,
		        harness, archived, created_at, updated_at
		   FROM bots WHERE tenant_id = $1 AND bot_id = $2`, tenantID, botID))
	if err != nil {
		return BotRecord{}, identityWriteError("get bot", err)
	}
	return record, nil
}

// ListBots returns up to limit bots ordered by handle. Archived bots are
// excluded unless includeArchived is true.
func (s *Store) ListBots(ctx context.Context, tenantID uuid.UUID, includeArchived bool, limit int) ([]BotRecord, error) {
	if tenantID == uuid.Nil {
		return nil, invalidIdentity("list bots", "tenant id is required")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, bot_id, name, profile_id, model_kind, default_model,
		        harness, archived, created_at, updated_at
		   FROM bots
		  WHERE tenant_id = $1 AND ($2 OR NOT archived)
		  ORDER BY bot_id
		  LIMIT $3`, tenantID, includeArchived, identityListLimit(limit))
	if err != nil {
		return nil, identityWriteError("list bots", err)
	}
	defer rows.Close()
	out := make([]BotRecord, 0, 8)
	for rows.Next() {
		record, err := scanBotRow(rows)
		if err != nil {
			return nil, identityWriteError("scan bot", err)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("list bots", err)
	}
	return out, nil
}

// UpdateBot replaces a bot's editable content. expectedArchived is the
// lifecycle bit the caller read: a concurrent archive turns the write into
// ErrConflict instead of silently resurrecting the bot. A bot can be archived
// through this call but never unarchived by it; ArchiveBot is the one-way
// lifecycle write.
func (s *Store) UpdateBot(ctx context.Context, record BotRecord, expectedArchived bool) (BotRecord, error) {
	if err := validateBotRecord(record); err != nil {
		return BotRecord{}, err
	}
	updated, err := scanBotRow(s.pool.QueryRow(ctx,
		`UPDATE bots
		    SET name = $3, profile_id = $4, model_kind = $5,
		        default_model = $6::jsonb, harness = $7::jsonb,
		        archived = $8, updated_at = now()
		  WHERE tenant_id = $1 AND bot_id = $2 AND archived = $9
		    AND ($8 OR NOT archived)
		  RETURNING tenant_id, bot_id, name, profile_id, model_kind, default_model,
		            harness, archived, created_at, updated_at`,
		record.TenantID, record.ID, record.Name, record.ProfileID, record.ModelKind,
		string(record.DefaultModel), string(record.Harness), record.Archived, expectedArchived))
	if err == nil {
		return updated, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return BotRecord{}, identityWriteError("update bot", err)
	}
	// Distinguish "no such bot" from "the lifecycle bit moved first".
	if _, existsErr := s.GetBot(ctx, record.TenantID, record.ID); existsErr != nil {
		return BotRecord{}, existsErr
	}
	return BotRecord{}, conflictIdentity("update bot", "lifecycle changed concurrently (archived=%v)", expectedArchived)
}

// ArchiveBot marks a bot archived. It is idempotent: archiving an already
// archived bot reports success. Existing sessions are kept; new sessions are
// A4's rule.
func (s *Store) ArchiveBot(ctx context.Context, tenantID uuid.UUID, botID string) error {
	botID = strings.TrimSpace(botID)
	if tenantID == uuid.Nil || botID == "" {
		return invalidIdentity("archive bot", "tenant and bot ids are required")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE bots SET archived = true, updated_at = now()
		  WHERE tenant_id = $1 AND bot_id = $2`, tenantID, botID)
	if err != nil {
		return identityWriteError("archive bot", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	if _, err := s.GetBot(ctx, tenantID, botID); err != nil {
		return err
	}
	return nil
}

// lockProfileAccount reads a profile's account under a share lock. The owner a
// caller wants to write came from an earlier profile read; this lock plus the
// equality check in the caller closes the window where a concurrent profile
// rebind commits between that read and the bot write.
func lockProfileAccount(ctx context.Context, tx pgx.Tx, tenantID, profileID uuid.UUID) (uuid.UUID, error) {
	var accountID uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT account_id FROM profiles
		  WHERE tenant_id = $1 AND profile_id = $2
		  FOR SHARE`, tenantID, profileID).Scan(&accountID); err != nil {
		return uuid.Nil, identityWriteError("lock profile", err)
	}
	return accountID, nil
}

func validateBotRecord(record BotRecord) error {
	switch {
	case record.TenantID == uuid.Nil:
		return invalidIdentity("bot", "tenant id is required")
	case strings.TrimSpace(record.ID) == "":
		return invalidIdentity("bot", "bot id is required")
	case strings.TrimSpace(record.Name) == "":
		return invalidIdentity("bot", "name is required")
	case record.ProfileID == uuid.Nil:
		return invalidIdentity("bot", "profile id is required")
	case strings.TrimSpace(record.ModelKind) == "":
		return invalidIdentity("bot", "model kind is required")
	case len(record.DefaultModel) == 0 || !json.Valid(record.DefaultModel):
		return invalidIdentity("bot", "default model is not valid JSON")
	case len(record.Harness) == 0 || !json.Valid(record.Harness):
		return invalidIdentity("bot", "harness is not valid JSON")
	}
	return nil
}

func scanBotRow(row rowScanner) (BotRecord, error) {
	var (
		record       BotRecord
		defaultModel []byte
		harness      []byte
	)
	err := row.Scan(&record.TenantID, &record.ID, &record.Name, &record.ProfileID,
		&record.ModelKind, &defaultModel, &harness, &record.Archived,
		&record.CreatedAt, &record.UpdatedAt)
	record.DefaultModel = json.RawMessage(defaultModel)
	record.Harness = json.RawMessage(harness)
	return record, err
}
