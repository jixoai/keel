package accesswire

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/store"
)

// testStore opens the KEEL_TEST_DSN store or skips. Every test uses fresh
// tenant ids, so the suite is independent of other packages sharing the
// database.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	st, err := store.Open(context.Background(), store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

type fixture struct {
	tenant  uuid.UUID
	owner   store.AccountRecord
	other   store.AccountRecord
	profile store.ProfileRecord
	bot     store.BotRecord
	group   store.PermissionGroupRecord
}

// newFixture creates one tenant with two accounts, a role/profile, a bot whose
// Harness narrows to session.read, and one group. The owner holds
// session.read/own directly and the group grants bot.read/tenant.
func newFixture(t *testing.T, st *store.Store) fixture {
	t.Helper()
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "accesswire-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	owner, err := st.CreateAccount(ctx, store.AccountRecord{
		TenantID: tenant, ID: uuid.New(), Username: "owner-" + tenant.String()[:8],
		DisplayName: "Owner", Kind: "human", PasswordHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateAccount(ctx, store.AccountRecord{
		TenantID: tenant, ID: uuid.New(), Username: "other-" + tenant.String()[:8],
		DisplayName: "Other", Kind: "human",
	})
	if err != nil {
		t.Fatal(err)
	}
	role, err := st.CreateRole(ctx, store.RoleRecord{
		TenantID: tenant, ID: uuid.New(), Name: "Support",
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := st.CreateProfile(ctx, store.ProfileRecord{
		TenantID: tenant, ID: uuid.New(), AccountID: owner.ID, RoleID: role.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	bot, err := st.CreateBot(ctx, store.BotRecord{
		TenantID: tenant, ID: "bot-" + tenant.String()[:8], Name: "Bot",
		ProfileID: profile.ID, ModelKind: "text",
		DefaultModel: json.RawMessage(`{"providerId":"magpie","modelId":"qwen"}`),
		Harness:      json.RawMessage(`{"tools":["bash"],"context":{"permissions":["session.read"]}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	group, err := st.CreatePermissionGroup(ctx, store.PermissionGroupRecord{
		TenantID: tenant, ID: uuid.New(), Name: "ops-" + tenant.String()[:8],
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddPermissionGroupGrant(ctx, tenant, group.ID, access.KeyBotRead, string(access.ScopeTenant)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddPermissionGroupMember(ctx, tenant, group.ID, owner.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePermissionGrant(ctx, store.PermissionGrantRecord{
		TenantID: tenant, ID: uuid.New(), SubjectType: store.PermissionSubjectAccount,
		SubjectID: owner.ID, PermissionKey: access.KeySessionRead, Scope: string(access.ScopeOwn),
		GrantedBy: owner.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutResourceRelation(ctx, store.ResourceRelation{
		TenantID: tenant, ResourceType: "bot", ResourceID: bot.ID,
		Relation: store.RelationOwner, SubjectType: store.RelationSubjectAccount,
		SubjectID: owner.ID.String(), CreatedBy: owner.ID,
	}); err != nil {
		t.Fatal(err)
	}
	return fixture{tenant: tenant, owner: owner, other: other, profile: profile, bot: bot, group: group}
}

func principalFor(f fixture) access.Principal {
	return access.Principal{TenantID: f.tenant, AccountID: f.owner.ID}
}

func TestSourceDirectAndGroupGrantsAreSeparate(t *testing.T) {
	st := testStore(t)
	f := newFixture(t, st)
	source, err := NewSource(st)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	direct, err := source.DirectGrants(ctx, f.tenant, f.owner.ID)
	if err != nil {
		t.Fatalf("DirectGrants: %v", err)
	}
	if len(direct) != 1 || direct[0].Key != access.KeySessionRead || direct[0].Scope != access.ScopeOwn {
		t.Fatalf("direct = %+v, want session.read/own only", direct)
	}
	grouped, err := source.GroupGrants(ctx, f.tenant, f.owner.ID)
	if err != nil {
		t.Fatalf("GroupGrants: %v", err)
	}
	if len(grouped) != 1 || grouped[0].Key != access.KeyBotRead || grouped[0].Scope != access.ScopeTenant {
		t.Fatalf("group = %+v, want bot.read/tenant", grouped)
	}
	if got, err := source.GroupGrants(ctx, f.tenant, f.other.ID); err != nil || len(got) != 0 {
		t.Fatalf("non-member group grants = %+v err=%v, want none", got, err)
	}
}

func TestDirectGrantGroupSubjectIsRejectedAtEveryLayer(t *testing.T) {
	st := testStore(t)
	f := newFixture(t, st)
	source, err := NewSource(st)
	if err != nil {
		t.Fatal(err)
	}
	// The store write refuses it...
	if _, err := st.CreatePermissionGrant(context.Background(), store.PermissionGrantRecord{
		TenantID: f.tenant, ID: uuid.New(), SubjectType: "group", SubjectID: f.group.ID,
		PermissionKey: access.KeyBotRead, Scope: string(access.ScopeTenant), GrantedBy: f.owner.ID,
	}); err == nil {
		t.Fatal("store accepted a group-subject direct grant")
	}
	// ...and the schema has the matching CHECK, so a row cannot exist to begin
	// with. The adapter's ValidateDirectGrantSubject is the third layer.
	var constraint bool
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'permission_grants_subject_check')`).
		Scan(&constraint); err != nil {
		t.Fatal(err)
	}
	if !constraint {
		t.Fatal("permission_grants_subject_check is missing")
	}
	if err := access.ValidateDirectGrantSubject("group"); err == nil {
		t.Fatal("access accepted a group direct-grant subject")
	}
	direct, err := source.DirectGrants(context.Background(), f.tenant, f.owner.ID)
	if err != nil {
		t.Fatalf("DirectGrants: %v", err)
	}
	for _, grant := range direct {
		if grant.Key == access.KeyBotRead {
			t.Fatal("a group grant leaked into DirectGrants")
		}
	}
}

func TestSourceRelationsAndBatch(t *testing.T) {
	st := testStore(t)
	f := newFixture(t, st)
	source, err := NewSource(st)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	one := access.Resource{TenantID: f.tenant, Type: "bot", ID: f.bot.ID}
	tuples, err := source.RelationsForResource(ctx, one)
	if err != nil {
		t.Fatalf("RelationsForResource: %v", err)
	}
	if len(tuples) != 1 || tuples[0].Relation != access.RelationOwner || tuples[0].SubjectID != f.owner.ID.String() {
		t.Fatalf("tuples = %+v", tuples)
	}
	batch, err := source.RelationsForResources(ctx, one, []string{f.bot.ID, "absent"})
	if err != nil {
		t.Fatalf("RelationsForResources: %v", err)
	}
	if len(batch[f.bot.ID]) != 1 || len(batch["absent"]) != 0 {
		t.Fatalf("batch = %+v", batch)
	}
}

func TestSourceResolveGroupAndTeamBot(t *testing.T) {
	st := testStore(t)
	f := newFixture(t, st)
	source, err := NewSource(st)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	principal := principalFor(f)

	member, err := source.ResolveSubject(ctx, principal, access.SubjectTypeGroup, f.group.ID.String())
	if err != nil || !member {
		t.Fatalf("owner group membership = %v err=%v, want true", member, err)
	}
	notMember, err := source.ResolveSubject(ctx, principal, access.SubjectTypeGroup, uuid.NewString())
	if err == nil || notMember {
		t.Fatalf("unknown group = %v err=%v, want unresolved", notMember, err)
	}
	otherMember, err := source.ResolveSubject(ctx, access.Principal{TenantID: f.tenant, AccountID: f.other.ID}, access.SubjectTypeGroup, f.group.ID.String())
	if err != nil || otherMember {
		t.Fatalf("non-member = %v err=%v, want false,nil", otherMember, err)
	}

	team, err := st.CreateTeamBot(ctx, store.TeamBotRecord{
		TenantID: f.tenant, ID: "team-" + f.tenant.String()[:8], Name: "Team",
		Mission: "resolve", Roster: json.RawMessage(`[{"botId":"` + f.bot.ID + `","role":"captain"}]`),
		Revision: 1, ContentHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	botPrincipal := access.Principal{TenantID: f.tenant, AccountID: f.owner.ID, BotID: f.bot.ID}
	inRoster, err := source.ResolveSubject(ctx, botPrincipal, access.SubjectTypeTeamBot, team.ID)
	if err != nil || !inRoster {
		t.Fatalf("roster membership = %v err=%v, want true", inRoster, err)
	}
	botPrincipal.BotID = "someone-else"
	outside, err := source.ResolveSubject(ctx, botPrincipal, access.SubjectTypeTeamBot, team.ID)
	if err != nil || outside {
		t.Fatalf("non-roster bot = %v err=%v, want false,nil", outside, err)
	}
	if _, err := source.ResolveSubject(ctx, botPrincipal, access.SubjectTypeTeamBot, "missing-team"); err == nil {
		t.Fatal("missing team_bot subject did not fail closed")
	}
	// A roster that is valid JSON but not an array cannot be resolved.
	broken, err := st.CreateTeamBot(ctx, store.TeamBotRecord{
		TenantID: f.tenant, ID: "broken-" + f.tenant.String()[:8], Name: "Broken",
		Mission: "broken", Roster: json.RawMessage(`"not-a-roster"`), Revision: 1, ContentHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.ResolveSubject(ctx, botPrincipal, access.SubjectTypeTeamBot, broken.ID); err == nil {
		t.Fatal("undecodable roster did not fail closed")
	}
}

func TestEnsureBuiltinGroupsIsIdempotent(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "seed-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ {
		if err := EnsureBuiltinGroups(ctx, st, tenant); err != nil {
			t.Fatalf("seed run %d: %v", run, err)
		}
	}
	groups, err := st.ListPermissionGroups(ctx, tenant, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != len(access.BuiltinGroups()) {
		t.Fatalf("groups = %d, want %d", len(groups), len(access.BuiltinGroups()))
	}
	total := 0
	for _, group := range groups {
		if !group.Builtin {
			t.Fatalf("group %q is not marked builtin", group.Name)
		}
		grants, err := st.ListPermissionGroupGrants(ctx, tenant, group.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(grants) == 0 {
			t.Fatalf("group %q has no grants", group.Name)
		}
		total += len(grants)
	}
	want := 0
	for _, preset := range access.BuiltinGroups() {
		want += len(preset.Grants)
	}
	if total != want {
		t.Fatalf("grant rows = %d, want %d (second seed duplicated or pruned)", total, want)
	}
}

func TestBridgeResolvesPrincipalAndNarrowsByHarness(t *testing.T) {
	st := testStore(t)
	f := newFixture(t, st)
	bridge, err := NewBridge(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	edge := edgePrincipal(f)
	principal, err := bridge.Principal(WithEdgePrincipal(ctx, edge), f.tenant)
	if err != nil {
		t.Fatalf("Principal: %v", err)
	}
	if principal.AccountID != f.owner.ID || principal.TenantID != f.tenant {
		t.Fatalf("principal = %+v", principal)
	}
	effective, err := bridge.Effective(ctx, principal)
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if scope, ok := effective.ScopeOf(access.KeyBotRead); !ok || scope != access.ScopeTenant {
		t.Fatalf("effective bot.read = %q ok=%v", scope, ok)
	}

	// A bot principal narrows to its Harness list (session.read only).
	botEdge := edge
	botEdge.Kind = "bot"
	botEdge.BotID = f.bot.ID
	botPrincipal, err := bridge.Principal(WithEdgePrincipal(ctx, botEdge), f.tenant)
	if err != nil {
		t.Fatalf("bot Principal: %v", err)
	}
	botEffective, err := bridge.Effective(ctx, botPrincipal)
	if err != nil {
		t.Fatalf("bot Effective: %v", err)
	}
	if _, ok := botEffective.ScopeOf(access.KeyBotRead); ok {
		t.Fatal("harness narrowing kept bot.read")
	}
	if _, ok := botEffective.ScopeOf(access.KeySessionRead); !ok {
		t.Fatal("harness narrowing dropped session.read")
	}

	// A tenant mismatch (header injection attempt) is not a principal.
	if _, err := bridge.Principal(WithEdgePrincipal(ctx, edge), uuid.New()); err == nil {
		t.Fatal("tenant mismatch resolved a principal")
	}
}

func TestBridgeAuditsDenial(t *testing.T) {
	st := testStore(t)
	f := newFixture(t, st)
	bridge, err := NewBridge(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	principal := principalFor(f)
	otherBot := "other-bot"
	decision, err := bridge.Authorize(ctx, principal, access.KeyBotManage, access.Resource{
		TenantID: f.tenant, Type: "bot", ID: otherBot,
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if decision.Allow {
		t.Fatal("bot.manage allowed without a grant")
	}
	events, err := st.ListAuthEvents(ctx, f.tenant, store.AuthEventFilter{
		AccountID: f.owner.ID, Kind: store.AuthEventPermissionDenied,
	}, 0)
	if err != nil {
		t.Fatalf("ListAuthEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("denial events = %d, want 1", len(events))
	}
	if got := events[0].Detail["reason"]; got != string(access.ReasonMissingGrant) {
		t.Fatalf("audit reason = %v, want missing_grant", got)
	}
	if events[0].Result != store.AuthResultDenied {
		t.Fatalf("audit result = %q, want denied", events[0].Result)
	}
}
