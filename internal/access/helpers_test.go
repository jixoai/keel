package access

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// Deterministic identities shared by the access tests. They are arbitrary but
// stable, so a failure reads as "Alice" rather than a random uuid.
var (
	tenantOne    = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tenantTwo    = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	accountAlice = uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	accountBob   = uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	accountCarol = uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	groupSupport = uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")

	botX        = "bot-x"
	botY        = "bot-y"
	teamSupport = "team-support"
)

// resourceOf builds a complete resource reference.
func resourceOf(tenantID uuid.UUID, resourceType, resourceID string) Resource {
	return Resource{TenantID: tenantID, Type: resourceType, ID: resourceID}
}

// relationResourceKey names a resource in the fake source's tuple index. The
// tenant is part of the key so a cross-tenant test can hold the same type/id
// in two tenants without the tuples colliding.
func relationResourceKey(resource Resource) string {
	return resource.TenantID.String() + "/" + resource.Type + "/" + resource.ID
}

// fakeRelationSource holds resource tuples plus the two membership sets the
// evaluator resolves: permission-group members and TeamBot rosters.
type fakeRelationSource struct {
	tuples map[string][]RelationTuple

	groups map[string]map[string]bool
	teams  map[string]map[string]bool

	unresolvedGroups map[string]bool
	unresolvedTeams  map[string]bool

	relationsErr error
	resolveErr   error

	relationsCalls int
	resolveCalls   int
}

func newFakeRelationSource() *fakeRelationSource {
	return &fakeRelationSource{
		tuples:           make(map[string][]RelationTuple),
		groups:           make(map[string]map[string]bool),
		teams:            make(map[string]map[string]bool),
		unresolvedGroups: make(map[string]bool),
		unresolvedTeams:  make(map[string]bool),
	}
}

func (s *fakeRelationSource) put(resource Resource, relation Relation, subjectType SubjectType, subjectID string) {
	key := relationResourceKey(resource)
	s.tuples[key] = append(s.tuples[key], RelationTuple{
		Relation:    relation,
		SubjectType: subjectType,
		SubjectID:   subjectID,
	})
}

func (s *fakeRelationSource) RelationsForResource(ctx context.Context, resource Resource) ([]RelationTuple, error) {
	s.relationsCalls++
	if s.relationsErr != nil {
		return nil, s.relationsErr
	}
	return append([]RelationTuple(nil), s.tuples[relationResourceKey(resource)]...), nil
}

func (s *fakeRelationSource) ResolveSubject(ctx context.Context, principal Principal, subjectType SubjectType, subjectID string) (bool, error) {
	s.resolveCalls++
	if s.resolveErr != nil {
		return false, s.resolveErr
	}
	switch subjectType {
	case SubjectTypeAccount:
		return subjectID == principal.AccountID.String(), nil
	case SubjectTypeGroup:
		if s.unresolvedGroups[subjectID] {
			return false, ErrUnresolvedSubject
		}
		return s.groups[subjectID][principal.AccountID.String()], nil
	case SubjectTypeTeamBot:
		if s.unresolvedTeams[subjectID] {
			return false, ErrUnresolvedSubject
		}
		return s.teams[subjectID][principal.BotID], nil
	default:
		return false, ErrUnresolvedSubject
	}
}

// failErr is a tiny sentinel for source-failure tests.
var failErr = errors.New("source unavailable")
