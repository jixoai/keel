// Resource relations (ReBAC-lite) persistence
// (change keel-bot-identity-access, A1).
//
// One row is a relation tuple: (resource_type, resource_id, relation,
// subject_type, subject_id), tenant-scoped. The relation and subject-type
// vocabularies are closed; resource_type stays open so new resource kinds need
// no migration. This layer writes and reads tuples; A3's evaluator owns the
// "which key plus which relation" decision.
package store

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Relation vocabulary (L3). There is no deny and no recursive rewrite.
const (
	RelationOwner      = "owner"
	RelationTeamMember = "team_member"
	RelationViewer     = "viewer"
	RelationEditor     = "editor"
)

// Relation subject types. A group is the permission group; a team_bot subject
// lets a team share a resource without becoming a permission subject itself.
const (
	RelationSubjectAccount = "account"
	RelationSubjectGroup   = "group"
	RelationSubjectTeamBot = "team_bot"
)

// ValidRelation reports whether relation is in the closed vocabulary.
func ValidRelation(relation string) bool {
	switch relation {
	case RelationOwner, RelationTeamMember, RelationViewer, RelationEditor:
		return true
	}
	return false
}

// ValidRelationSubject reports whether subjectType is account, group or
// team_bot.
func ValidRelationSubject(subjectType string) bool {
	switch subjectType {
	case RelationSubjectAccount, RelationSubjectGroup, RelationSubjectTeamBot:
		return true
	}
	return false
}

// ResourceRelation is one stored relation tuple.
type ResourceRelation struct {
	TenantID     uuid.UUID
	ResourceType string
	ResourceID   string
	Relation     string
	SubjectType  string
	SubjectID    string
	CreatedBy    uuid.UUID
	CreatedAt    time.Time
}

// PutResourceRelation writes one tuple. created=false means the exact tuple
// already existed (idempotent), not a failure.
func (s *Store) PutResourceRelation(ctx context.Context, relation ResourceRelation) (bool, error) {
	relation.ResourceType = strings.TrimSpace(relation.ResourceType)
	relation.ResourceID = strings.TrimSpace(relation.ResourceID)
	relation.Relation = strings.TrimSpace(relation.Relation)
	relation.SubjectType = strings.TrimSpace(relation.SubjectType)
	relation.SubjectID = strings.TrimSpace(relation.SubjectID)
	if err := validateResourceRelation(relation); err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO resource_relations
		    (tenant_id, resource_type, resource_id, relation, subject_type, subject_id, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (tenant_id, resource_type, resource_id, relation, subject_type, subject_id)
		 DO NOTHING`,
		relation.TenantID, relation.ResourceType, relation.ResourceID, relation.Relation,
		relation.SubjectType, relation.SubjectID, relation.CreatedBy)
	if err != nil {
		return false, identityWriteError("put resource relation", err)
	}
	return tag.RowsAffected() == 1, nil
}

// DeleteResourceRelation removes one exact tuple. An absent tuple is
// ErrNotFound.
func (s *Store) DeleteResourceRelation(ctx context.Context, tenantID uuid.UUID, resourceType, resourceID, relation, subjectType, subjectID string) error {
	candidate := ResourceRelation{
		TenantID:     tenantID,
		ResourceType: strings.TrimSpace(resourceType),
		ResourceID:   strings.TrimSpace(resourceID),
		Relation:     strings.TrimSpace(relation),
		SubjectType:  strings.TrimSpace(subjectType),
		SubjectID:    strings.TrimSpace(subjectID),
	}
	if err := validateResourceRelationTuple(candidate); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM resource_relations
		  WHERE tenant_id = $1 AND resource_type = $2 AND resource_id = $3
		    AND relation = $4 AND subject_type = $5 AND subject_id = $6`,
		candidate.TenantID, candidate.ResourceType, candidate.ResourceID,
		candidate.Relation, candidate.SubjectType, candidate.SubjectID)
	if err != nil {
		return identityWriteError("delete resource relation", err)
	}
	return identityRowsResult("delete resource relation", tag.RowsAffected())
}

