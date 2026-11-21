// Permission groups and grants persistence (change keel-bot-identity-access,
// A1).
//
// The store keeps the rows; it does not evaluate them. A3's access package owns
// the key registry, scope ranking, union and grant ⊆ own. Here a grant is three
// strings and an id, and the only vocabulary enforced is the closed scope and
// subject-type set the database also checks.
package store

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PermissionGroupRecord is one permission-group row. Builtin groups are seeded
// by the deployment, not by ordinary API calls (A3/A4 own the seeding).
type PermissionGroupRecord struct {
	TenantID  uuid.UUID
	ID        uuid.UUID
	Name      string
	Builtin   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PermissionGroupMemberRecord is one account's membership in a group.
type PermissionGroupMemberRecord struct {
	TenantID  uuid.UUID
	GroupID   uuid.UUID
	AccountID uuid.UUID
	AddedBy   uuid.UUID
	CreatedAt time.Time
}

// PermissionGroupGrantRecord is one (key, scope) pair granted to a group.
type PermissionGroupGrantRecord struct {
	TenantID      uuid.UUID
	GroupID       uuid.UUID
	PermissionKey string
	Scope         string
}

// PermissionGrantRecord is one direct grant to an account or a group. The
// subject id is a uuid because both accounts and groups are uuid identities.
type PermissionGrantRecord struct {
	TenantID      uuid.UUID
	ID            uuid.UUID
	SubjectType   string
	SubjectID     uuid.UUID
	PermissionKey string
	Scope         string
	GrantedBy     uuid.UUID
	CreatedAt     time.Time
}

// Permission scopes, ordered from narrowest to widest. The ranking is A3's
// contract; the store only rejects values outside this set.
const (
	PermissionScopeOwn    = "own"
	PermissionScopeTeam   = "team"
	PermissionScopeTenant = "tenant"
	PermissionScopeAll    = "all"
)

// Permission subject type for direct grants. Group authorization is stored
// exclusively in permission_group_grants; a group subject on a direct grant is
// rejected so there is only one group-authorization truth (L5 / A1 decision 5).
const PermissionSubjectAccount = "account"

// ValidPermissionScope reports whether scope is in the closed scope set.
func ValidPermissionScope(scope string) bool {
	switch scope {
	case PermissionScopeOwn, PermissionScopeTeam, PermissionScopeTenant, PermissionScopeAll:
		return true
	}
	return false
}

// ValidPermissionSubject reports whether subjectType is the account (direct
// grants only).
func ValidPermissionSubject(subjectType string) bool {
	return subjectType == PermissionSubjectAccount
}

// CreatePermissionGroup inserts a group. A duplicate id or name (case folded)
// is ErrConflict.
func (s *Store) CreatePermissionGroup(ctx context.Context, record PermissionGroupRecord) (PermissionGroupRecord, error) {
	if err := validatePermissionGroupRecord(record); err != nil {
		return PermissionGroupRecord{}, err
	}
	created, err := scanPermissionGroupRow(s.pool.QueryRow(ctx,
		`INSERT INTO permission_groups (tenant_id, group_id, name, builtin)
		 VALUES ($1, $2, $3, $4)
		 RETURNING tenant_id, group_id, name, builtin, created_at, updated_at`,
		record.TenantID, record.ID, record.Name, record.Builtin))
	if err != nil {
		return PermissionGroupRecord{}, identityWriteError("create permission group", err)
	}
	return created, nil
}

// GetPermissionGroup reads one group.
func (s *Store) GetPermissionGroup(ctx context.Context, tenantID, groupID uuid.UUID) (PermissionGroupRecord, error) {
	record, err := scanPermissionGroupRow(s.pool.QueryRow(ctx,
		`SELECT tenant_id, group_id, name, builtin, created_at, updated_at
		   FROM permission_groups WHERE tenant_id = $1 AND group_id = $2`, tenantID, groupID))
	if err != nil {
		return PermissionGroupRecord{}, identityWriteError("get permission group", err)
	}
	return record, nil
}

// ListPermissionGroups returns up to limit groups ordered by name.
func (s *Store) ListPermissionGroups(ctx context.Context, tenantID uuid.UUID, limit int) ([]PermissionGroupRecord, error) {
	if tenantID == uuid.Nil {
		return nil, invalidIdentity("list permission groups", "tenant id is required")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, group_id, name, builtin, created_at, updated_at
		   FROM permission_groups WHERE tenant_id = $1
		  ORDER BY lower(name), group_id
		  LIMIT $2`, tenantID, identityListLimit(limit))
	if err != nil {
		return nil, identityWriteError("list permission groups", err)
	}
	defer rows.Close()
	out := make([]PermissionGroupRecord, 0, 8)
	for rows.Next() {
		record, err := scanPermissionGroupRow(rows)
		if err != nil {
			return nil, identityWriteError("scan permission group", err)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("list permission groups", err)
	}
	return out, nil
}

// RenamePermissionGroup changes a group's display name.
func (s *Store) RenamePermissionGroup(ctx context.Context, tenantID, groupID uuid.UUID, name string) (PermissionGroupRecord, error) {
	name = strings.TrimSpace(name)
	if tenantID == uuid.Nil || groupID == uuid.Nil || name == "" {
		return PermissionGroupRecord{}, invalidIdentity("rename permission group", "tenant, group and name are required")
	}
	updated, err := scanPermissionGroupRow(s.pool.QueryRow(ctx,
		`UPDATE permission_groups SET name = $3, updated_at = now()
		  WHERE tenant_id = $1 AND group_id = $2
		  RETURNING tenant_id, group_id, name, builtin, created_at, updated_at`,
		tenantID, groupID, name))
	if err != nil {
		return PermissionGroupRecord{}, identityWriteError("rename permission group", err)
	}
	return updated, nil
}

// DeletePermissionGroup removes a group; its memberships and group grants are
// deleted by the ON DELETE CASCADE foreign keys.
func (s *Store) DeletePermissionGroup(ctx context.Context, tenantID, groupID uuid.UUID) error {
	if tenantID == uuid.Nil || groupID == uuid.Nil {
		return invalidIdentity("delete permission group", "tenant and group ids are required")
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM permission_groups WHERE tenant_id = $1 AND group_id = $2`, tenantID, groupID)
	if err != nil {
		return identityWriteError("delete permission group", err)
	}
	return identityRowsResult("delete permission group", tag.RowsAffected())
}

// AddPermissionGroupMember adds an account to a group. created=false means the
// membership already existed (idempotent); a missing group/account is
// ErrConflict.
func (s *Store) AddPermissionGroupMember(ctx context.Context, tenantID, groupID, accountID, addedBy uuid.UUID) (bool, error) {
	if tenantID == uuid.Nil || groupID == uuid.Nil || accountID == uuid.Nil || addedBy == uuid.Nil {
		return false, invalidIdentity("add permission group member", "tenant, group, account and added-by ids are required")
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO permission_group_members (tenant_id, group_id, account_id, added_by)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (tenant_id, group_id, account_id) DO NOTHING`,
		tenantID, groupID, accountID, addedBy)
	if err != nil {
		return false, identityWriteError("add permission group member", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RemovePermissionGroupMember removes a membership; an absent one is
// ErrNotFound.
func (s *Store) RemovePermissionGroupMember(ctx context.Context, tenantID, groupID, accountID uuid.UUID) error {
	if tenantID == uuid.Nil || groupID == uuid.Nil || accountID == uuid.Nil {
		return invalidIdentity("remove permission group member", "tenant, group and account ids are required")
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM permission_group_members
		  WHERE tenant_id = $1 AND group_id = $2 AND account_id = $3`,
		tenantID, groupID, accountID)
	if err != nil {
		return identityWriteError("remove permission group member", err)
	}
	return identityRowsResult("remove permission group member", tag.RowsAffected())
}

// ListPermissionGroupMembers returns up to limit memberships of one group.
func (s *Store) ListPermissionGroupMembers(ctx context.Context, tenantID, groupID uuid.UUID, limit int) ([]PermissionGroupMemberRecord, error) {
	if tenantID == uuid.Nil || groupID == uuid.Nil {
		return nil, invalidIdentity("list permission group members", "tenant and group ids are required")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, group_id, account_id, added_by, created_at
		   FROM permission_group_members
		  WHERE tenant_id = $1 AND group_id = $2
		  ORDER BY account_id
		  LIMIT $3`, tenantID, groupID, identityListLimit(limit))
	if err != nil {
		return nil, identityWriteError("list permission group members", err)
	}
	defer rows.Close()
	out := make([]PermissionGroupMemberRecord, 0, 8)
	for rows.Next() {
		var record PermissionGroupMemberRecord
		if err := rows.Scan(&record.TenantID, &record.GroupID, &record.AccountID,
			&record.AddedBy, &record.CreatedAt); err != nil {
			return nil, identityWriteError("scan permission group member", err)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("list permission group members", err)
	}
	return out, nil
}

// AddPermissionGroupGrant adds one (key, scope) grant to a group. created=false
// means the exact pair already existed; a missing group is ErrConflict.
func (s *Store) AddPermissionGroupGrant(ctx context.Context, tenantID, groupID uuid.UUID, permissionKey, scope string) (bool, error) {
	permissionKey = strings.TrimSpace(permissionKey)
	if tenantID == uuid.Nil || groupID == uuid.Nil {
		return false, invalidIdentity("add permission group grant", "tenant and group ids are required")
	}
	if err := validatePermissionGrantFields(permissionKey, scope); err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO permission_group_grants (tenant_id, group_id, permission_key, scope)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (tenant_id, group_id, permission_key, scope) DO NOTHING`,
		tenantID, groupID, permissionKey, scope)
	if err != nil {
		return false, identityWriteError("add permission group grant", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RemovePermissionGroupGrant removes one exact (key, scope) pair; an absent
// pair is ErrNotFound.
func (s *Store) RemovePermissionGroupGrant(ctx context.Context, tenantID, groupID uuid.UUID, permissionKey, scope string) error {
	permissionKey = strings.TrimSpace(permissionKey)
	if tenantID == uuid.Nil || groupID == uuid.Nil {
		return invalidIdentity("remove permission group grant", "tenant and group ids are required")
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM permission_group_grants
		  WHERE tenant_id = $1 AND group_id = $2 AND permission_key = $3 AND scope = $4`,
		tenantID, groupID, permissionKey, scope)
	if err != nil {
		return identityWriteError("remove permission group grant", err)
	}
	return identityRowsResult("remove permission group grant", tag.RowsAffected())
}

// ListPermissionGroupGrants returns up to limit grants of one group.
func (s *Store) ListPermissionGroupGrants(ctx context.Context, tenantID, groupID uuid.UUID, limit int) ([]PermissionGroupGrantRecord, error) {
	if tenantID == uuid.Nil || groupID == uuid.Nil {
		return nil, invalidIdentity("list permission group grants", "tenant and group ids are required")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, group_id, permission_key, scope
		   FROM permission_group_grants
		  WHERE tenant_id = $1 AND group_id = $2
		  ORDER BY permission_key, scope
		  LIMIT $3`, tenantID, groupID, identityListLimit(limit))
	if err != nil {
		return nil, identityWriteError("list permission group grants", err)
	}
	defer rows.Close()
	out := make([]PermissionGroupGrantRecord, 0, 8)
	for rows.Next() {
		var record PermissionGroupGrantRecord
		if err := rows.Scan(&record.TenantID, &record.GroupID,
			&record.PermissionKey, &record.Scope); err != nil {
			return nil, identityWriteError("scan permission group grant", err)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("list permission group grants", err)
	}
	return out, nil
}

// CreatePermissionGrant inserts one direct grant. The same
// (subject, key, scope) tuple twice is ErrConflict; a different scope for the
// same key is a legal additional row (the union takes the maximum).
func (s *Store) CreatePermissionGrant(ctx context.Context, record PermissionGrantRecord) (PermissionGrantRecord, error) {
	record.PermissionKey = strings.TrimSpace(record.PermissionKey)
	record.SubjectType = strings.TrimSpace(record.SubjectType)
	switch {
	case record.TenantID == uuid.Nil:
		return PermissionGrantRecord{}, invalidIdentity("permission grant", "tenant id is required")
	case record.ID == uuid.Nil:
		return PermissionGrantRecord{}, invalidIdentity("permission grant", "grant id is required")
	case !ValidPermissionSubject(record.SubjectType):
		return PermissionGrantRecord{}, invalidIdentity("permission grant", "unknown subject type %q (direct grants are account-only; use permission_group_grants for groups)", record.SubjectType)
	case record.SubjectID == uuid.Nil:
		return PermissionGrantRecord{}, invalidIdentity("permission grant", "subject id is required")
	case record.GrantedBy == uuid.Nil:
		return PermissionGrantRecord{}, invalidIdentity("permission grant", "granted-by id is required")
	}
	if err := validatePermissionGrantFields(record.PermissionKey, record.Scope); err != nil {
		return PermissionGrantRecord{}, err
	}
	created, err := scanPermissionGrantRow(s.pool.QueryRow(ctx,
		`INSERT INTO permission_grants
		    (tenant_id, grant_id, subject_type, subject_id, permission_key, scope, granted_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING tenant_id, grant_id, subject_type, subject_id, permission_key,
		           scope, granted_by, created_at`,
		record.TenantID, record.ID, record.SubjectType, record.SubjectID,
		record.PermissionKey, record.Scope, record.GrantedBy))
	if err != nil {
		return PermissionGrantRecord{}, identityWriteError("create permission grant", err)
	}
	return created, nil
}

// ListPermissionGrants returns up to limit direct grants of one subject.
func (s *Store) ListPermissionGrants(ctx context.Context, tenantID uuid.UUID, subjectType string, subjectID uuid.UUID, limit int) ([]PermissionGrantRecord, error) {
	subjectType = strings.TrimSpace(subjectType)
	if tenantID == uuid.Nil || !ValidPermissionSubject(subjectType) || subjectID == uuid.Nil {
		return nil, invalidIdentity("list permission grants", "tenant, subject type and subject id are required")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, grant_id, subject_type, subject_id, permission_key,
		        scope, granted_by, created_at
		   FROM permission_grants
		  WHERE tenant_id = $1 AND subject_type = $2 AND subject_id = $3
		  ORDER BY permission_key, scope, grant_id
		  LIMIT $4`, tenantID, subjectType, subjectID, identityListLimit(limit))
	if err != nil {
		return nil, identityWriteError("list permission grants", err)
	}
	defer rows.Close()
	out := make([]PermissionGrantRecord, 0, 8)
	for rows.Next() {
		record, err := scanPermissionGrantRow(rows)
		if err != nil {
			return nil, identityWriteError("scan permission grant", err)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("list permission grants", err)
	}
	return out, nil
}

// DeletePermissionGrant removes one direct grant by id.
func (s *Store) DeletePermissionGrant(ctx context.Context, tenantID, grantID uuid.UUID) error {
	if tenantID == uuid.Nil || grantID == uuid.Nil {
		return invalidIdentity("delete permission grant", "tenant and grant ids are required")
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM permission_grants WHERE tenant_id = $1 AND grant_id = $2`, tenantID, grantID)
	if err != nil {
		return identityWriteError("delete permission grant", err)
	}
	return identityRowsResult("delete permission grant", tag.RowsAffected())
}

func validatePermissionGroupRecord(record PermissionGroupRecord) error {
	switch {
	case record.TenantID == uuid.Nil:
		return invalidIdentity("permission group", "tenant id is required")
	case record.ID == uuid.Nil:
		return invalidIdentity("permission group", "group id is required")
	case strings.TrimSpace(record.Name) == "":
		return invalidIdentity("permission group", "name is required")
	}
	return nil
}

// validatePermissionGrantFields checks the two strings every grant carries. The
// key's existence in the permission registry is A3's check; here a key must at
// least be a non-empty token without whitespace.
func validatePermissionGrantFields(permissionKey, scope string) error {
	switch {
	case permissionKey == "":
		return invalidIdentity("permission grant", "permission key is required")
	case strings.ContainsAny(permissionKey, " \t\r\n"):
		return invalidIdentity("permission grant", "permission key %q must not contain whitespace", permissionKey)
	case !ValidPermissionScope(scope):
		return invalidIdentity("permission grant", "unknown scope %q (want own, team, tenant or all)", scope)
	}
	return nil
}

func scanPermissionGroupRow(row rowScanner) (PermissionGroupRecord, error) {
	var record PermissionGroupRecord
	err := row.Scan(&record.TenantID, &record.ID, &record.Name, &record.Builtin,
		&record.CreatedAt, &record.UpdatedAt)
	return record, err
}

func scanPermissionGrantRow(row rowScanner) (PermissionGrantRecord, error) {
	var record PermissionGrantRecord
	err := row.Scan(&record.TenantID, &record.ID, &record.SubjectType, &record.SubjectID,
		&record.PermissionKey, &record.Scope, &record.GrantedBy, &record.CreatedAt)
	return record, err
}
