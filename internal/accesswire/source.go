// Package accesswire adapts the identity store to the pure authorization core
// (change keel-bot-identity-access, A4.1).
//
// The dependency direction is fixed: internal/access owns the decision and
// imports nothing from the persistence layer; this package reads the store and
// implements access.GrantSource and access.RelationSource. It is the only place
// where permission rows, group membership and relation tuples become engine
// input.
package accesswire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/store"
)

// Source is the store-backed GrantSource and RelationSource.
type Source struct {
	// Store is required.
	Store *store.Store
}

// NewSource builds the adapter over one store.
func NewSource(st *store.Store) (*Source, error) {
	if st == nil {
		return nil, errors.New("accesswire: store is required")
	}
	return &Source{Store: st}, nil
}

// DirectGrants returns the account's direct grants only. A stored row with a
// non-account subject is a second source of group authority and is rejected
// (A1 decision 5), not silently skipped.
func (s *Source) DirectGrants(ctx context.Context, tenantID, accountID uuid.UUID) ([]access.Grant, error) {
	if s == nil || s.Store == nil {
		return nil, errors.New("accesswire: source is not configured")
	}
	// Ask for the store's maximum identity page: direct grants are rare, but a
	// truncated read would silently narrow authority.
	rows, err := s.Store.ListPermissionGrants(ctx, tenantID, store.PermissionSubjectAccount, accountID, 500)
	if err != nil {
		return nil, err
	}
	out := make([]access.Grant, 0, len(rows))
	for _, row := range rows {
		if err := access.ValidateDirectGrantSubject(row.SubjectType); err != nil {
			return nil, fmt.Errorf("accesswire: direct grant %s: %w", row.ID, err)
		}
		grant, err := grantOf(row.PermissionKey, row.Scope)
		if err != nil {
			return nil, fmt.Errorf("accesswire: direct grant %s: %w", row.ID, err)
		}
		out = append(out, grant)
	}
	return out, nil
}

// GroupGrants returns every grant of every group the account belongs to. It is
// the membership join, never a direct row with subject_type=group.
func (s *Source) GroupGrants(ctx context.Context, tenantID, accountID uuid.UUID) ([]access.Grant, error) {
	if s == nil || s.Store == nil {
		return nil, errors.New("accesswire: source is not configured")
	}
	rows, err := s.Store.ListPermissionGroupGrantsForAccount(ctx, tenantID, accountID, 0)
	if err != nil {
		return nil, err
	}
	out := make([]access.Grant, 0, len(rows))
	for _, row := range rows {
		grant, err := grantOf(row.PermissionKey, row.Scope)
		if err != nil {
			return nil, fmt.Errorf("accesswire: group %s grant: %w", row.GroupID, err)
		}
		out = append(out, grant)
	}
	return out, nil
}

// RelationsForResource returns one resource's tuples.
func (s *Source) RelationsForResource(ctx context.Context, resource access.Resource) ([]access.RelationTuple, error) {
	if s == nil || s.Store == nil {
		return nil, errors.New("accesswire: source is not configured")
	}
	rows, err := s.Store.ListResourceRelations(ctx, resource.TenantID, resource.Type, resource.ID, 0)
	if err != nil {
		return nil, err
	}
	return tuplesOf(rows), nil
}

// RelationsForResources implements access.BatchRelationSource: one read for a
// bounded list page. Resource ids are taken from the candidate set; the tenant
// and type come from resource.
func (s *Source) RelationsForResources(ctx context.Context, resource access.Resource, ids []string) (map[string][]access.RelationTuple, error) {
	if s == nil || s.Store == nil {
		return nil, errors.New("accesswire: source is not configured")
	}
	rows, err := s.Store.ListResourceRelationsForResources(ctx, resource.TenantID, resource.Type, ids, 0)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]access.RelationTuple, len(rows))
	for id, relations := range rows {
		out[id] = tuplesOf(relations)
	}
	return out, nil
}

