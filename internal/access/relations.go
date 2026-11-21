package access

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Relation is one of the four fixed resource-relation words. The vocabulary is
// closed; new sharing semantics need a new change, not a new word at runtime
// (L3).
type Relation string

const (
	// RelationNone is the metadata marker for a key that has no resource
	// relation. It is never stored in resource_relations.
	RelationNone Relation = ""
	// RelationOwner is full control over the resource.
	RelationOwner Relation = "owner"
	// RelationTeamMember marks the principal as a member of the team that owns
	// or shares the resource.
	RelationTeamMember Relation = "team_member"
	// RelationViewer can read the resource.
	RelationViewer Relation = "viewer"
	// RelationEditor can write the resource.
	RelationEditor Relation = "editor"
)

// ValidRelation reports whether relation is one of the four stored relation
// words. RelationNone is a metadata marker and is not valid here.
func ValidRelation(relation Relation) bool {
	switch relation {
	case RelationOwner, RelationTeamMember, RelationViewer, RelationEditor:
		return true
	}
	return false
}

// Action is the verb a permission key names. It maps to the minimum relation
// the action needs; individual key metadata may override the mapping when a
// key's semantics differ.
type Action string

// Actions understood by the default relation mapping.
const (
	ActionRead   Action = "read"
	ActionWrite  Action = "write"
	ActionManage Action = "manage"
	ActionDelete Action = "delete"
	ActionInvoke Action = "invoke"
	ActionCreate Action = "create"
)

// RelationForAction maps an action to its minimum resource relation:
//
//	read, invoke   -> viewer
//	write          -> editor
//	manage, delete -> owner
//	create, other  -> no resource relation
//
// A key may declare a different relation in its KeySpec; the registry is the
// authority, this function is the default (design §3.1/§3.3).
func RelationForAction(action Action) Relation {
	switch action {
	case ActionRead, ActionInvoke:
		return RelationViewer
	case ActionWrite:
		return RelationEditor
	case ActionManage, ActionDelete:
		return RelationOwner
	default:
		return RelationNone
	}
}

// relationSatisfaction is the fixed relation lattice. It is not a recursive
// rewrite rule: a held relation directly satisfies a required one. Owner is
// full control, editor includes viewer, and a team member can see team
// resources. Viewer never implies editor and no relation implies team
// membership.
var relationSatisfaction = map[Relation]map[Relation]bool{
	RelationOwner: {
		RelationOwner:  true,
		RelationEditor: true,
		RelationViewer: true,
	},
	RelationEditor: {
		RelationEditor: true,
		RelationViewer: true,
	},
	RelationViewer: {
		RelationViewer: true,
	},
	RelationTeamMember: {
		RelationTeamMember: true,
		RelationViewer:     true,
	},
}

// relationSatisfied reports whether one of the held relations satisfies
// required. RelationNone is always satisfied because the caller has already
// decided the key is not resource-scoped.
func relationSatisfied(held map[Relation]bool, required Relation) bool {
	if required == RelationNone {
		return true
	}
	for relation := range held {
		if relationSatisfaction[relation][required] {
			return true
		}
	}
	return false
}

// SubjectType is the kind of subject a relation tuple names.
type SubjectType string

// Relation subject types. A group is a permission group; a team_bot subject
// lets a team share a resource without TeamBot becoming a permission subject
// (L12).
const (
	SubjectTypeAccount SubjectType = "account"
	SubjectTypeGroup   SubjectType = "group"
	SubjectTypeTeamBot SubjectType = "team_bot"
)

// ValidSubjectType reports whether subjectType is account, group or team_bot.
func ValidSubjectType(subjectType SubjectType) bool {
	switch subjectType {
	case SubjectTypeAccount, SubjectTypeGroup, SubjectTypeTeamBot:
		return true
	}
	return false
}

// Resource is the object of an authorization decision. ResourceType stays
// open (session, bot, team_bot, task, ...); the relation vocabulary is closed.
// IDs are text so one relation table can name uuid resources and text handles
// (A1 decision 1).
type Resource struct {
	TenantID uuid.UUID
	Type     string
	ID       string
}

// Valid reports whether the resource is a complete reference. An incomplete
// resource can never have a relation, so authorization fails closed.
func (r Resource) Valid() bool {
	return r.TenantID != uuid.Nil && r.Type != "" && r.ID != ""
}

// RelationTuple is one stored relation row as the evaluator sees it.
type RelationTuple struct {
	Relation    Relation
	SubjectType SubjectType
	SubjectID   string
}

// ErrUnresolvedSubject marks a relation tuple whose subject cannot be
// resolved: a missing group or team bot, a malformed id, or a roster that
// cannot be read. The evaluator treats it as a fail-closed denial, never as
// "not a member".
var ErrUnresolvedSubject = errors.New("access: unresolved relation subject")

