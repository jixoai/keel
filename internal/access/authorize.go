package access

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Decision is one authorization result. Allow is the only granted outcome;
// every other value is a denial with a stable Reason for 403 copy and audit.
// Detail is human-readable and never carries a secret value.
type Decision struct {
	// Allow reports whether the action is authorized.
	Allow bool
	// Reason is the machine-readable outcome.
	Reason Reason
	// Key is the action key the decision was made for.
	Key string
	// Scope is the effective scope used (on allow) or the scope that was
	// present but insufficient (on some denials).
	Scope Scope
	// Detail explains the decision for logs and UI copy.
	Detail string
}

// Denied reports whether the decision is a denial.
func (d Decision) Denied() bool { return !d.Allow }

// String renders the decision with its stable reason.
func (d Decision) String() string {
	if d.Allow {
		return fmt.Sprintf("allow %s at scope %s", d.Key, d.Scope)
	}
	if d.Detail == "" {
		return fmt.Sprintf("deny %s: %s", d.Key, d.Reason)
	}
	return fmt.Sprintf("deny %s: %s: %s", d.Key, d.Reason, d.Detail)
}

// Authorizer is the authorization pipeline. It is immutable after
// construction and safe for concurrent use; A4 builds one at startup and
// shares it across HTTP, tools, CLI and the session start path.
type Authorizer struct {
	grants    GrantSource
	relations RelationSource
}

// NewAuthorizer builds an Authorizer. Both sources are required: tenant-level
// keys only need grants, but a resource-scoped key without a relation source
// could never be evaluated safely, and a missing source must fail at startup,
// not at the first 403.
func NewAuthorizer(grants GrantSource, relations RelationSource) (*Authorizer, error) {
	if grants == nil {
		return nil, errors.New("access: grant source is required")
	}
	if relations == nil {
		return nil, errors.New("access: relation source is required")
	}
	return &Authorizer{grants: grants, relations: relations}, nil
}

// Authorize decides whether principal may perform actionKey on resource.
//
// The order is deliberate and each step fails closed:
//
//  1. the key exists in the registry;
//  2. the principal identity is valid (tenant, account, not disabled, no Bot
//     super admin);
//  3. a deployment super admin is allowed every known key;
//  4. the principal's effective grants (direct ∪ groups, widest scope) contain
//     the key;
//  5. for a Bot, the Harness narrowing also contains the key (L11);
//  6. a resource-scoped key needs the required relation, reachable at the
//     granted scope; a tenant-level key stops at step 5.
//
// An infrastructure failure (a source error) is returned as an error; callers
// must deny when err != nil. A denial is not an error: it is a Decision.
func (a *Authorizer) Authorize(ctx context.Context, principal Principal, actionKey string, resource Resource) (Decision, error) {
	if a == nil {
		return Decision{}, errors.New("access: authorizer is nil")
	}
	spec, ok := Lookup(actionKey)
	if !ok {
		return denyDecision(actionKey, "", ReasonUnknownKey,
			fmt.Sprintf("permission key %q is not in the registry", actionKey)), nil
	}
	if err := principal.Validate(); err != nil {
		return denyDecision(actionKey, "", ReasonInvalidPrincipal, err.Error()), nil
	}
	if principal.IsSuperAdmin {
		return allowDecision(actionKey, ScopeAll), nil
	}

	effective, err := EffectiveGrants(ctx, a.grants, principal.TenantID, principal.AccountID)
	if err != nil {
		return Decision{}, fmt.Errorf("access: load effective grants: %w", err)
	}
	scope, ok := effective[actionKey]
	if !ok {
		return denyDecision(actionKey, "", ReasonMissingGrant,
			fmt.Sprintf("account %s has no grant for %q", principal.AccountID, actionKey)), nil
	}
	if principal.BotID != "" && !harnessAllows(principal.HarnessPermissions, actionKey) {
		return denyDecision(actionKey, scope, ReasonMissingKey,
			fmt.Sprintf("bot %q harness does not include %q", principal.BotID, actionKey)), nil
	}
	if !spec.RequiresRelation() {
		// A tenant-level key has no per-resource relation, but a caller may still
		// pass the target tenant (an account, group or bot being managed). L1
		// keeps ordinary accounts inside their tenant; only scope all (normally
		// only a super admin) crosses the boundary.
		if resource.TenantID != uuid.Nil && resource.TenantID != principal.TenantID && scope != ScopeAll {
			return denyDecision(actionKey, scope, ReasonMissingGrant,
				fmt.Sprintf("scope %q does not reach another tenant", scope)), nil
		}
		return allowDecision(actionKey, scope), nil
	}

	if !resource.Valid() {
		return denyDecision(actionKey, scope, ReasonMissingRelation,
			"resource is not a complete (tenant, type, id) reference"), nil
	}
	if resource.TenantID != principal.TenantID && scope != ScopeAll {
		return denyDecision(actionKey, scope, ReasonMissingGrant,
			fmt.Sprintf("scope %q does not reach another tenant", scope)), nil
	}

	evaluation, err := EvaluateRelations(ctx, a.relations, principal, resource)
	if err != nil {
		return Decision{}, fmt.Errorf("access: evaluate relations: %w", err)
	}
	if !relationSatisfied(evaluation.Held, spec.Relation) {
		return relationDenial(actionKey, scope, evaluation,
			fmt.Sprintf("no relation satisfies %q", spec.Relation)), nil
	}
	if !scopeReachSatisfied(scope, evaluation, spec.Relation) {
		detail := fmt.Sprintf("scope %q does not reach this resource", scope)
		if scope == ScopeOwn {
			detail = fmt.Sprintf("scope own requires the %q relation to be held directly by account %s",
				spec.Relation, principal.AccountID)
		}
		return relationDenial(actionKey, scope, evaluation, detail), nil
	}
	return allowDecision(actionKey, scope), nil
}

func allowDecision(actionKey string, scope Scope) Decision {
	return Decision{Allow: true, Reason: ReasonAllow, Key: actionKey, Scope: scope}
}

func denyDecision(actionKey string, scope Scope, reason Reason, detail string) Decision {
	return Decision{Allow: false, Reason: reason, Key: actionKey, Scope: scope, Detail: detail}
}

// relationDenial picks unresolved_subject over missing_relation when any tuple
// could not be resolved: the caller must not conclude "no relation" from data
// the engine could not read.
func relationDenial(actionKey string, scope Scope, evaluation RelationEvaluation, detail string) Decision {
	if evaluation.Unresolved {
		if evaluation.Detail != "" {
			detail = detail + "; " + evaluation.Detail
		}
		return denyDecision(actionKey, scope, ReasonUnresolvedSubject, detail)
	}
	return denyDecision(actionKey, scope, ReasonMissingRelation, detail)
}
