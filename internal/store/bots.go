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
