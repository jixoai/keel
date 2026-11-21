package agentapi

import (
	"context"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
)

// fakeAccess is a test double for the Access seam. The identity endpoint tests
// that need a real store use accesswire.Bridge over KEEL_TEST_DSN; this fake is
// for the permission-free whoami contract and for fail-closed plumbing checks.
type fakeAccess struct {
	principal    access.Principal
	principalErr error
	grants       access.Grants
	decisions    map[string]access.Decision
	visible      access.VisibleResult
	visibleAll   bool
	visibleErr   error
	canGrantErr  error
	owner        uuid.UUID
	archived     bool

	denials []access.Decision
	granted []string
}

func (f *fakeAccess) Principal(context.Context, uuid.UUID) (access.Principal, error) {
	return f.principal, f.principalErr
}

func (f *fakeAccess) Authorize(_ context.Context, _ access.Principal, key string, _ access.Resource) (access.Decision, error) {
	if decision, ok := f.decisions[key]; ok {
		return decision, nil
	}
	return access.Decision{Allow: true, Reason: access.ReasonAllow, Key: key, Scope: access.ScopeTenant}, nil
}

func (f *fakeAccess) Visible(_ context.Context, _ access.Principal, key, resourceType string, candidateIDs []string) (access.VisibleResult, error) {
	if f.visibleErr != nil {
		return access.VisibleResult{}, f.visibleErr
	}
	if f.visibleAll {
		return access.VisibleResult{
			IDs:      candidateIDs,
			Scope:    access.ScopeTenant,
			Decision: access.Decision{Allow: true, Reason: access.ReasonAllow, Key: key, Scope: access.ScopeTenant},
		}, nil
	}
	return f.visible, nil
}

func (f *fakeAccess) Effective(context.Context, access.Principal) (access.Grants, error) {
	if f.grants != nil {
		return f.grants, nil
	}
	return access.Grants{}, nil
}

func (f *fakeAccess) CanGrant(context.Context, access.Principal, []access.Grant) error {
	return f.canGrantErr
}

func (f *fakeAccess) BotOwnerAccount(context.Context, uuid.UUID, string) (uuid.UUID, bool, error) {
	return f.owner, f.archived, nil
}

func (f *fakeAccess) AuditGrant(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string, string, string, string, map[string]any) error {
	return nil
}

func (f *fakeAccess) AuditRelation(context.Context, uuid.UUID, uuid.UUID, string, string, string, string, string, string) error {
	return nil
}

func (f *fakeAccess) AuditSuperAdmin(context.Context, access.Principal, string, map[string]any) error {
	return nil
}

func (f *fakeAccess) AuditInvoke(context.Context, uuid.UUID, access.Principal, string, uuid.UUID, string) error {
	return nil
}

func (f *fakeAccess) AuditBotKey(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string, string, string, string) error {
	return nil
}

func (f *fakeAccess) Denied(_ context.Context, _ access.Principal, decision access.Decision, _ access.Resource) {
	f.denials = append(f.denials, decision)
}

// permitAllAccess is the explicit test seam for tests that exercise listing
// mechanics rather than visibility. Production always wires the real bridge.
func permitAllAccess() *fakeAccess {
	return &fakeAccess{visibleAll: true}
}
