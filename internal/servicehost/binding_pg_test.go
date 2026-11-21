package servicehost

// Real-PostgreSQL binding integration: the resolver reads the same rows the
// product writes (accounts/roles/profiles/bots/team_bots), so a Bot's Role +
// Harness prompt, tool whitelist and default model, and a TeamBot's
// mission/roster, reach the run surface without a second mapping.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/botteam"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

func bindingPGStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("KEEL_TEST_DSN"))
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

func seedBindingPG(t *testing.T, st *store.Store) (tenant, accountID uuid.UUID, bot store.BotRecord, team store.TeamBotRecord) {
	t.Helper()
	ctx := context.Background()
	tenant = uuid.New()
	if err := st.CreateTenant(ctx, tenant, "binding-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	account, err := st.CreateAccount(ctx, store.AccountRecord{
		TenantID: tenant, ID: uuid.New(), Username: "owner-" + tenant.String()[:8],
		DisplayName: "Owner", Kind: "human",
	})
	if err != nil {
		t.Fatal(err)
	}
	role, err := st.CreateRole(ctx, store.RoleRecord{
		TenantID: tenant, ID: uuid.New(), Name: "Captain", Instruction: "You lead the support desk.",
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := st.CreateProfile(ctx, store.ProfileRecord{
		TenantID: tenant, ID: uuid.New(), AccountID: account.ID, RoleID: role.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	bot, err = st.CreateBot(ctx, store.BotRecord{
		TenantID: tenant, ID: "captain-" + tenant.String()[:8], Name: "Captain",
		ProfileID: profile.ID, ModelKind: "text",
		DefaultModel: json.RawMessage(`{"providerId":"magpie","modelId":"qwen"}`),
		Harness:      json.RawMessage(`{"tools":["bash","read"],"context":{"instruction":"Resolve tickets.","permissions":["session.read"]}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	member, err := st.CreateBot(ctx, store.BotRecord{
		TenantID: tenant, ID: "member-" + tenant.String()[:8], Name: "Member",
		ProfileID: profile.ID, ModelKind: "text",
		DefaultModel: json.RawMessage(`{"providerId":"magpie","modelId":"qwen"}`),
		Harness:      json.RawMessage(`{"tools":["read"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	domain := botteam.NewTeamBot("team-"+tenant.String()[:8], "Support", "Resolve tickets within 24h.")
	domain.Roster = []botteam.RosterEntry{
		{BotID: bot.ID, Role: botteam.RosterRoleCaptain},
		{BotID: member.ID, Role: botteam.RosterRoleMember},
	}
	domain.Seal()
	roster, _ := json.Marshal(domain.Roster)
	team, err = st.CreateTeamBot(ctx, store.TeamBotRecord{
		TenantID: tenant, ID: domain.ID, Name: domain.Name,
		Mission: domain.Mission, Roster: roster,
		Revision: int64(domain.Revision()), ContentHash: domain.Hash(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = st.Pool().Exec(ctx, `DELETE FROM events WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(ctx, `DELETE FROM sessions WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(ctx, `DELETE FROM team_bots WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(ctx, `DELETE FROM bots WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(ctx, `DELETE FROM profiles WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(ctx, `DELETE FROM roles WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(ctx, `DELETE FROM accounts WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(ctx, `DELETE FROM tenants WHERE tenant_id = $1`, tenant)
	})
	return tenant, account.ID, bot, team
}

func TestBindingPGResolvesBotAndTeamBot(t *testing.T) {
	st := bindingPGStore(t)
	tenant, accountID, bot, team := seedBindingPG(t, st)
	ctx := context.Background()
	sessionID := uuid.New()
	if err := st.CreateOwnedSession(ctx, store.SessionOwnership{
		TenantID: tenant, SessionID: sessionID, BotID: bot.ID,
		AccountID: accountID, CreatedByAccountID: accountID,
	}); err != nil {
		t.Fatal(err)
	}
	resolver := &bindingResolver{reader: st, tenantID: tenant}

	// No selection: the session's Bot answers with its Role+Harness prompt, its
	// whitelist and its default model.
	surface, err := resolver.ResolveBinding(ctx, kernel.Scope{TenantID: tenant, SessionID: sessionID}, nil)
	if err != nil {
		t.Fatalf("ResolveBinding: %v", err)
	}
	if surface.Prompt != "You lead the support desk.\n\nResolve tickets." {
		t.Fatalf("prompt = %q", surface.Prompt)
	}
	if surface.ToolIDs == nil || !surface.ToolIDs["bash"] || !surface.ToolIDs["read"] || len(surface.ToolIDs) != 2 {
		t.Fatalf("tools = %+v", surface.ToolIDs)
	}
	if surface.DefaultModel == nil || surface.DefaultModel.ProviderID != "magpie" {
		t.Fatalf("default model = %+v", surface.DefaultModel)
	}
	if surface.TeamMode || surface.Mission != "" {
		t.Fatalf("standard binding leaked team context: %+v", surface)
	}

	// A TeamBot session: the captain owns it, the mission and the member roster
	// are injected, and the TeamBot identity is frozen.
	teamSession := uuid.New()
	if err := st.CreateOwnedSession(ctx, store.SessionOwnership{
		TenantID: tenant, SessionID: teamSession, BotID: bot.ID,
		AccountID: accountID, CreatedByAccountID: accountID, TeamBotID: team.ID,
	}); err != nil {
		t.Fatal(err)
	}
	surface, err = resolver.ResolveBinding(ctx, kernel.Scope{TenantID: tenant, SessionID: teamSession},
		json.RawMessage(`{"teamBotId":"`+team.ID+`"}`))
	if err != nil {
		t.Fatalf("team ResolveBinding: %v", err)
	}
	if !surface.TeamMode || surface.TeamID != team.ID || surface.AgentID != bot.ID {
		t.Fatalf("team surface = %+v", surface)
	}
	if surface.Mission != "Resolve tickets within 24h." {
		t.Fatalf("mission = %q", surface.Mission)
	}
	if len(surface.Members) != 1 || !strings.HasPrefix(surface.Members[0].BotID, "member-") ||
		surface.Members[0].Label != "Member" {
		t.Fatalf("members = %+v", surface.Members)
	}
	if surface.TeamRevision != 1 || surface.TeamHash == "" {
		t.Fatalf("team seal = %d/%q", surface.TeamRevision, surface.TeamHash)
	}
}

func TestBindingPGRefusesArchivedBotAtRunTime(t *testing.T) {
	st := bindingPGStore(t)
	tenant, accountID, bot, _ := seedBindingPG(t, st)
	ctx := context.Background()
	sessionID := uuid.New()
	if err := st.CreateOwnedSession(ctx, store.SessionOwnership{
		TenantID: tenant, SessionID: sessionID, BotID: bot.ID,
		AccountID: accountID, CreatedByAccountID: accountID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ArchiveBot(ctx, tenant, bot.ID); err != nil {
		t.Fatal(err)
	}
	resolver := &bindingResolver{reader: st, tenantID: tenant}
	if _, err := resolver.ResolveBinding(ctx, kernel.Scope{TenantID: tenant, SessionID: sessionID}, nil); err == nil {
		t.Fatal("an archived Bot still answered an attempt")
	}
	// A new session for the archived Bot is refused by the ownership write.
	if err := st.CreateOwnedSession(ctx, store.SessionOwnership{
		TenantID: tenant, SessionID: uuid.New(), BotID: bot.ID,
		AccountID: accountID, CreatedByAccountID: accountID,
	}); err == nil {
		t.Fatal("an archived Bot accepted a new session")
	}
}
