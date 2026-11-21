package access

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// Grant is one (permission key, scope) pair. There is no deny grant and no
// negative scope; a grant only adds authority (L5).
type Grant struct {
	Key   string
	Scope Scope
}

// Grants is an effective permission set: at most one scope per key, already
// reduced to the widest scope. A nil Grants is a valid empty set.
type Grants map[string]Scope

// ScopeOf returns the effective scope of key.
func (g Grants) ScopeOf(key string) (Scope, bool) {
	scope, ok := g[key]
	return scope, ok
}

// Covers reports whether the set holds key at least at scope.
func (g Grants) Covers(key string, scope Scope) bool {
	held, ok := g[key]
	return ok && held.Covers(scope)
}

// Keys returns the keys in the set, sorted.
func (g Grants) Keys() []string {
	out := make([]string, 0, len(g))
	for key := range g {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// Clone returns an independent copy.
func (g Grants) Clone() Grants {
	out := make(Grants, len(g))
	for key, scope := range g {
		out[key] = scope
	}
	return out
}

// GrantSource is the read seam for effective permissions. Implementations
// scope both reads by tenant and account.
//
// DirectGrants MUST return only direct account grants
// (permission_grants.subject_type = account). Group authority lives in
// permission_group_grants and MUST come through GroupGrants; a direct
// group-subject row is a second source of truth and must never be counted
// (A1 decision 5).
type GrantSource interface {
	// DirectGrants returns the account's direct grants, before any group
	// grants are added.
	DirectGrants(ctx context.Context, tenantID, accountID uuid.UUID) ([]Grant, error)
	// GroupGrants returns the grants of every group the account belongs to.
	GroupGrants(ctx context.Context, tenantID, accountID uuid.UUID) ([]Grant, error)
}

// ErrInvalidGrant marks a grant whose key or scope is not legal.
var ErrInvalidGrant = errors.New("access: invalid grant")

// ValidateGrant checks one grant against the registry: the key must exist and
// the scope must be valid and allowed for that key. Unknown keys and
// disallowed scopes are rejected rather than dropped (spec: 未知 key 写入被拒绝).
func ValidateGrant(grant Grant) error {
	spec, ok := Lookup(grant.Key)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownKey, grant.Key)
	}
	if !ValidScope(grant.Scope) {
		return fmt.Errorf("%w: unknown scope %q for key %q", ErrInvalidGrant, grant.Scope, grant.Key)
	}
	if !spec.AllowsScope(grant.Scope) {
		return fmt.Errorf("%w: key %q does not allow scope %q", ErrInvalidGrant, grant.Key, grant.Scope)
	}
	return nil
}

// EffectiveGrants merges the account's direct grants with every group grant.
// The union is additive and the widest scope wins per key (L5). An invalid
// grant in either source is an error, never silently ignored: a corrupted row
// must not change authority by accident.
func EffectiveGrants(ctx context.Context, source GrantSource, tenantID, accountID uuid.UUID) (Grants, error) {
	if source == nil {
		return nil, errors.New("access: grant source is nil")
	}
	if tenantID == uuid.Nil || accountID == uuid.Nil {
		return nil, errors.New("access: tenant and account ids are required")
	}
	direct, err := source.DirectGrants(ctx, tenantID, accountID)
	if err != nil {
		return nil, fmt.Errorf("access: load direct grants: %w", err)
	}
	grouped, err := source.GroupGrants(ctx, tenantID, accountID)
	if err != nil {
		return nil, fmt.Errorf("access: load group grants: %w", err)
	}
	effective := make(Grants, len(direct)+len(grouped))
	merge := func(grants []Grant) error {
		for _, grant := range grants {
			if err := ValidateGrant(grant); err != nil {
				return err
			}
			current, ok := effective[grant.Key]
			if !ok || grant.Scope.Covers(current) {
				effective[grant.Key] = grant.Scope
			}
		}
		return nil
	}
	if err := merge(direct); err != nil {
		return nil, err
	}
	if err := merge(grouped); err != nil {
		return nil, err
	}
	return effective, nil
}

