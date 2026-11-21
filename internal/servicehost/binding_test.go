package servicehost

// The execution binding over the Bot/TeamBot identity graph (A4.2b3a): a
// selection names a Bot or TeamBot, the resolver reads Bot + Profile + Role +
// TeamBot, and the surface is the Role/Harness prompt, the Harness tool
// whitelist and the Bot's default model. A session must be owned by a Bot;
// an unowned pre-A4 session is refused (fail closed).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/botteam"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

type fakeBindingReader struct {
	bots      map[string]store.BotRecord
	teams     map[string]store.TeamBotRecord
	profiles  map[uuid.UUID]store.ProfileRecord
	roles     map[uuid.UUID]store.RoleRecord
	ownership map[uuid.UUID]store.SessionOwnership
}

func (f *fakeBindingReader) GetBot(_ context.Context, tenantID uuid.UUID, botID string) (store.BotRecord, error) {
	record, ok := f.bots[botID]
	if !ok || record.TenantID != tenantID {
		return store.BotRecord{}, store.ErrNotFound
	}
	return record, nil
}

func (f *fakeBindingReader) GetTeamBot(_ context.Context, tenantID uuid.UUID, teamBotID string) (store.TeamBotRecord, error) {
	record, ok := f.teams[teamBotID]
	if !ok || record.TenantID != tenantID {
		return store.TeamBotRecord{}, store.ErrNotFound
	}
	return record, nil
}

func (f *fakeBindingReader) GetProfile(_ context.Context, tenantID, profileID uuid.UUID) (store.ProfileRecord, error) {
	record, ok := f.profiles[profileID]
	if !ok || record.TenantID != tenantID {
		return store.ProfileRecord{}, store.ErrNotFound
	}
	return record, nil
}

func (f *fakeBindingReader) GetRole(_ context.Context, tenantID, roleID uuid.UUID) (store.RoleRecord, error) {
	record, ok := f.roles[roleID]
	if !ok || record.TenantID != tenantID {
		return store.RoleRecord{}, store.ErrNotFound
	}
	return record, nil
}

func (f *fakeBindingReader) GetSessionOwnership(_ context.Context, tenantID, sessionID uuid.UUID) (store.SessionOwnership, error) {
	record, ok := f.ownership[sessionID]
	if !ok || record.TenantID != tenantID {
		return store.SessionOwnership{}, store.ErrNotFound
	}
	return record, nil
}

type bindingFixture struct {
	tenant  uuid.UUID
	account uuid.UUID
	role    store.RoleRecord
	profile store.ProfileRecord
	reader  *fakeBindingReader
}

func newBindingFixture(t *testing.T) *bindingFixture {
	t.Helper()
	tenant, account, roleID, profileID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	role := store.RoleRecord{TenantID: tenant, ID: roleID, Name: "Reviewer", Instruction: "You review drafts."}
	profile := store.ProfileRecord{TenantID: tenant, ID: profileID, AccountID: account, RoleID: roleID}
	reader := &fakeBindingReader{
		bots:      map[string]store.BotRecord{},
		teams:     map[string]store.TeamBotRecord{},
		profiles:  map[uuid.UUID]store.ProfileRecord{profileID: profile},
		roles:     map[uuid.UUID]store.RoleRecord{roleID: role},
		ownership: map[uuid.UUID]store.SessionOwnership{},
	}
	return &bindingFixture{tenant: tenant, account: account, role: role, profile: profile, reader: reader}
}

func (f *bindingFixture) addBot(t *testing.T, id, name string, archived bool, tools []string, instruction string) store.BotRecord {
	t.Helper()
	model, _ := json.Marshal(botteam.ModelRef{ProviderID: "magpie", ModelID: "bailian-token-plan/qwen3.8-flash"})
	harness, _ := json.Marshal(botteam.Harness{
		Tools: tools,
		Context: botteam.HarnessContext{
			Instruction: instruction,
			Permissions: []string{"session.read"},
		},
	})
	record := store.BotRecord{
		TenantID: f.tenant, ID: id, Name: name, ProfileID: f.profile.ID,
		ModelKind: "text", DefaultModel: model, Harness: harness, Archived: archived,
	}
	f.reader.bots[id] = record
	return record
}