// RelationSource is the read seam for ReBAC-lite. A4 adapts it to
// resource_relations, permission_group_members and the TeamBot roster.
//
// The source is trusted to scope its reads by resource.TenantID; the evaluator
// cannot verify a tuple's tenant from the tuple itself.
type RelationSource interface {
	// RelationsForResource returns every tuple attached to resource. Order is
	// irrelevant. A malformed tuple is rejected by the evaluator, fail closed.
	RelationsForResource(ctx context.Context, resource Resource) ([]RelationTuple, error)

	// ResolveSubject reports whether principal is the subject or a member of
	// it. A subject that cannot be resolved returns ErrUnresolvedSubject; a
	// resolvable subject the principal does not belong to returns (false, nil).
	// Account subjects are matched by the evaluator and need not be handled
	// here, although a source may implement them.
	ResolveSubject(ctx context.Context, principal Principal, subjectType SubjectType, subjectID string) (bool, error)
}

// RelationEvaluation is the result of matching a principal against one
// resource's relation tuples.
type RelationEvaluation struct {
	// Held is every relation the principal holds, directly or through a group
	// or team bot.
	Held map[Relation]bool
	// Direct is the subset of Held that comes from a tuple naming the
	// principal's own account. It is what "own" scope reaches.
	Direct map[Relation]bool
	// Unresolved is true when at least one tuple could not be resolved. That
	// tuple never grants access; if no other tuple allows the action, the
	// decision reason is unresolved_subject.
	Unresolved bool
	// Detail explains the first unresolved tuple, for audit and 403 copy.
	Detail string
}

func (e *RelationEvaluation) markUnresolved(detail string) {
	e.Unresolved = true
	if e.Detail == "" {
		e.Detail = detail
	}
}

// EvaluateRelations matches principal against every tuple attached to
// resource. Account tuples are matched locally; group and team_bot tuples go
// through RelationSource.ResolveSubject. An unresolvable subject or a
// malformed tuple marks the evaluation unresolved and never grants.
func EvaluateRelations(ctx context.Context, source RelationSource, principal Principal, resource Resource) (RelationEvaluation, error) {
	evaluation := RelationEvaluation{
		Held:   make(map[Relation]bool, 4),
		Direct: make(map[Relation]bool, 4),
	}
	if source == nil {
		return evaluation, errors.New("access: relation source is nil")
	}
	if !resource.Valid() {
		return evaluation, errors.New("access: relation resource is incomplete")
	}
	tuples, err := source.RelationsForResource(ctx, resource)
	if err != nil {
		return evaluation, err
	}
	for _, tuple := range tuples {
		if !ValidRelation(tuple.Relation) || !ValidSubjectType(tuple.SubjectType) || tuple.SubjectID == "" {
			evaluation.markUnresolved(fmt.Sprintf("malformed relation tuple (relation %q, subject %q %q)",
				tuple.Relation, tuple.SubjectType, tuple.SubjectID))
			continue
		}
		switch tuple.SubjectType {
		case SubjectTypeAccount:
			if _, err := uuid.Parse(tuple.SubjectID); err != nil {
				evaluation.markUnresolved(fmt.Sprintf("account subject %q is not a uuid", tuple.SubjectID))
				continue
			}
			if tuple.SubjectID == principal.AccountID.String() {
				evaluation.Held[tuple.Relation] = true
				evaluation.Direct[tuple.Relation] = true
			}
		case SubjectTypeGroup:
			matched, err := source.ResolveSubject(ctx, principal, tuple.SubjectType, tuple.SubjectID)
			if errors.Is(err, ErrUnresolvedSubject) {
				evaluation.markUnresolved(fmt.Sprintf("group subject %q cannot be resolved", tuple.SubjectID))
				continue
			}
			if err != nil {
				return evaluation, err
			}
			if matched {
				evaluation.Held[tuple.Relation] = true
			}
		case SubjectTypeTeamBot:
			// A human principal has no BotID and can never be a member of a
			// TeamBot roster. That is a resolvable "not a member", not an
			// unresolved subject.
			if principal.BotID == "" {
				continue
			}
			matched, err := source.ResolveSubject(ctx, principal, tuple.SubjectType, tuple.SubjectID)
			if errors.Is(err, ErrUnresolvedSubject) {
				evaluation.markUnresolved(fmt.Sprintf("team_bot subject %q cannot be resolved", tuple.SubjectID))
				continue
			}
			if err != nil {
				return evaluation, err
			}
			if matched {
				evaluation.Held[tuple.Relation] = true
			}
		}
	}
	return evaluation, nil
}

// scopeReachSatisfied reports whether the scope reaches a relation that
// satisfies required:
//
//	own          the required relation must be held directly by the account
//	team         the required relation may come from a group or team bot
//	tenant       same as team for one resource; the wider rank matters for
//	             tenant-level keys, delegation and aggregate queries
//	all          same as team, including resources in another tenant
//
// Scope never substitutes for a relation: a tenant- or all-scoped grant with
// no relation is still denied (spec: 有 key 但无关系仍拒绝).
func scopeReachSatisfied(scope Scope, evaluation RelationEvaluation, required Relation) bool {
	switch scope {
	case ScopeOwn:
		return relationSatisfied(evaluation.Direct, required)
	case ScopeTeam, ScopeTenant, ScopeAll:
		return relationSatisfied(evaluation.Held, required)
	default:
		return false
	}
}