// GrantSubject values mirror the permission_grants.subject_type vocabulary
// without importing internal/store.
const (
	GrantSubjectAccount = "account"
	GrantSubjectGroup   = "group"
)

// ValidateDirectGrantSubject enforces A1 decision 5: a direct permission_grants
// row may name an account only. Group grants live exclusively in
// permission_group_grants, so accepting a direct group-subject row would create
// a second source of truth and a way to bypass group membership bookkeeping.
func ValidateDirectGrantSubject(subjectType string) error {
	switch subjectType {
	case GrantSubjectAccount:
		return nil
	case GrantSubjectGroup:
		return errors.New("access: group grants must use permission_group_grants, not a direct permission_grants row")
	default:
		return fmt.Errorf("access: unknown direct grant subject type %q (want account)", subjectType)
	}
}

// Grantor is the authority behind a grant operation: either a super admin or
// an ordinary principal's effective grant set.
type Grantor struct {
	// SuperAdmin short-circuits the subset check (L6). It does not make an
	// unknown key legal.
	SuperAdmin bool
	// Effective is the grantor's own effective permission set; it may include
	// group grants because those are the grantor's authority too.
	Effective Grants
}

// GrantRejection is a structured "grant ⊆ own" failure. A4 maps Reason to 403
// copy; the audit row records Key and Scope.
type GrantRejection struct {
	Key    string
	Scope  Scope
	Reason Reason
	Detail string
}

// Error renders the rejection with the stable reason and the missing tuple.
func (e *GrantRejection) Error() string {
	if e.Scope == "" {
		return fmt.Sprintf("access: cannot grant %q: %s", e.Key, e.Detail)
	}
	return fmt.Sprintf("access: cannot grant %q at scope %q: %s", e.Key, e.Scope, e.Detail)
}

// CanGrant checks "授予不得超过自己" (design invariant 5): except for a super
// admin, every proposed grant must be covered by the grantor's own effective
// set. The check is per tuple, so a grantor cannot combine a narrower scope
// with a wider key to bypass the subset rule.
//
// A proposed group-subject grant is rejected by ValidateDirectGrantSubject
// before it reaches this function; CanGrant itself operates on (key, scope)
// and cannot tell where the grant will be stored.
func CanGrant(grantor Grantor, proposed []Grant) error {
	for _, grant := range proposed {
		if err := ValidateGrant(grant); err != nil {
			reason := ReasonInvalidScope
			if errors.Is(err, ErrUnknownKey) {
				reason = ReasonUnknownKey
			}
			return &GrantRejection{Key: grant.Key, Scope: grant.Scope, Reason: reason, Detail: err.Error()}
		}
		if grantor.SuperAdmin {
			continue
		}
		held, ok := grantor.Effective[grant.Key]
		if !ok {
			return &GrantRejection{
				Key:    grant.Key,
				Scope:  grant.Scope,
				Reason: ReasonMissingGrant,
				Detail: fmt.Sprintf("grantor does not hold %q", grant.Key),
			}
		}
		if !held.Covers(grant.Scope) {
			return &GrantRejection{
				Key:    grant.Key,
				Scope:  grant.Scope,
				Reason: ReasonMissingGrant,
				Detail: fmt.Sprintf("grantor holds %q at scope %q, which does not cover %q", grant.Key, held, grant.Scope),
			}
		}
	}
	return nil
}

// NarrowToHarness applies L11: a Bot's effective grants are its Account grants
// narrowed to the Harness permission list. A harness key the account does not
// hold contributes nothing; an empty (or nil) harness narrows to nothing. An
// unknown harness key is an error, because it would make the Bot's configured
// narrowing depend on a key the product does not recognize.
func NarrowToHarness(account Grants, harness []string) (Grants, error) {
	narrowed := make(Grants, len(harness))
	for _, raw := range harness {
		key := strings.TrimSpace(raw)
		if key == "" {
			return nil, fmt.Errorf("%w: harness permission is empty", ErrInvalidGrant)
		}
		if err := ValidateKey(key); err != nil {
			return nil, err
		}
		if scope, ok := account[key]; ok {
			narrowed[key] = scope
		}
	}
	return narrowed, nil
}
