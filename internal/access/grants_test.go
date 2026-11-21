package access

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// grantOf is a readability helper for the test tables.
func grantOf(key string, scope Scope) Grant {
	return Grant{Key: key, Scope: scope}
}

// fakeGrantSource is a GrantSource with separate direct and group buckets.
type fakeGrantSource struct {
	direct    map[uuid.UUID][]Grant
	group     map[uuid.UUID][]Grant
	directErr error
	groupErr  error

	directCalls int
	groupCalls  int
}

func (s *fakeGrantSource) DirectGrants(ctx context.Context, tenantID, accountID uuid.UUID) ([]Grant, error) {
	s.directCalls++
	if s.directErr != nil {
		return nil, s.directErr
	}
	return append([]Grant(nil), s.direct[accountID]...), nil
}

func (s *fakeGrantSource) GroupGrants(ctx context.Context, tenantID, accountID uuid.UUID) ([]Grant, error) {
	s.groupCalls++
	if s.groupErr != nil {
		return nil, s.groupErr
	}
	return append([]Grant(nil), s.group[accountID]...), nil
}

func TestEffectiveGrantsUnionTakesMax(t *testing.T) {
	source := &fakeGrantSource{
		direct: map[uuid.UUID][]Grant{
			accountAlice: {
				grantOf(KeySessionRead, ScopeOwn),
				grantOf(KeySessionWrite, ScopeTeam),
			},
		},
		group: map[uuid.UUID][]Grant{
			accountAlice: {
				grantOf(KeySessionRead, ScopeTenant),
				grantOf(KeyBotInvoke, ScopeTenant),
			},
		},
	}
	effective, err := EffectiveGrants(context.Background(), source, tenantOne, accountAlice)
	if err != nil {
		t.Fatalf("EffectiveGrants: %v", err)
	}
	cases := []struct {
		key   string
		want  Scope
		found bool
	}{
		{KeySessionRead, ScopeTenant, true},
		{KeySessionWrite, ScopeTeam, true},
		{KeyBotInvoke, ScopeTenant, true},
		{KeySecretReadValue, "", false},
	}
	for _, testCase := range cases {
		got, ok := effective.ScopeOf(testCase.key)
		if ok != testCase.found || got != testCase.want {
			t.Fatalf("ScopeOf(%q) = (%q, %v), want (%q, %v)", testCase.key, got, ok, testCase.want, testCase.found)
		}
	}
	if source.directCalls != 1 || source.groupCalls != 1 {
		t.Fatalf("source calls = direct %d / group %d, want 1 / 1", source.directCalls, source.groupCalls)
	}
}

func TestEffectiveGrantsDirectAndGroupDoNotMaskEachOther(t *testing.T) {
	source := &fakeGrantSource{
		direct: map[uuid.UUID][]Grant{
			accountAlice: {grantOf(KeySessionRead, ScopeOwn)},
		},
		group: map[uuid.UUID][]Grant{
			accountAlice: {grantOf(KeySessionRead, ScopeAll)},
		},
	}
	effective, err := EffectiveGrants(context.Background(), source, tenantOne, accountAlice)
	if err != nil {
		t.Fatalf("EffectiveGrants: %v", err)
	}
	scope, ok := effective.ScopeOf(KeySessionRead)
	if !ok || scope != ScopeAll {
		t.Fatalf("session.read scope = (%q, %v), want all", scope, ok)
	}
}

func TestEffectiveGrantsRejectsInvalidRows(t *testing.T) {
	cases := []struct {
		name   string
		source *fakeGrantSource
	}{
		{
			name: "unknown direct key",
			source: &fakeGrantSource{direct: map[uuid.UUID][]Grant{
				accountAlice: {grantOf("session.fly", ScopeOwn)},
			}},
		},
		{
			name: "unknown group scope",
			source: &fakeGrantSource{group: map[uuid.UUID][]Grant{
				accountAlice: {grantOf(KeySessionRead, "world")},
			}},
		},
		{
			name: "scope not allowed for key",
			source: &fakeGrantSource{direct: map[uuid.UUID][]Grant{
				accountAlice: {grantOf(KeyAccountManage, ScopeOwn)},
			}},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := EffectiveGrants(context.Background(), testCase.source, tenantOne, accountAlice); err == nil {
				t.Fatal("EffectiveGrants succeeded, want an error")
			}
		})
	}
}

