package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// identityFixture creates one tenant with an account, role, profile and bot.
func identityFixture(t *testing.T, st *Store) (AccountRecord, RoleRecord, ProfileRecord, BotRecord) {
	t.Helper()
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "identity-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	account, err := st.CreateAccount(ctx, AccountRecord{
		TenantID: tenant, ID: uuid.New(), Username: "owner-" + tenant.String()[:8],
		DisplayName: "Owner", Kind: "human", PasswordHash: "argon2id-demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	role, err := st.CreateRole(ctx, RoleRecord{
		TenantID: tenant, ID: uuid.New(), Name: "Support", Instruction: "Be kind.",
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := st.CreateProfile(ctx, ProfileRecord{
		TenantID: tenant, ID: uuid.New(), AccountID: account.ID, RoleID: role.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	bot, err := st.CreateBot(ctx, BotRecord{
		TenantID: tenant, ID: "bot-" + tenant.String()[:8], Name: "Support One",
		ProfileID: profile.ID, ModelKind: "text",
		DefaultModel: json.RawMessage(`{"providerId":"magpie","modelId":"qwen"}`),
		Harness:      json.RawMessage(`{"tools":["bash"],"context":{"permissions":["session.read"]}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return account, role, profile, bot
}

func TestAccountStoreRoundTrip(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "account-crud"); err != nil {
		t.Fatal(err)
	}
	record := AccountRecord{
		TenantID: tenant, ID: uuid.New(), Username: "Alice",
		DisplayName: " Alice ", Kind: "human", PasswordHash: "hash-1",
	}
	created, err := st.CreateAccount(ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	if created.Username != "Alice" || created.DisplayName != " Alice " || created.PasswordHash != "hash-1" {
		t.Fatalf("created = %+v", created)
	}
	if created.IsSuperAdmin || created.DisabledAt != nil {
		t.Fatalf("created flags = %+v", created)
	}

	got, err := st.GetAccount(ctx, tenant, created.ID)
	if err != nil || got.ID != created.ID {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	byName, err := st.GetAccountByUsername(ctx, tenant, "  alice  ")
	if err != nil || byName.ID != created.ID {
		t.Fatalf("get by username: %+v err=%v", byName, err)
	}
	accounts, err := st.ListAccounts(ctx, tenant, 0)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("list = %+v err=%v", accounts, err)
	}

	renamed, err := st.UpdateAccountDisplayName(ctx, tenant, created.ID, "Alice Cooper")
	if err != nil || renamed.DisplayName != "Alice Cooper" {
		t.Fatalf("rename: %+v err=%v", renamed, err)
	}
	if err := st.SetAccountPasswordHash(ctx, tenant, created.ID, "hash-2"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAccountDisabled(ctx, tenant, created.ID, true); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetAccount(ctx, tenant, created.ID)
	if err != nil || got.DisabledAt == nil || got.PasswordHash != "hash-2" {
		t.Fatalf("after disable: %+v err=%v", got, err)
	}
	if err := st.SetAccountDisabled(ctx, tenant, created.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetAccount(ctx, tenant, created.ID)
	if got.DisabledAt != nil {
		t.Fatal("account stayed disabled")
	}

	// Case-folded usernames collide; a second row cannot take the login name.
	if _, err := st.CreateAccount(ctx, AccountRecord{
		TenantID: tenant, ID: uuid.New(), Username: "ALICE", DisplayName: "Other", Kind: "human",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate username err = %v, want ErrConflict", err)
	}
	if _, err := st.CreateAccount(ctx, AccountRecord{
		TenantID: tenant, ID: created.ID, Username: "other", DisplayName: "Other", Kind: "human",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate id err = %v, want ErrConflict", err)
	}
	if _, err := st.CreateAccount(ctx, AccountRecord{
		TenantID: tenant, ID: uuid.New(), Username: "robot", DisplayName: "Robot", Kind: "robot",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown kind err = %v, want ErrInvalid", err)
	}

	// Another tenant sees nothing, including by id and by username.
	other := uuid.New()
	if err := st.CreateTenant(ctx, other, "account-crud-other"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAccount(ctx, other, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign get err = %v, want ErrNotFound", err)
	}
	if _, err := st.GetAccountByUsername(ctx, other, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign get by username err = %v, want ErrNotFound", err)
	}
	if err := st.SetAccountDisabled(ctx, other, created.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign disable err = %v, want ErrNotFound", err)
	}
	if list, err := st.ListAccounts(ctx, other, 0); err != nil || len(list) != 0 {
		t.Fatalf("foreign list = %+v err=%v", list, err)
	}
}

func TestRoleProfileBotStore(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	account, role, profile, bot := identityFixture(t, st)
	tenant := account.TenantID

	updatedRole, err := st.UpdateRole(ctx, RoleRecord{
		TenantID: tenant, ID: role.ID, Name: "Support Lead", Instruction: "Lead.",
	})
	if err != nil || updatedRole.Name != "Support Lead" {
		t.Fatalf("update role: %+v err=%v", updatedRole, err)
	}
	if _, err := st.CreateProfile(ctx, ProfileRecord{
		TenantID: tenant, ID: uuid.New(), AccountID: uuid.New(), RoleID: role.ID,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("profile with missing account err = %v, want ErrConflict", err)
	}

	gotProfile, err := st.GetProfile(ctx, tenant, profile.ID)
	if err != nil || gotProfile.AccountID != account.ID || gotProfile.RoleID != role.ID {
		t.Fatalf("get profile: %+v err=%v", gotProfile, err)
	}
	profiles, err := st.ListProfilesForAccount(ctx, tenant, account.ID, 0)
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profiles for account = %+v err=%v", profiles, err)
	}

	gotBot, err := st.GetBot(ctx, tenant, bot.ID)
	if err != nil || gotBot.ProfileID != profile.ID || gotBot.ModelKind != "text" {
		t.Fatalf("get bot: %+v err=%v", gotBot, err)
	}
	bots, err := st.ListBots(ctx, tenant, false, 0)
	if err != nil || len(bots) != 1 {
		t.Fatalf("list bots = %+v err=%v", bots, err)
	}

	bot.Name = "Support Two"
	bot.Harness = json.RawMessage(`{"tools":["bash","tasks"]}`)
	updatedBot, err := st.UpdateBot(ctx, bot, false)
	if err != nil || updatedBot.Name != "Support Two" {
		t.Fatalf("update bot: %+v err=%v", updatedBot, err)
	}

	if err := st.ArchiveBot(ctx, tenant, bot.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.ArchiveBot(ctx, tenant, bot.ID); err != nil {
		t.Fatalf("archive is not idempotent: %v", err)
	}
	if active, err := st.ListBots(ctx, tenant, false, 0); err != nil || len(active) != 0 {
		t.Fatalf("archived bot leaked into active list: %+v err=%v", active, err)
	}
	if all, err := st.ListBots(ctx, tenant, true, 0); err != nil || len(all) != 1 || !all[0].Archived {
		t.Fatalf("archived list = %+v err=%v", all, err)
	}
	// The lifecycle CAS refuses a stale update instead of resurrecting the bot.
	bot.Archived = false
	if _, err := st.UpdateBot(ctx, bot, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale bot update err = %v, want ErrConflict", err)
	}

	// A referenced profile and role cannot be deleted (the FKs restrict).
	if err := st.DeleteProfile(ctx, tenant, profile.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete referenced profile err = %v, want ErrConflict", err)
	}
	if err := st.DeleteRole(ctx, tenant, role.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete referenced role err = %v, want ErrConflict", err)
	}
	// Unreferenced rows delete for real.
	spare, err := st.CreateProfile(ctx, ProfileRecord{
		TenantID: tenant, ID: uuid.New(), AccountID: account.ID, RoleID: role.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteProfile(ctx, tenant, spare.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteProfile(ctx, tenant, spare.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete err = %v, want ErrNotFound", err)
	}
	spareRole, err := st.CreateRole(ctx, RoleRecord{TenantID: tenant, ID: uuid.New(), Name: "Spare"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteRole(ctx, tenant, spareRole.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := st.CreateBot(ctx, BotRecord{
		TenantID: tenant, ID: "bad-json", Name: "Bad", ProfileID: profile.ID,
		ModelKind: "text", DefaultModel: json.RawMessage(`not json`),
		Harness: json.RawMessage(`{}`),
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid bot document err = %v, want ErrInvalid", err)
	}
	if _, err := st.CreateBot(ctx, BotRecord{
		TenantID: tenant, ID: "missing-profile", Name: "Missing", ProfileID: uuid.New(),
		ModelKind: "text", DefaultModel: json.RawMessage(`{}`), Harness: json.RawMessage(`{}`),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing profile err = %v, want ErrConflict", err)
	}

	other := uuid.New()
	if err := st.CreateTenant(ctx, other, "bot-crud-other"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetBot(ctx, other, bot.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign bot err = %v, want ErrNotFound", err)
	}
	if _, err := st.GetRole(ctx, other, role.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign role err = %v, want ErrNotFound", err)
	}
}

func TestTeamBotStoreAndRevisionGuard(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "team-bot-crud"); err != nil {
		t.Fatal(err)
	}
	team, err := st.CreateTeamBot(ctx, TeamBotRecord{
		TenantID: tenant, ID: "support-team", Name: "Support Team",
		Mission: "Resolve issues.", Roster: json.RawMessage(`[{"botId":"support-1","role":"captain"}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if team.Revision != 0 || team.ContentHash != "" {
		t.Fatalf("fresh team = %+v", team)
	}
	if _, err := st.CreateTeamBot(ctx, TeamBotRecord{
		TenantID: tenant, ID: team.ID, Name: "Duplicate", Mission: "m",
		Roster: json.RawMessage(`[]`),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate team err = %v, want ErrConflict", err)
	}

	team.Mission = "Resolve issues within 24 hours."
	team.Revision = 1
	team.ContentHash = "hash-1"
	updated, err := st.UpdateTeamBot(ctx, team, 0)
	if err != nil || updated.Revision != 1 || updated.ContentHash != "hash-1" {
		t.Fatalf("update team: %+v err=%v", updated, err)
	}
	// A stale revision is a conflict, not a silent overwrite.
	stale := team
	stale.Mission = "Stale mission."
	if _, err := st.UpdateTeamBot(ctx, stale, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update err = %v, want ErrConflict", err)
	}

	if err := st.ArchiveTeamBot(ctx, tenant, team.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.ArchiveTeamBot(ctx, tenant, team.ID); err != nil {
		t.Fatalf("archive is not idempotent: %v", err)
	}
	if active, err := st.ListTeamBots(ctx, tenant, false, 0); err != nil || len(active) != 0 {
		t.Fatalf("archived team leaked: %+v err=%v", active, err)
	}
	if all, err := st.ListTeamBots(ctx, tenant, true, 0); err != nil || len(all) != 1 || !all[0].Archived {
		t.Fatalf("archived list = %+v err=%v", all, err)
	}
	// Archiving is one-way through UpdateTeamBot.
	unarchive := team
	unarchive.Archived = false
	unarchive.Revision = 2
	if _, err := st.UpdateTeamBot(ctx, unarchive, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("unarchive err = %v, want ErrConflict", err)
	}

	if _, err := st.CreateTeamBot(ctx, TeamBotRecord{
		TenantID: tenant, ID: "bad-roster", Name: "Bad", Mission: "m",
		Roster: json.RawMessage(`not json`),
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid roster err = %v, want ErrInvalid", err)
	}
	if _, err := st.CreateTeamBot(ctx, TeamBotRecord{
		TenantID: tenant, ID: "no-mission", Name: "No Mission",
		Roster: json.RawMessage(`[]`),
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing mission err = %v, want ErrInvalid", err)
	}

	other := uuid.New()
	if _, err := st.GetTeamBot(ctx, other, team.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign team err = %v, want ErrNotFound", err)
	}
}

func TestPermissionGroupsAndGrantsStore(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	account, _, _, _ := identityFixture(t, st)
	tenant := account.TenantID

	group, err := st.CreatePermissionGroup(ctx, PermissionGroupRecord{
		TenantID: tenant, ID: uuid.New(), Name: "Operators",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePermissionGroup(ctx, PermissionGroupRecord{
		TenantID: tenant, ID: uuid.New(), Name: "operators",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate group name err = %v, want ErrConflict", err)
	}
	second, err := st.CreatePermissionGroup(ctx, PermissionGroupRecord{
		TenantID: tenant, ID: uuid.New(), Name: "Viewers",
	})
	if err != nil {
		t.Fatal(err)
	}

	created, err := st.AddPermissionGroupMember(ctx, tenant, group.ID, account.ID, account.ID)
	if err != nil || !created {
		t.Fatalf("add member created=%v err=%v", created, err)
	}
	created, err = st.AddPermissionGroupMember(ctx, tenant, group.ID, account.ID, account.ID)
	if err != nil || created {
		t.Fatalf("duplicate member created=%v err=%v", created, err)
	}
	if _, err := st.AddPermissionGroupMember(ctx, tenant, uuid.New(), account.ID, account.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing group err = %v, want ErrConflict", err)
	}
	members, err := st.ListPermissionGroupMembers(ctx, tenant, group.ID, 0)
	if err != nil || len(members) != 1 || members[0].AccountID != account.ID {
		t.Fatalf("members = %+v err=%v", members, err)
	}

	added, err := st.AddPermissionGroupGrant(ctx, tenant, group.ID, "session.read", PermissionScopeTenant)
	if err != nil || !added {
		t.Fatalf("add grant added=%v err=%v", added, err)
	}
	added, err = st.AddPermissionGroupGrant(ctx, tenant, group.ID, "session.read", PermissionScopeTenant)
	if err != nil || added {
		t.Fatalf("duplicate grant added=%v err=%v", added, err)
	}
	if _, err := st.AddPermissionGroupGrant(ctx, tenant, group.ID, "session.read", "world"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad scope err = %v, want ErrInvalid", err)
	}
	if _, err := st.AddPermissionGroupGrant(ctx, tenant, group.ID, " ", PermissionScopeOwn); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty key err = %v, want ErrInvalid", err)
	}
	grants, err := st.ListPermissionGroupGrants(ctx, tenant, group.ID, 0)
	if err != nil || len(grants) != 1 || grants[0].Scope != PermissionScopeTenant {
		t.Fatalf("group grants = %+v err=%v", grants, err)
	}

	direct, err := st.CreatePermissionGrant(ctx, PermissionGrantRecord{
		TenantID: tenant, ID: uuid.New(), SubjectType: PermissionSubjectAccount,
		SubjectID: account.ID, PermissionKey: "session.read", Scope: PermissionScopeOwn,
		GrantedBy: account.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePermissionGrant(ctx, PermissionGrantRecord{
		TenantID: tenant, ID: uuid.New(), SubjectType: PermissionSubjectAccount,
		SubjectID: account.ID, PermissionKey: "session.read", Scope: PermissionScopeOwn,
		GrantedBy: account.ID,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate direct grant err = %v, want ErrConflict", err)
	}
	if _, err := st.CreatePermissionGrant(ctx, PermissionGrantRecord{
		TenantID: tenant, ID: uuid.New(), SubjectType: PermissionSubjectAccount,
		SubjectID: account.ID, PermissionKey: "session.read", Scope: PermissionScopeAll,
		GrantedBy: account.ID,
	}); err != nil {
		t.Fatalf("a wider scope for the same key must be a second legal row: %v", err)
	}
	list, err := st.ListPermissionGrants(ctx, tenant, PermissionSubjectAccount, account.ID, 0)
	if err != nil || len(list) != 2 {
		t.Fatalf("direct grants = %+v err=%v", list, err)
	}
	if err := st.DeletePermissionGrant(ctx, tenant, direct.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePermissionGrant(ctx, tenant, direct.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete err = %v, want ErrNotFound", err)
	}
	if _, err := st.CreatePermissionGrant(ctx, PermissionGrantRecord{
		TenantID: tenant, ID: uuid.New(), SubjectType: "team_bot", SubjectID: account.ID,
		PermissionKey: "session.read", Scope: PermissionScopeOwn, GrantedBy: account.ID,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("team_bot direct grant err = %v, want ErrInvalid", err)
	}
	if _, err := st.CreatePermissionGrant(ctx, PermissionGrantRecord{
		TenantID: tenant, ID: uuid.New(), SubjectType: "group", SubjectID: group.ID,
		PermissionKey: "session.read", Scope: PermissionScopeOwn, GrantedBy: account.ID,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("group direct grant err = %v, want ErrInvalid (groups use permission_group_grants)", err)
	}

	// Deleting a group cascades its memberships and grants.
	if _, err := st.AddPermissionGroupMember(ctx, tenant, second.ID, account.ID, account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddPermissionGroupGrant(ctx, tenant, second.ID, "session.write", PermissionScopeOwn); err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePermissionGroup(ctx, tenant, second.ID); err != nil {
		t.Fatal(err)
	}
	if members, err := st.ListPermissionGroupMembers(ctx, tenant, second.ID, 0); err != nil || len(members) != 0 {
		t.Fatalf("cascaded members = %+v err=%v", members, err)
	}
	if grants, err := st.ListPermissionGroupGrants(ctx, tenant, second.ID, 0); err != nil || len(grants) != 0 {
		t.Fatalf("cascaded grants = %+v err=%v", grants, err)
	}

	// A foreign tenant cannot read the group or its grants.
	other := uuid.New()
	if _, err := st.GetPermissionGroup(ctx, other, group.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign group err = %v, want ErrNotFound", err)
	}
	if list, err := st.ListPermissionGroups(ctx, other, 0); err != nil || len(list) != 0 {
		t.Fatalf("foreign groups = %+v err=%v", list, err)
	}
}

func TestResourceRelationsStore(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "relations"); err != nil {
		t.Fatal(err)
	}
	creator := uuid.New()
	relation := ResourceRelation{
		TenantID: tenant, ResourceType: "session", ResourceID: uuid.NewString(),
		Relation: RelationOwner, SubjectType: RelationSubjectAccount,
		SubjectID: creator.String(), CreatedBy: creator,
	}
	created, err := st.PutResourceRelation(ctx, relation)
	if err != nil || !created {
		t.Fatalf("put relation created=%v err=%v", created, err)
	}
	created, err = st.PutResourceRelation(ctx, relation)
	if err != nil || created {
		t.Fatalf("duplicate relation created=%v err=%v", created, err)
	}
	ok, err := st.HasResourceRelation(ctx, tenant, relation.ResourceType, relation.ResourceID,
		relation.Relation, relation.SubjectType, relation.SubjectID)
	if err != nil || !ok {
		t.Fatalf("has relation ok=%v err=%v", ok, err)
	}
	ok, err = st.HasResourceRelation(ctx, tenant, relation.ResourceType, relation.ResourceID,
		RelationViewer, relation.SubjectType, relation.SubjectID)
	if err != nil || ok {
		t.Fatalf("wrong relation ok=%v err=%v", ok, err)
	}
	list, err := st.ListResourceRelations(ctx, tenant, relation.ResourceType, relation.ResourceID, 0)
	if err != nil || len(list) != 1 || list[0].CreatedBy != creator {
		t.Fatalf("relations = %+v err=%v", list, err)
	}
	subjectList, err := st.ListResourceRelationsForSubject(ctx, tenant,
		relation.SubjectType, relation.SubjectID, "session", 0)
	if err != nil || len(subjectList) != 1 {
		t.Fatalf("subject relations = %+v err=%v", subjectList, err)
	}

	if _, err := st.PutResourceRelation(ctx, ResourceRelation{
		TenantID: tenant, ResourceType: "session", ResourceID: "r", Relation: "reader",
		SubjectType: RelationSubjectAccount, SubjectID: "a", CreatedBy: creator,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad relation err = %v, want ErrInvalid", err)
	}
	if _, err := st.PutResourceRelation(ctx, ResourceRelation{
		TenantID: tenant, ResourceType: "session", ResourceID: "r", Relation: RelationViewer,
		SubjectType: "person", SubjectID: "a", CreatedBy: creator,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad subject err = %v, want ErrInvalid", err)
	}

	if err := st.DeleteResourceRelation(ctx, tenant, relation.ResourceType, relation.ResourceID,
		relation.Relation, relation.SubjectType, relation.SubjectID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteResourceRelation(ctx, tenant, relation.ResourceType, relation.ResourceID,
		relation.Relation, relation.SubjectType, relation.SubjectID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete err = %v, want ErrNotFound", err)
	}
	if ok, err := st.HasResourceRelation(ctx, uuid.New(), relation.ResourceType, relation.ResourceID,
		relation.Relation, relation.SubjectType, relation.SubjectID); err != nil || ok {
		t.Fatalf("foreign has ok=%v err=%v", ok, err)
	}
}

func TestBotAPIKeysStore(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	account, _, _, bot := identityFixture(t, st)
	tenant := account.TenantID

	keyID := uuid.New()
	// The hash index is global, so the plaintext must be unique per test run on
	// a database that is reused across runs.
	hash := BotAPIKeyHash("keel_bot_plaintext_" + uuid.NewString())
	key, err := st.CreateBotAPIKey(ctx, tenant, keyID, hash, "keel_bot_abcd1234", account.ID, bot.ID, "primary")
	if err != nil {
		t.Fatal(err)
	}
	if key.KeyID != keyID || key.AccountID != account.ID || key.BotID != bot.ID ||
		key.Prefix != "keel_bot_abcd1234" || key.Label != "primary" {
		t.Fatalf("created key = %+v", key)
	}
	if _, err := st.CreateBotAPIKey(ctx, tenant, uuid.New(), hash, "keel_bot_other", account.ID, bot.ID, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate hash err = %v, want ErrConflict", err)
	}
	if _, err := st.CreateBotAPIKey(ctx, tenant, uuid.New(), hash[:16], "prefix", account.ID, bot.ID, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("short hash err = %v, want ErrInvalid", err)
	}
	if _, err := st.CreateBotAPIKey(ctx, tenant, uuid.New(), BotAPIKeyHash("other"), "prefix", account.ID, "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing bot err = %v, want ErrInvalid", err)
	}
	mismatchAccount, err := st.CreateAccount(ctx, AccountRecord{
		TenantID: tenant, ID: uuid.New(), Username: "other-" + tenant.String()[:8],
		DisplayName: "Other", Kind: "human", PasswordHash: "argon2id-demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateBotAPIKey(ctx, tenant, uuid.New(),
		BotAPIKeyHash("mismatch-"+uuid.NewString()), "prefix", mismatchAccount.ID, bot.ID, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("account/bot mismatch err = %v, want ErrInvalid", err)
	}

	lookedUp, err := st.LookupBotAPIKey(ctx, hash)
	if err != nil || lookedUp.KeyID != keyID || lookedUp.BotID != bot.ID {
		t.Fatalf("lookup = %+v err=%v", lookedUp, err)
	}
	first := time.Now().UTC().Truncate(time.Microsecond)
	if err := st.TouchBotAPIKey(ctx, tenant, keyID, first); err != nil {
		t.Fatal(err)
	}
	// A delayed older touch must not move last_used_at backwards.
	if err := st.TouchBotAPIKey(ctx, tenant, keyID, first.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	touched, err := st.GetBotAPIKey(ctx, tenant, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if touched.LastUsedAt == nil || !touched.LastUsedAt.Equal(first) {
		t.Fatalf("last_used_at = %v, want %v", touched.LastUsedAt, first)
	}

	if keys, err := st.ListBotAPIKeys(ctx, tenant, bot.ID, 0); err != nil || len(keys) != 1 {
		t.Fatalf("keys by bot = %+v err=%v", keys, err)
	}
	if err := st.RevokeBotAPIKey(ctx, tenant, keyID); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeBotAPIKey(ctx, tenant, keyID); err != nil {
		t.Fatalf("revoke is not idempotent: %v", err)
	}
	if _, err := st.LookupBotAPIKey(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked key lookup err = %v, want ErrNotFound", err)
	}
	if keys, err := st.ListBotAPIKeys(ctx, tenant, "", 0); err != nil || len(keys) != 1 || keys[0].RevokedAt == nil {
		t.Fatalf("revoked key list = %+v err=%v", keys, err)
	}

	other := uuid.New()
	if err := st.CreateTenant(ctx, other, "bot-key-other"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetBotAPIKey(ctx, other, keyID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign key err = %v, want ErrNotFound", err)
	}
	if keys, err := st.ListBotAPIKeys(ctx, other, "", 0); err != nil || len(keys) != 0 {
		t.Fatalf("foreign keys = %+v err=%v", keys, err)
	}
}

// TestBotOwnerBindingRejectsStaleProfileAccount pins the profile-share lock: a
// caller whose owner came from an earlier profile read cannot write a bot owner
// that the profile no longer names. This closes the create/rebind race without
// putting profile reads in the handler's transaction.
func TestBotOwnerBindingRejectsStaleProfileAccount(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	owner, _, profile, _ := identityFixture(t, st)
	other, err := st.CreateAccount(ctx, AccountRecord{
		TenantID: owner.TenantID, ID: uuid.New(),
		Username: "other-" + owner.TenantID.String()[:8], DisplayName: "Other", Kind: "human",
	})
	if err != nil {
		t.Fatal(err)
	}
	record := BotRecord{
		TenantID: owner.TenantID, ID: "stale-" + owner.TenantID.String()[:8],
		Name: "Stale", ProfileID: profile.ID, ModelKind: "text",
		DefaultModel: json.RawMessage(`{"providerId":"fake","modelId":"fake"}`),
		Harness:      json.RawMessage(`{"tools":[],"context":{"permissions":[]}}`),
	}
	if _, err := st.CreateBotWithOwner(ctx, record, other.ID, owner.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale owner create err = %v, want ErrConflict", err)
	}
	bot, err := st.CreateBotWithOwner(ctx, record, owner.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateBotWithOwner(ctx, bot, false, other.ID, owner.ID, owner.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale owner update err = %v, want ErrConflict", err)
	}
}