func (f *bindingFixture) addTeam(t *testing.T, id, mission string, archived bool, roster ...botteam.RosterEntry) store.TeamBotRecord {
	t.Helper()
	team := botteam.NewTeamBot(id, "Team "+id, mission)
	team.Roster = roster
	team.Archived = archived
	team.Seal()
	raw, err := json.Marshal(team)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Revision uint64 `json:"revision"`
		Hash     string `json:"hash"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	rosterRaw, _ := json.Marshal(team.Roster)
	record := store.TeamBotRecord{
		TenantID: f.tenant, ID: id, Name: team.Name, Mission: mission,
		Roster: rosterRaw, Revision: int64(wire.Revision), ContentHash: wire.Hash, Archived: archived,
	}
	f.reader.teams[id] = record
	return record
}

func (f *bindingFixture) ownSession(sessionID uuid.UUID, botID, teamBotID string) {
	f.reader.ownership[sessionID] = store.SessionOwnership{
		TenantID: f.tenant, SessionID: sessionID, BotID: botID,
		AccountID: f.account, CreatedByAccountID: f.account, TeamBotID: teamBotID,
	}
}

func TestLoadBotBindingPromptToolsAndModel(t *testing.T) {
	fixture := newBindingFixture(t)
	fixture.addBot(t, "support-1", "Support One", false, []string{"bash", "read"}, "Answer support tickets.")
	binding, err := LoadBinding(context.Background(), fixture.reader, fixture.tenant,
		json.RawMessage(`{"botId":"support-1"}`))
	if err != nil {
		t.Fatalf("LoadBinding: %v", err)
	}
	if !binding.Bound() || binding.Bot.ID != "support-1" {
		t.Fatalf("binding = %+v", binding)
	}
	wantPrompt := "You review drafts.\n\nAnswer support tickets."
	if got := binding.PromptText(); got != wantPrompt {
		t.Fatalf("prompt = %q, want %q", got, wantPrompt)
	}
	tools := binding.ToolIDs()
	if tools == nil || !tools["bash"] || !tools["read"] || len(tools) != 2 {
		t.Fatalf("tools = %+v, want bash+read", tools)
	}
	model := binding.effectiveDefaultModel()
	if model.ProviderID != "magpie" || model.ModelID != "bailian-token-plan/qwen3.8-flash" {
		t.Fatalf("model = %+v", model)
	}
}

func TestLoadBotBindingPromptDeduplicatesAndEmptyToolsGrantNothing(t *testing.T) {
	fixture := newBindingFixture(t)
	fixture.addBot(t, "same", "Same", false, nil, "You review drafts.")
	binding, err := LoadBinding(context.Background(), fixture.reader, fixture.tenant,
		json.RawMessage(`{"botId":"same"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := binding.PromptText(); got != "You review drafts." {
		t.Fatalf("duplicate prompt = %q", got)
	}
	tools := binding.ToolIDs()
	if tools == nil {
		t.Fatal("an empty Harness must yield an empty non-nil map (grant nothing, not everything)")
	}
	if len(tools) != 0 {
		t.Fatalf("tools = %+v, want empty", tools)
	}
}

func TestLoadBindingRefusesMissingArchivedAndBrokenBots(t *testing.T) {
	fixture := newBindingFixture(t)
	fixture.addBot(t, "archived", "Archived", true, []string{"bash"}, "")
	for _, tc := range []struct {
		name      string
		selection string
		want      string
	}{
		{"missing", `{"botId":"ghost"}`, "does not exist"},
		{"archived", `{"botId":"archived"}`, "is archived"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBinding(context.Background(), fixture.reader, fixture.tenant, json.RawMessage(tc.selection))
			if !errors.Is(err, ErrBindingMissing) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadTeamBotBindingCaptainAndMembers(t *testing.T) {
	fixture := newBindingFixture(t)
	fixture.addBot(t, "captain", "Captain", false, []string{"bash"}, "Lead the team.")
	fixture.addBot(t, "member", "Member", false, []string{"read"}, "Do the work.")
	fixture.addTeam(t, "support-team", "Resolve tickets within 24h.", false,
		botteam.RosterEntry{BotID: "captain", Role: botteam.RosterRoleCaptain},
		botteam.RosterEntry{BotID: "member", Role: botteam.RosterRoleMember},
	)
	binding, err := LoadBinding(context.Background(), fixture.reader, fixture.tenant,
		json.RawMessage(`{"teamBotId":"support-team"}`))
	if err != nil {
		t.Fatalf("LoadBinding: %v", err)
	}
	if binding.Bot.ID != "captain" || binding.TeamBot == nil || binding.TeamBot.ID != "support-team" {
		t.Fatalf("binding = %+v", binding)
	}
	if binding.Mission() != "Resolve tickets within 24h." {
		t.Fatalf("mission = %q", binding.Mission())
	}
	if binding.TeamBotRevision == 0 || binding.TeamBotHash == "" {
		t.Fatalf("team seal not frozen: %d/%q", binding.TeamBotRevision, binding.TeamBotHash)
	}
	if len(binding.Members) != 1 || binding.Members[0].BotID != "member" {
		t.Fatalf("members = %+v, want member only", binding.Members)
	}
	// The captain seat is the effective Bot, not a roster member.
	for _, member := range binding.Members {
		if member.BotID == "captain" {
			t.Fatal("captain appears in the member roster")
		}
	}
}

func TestLoadTeamBotRefusesBrokenRosters(t *testing.T) {
	fixture := newBindingFixture(t)
	fixture.addBot(t, "captain", "Captain", false, []string{"bash"}, "")
	fixture.addBot(t, "archived", "Archived", true, []string{"bash"}, "")
	fixture.addTeam(t, "missing-member", "mission", false,
		botteam.RosterEntry{BotID: "captain", Role: botteam.RosterRoleCaptain},
		botteam.RosterEntry{BotID: "ghost", Role: botteam.RosterRoleMember},
	)
	fixture.addTeam(t, "archived-member", "mission", false,
		botteam.RosterEntry{BotID: "captain", Role: botteam.RosterRoleCaptain},
		botteam.RosterEntry{BotID: "archived", Role: botteam.RosterRoleMember},
	)
	archivedTeam := fixture.addTeam(t, "archived-team", "mission", true,
		botteam.RosterEntry{BotID: "captain", Role: botteam.RosterRoleCaptain},
	)
	if archivedTeam.Archived != true {
		t.Fatal("fixture team not archived")
	}
	for _, tc := range []struct {
		selection string
		want      string
	}{
		{`{"teamBotId":"missing-member"}`, "does not exist"},
		{`{"teamBotId":"archived-member"}`, "is archived"},
		{`{"teamBotId":"archived-team"}`, "is archived"},
	} {
		_, err := LoadBinding(context.Background(), fixture.reader, fixture.tenant, json.RawMessage(tc.selection))
		if !errors.Is(err, ErrBindingMissing) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.selection, err, tc.want)
		}
	}
}

func TestLoadSessionBindingUsesOwnershipAndFallsBack(t *testing.T) {
	fixture := newBindingFixture(t)
	fixture.addBot(t, "owner", "Owner", false, []string{"bash"}, "own")
	fixture.addBot(t, "member", "Member", false, []string{"read"}, "member")
	sessionID := uuid.New()
	fixture.ownSession(sessionID, "owner", "")
	ctx := context.Background()

	// No selection: the session's Bot answers.
	binding, err := LoadSessionBinding(ctx, fixture.reader, fixture.tenant, sessionID, nil)
	if err != nil || binding.Bot.ID != "owner" {
		t.Fatalf("fallback binding = %+v err=%v", binding, err)
	}
	// A member selection changes the surface, never the ownership.
	binding, err = LoadSessionBinding(ctx, fixture.reader, fixture.tenant, sessionID, json.RawMessage(`{"botId":"member"}`))
	if err != nil || binding.Bot.ID != "member" {
		t.Fatalf("member binding = %+v err=%v", binding, err)
	}
	ownership, err := fixture.reader.GetSessionOwnership(ctx, fixture.tenant, sessionID)
	if err != nil || ownership.BotID != "owner" {
		t.Fatalf("ownership moved: %+v err=%v", ownership, err)
	}
}

func TestLoadSessionBindingRefusesUnownedAndMissingSessions(t *testing.T) {
	fixture := newBindingFixture(t)
	fixture.addBot(t, "owner", "Owner", false, nil, "")
	unowned := uuid.New()
	fixture.reader.ownership[unowned] = store.SessionOwnership{TenantID: fixture.tenant, SessionID: unowned, AccountID: fixture.account}
	if _, err := LoadSessionBinding(context.Background(), fixture.reader, fixture.tenant, unowned,
		json.RawMessage(`{"botId":"owner"}`)); !errors.Is(err, ErrBindingMissing) || !strings.Contains(err.Error(), "not owned by a bot") {
		t.Fatalf("unowned session err = %v", err)
	}
	if _, err := LoadSessionBinding(context.Background(), fixture.reader, fixture.tenant, uuid.New(), nil); !errors.Is(err, ErrBindingMissing) {
		t.Fatalf("missing session err = %v", err)
	}
}

func TestBindingResolverBuildsTheRunSurface(t *testing.T) {
	fixture := newBindingFixture(t)
	fixture.addBot(t, "captain", "Captain", false, []string{"bash"}, "Lead.")
	fixture.addBot(t, "member", "Member", false, []string{"read"}, "Work.")
	fixture.addTeam(t, "team", "Ship it.", false,
		botteam.RosterEntry{BotID: "captain", Role: botteam.RosterRoleCaptain},
		botteam.RosterEntry{BotID: "member", Role: botteam.RosterRoleMember},
	)
	sessionID := uuid.New()
	fixture.ownSession(sessionID, "captain", "team")
	resolver := &bindingResolver{reader: fixture.reader, tenantID: fixture.tenant}
	surface, err := resolver.ResolveBinding(context.Background(),
		kernel.Scope{TenantID: fixture.tenant, SessionID: sessionID}, nil)
	if err != nil {
		t.Fatalf("ResolveBinding: %v", err)
	}
	if surface.AgentID != "captain" || surface.TeamID != "team" || !surface.TeamMode {
		t.Fatalf("surface = %+v", surface)
	}
	if surface.Mission != "Ship it." {
		t.Fatalf("mission = %q", surface.Mission)
	}
	if surface.Prompt != "You review drafts.\n\nLead." {
		t.Fatalf("prompt = %q", surface.Prompt)
	}
	if surface.ToolIDs == nil || !surface.ToolIDs["bash"] {
		t.Fatalf("tools = %+v", surface.ToolIDs)
	}
	if len(surface.Members) != 1 || surface.Members[0].BotID != "member" || surface.Members[0].Label != "Member" {
		t.Fatalf("members = %+v", surface.Members)
	}
	if surface.DefaultModel == nil || surface.DefaultModel.ProviderID != "magpie" {
		t.Fatalf("default model = %+v", surface.DefaultModel)
	}
}

func TestBindingResolverRefusesAnUnownedSession(t *testing.T) {
	fixture := newBindingFixture(t)
	fixture.addBot(t, "owner", "Owner", false, nil, "")
	sessionID := uuid.New()
	resolver := &bindingResolver{reader: fixture.reader, tenantID: fixture.tenant}
	if _, err := resolver.ResolveBinding(context.Background(),
		kernel.Scope{TenantID: fixture.tenant, SessionID: sessionID}, json.RawMessage(`{"botId":"owner"}`)); !errors.Is(err, ErrBindingMissing) {
		t.Fatalf("unowned session resolved: %v", err)
	}
}

// TestBindingRereadsTheCurrentDocuments pins the "edit applies to the next run"
// rule: the resolver reads Bot/Profile/Role per attempt, so a Harness or Role
// edit is visible on the next load without a restart (an attempt in flight keeps
// what it resolved).
func TestBindingRereadsTheCurrentDocuments(t *testing.T) {
	fixture := newBindingFixture(t)
	fixture.addBot(t, "support-1", "Support One", false, []string{"bash"}, "old instruction")
	ctx := context.Background()
	first, err := LoadBinding(ctx, fixture.reader, fixture.tenant, json.RawMessage(`{"botId":"support-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.PromptText(), "old instruction") || !first.ToolIDs()["bash"] {
		t.Fatalf("first binding = %+v", first)
	}

	// Edit the Role instruction and the Bot Harness/default model.
	role := fixture.reader.roles[fixture.role.ID]
	role.Instruction = "new role instruction"
	fixture.reader.roles[role.ID] = role
	model, _ := json.Marshal(botteam.ModelRef{ProviderID: "other", ModelID: "m2"})
	harness, _ := json.Marshal(botteam.Harness{Tools: []string{"read"}})
	record := fixture.reader.bots["support-1"]
	record.DefaultModel = model
	record.Harness = harness
	fixture.reader.bots["support-1"] = record

	second, err := LoadBinding(ctx, fixture.reader, fixture.tenant, json.RawMessage(`{"botId":"support-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.PromptText(), "new role instruction") || strings.Contains(second.PromptText(), "old instruction") {
		t.Fatalf("second prompt = %q", second.PromptText())
	}
	if second.ToolIDs()["bash"] || !second.ToolIDs()["read"] || len(second.ToolIDs()) != 1 {
		t.Fatalf("second tools = %+v", second.ToolIDs())
	}
	if got := second.effectiveDefaultModel(); got.ModelID != "m2" {
		t.Fatalf("second model = %+v", got)
	}
}
