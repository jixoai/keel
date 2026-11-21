// Accounts, roles and profiles: the identity graph of a tenant
// (change keel-bot-identity-access, A1).
//
// This layer is scalar and document-free: it stores exactly the columns the
// schema declares and never imports internal/botteam (the architecture guard
// keeps the domain package a value package). Validation here is the minimum a
// SQL write needs; normalization and vocabulary checks belong to botteam and
// the A2/A4 callers.
package store

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AccountRecord is one tenant account row. PasswordHash is the Argon2id
// envelope as produced by A2; the store never interprets it.
type AccountRecord struct {
	TenantID     uuid.UUID
	ID           uuid.UUID
	Username     string
	DisplayName  string
	Kind         string
	PasswordHash string
	IsSuperAdmin bool
	DisabledAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// RoleRecord is one role row: identity and soul, never a permission source.
type RoleRecord struct {
	TenantID    uuid.UUID
	ID          uuid.UUID
	Name        string
	Instruction string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ProfileRecord binds one account to one role.
type ProfileRecord struct {
	TenantID  uuid.UUID
	ID        uuid.UUID
	AccountID uuid.UUID
	RoleID    uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateAccount inserts an account. A duplicate account id or username is
// ErrConflict; a missing tenant is not checked here (tenants are provisioned by
// the operator path, and accounts have no FK to them for the same reason
// model_settings/agents do not).
func (s *Store) CreateAccount(ctx context.Context, record AccountRecord) (AccountRecord, error) {
	if err := validateAccountRecord(record); err != nil {
		return AccountRecord{}, err
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO accounts
		    (tenant_id, account_id, username, display_name, kind, password_hash,
		     is_super_admin, disabled_at)
		 VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8)
		 RETURNING tenant_id, account_id, username, display_name, kind,
		           COALESCE(password_hash, ''), is_super_admin, disabled_at,
		           created_at, updated_at`,
		record.TenantID, record.ID, record.Username, record.DisplayName, record.Kind,
		record.PasswordHash, record.IsSuperAdmin, record.DisabledAt)
	out, err := scanAccountRow(row)
	if err != nil {
		return AccountRecord{}, identityWriteError("create account", err)
	}
	return out, nil
}

// GetAccount reads one account by id. Another tenant's account is ErrNotFound.
func (s *Store) GetAccount(ctx context.Context, tenantID, accountID uuid.UUID) (AccountRecord, error) {
	record, err := scanAccountRow(s.pool.QueryRow(ctx,
		`SELECT tenant_id, account_id, username, display_name, kind,
		        COALESCE(password_hash, ''), is_super_admin, disabled_at,
		        created_at, updated_at
		   FROM accounts WHERE tenant_id = $1 AND account_id = $2`,
		tenantID, accountID))
	if err != nil {
		return AccountRecord{}, identityWriteError("get account", err)
	}
	return record, nil
}

// GetAccountByUsername reads one account by login name. The match is
// case-insensitive, mirroring the unique index; the stored spelling is
// returned.
func (s *Store) GetAccountByUsername(ctx context.Context, tenantID uuid.UUID, username string) (AccountRecord, error) {
	username = strings.TrimSpace(username)
	if tenantID == uuid.Nil || username == "" {
		return AccountRecord{}, invalidIdentity("get account by username", "tenant and username are required")
	}
	record, err := scanAccountRow(s.pool.QueryRow(ctx,
		`SELECT tenant_id, account_id, username, display_name, kind,
		        COALESCE(password_hash, ''), is_super_admin, disabled_at,
		        created_at, updated_at
		   FROM accounts WHERE tenant_id = $1 AND lower(username) = lower($2)`,
		tenantID, username))
	if err != nil {
		return AccountRecord{}, identityWriteError("get account by username", err)
	}
	return record, nil
}

// ListAccounts returns up to limit accounts ordered by username. The result is
// always bounded (see identityListLimit).
func (s *Store) ListAccounts(ctx context.Context, tenantID uuid.UUID, limit int) ([]AccountRecord, error) {
	if tenantID == uuid.Nil {
		return nil, invalidIdentity("list accounts", "tenant id is required")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, account_id, username, display_name, kind,
		        COALESCE(password_hash, ''), is_super_admin, disabled_at,
		        created_at, updated_at
		   FROM accounts WHERE tenant_id = $1
		  ORDER BY lower(username), account_id
		  LIMIT $2`, tenantID, identityListLimit(limit))
	if err != nil {
		return nil, identityWriteError("list accounts", err)
	}
	defer rows.Close()
	out := make([]AccountRecord, 0, 8)
	for rows.Next() {
		record, err := scanAccountRow(rows)
		if err != nil {
			return nil, identityWriteError("scan account", err)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("list accounts", err)
	}
	return out, nil
}

// UpdateAccountDisplayName updates the display name. Username (login identity)
// and kind are not changed by this method.
func (s *Store) UpdateAccountDisplayName(ctx context.Context, tenantID, accountID uuid.UUID, displayName string) (AccountRecord, error) {
	displayName = strings.TrimSpace(displayName)
	if tenantID == uuid.Nil || accountID == uuid.Nil || displayName == "" {
		return AccountRecord{}, invalidIdentity("update account", "tenant, account and display name are required")
	}
	record, err := scanAccountRow(s.pool.QueryRow(ctx,
		`UPDATE accounts SET display_name = $3, updated_at = now()
		  WHERE tenant_id = $1 AND account_id = $2
		  RETURNING tenant_id, account_id, username, display_name, kind,
		            COALESCE(password_hash, ''), is_super_admin, disabled_at,
		            created_at, updated_at`,
		tenantID, accountID, displayName))
	if err != nil {
		return AccountRecord{}, identityWriteError("update account", err)
	}
	return record, nil
}

// SetAccountPasswordHash replaces the stored credential hash. An empty hash is
// allowed (bot/service accounts and a password reset-to-unset); A2 decides when
// that is legal.
func (s *Store) SetAccountPasswordHash(ctx context.Context, tenantID, accountID uuid.UUID, passwordHash string) error {
	if tenantID == uuid.Nil || accountID == uuid.Nil {
		return invalidIdentity("set account password", "tenant and account ids are required")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE accounts SET password_hash = NULLIF($3, ''), updated_at = now()
		  WHERE tenant_id = $1 AND account_id = $2`, tenantID, accountID, passwordHash)
	if err != nil {
		return identityWriteError("set account password", err)
	}
	return identityRowsResult("set account password", tag.RowsAffected())
}

// SetAccountDisabled soft-deletes (disables) or re-enables an account. Disabling
// sets the mark once; enabling clears it. The row, its bots and its audit
// history stay either way.
func (s *Store) SetAccountDisabled(ctx context.Context, tenantID, accountID uuid.UUID, disabled bool) error {
	if tenantID == uuid.Nil || accountID == uuid.Nil {
		return invalidIdentity("set account disabled", "tenant and account ids are required")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE accounts
		    SET disabled_at = CASE WHEN $3 THEN COALESCE(disabled_at, now()) ELSE NULL END,
		        updated_at = now()
		  WHERE tenant_id = $1 AND account_id = $2`, tenantID, accountID, disabled)
	if err != nil {
		return identityWriteError("set account disabled", err)
	}
	return identityRowsResult("set account disabled", tag.RowsAffected())
}

// CreateRole inserts a role.
func (s *Store) CreateRole(ctx context.Context, record RoleRecord) (RoleRecord, error) {
	if err := validateRoleRecord(record); err != nil {
		return RoleRecord{}, err
	}
	record, err := scanRoleRow(s.pool.QueryRow(ctx,
		`INSERT INTO roles (tenant_id, role_id, name, instruction)
		 VALUES ($1, $2, $3, $4)
		 RETURNING tenant_id, role_id, name, instruction, created_at, updated_at`,
		record.TenantID, record.ID, record.Name, record.Instruction))
	if err != nil {
		return RoleRecord{}, identityWriteError("create role", err)
	}
	return record, nil
}

// GetRole reads one role.
func (s *Store) GetRole(ctx context.Context, tenantID, roleID uuid.UUID) (RoleRecord, error) {
	record, err := scanRoleRow(s.pool.QueryRow(ctx,
		`SELECT tenant_id, role_id, name, instruction, created_at, updated_at
		   FROM roles WHERE tenant_id = $1 AND role_id = $2`, tenantID, roleID))
	if err != nil {
		return RoleRecord{}, identityWriteError("get role", err)
	}
	return record, nil
}

// ListRoles returns up to limit roles ordered by name.
func (s *Store) ListRoles(ctx context.Context, tenantID uuid.UUID, limit int) ([]RoleRecord, error) {
	if tenantID == uuid.Nil {
		return nil, invalidIdentity("list roles", "tenant id is required")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, role_id, name, instruction, created_at, updated_at
		   FROM roles WHERE tenant_id = $1
		  ORDER BY lower(name), role_id
		  LIMIT $2`, tenantID, identityListLimit(limit))
	if err != nil {
		return nil, identityWriteError("list roles", err)
	}
	defer rows.Close()
	out := make([]RoleRecord, 0, 8)
	for rows.Next() {
		record, err := scanRoleRow(rows)
		if err != nil {
			return nil, identityWriteError("scan role", err)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("list roles", err)
	}
	return out, nil
}

// UpdateRole replaces the role's name and instruction.
func (s *Store) UpdateRole(ctx context.Context, record RoleRecord) (RoleRecord, error) {
	if err := validateRoleRecord(record); err != nil {
		return RoleRecord{}, err
	}
	updated, err := scanRoleRow(s.pool.QueryRow(ctx,
		`UPDATE roles SET name = $3, instruction = $4, updated_at = now()
		  WHERE tenant_id = $1 AND role_id = $2
		  RETURNING tenant_id, role_id, name, instruction, created_at, updated_at`,
		record.TenantID, record.ID, record.Name, record.Instruction))
	if err != nil {
		return RoleRecord{}, identityWriteError("update role", err)
	}
	return updated, nil
}

// DeleteRole removes a role. A profile that still references it makes the
// delete ErrConflict (the FK is RESTRICT): rebind or delete the profile first.
func (s *Store) DeleteRole(ctx context.Context, tenantID, roleID uuid.UUID) error {
	if tenantID == uuid.Nil || roleID == uuid.Nil {
		return invalidIdentity("delete role", "tenant and role ids are required")
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM roles WHERE tenant_id = $1 AND role_id = $2`, tenantID, roleID)
	if err != nil {
		return identityWriteError("delete role", err)
	}
	return identityRowsResult("delete role", tag.RowsAffected())
}

// CreateProfile inserts a profile. A missing account or role is ErrConflict
// (the composite FK is tenant-consistent).
func (s *Store) CreateProfile(ctx context.Context, record ProfileRecord) (ProfileRecord, error) {
	if err := validateProfileRecord(record); err != nil {
		return ProfileRecord{}, err
	}
	record, err := scanProfileRow(s.pool.QueryRow(ctx,
		`INSERT INTO profiles (tenant_id, profile_id, account_id, role_id)
		 VALUES ($1, $2, $3, $4)
		 RETURNING tenant_id, profile_id, account_id, role_id, created_at, updated_at`,
		record.TenantID, record.ID, record.AccountID, record.RoleID))
	if err != nil {
		return ProfileRecord{}, identityWriteError("create profile", err)
	}
	return record, nil
}

// GetProfile reads one profile.
func (s *Store) GetProfile(ctx context.Context, tenantID, profileID uuid.UUID) (ProfileRecord, error) {
	record, err := scanProfileRow(s.pool.QueryRow(ctx,
		`SELECT tenant_id, profile_id, account_id, role_id, created_at, updated_at
		   FROM profiles WHERE tenant_id = $1 AND profile_id = $2`, tenantID, profileID))
	if err != nil {
		return ProfileRecord{}, identityWriteError("get profile", err)
	}
	return record, nil
}

// ListProfiles returns up to limit profiles ordered by profile id.
func (s *Store) ListProfiles(ctx context.Context, tenantID uuid.UUID, limit int) ([]ProfileRecord, error) {
	return s.listProfiles(ctx, tenantID, uuid.Nil, limit)
}

// ListProfilesForAccount returns up to limit profiles of one account.
func (s *Store) ListProfilesForAccount(ctx context.Context, tenantID, accountID uuid.UUID, limit int) ([]ProfileRecord, error) {
	if accountID == uuid.Nil {
		return nil, invalidIdentity("list profiles", "account id is required")
	}
	return s.listProfiles(ctx, tenantID, accountID, limit)
}

func (s *Store) listProfiles(ctx context.Context, tenantID, accountID uuid.UUID, limit int) ([]ProfileRecord, error) {
	if tenantID == uuid.Nil {
		return nil, invalidIdentity("list profiles", "tenant id is required")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, profile_id, account_id, role_id, created_at, updated_at
		   FROM profiles
		  WHERE tenant_id = $1 AND ($2::uuid IS NULL OR account_id = $2)
		  ORDER BY profile_id
		  LIMIT $3`, tenantID, nullableUUID(accountID), identityListLimit(limit))
	if err != nil {
		return nil, identityWriteError("list profiles", err)
	}
	defer rows.Close()
	out := make([]ProfileRecord, 0, 8)
	for rows.Next() {
		record, err := scanProfileRow(rows)
		if err != nil {
			return nil, identityWriteError("scan profile", err)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("list profiles", err)
	}
	return out, nil
}

// UpdateProfile rebinds a profile to another account/role pair.
func (s *Store) UpdateProfile(ctx context.Context, record ProfileRecord) (ProfileRecord, error) {
	if err := validateProfileRecord(record); err != nil {
		return ProfileRecord{}, err
	}
	updated, err := scanProfileRow(s.pool.QueryRow(ctx,
		`UPDATE profiles SET account_id = $3, role_id = $4, updated_at = now()
		  WHERE tenant_id = $1 AND profile_id = $2
		  RETURNING tenant_id, profile_id, account_id, role_id, created_at, updated_at`,
		record.TenantID, record.ID, record.AccountID, record.RoleID))
	if err != nil {
		return ProfileRecord{}, identityWriteError("update profile", err)
	}
	return updated, nil
}

// DeleteProfile removes a profile. A bot still bound to it makes the delete
// ErrConflict; rebind the bot first.
func (s *Store) DeleteProfile(ctx context.Context, tenantID, profileID uuid.UUID) error {
	if tenantID == uuid.Nil || profileID == uuid.Nil {
		return invalidIdentity("delete profile", "tenant and profile ids are required")
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM profiles WHERE tenant_id = $1 AND profile_id = $2`, tenantID, profileID)
	if err != nil {
		return identityWriteError("delete profile", err)
	}
	return identityRowsResult("delete profile", tag.RowsAffected())
}

// identityRowsResult turns a zero-row write into ErrNotFound.
func identityRowsResult(op string, rows int64) error {
	if rows == 0 {
		return identityWriteError(op, pgx.ErrNoRows)
	}
	return nil
}

// nullableUUID renders the nil uuid as NULL for an optional filter.
func nullableUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func validateAccountRecord(record AccountRecord) error {
	switch {
	case record.TenantID == uuid.Nil:
		return invalidIdentity("account", "tenant id is required")
	case record.ID == uuid.Nil:
		return invalidIdentity("account", "account id is required")
	case strings.TrimSpace(record.Username) == "":
		return invalidIdentity("account", "username is required")
	case strings.TrimSpace(record.DisplayName) == "":
		return invalidIdentity("account", "display name is required")
	case !validAccountKind(record.Kind):
		return invalidIdentity("account", "unknown kind %q (want human, bot or service)", record.Kind)
	}
	return nil
}

// validAccountKind mirrors the accounts_kind_check constraint. The store stays
// independent of botteam; the database constraint and this check say the same
// three words.
func validAccountKind(kind string) bool {
	return kind == "human" || kind == "bot" || kind == "service"
}

func validateRoleRecord(record RoleRecord) error {
	switch {
	case record.TenantID == uuid.Nil:
		return invalidIdentity("role", "tenant id is required")
	case record.ID == uuid.Nil:
		return invalidIdentity("role", "role id is required")
	case strings.TrimSpace(record.Name) == "":
		return invalidIdentity("role", "name is required")
	}
	return nil
}

func validateProfileRecord(record ProfileRecord) error {
	switch {
	case record.TenantID == uuid.Nil:
		return invalidIdentity("profile", "tenant id is required")
	case record.ID == uuid.Nil:
		return invalidIdentity("profile", "profile id is required")
	case record.AccountID == uuid.Nil:
		return invalidIdentity("profile", "account id is required")
	case record.RoleID == uuid.Nil:
		return invalidIdentity("profile", "role id is required")
	}
	return nil
}

func scanAccountRow(row rowScanner) (AccountRecord, error) {
	var record AccountRecord
	err := row.Scan(&record.TenantID, &record.ID, &record.Username, &record.DisplayName,
		&record.Kind, &record.PasswordHash, &record.IsSuperAdmin, &record.DisabledAt,
		&record.CreatedAt, &record.UpdatedAt)
	return record, err
}

func scanRoleRow(row rowScanner) (RoleRecord, error) {
	var record RoleRecord
	err := row.Scan(&record.TenantID, &record.ID, &record.Name, &record.Instruction,
		&record.CreatedAt, &record.UpdatedAt)
	return record, err
}

func scanProfileRow(row rowScanner) (ProfileRecord, error) {
	var record ProfileRecord
	err := row.Scan(&record.TenantID, &record.ID, &record.AccountID, &record.RoleID,
		&record.CreatedAt, &record.UpdatedAt)
	return record, err
}