// PutResourceRelationIfOrphan writes one relation only when the resource and
// every ancestor in ancestorIDs have no relation tuples at all. It is the
// explicit adoption path for a resource whose ACL was never written
// (keel-board-v1 Phase 1 P0-1): a board.manage holder may claim a fully
// relation-free board channel, but can never take over a channel whose own or
// inherited ACL already authorizes somebody.
//
// The emptiness check and the insert share one transaction. Every checked id
// takes a transaction-scoped advisory lock (sorted), so two concurrent adopters
// of the same channel or of an ancestor/descendant pair cannot both become
// owner. created=false means an existing relation was found; the caller keeps
// whatever ACL it had.
func (s *Store) PutResourceRelationIfOrphan(ctx context.Context, relation ResourceRelation, ancestorIDs []string) (bool, error) {
	relation.ResourceType = strings.TrimSpace(relation.ResourceType)
	relation.ResourceID = strings.TrimSpace(relation.ResourceID)
	relation.Relation = strings.TrimSpace(relation.Relation)
	relation.SubjectType = strings.TrimSpace(relation.SubjectType)
	relation.SubjectID = strings.TrimSpace(relation.SubjectID)
	if err := validateResourceRelation(relation); err != nil {
		return false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, identityWriteError("begin orphan relation", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	ids := normalizedIDs(append([]string{relation.ResourceID}, ancestorIDs...))
	locked := append([]string(nil), ids...)
	sort.Strings(locked)
	for _, id := range locked {
		lockKey := relation.TenantID.String() + "|" + relation.ResourceType + "|" + id
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
			return false, identityWriteError("lock orphan relation", err)
		}
	}
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM resource_relations
		    WHERE tenant_id = $1 AND resource_type = $2 AND resource_id = ANY($3))`,
		relation.TenantID, relation.ResourceType, ids).Scan(&exists); err != nil {
		return false, identityWriteError("check orphan relation", err)
	}
	if exists {
		return false, nil
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO resource_relations
		    (tenant_id, resource_type, resource_id, relation, subject_type, subject_id, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (tenant_id, resource_type, resource_id, relation, subject_type, subject_id)
		 DO NOTHING`,
		relation.TenantID, relation.ResourceType, relation.ResourceID, relation.Relation,
		relation.SubjectType, relation.SubjectID, relation.CreatedBy)
	if err != nil {
		return false, identityWriteError("adopt orphan resource", err)
	}
	created := tag.RowsAffected() == 1
	if err := tx.Commit(ctx); err != nil {
		return false, identityWriteError("commit orphan relation", err)
	}
	return created, nil
}

// HasResourceRelation reports whether one exact tuple exists.
func (s *Store) HasResourceRelation(ctx context.Context, tenantID uuid.UUID, resourceType, resourceID, relation, subjectType, subjectID string) (bool, error) {
	candidate := ResourceRelation{
		TenantID:     tenantID,
		ResourceType: strings.TrimSpace(resourceType),
		ResourceID:   strings.TrimSpace(resourceID),
		Relation:     strings.TrimSpace(relation),
		SubjectType:  strings.TrimSpace(subjectType),
		SubjectID:    strings.TrimSpace(subjectID),
	}
	if err := validateResourceRelationTuple(candidate); err != nil {
		return false, err
	}
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM resource_relations
		    WHERE tenant_id = $1 AND resource_type = $2 AND resource_id = $3
		      AND relation = $4 AND subject_type = $5 AND subject_id = $6)`,
		candidate.TenantID, candidate.ResourceType, candidate.ResourceID,
		candidate.Relation, candidate.SubjectType, candidate.SubjectID).Scan(&exists)
	if err != nil {
		return false, identityWriteError("has resource relation", err)
	}
	return exists, nil
}

// ListResourceRelations returns up to limit tuples of one resource, ordered by
// relation then subject.
func (s *Store) ListResourceRelations(ctx context.Context, tenantID uuid.UUID, resourceType, resourceID string, limit int) ([]ResourceRelation, error) {
	resourceType = strings.TrimSpace(resourceType)
	resourceID = strings.TrimSpace(resourceID)
	if tenantID == uuid.Nil || resourceType == "" || resourceID == "" {
		return nil, invalidIdentity("list resource relations", "tenant, resource type and resource id are required")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, resource_type, resource_id, relation, subject_type,
		        subject_id, created_by, created_at
		   FROM resource_relations
		  WHERE tenant_id = $1 AND resource_type = $2 AND resource_id = $3
		  ORDER BY relation, subject_type, subject_id
		  LIMIT $4`, tenantID, resourceType, resourceID, identityListLimit(limit))
	if err != nil {
		return nil, identityWriteError("list resource relations", err)
	}
	defer rows.Close()
	out := make([]ResourceRelation, 0, 8)
	for rows.Next() {
		relation, err := scanResourceRelationRow(rows)
		if err != nil {
			return nil, identityWriteError("scan resource relation", err)
		}
		out = append(out, relation)
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("list resource relations", err)
	}
	return out, nil
}