func TestEffectiveGrantsSourceErrors(t *testing.T) {
	if _, err := EffectiveGrants(context.Background(), &fakeGrantSource{directErr: failErr}, tenantOne, accountAlice); !errors.Is(err, failErr) {
		t.Fatalf("direct error = %v, want %v", err, failErr)
	}
	if _, err := EffectiveGrants(context.Background(), &fakeGrantSource{groupErr: failErr}, tenantOne, accountAlice); !errors.Is(err, failErr) {
		t.Fatalf("group error = %v, want %v", err, failErr)
	}
	if _, err := EffectiveGrants(context.Background(), nil, tenantOne, accountAlice); err == nil {
		t.Fatal("nil source succeeded")
	}
	if _, err := EffectiveGrants(context.Background(), &fakeGrantSource{}, uuid.Nil, accountAlice); err == nil {
		t.Fatal("missing tenant succeeded")
	}
	if _, err := EffectiveGrants(context.Background(), &fakeGrantSource{}, tenantOne, uuid.Nil); err == nil {
		t.Fatal("missing account succeeded")
	}
}

func TestCanGrant(t *testing.T) {
	grantor := Grantor{Effective: Grants{
		KeySessionRead: ScopeOwn,
		KeyBotInvoke:   ScopeTenant,
	}}
	cases := []struct {
		name     string
		proposed []Grant
		wantErr  bool
		reason   Reason
	}{
		{name: "same tuple", proposed: []Grant{grantOf(KeySessionRead, ScopeOwn)}},
		{name: "narrower", proposed: []Grant{grantOf(KeySessionRead, ScopeOwn)}},
		{name: "wider scope", proposed: []Grant{grantOf(KeySessionRead, ScopeTeam)}, wantErr: true, reason: ReasonMissingGrant},
		{name: "key not held", proposed: []Grant{grantOf(KeySecretReadValue, ScopeTenant)}, wantErr: true, reason: ReasonMissingGrant},
		{name: "unknown key", proposed: []Grant{grantOf("session.fly", ScopeOwn)}, wantErr: true, reason: ReasonUnknownKey},
		{name: "invalid scope", proposed: []Grant{grantOf(KeySessionRead, "world")}, wantErr: true, reason: ReasonInvalidScope},
		{name: "disallowed scope", proposed: []Grant{grantOf(KeyAccountManage, ScopeOwn)}, wantErr: true, reason: ReasonInvalidScope},
		{name: "empty", proposed: nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := CanGrant(grantor, testCase.proposed)
			if !testCase.wantErr {
				if err != nil {
					t.Fatalf("CanGrant: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("CanGrant succeeded, want a rejection")
			}
			var rejection *GrantRejection
			if !errors.As(err, &rejection) {
				t.Fatalf("error = %T, want *GrantRejection", err)
			}
			if rejection.Reason != testCase.reason {
				t.Fatalf("reason = %q, want %q", rejection.Reason, testCase.reason)
			}
		})
	}
}

func TestCanGrantSuperAdmin(t *testing.T) {
	grantor := Grantor{SuperAdmin: true}
	if err := CanGrant(grantor, []Grant{
		grantOf(KeySecretReadValue, ScopeAll),
		grantOf(KeyAccountPermissionGrant, ScopeTenant),
	}); err != nil {
		t.Fatalf("super admin cannot grant a known pair: %v", err)
	}
	err := CanGrant(grantor, []Grant{grantOf("session.fly", ScopeOwn)})
	if err == nil {
		t.Fatal("super admin granted an unknown key")
	}
	var rejection *GrantRejection
	if !errors.As(err, &rejection) || rejection.Reason != ReasonUnknownKey {
		t.Fatalf("unknown-key rejection = %v", err)
	}
}

func TestCanGrantUsesGroupGrantAuthority(t *testing.T) {
	// EffectiveGrants has already merged group grants; CanGrant must treat a
	// group-derived tuple as the grantor's own authority. The detail that
	// matters is that the tuple is in the set, not where it came from.
	grantor := Grantor{Effective: Grants{KeySessionRead: ScopeTenant}}
	if err := CanGrant(grantor, []Grant{grantOf(KeySessionRead, ScopeTenant)}); err != nil {
		t.Fatalf("grantor with a group-derived tenant scope cannot grant tenant scope: %v", err)
	}
	if err := CanGrant(grantor, []Grant{grantOf(KeySessionRead, ScopeAll)}); err == nil {
		t.Fatal("group-derived tenant scope was stretched to all")
	}
}

func TestValidateDirectGrantSubject(t *testing.T) {
	if err := ValidateDirectGrantSubject(GrantSubjectAccount); err != nil {
		t.Fatalf("account subject rejected: %v", err)
	}
	if err := ValidateDirectGrantSubject(GrantSubjectGroup); err == nil {
		t.Fatal("group subject was accepted as a direct grant")
	}
	if err := ValidateDirectGrantSubject("bot"); err == nil {
		t.Fatal("unknown subject was accepted")
	}
}

func TestNarrowToHarness(t *testing.T) {
	account := Grants{
		KeySessionRead:  ScopeTenant,
		KeySessionWrite: ScopeOwn,
		KeyBotInvoke:    ScopeTenant,
	}
	narrowed, err := NarrowToHarness(account, []string{KeySessionRead})
	if err != nil {
		t.Fatalf("NarrowToHarness: %v", err)
	}
	if scope, ok := narrowed.ScopeOf(KeySessionRead); !ok || scope != ScopeTenant {
		t.Fatalf("session.read = (%q, %v), want tenant", scope, ok)
	}
	if _, ok := narrowed.ScopeOf(KeySessionWrite); ok {
		t.Fatal("session.write survived a harness that did not list it")
	}

	empty, err := NarrowToHarness(account, nil)
	if err != nil {
		t.Fatalf("NarrowToHarness(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty harness narrowed to %d keys, want 0", len(empty))
	}

	knownButAbsent, err := NarrowToHarness(account, []string{KeySecretReadValue})
	if err != nil {
		t.Fatalf("NarrowToHarness(known absent): %v", err)
	}
	if len(knownButAbsent) != 0 {
		t.Fatalf("harness key the account does not hold produced %v", knownButAbsent)
	}

	if _, err := NarrowToHarness(account, []string{"session.fly"}); err == nil {
		t.Fatal("unknown harness key was accepted")
	}
	if _, err := NarrowToHarness(account, []string{" "}); err == nil {
		t.Fatal("blank harness key was accepted")
	}
}

func TestGrantsHelpers(t *testing.T) {
	grants := Grants{
		KeySessionRead: ScopeTeam,
		KeyBotInvoke:   ScopeTenant,
	}
	if scope, ok := grants.ScopeOf(KeySessionRead); !ok || scope != ScopeTeam {
		t.Fatalf("ScopeOf = (%q, %v)", scope, ok)
	}
	if !grants.Covers(KeySessionRead, ScopeOwn) {
		t.Fatal("team should cover own")
	}
	if grants.Covers(KeySessionRead, ScopeTenant) {
		t.Fatal("team should not cover tenant")
	}
	keys := grants.Keys()
	if len(keys) != 2 || keys[0] != KeyBotInvoke || keys[1] != KeySessionRead {
		t.Fatalf("Keys() = %v, want sorted keys", keys)
	}
	clone := grants.Clone()
	clone[KeySessionRead] = ScopeAll
	if scope, _ := grants.ScopeOf(KeySessionRead); scope != ScopeTeam {
		t.Fatal("Clone shared the backing map")
	}
	var nilGrants Grants
	if _, ok := nilGrants.ScopeOf(KeySessionRead); ok {
		t.Fatal("nil grants reported a key")
	}
}