// ResolveSubject resolves a group or TeamBot subject for the evaluator.
//
// Fail closed: a group or TeamBot that does not exist, or a roster that cannot
// be decoded, is access.ErrUnresolvedSubject — never "not a member". A missing
// account is "not a member" of a group; a missing group makes membership
// unknowable.
func (s *Source) ResolveSubject(ctx context.Context, principal access.Principal, subjectType access.SubjectType, subjectID string) (bool, error) {
	if s == nil || s.Store == nil {
		return false, errors.New("accesswire: source is not configured")
	}
	switch subjectType {
	case access.SubjectTypeAccount:
		accountID, err := uuid.Parse(strings.TrimSpace(subjectID))
		if err != nil {
			return false, fmt.Errorf("%w: account subject %q is not a uuid", access.ErrUnresolvedSubject, subjectID)
		}
		return accountID == principal.AccountID, nil
	case access.SubjectTypeGroup:
		groupID, err := uuid.Parse(strings.TrimSpace(subjectID))
		if err != nil {
			return false, fmt.Errorf("%w: group subject %q is not a uuid", access.ErrUnresolvedSubject, subjectID)
		}
		member, err := s.Store.PermissionGroupMembership(ctx, principal.TenantID, groupID, principal.AccountID)
		if errors.Is(err, store.ErrNotFound) {
			return false, fmt.Errorf("%w: permission group %s does not exist", access.ErrUnresolvedSubject, groupID)
		}
		if err != nil {
			return false, err
		}
		return member, nil
	case access.SubjectTypeTeamBot:
		if principal.BotID == "" {
			// A human principal can never be a TeamBot roster member; that is a
			// resolvable "no".
			if _, err := s.Store.GetTeamBot(ctx, principal.TenantID, subjectID); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return false, fmt.Errorf("%w: team bot %q does not exist", access.ErrUnresolvedSubject, subjectID)
				}
				return false, err
			}
			return false, nil
		}
		team, err := s.Store.GetTeamBot(ctx, principal.TenantID, subjectID)
		if errors.Is(err, store.ErrNotFound) {
			return false, fmt.Errorf("%w: team bot %q does not exist", access.ErrUnresolvedSubject, subjectID)
		}
		if err != nil {
			return false, err
		}
		roster, err := decodeRoster(team.Roster)
		if err != nil {
			return false, fmt.Errorf("%w: team bot %q roster: %v", access.ErrUnresolvedSubject, subjectID, err)
		}
		for _, entry := range roster {
			if entry.BotID == principal.BotID {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("%w: unknown subject type %q", access.ErrUnresolvedSubject, subjectType)
	}
}

// rosterEntry mirrors botteam.RosterEntry's persisted shape. This package only
// needs the bot reference; importing the domain package is unnecessary here.
type rosterEntry struct {
	BotID string `json:"botId"`
}

func decodeRoster(raw json.RawMessage) ([]rosterEntry, error) {
	if len(raw) == 0 {
		return nil, errors.New("roster is empty")
	}
	var roster []rosterEntry
	if err := json.Unmarshal(raw, &roster); err != nil {
		return nil, err
	}
	return roster, nil
}

// grantOf parses one stored (key, scope) pair. A stored value outside the
// engine's vocabulary is an error: a corrupted row must not change authority by
// being silently dropped.
func grantOf(key, scope string) (access.Grant, error) {
	key = strings.TrimSpace(key)
	if err := access.ValidateKey(key); err != nil {
		return access.Grant{}, err
	}
	parsed, err := access.ParseScope(scope)
	if err != nil {
		return access.Grant{}, err
	}
	return access.Grant{Key: key, Scope: parsed}, nil
}

func tuplesOf(rows []store.ResourceRelation) []access.RelationTuple {
	out := make([]access.RelationTuple, 0, len(rows))
	for _, row := range rows {
		out = append(out, access.RelationTuple{
			Relation:    access.Relation(row.Relation),
			SubjectType: access.SubjectType(row.SubjectType),
			SubjectID:   row.SubjectID,
		})
	}
	return out
}