// ListResourceRelationsForSubject returns up to limit tuples held by one
// subject. An empty resourceType means every resource type.
func (s *Store) ListResourceRelationsForSubject(ctx context.Context, tenantID uuid.UUID, subjectType, subjectID, resourceType string, limit int) ([]ResourceRelation, error) {
	subjectType = strings.TrimSpace(subjectType)
	subjectID = strings.TrimSpace(subjectID)
	resourceType = strings.TrimSpace(resourceType)
	if tenantID == uuid.Nil || !ValidRelationSubject(subjectType) || subjectID == "" {
		return nil, invalidIdentity("list subject relations", "tenant, subject type and subject id are required")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, resource_type, resource_id, relation, subject_type,
		        subject_id, created_by, created_at
		   FROM resource_relations
		  WHERE tenant_id = $1 AND subject_type = $2 AND subject_id = $3
		    AND ($4 = '' OR resource_type = $4)
		  ORDER BY resource_type, resource_id, relation
		  LIMIT $5`, tenantID, subjectType, subjectID, resourceType, identityListLimit(limit))
	if err != nil {
		return nil, identityWriteError("list subject relations", err)
	}
	defer rows.Close()
	out := make([]ResourceRelation, 0, 8)
	for rows.Next() {
		relation, err := scanResourceRelationRow(rows)
		if err != nil {
			return nil, identityWriteError("scan subject relation", err)
		}
		out = append(out, relation)
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("list subject relations", err)
	}
	return out, nil
}

// ResourceOwnerAccounts returns the owner account of every named resource, in
// one query. A resource with no owner tuple is absent from the map (the caller
// decides whether that is an error); a malformed stored subject is an error
// rather than a silently dropped row.
func (s *Store) ResourceOwnerAccounts(ctx context.Context, tenantID uuid.UUID, resourceType string, resourceIDs []string) (map[string]uuid.UUID, error) {
	resourceType = strings.TrimSpace(resourceType)
	ids := normalizedIDs(resourceIDs)
	if tenantID == uuid.Nil || resourceType == "" {
		return nil, invalidIdentity("resource owners", "tenant and resource type are required")
	}
	out := make(map[string]uuid.UUID, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT resource_id, subject_id
		   FROM resource_relations
		  WHERE tenant_id = $1 AND resource_type = $2
		    AND relation = $3 AND subject_type = $4
		    AND resource_id = ANY($5)`,
		tenantID, resourceType, RelationOwner, RelationSubjectAccount, ids)
	if err != nil {
		return nil, identityWriteError("resource owners", err)
	}
	defer rows.Close()
	for rows.Next() {
		var resourceID, subjectID string
		if err := rows.Scan(&resourceID, &subjectID); err != nil {
			return nil, identityWriteError("scan resource owner", err)
		}
		accountID, err := uuid.Parse(subjectID)
		if err != nil {
			return nil, identityWriteError("resource owner", err)
		}
		out[resourceID] = accountID
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("resource owners", err)
	}
	return out, nil
}

// ResourceRelationCounts counts the named relations on each resource, in one
// query. It powers the "shared" badge on a list without a query per row.
func (s *Store) ResourceRelationCounts(ctx context.Context, tenantID uuid.UUID, resourceType string, resourceIDs []string, relations []string) (map[string]int, error) {
	resourceType = strings.TrimSpace(resourceType)
	ids := normalizedIDs(resourceIDs)
	if tenantID == uuid.Nil || resourceType == "" {
		return nil, invalidIdentity("resource relation counts", "tenant and resource type are required")
	}
	out := make(map[string]int, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	relationSet := make([]string, 0, len(relations))
	for _, relation := range relations {
		relation = strings.TrimSpace(relation)
		if !ValidRelation(relation) {
			return nil, invalidIdentity("resource relation counts", "unknown relation %q", relation)
		}
		relationSet = append(relationSet, relation)
	}
	if len(relationSet) == 0 {
		relationSet = []string{RelationViewer, RelationEditor}
	}
	rows, err := s.pool.Query(ctx,
		`SELECT resource_id, count(*)
		   FROM resource_relations
		  WHERE tenant_id = $1 AND resource_type = $2
		    AND relation = ANY($3) AND resource_id = ANY($4)
		  GROUP BY resource_id`,
		tenantID, resourceType, relationSet, ids)
	if err != nil {
		return nil, identityWriteError("resource relation counts", err)
	}
	defer rows.Close()
	for rows.Next() {
		var resourceID string
		var count int
		if err := rows.Scan(&resourceID, &count); err != nil {
			return nil, identityWriteError("scan resource relation count", err)
		}
		out[resourceID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("resource relation counts", err)
	}
	return out, nil
}

// normalizedIDs trims and de-duplicates a bounded resource-id list.
func normalizedIDs(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func validateResourceRelation(relation ResourceRelation) error {
	if err := validateResourceRelationTuple(relation); err != nil {
		return err
	}
	if relation.CreatedBy == uuid.Nil {
		return invalidIdentity("resource relation", "created-by account is required")
	}
	return nil
}

// validateResourceRelationTuple checks the tuple itself; reads and deletes do
// not need (or know) the original creator.
func validateResourceRelationTuple(relation ResourceRelation) error {
	switch {
	case relation.TenantID == uuid.Nil:
		return invalidIdentity("resource relation", "tenant id is required")
	case relation.ResourceType == "":
		return invalidIdentity("resource relation", "resource type is required")
	case relation.ResourceID == "":
		return invalidIdentity("resource relation", "resource id is required")
	case !ValidRelation(relation.Relation):
		return invalidIdentity("resource relation", "unknown relation %q (want owner, team_member, viewer or editor)", relation.Relation)
	case !ValidRelationSubject(relation.SubjectType):
		return invalidIdentity("resource relation", "unknown subject type %q (want account, group or team_bot)", relation.SubjectType)
	case relation.SubjectID == "":
		return invalidIdentity("resource relation", "subject id is required")
	}
	return nil
}

func scanResourceRelationRow(row rowScanner) (ResourceRelation, error) {
	var relation ResourceRelation
	err := row.Scan(&relation.TenantID, &relation.ResourceType, &relation.ResourceID,
		&relation.Relation, &relation.SubjectType, &relation.SubjectID,
		&relation.CreatedBy, &relation.CreatedAt)
	return relation, err
}
