package access

import (
	"context"
	"errors"
	"fmt"
)

// BatchRelationSource is the optional efficiency upgrade over RelationSource:
// it answers the same tuples for many resources of one type in one call, so a
// bounded list endpoint does not issue one query per row. The result is keyed
// by resource id; a missing id simply has no tuples. An implementation MUST
// scope the read by resource.TenantID and resource.Type exactly like
// RelationsForResource.
type BatchRelationSource interface {
	RelationsForResources(ctx context.Context, resource Resource, ids []string) (map[string][]RelationTuple, error)
}

// ErrVisibilityNotResourceScoped marks a caller mistake: VisibleResources only
// filters resources, so a tenant-level key has nothing to filter.
var ErrVisibilityNotResourceScoped = errors.New("access: visibility filtering requires a resource-scoped permission key")

// VisibleResult is the list-filter answer for one key and resource type.
//
// Decision is the key-level gate: when it is denied, IDs is empty and the
// caller must answer a 403 (never "an empty list"), so a missing grant cannot
// be mistaken for "no resources". When it is allowed, IDs is the subset of the
// candidates the principal holds the required relation for at the granted
// scope; a candidate with no relation, an unresolvable subject or a malformed
// tuple is absent (fail closed).
type VisibleResult struct {
	IDs      []string
	Scope    Scope
	Decision Decision
}

// Allowed reports whether the key-level gate passed.
func (r VisibleResult) Allowed() bool { return r.Decision.Allow }

// VisibleResources is the engine-level list filter (design §3.3: 列表端点禁止
// 用 scope 短路关系). It applies the same pipeline as Authorize — key in the
// registry, valid principal, super-admin short circuit, effective grants, Bot
// Harness narrowing, resource relation reachable at the granted scope — but
// answers for a bounded candidate set instead of one resource.
//
// There is deliberately no "scope tenant/all means skip the relation" branch:
// scope decides how far a held relation reaches, never whether one exists.
func (a *Authorizer) VisibleResources(ctx context.Context, principal Principal, actionKey, resourceType string, candidateIDs []string) (VisibleResult, error) {
	ids := uniqueIDs(candidateIDs)
	if a == nil {
		return VisibleResult{}, errors.New("access: authorizer is nil")
	}
	if resourceType == "" {
		return VisibleResult{}, errors.New("access: resource type is required")
	}
	spec, ok := Lookup(actionKey)
	if !ok {
		return VisibleResult{Decision: denyDecision(actionKey, "", ReasonUnknownKey,
			fmt.Sprintf("permission key %q is not in the registry", actionKey))}, nil
	}
	if err := principal.Validate(); err != nil {
		return VisibleResult{Decision: denyDecision(actionKey, "", ReasonInvalidPrincipal, err.Error())}, nil
	}
	if !spec.RequiresRelation() {
		return VisibleResult{}, fmt.Errorf("%w: %q", ErrVisibilityNotResourceScoped, actionKey)
	}
	if principal.IsSuperAdmin {
		return VisibleResult{IDs: ids, Scope: ScopeAll, Decision: allowDecision(actionKey, ScopeAll)}, nil
	}
	effective, err := EffectiveGrants(ctx, a.grants, principal.TenantID, principal.AccountID)
	if err != nil {
		return VisibleResult{}, fmt.Errorf("access: load effective grants: %w", err)
	}
	scope, ok := effective[actionKey]
	if !ok {
		return VisibleResult{Decision: denyDecision(actionKey, "", ReasonMissingGrant,
			fmt.Sprintf("account %s has no grant for %q", principal.AccountID, actionKey))}, nil
	}
	if principal.BotID != "" && !harnessAllows(principal.HarnessPermissions, actionKey) {
		return VisibleResult{Decision: denyDecision(actionKey, scope, ReasonMissingKey,
			fmt.Sprintf("bot %q harness does not include %q", principal.BotID, actionKey))}, nil
	}
	if len(ids) == 0 {
		return VisibleResult{IDs: []string{}, Scope: scope, Decision: allowDecision(actionKey, scope)}, nil
	}
	resource := Resource{TenantID: principal.TenantID, Type: resourceType}
	tuplesByID, err := loadRelations(ctx, a.relations, resource, ids)
	if err != nil {
		return VisibleResult{}, fmt.Errorf("access: evaluate visibility: %w", err)
	}
	visible := make([]string, 0, len(ids))
	for _, id := range ids {
		held := make(map[Relation]bool, 4)
		direct := make(map[Relation]bool, 4)
		for _, tuple := range tuplesByID[id] {
			match, err := matchTuple(ctx, a.relations, principal, tuple)
			if err != nil {
				return VisibleResult{}, fmt.Errorf("access: evaluate visibility: %w", err)
			}
			if !match.matched {
				// Unresolved and malformed tuples never grant; the candidate simply
				// stays invisible.
				continue
			}
			held[tuple.Relation] = true
			if match.direct {
				direct[tuple.Relation] = true
			}
		}
		reachable := held
		if scope == ScopeOwn {
			reachable = direct
		}
		if relationSatisfied(reachable, spec.Relation) {
			visible = append(visible, id)
		}
	}
	return VisibleResult{IDs: visible, Scope: scope, Decision: allowDecision(actionKey, scope)}, nil
}

// loadRelations reads the tuples of every candidate, using the batch seam when
// the source has one and the single-resource read otherwise.
func loadRelations(ctx context.Context, source RelationSource, resource Resource, ids []string) (map[string][]RelationTuple, error) {
	if batch, ok := source.(BatchRelationSource); ok {
		return batch.RelationsForResources(ctx, resource, ids)
	}
	out := make(map[string][]RelationTuple, len(ids))
	for _, id := range ids {
		one := Resource{TenantID: resource.TenantID, Type: resource.Type, ID: id}
		tuples, err := source.RelationsForResource(ctx, one)
		if err != nil {
			return nil, err
		}
		out[id] = tuples
	}
	return out, nil
}

// uniqueIDs drops empty and repeated candidate ids while preserving order.
func uniqueIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
