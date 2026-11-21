package store

// Session binding projection (change keel-bot-identity-access, A4.2b3a). These
// tests run against the real test database (KEEL_TEST_DSN): the schema no
// longer carries the pre-A4 bound_* columns, sessions.bot_id/team_bot_id are
// the ownership truth, and a selection can never move that ownership.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func cleanupSessionBindingTenant(t *testing.T, st *Store, tenants ...uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, tenant := range tenants {
			_, _ = st.Pool().Exec(ctx, `DELETE FROM events WHERE tenant_id = $1`, tenant)
			_, _ = st.Pool().Exec(ctx, `DELETE FROM sessions WHERE tenant_id = $1`, tenant)
			_, _ = st.Pool().Exec(ctx, `DELETE FROM team_bots WHERE tenant_id = $1`, tenant)
			_, _ = st.Pool().Exec(ctx, `DELETE FROM bots WHERE tenant_id = $1`, tenant)
			_, _ = st.Pool().Exec(ctx, `DELETE FROM profiles WHERE tenant_id = $1`, tenant)
			_, _ = st.Pool().Exec(ctx, `DELETE FROM roles WHERE tenant_id = $1`, tenant)
			_, _ = st.Pool().Exec(ctx, `DELETE FROM accounts WHERE tenant_id = $1`, tenant)
			_, _ = st.Pool().Exec(ctx, `DELETE FROM tenants WHERE tenant_id = $1`, tenant)
		}
	})
}

// TestSessionBindingSchemaDropsLegacyColumns pins the retirement: the old
// newest-selection columns and indexes are gone, and the ownership columns the
// projection reads exist.
func TestSessionBindingSchemaDropsLegacyColumns(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()

	for _, column := range []string{"bound_agent_id", "bound_team_id", "bound_seq", "bound_at"} {
		var exists bool
		if err := st.Pool().QueryRow(ctx,
			`SELECT EXISTS (
			    SELECT 1 FROM information_schema.columns
			     WHERE table_schema = current_schema() AND table_name = 'sessions' AND column_name = $1
			 )`, column).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("sessions.%s still exists; the legacy binding projection was not dropped", column)
		}
	}
	for _, index := range []string{"sessions_bound_agent", "sessions_bound_team"} {
		var exists bool
		if err := st.Pool().QueryRow(ctx,
			`SELECT to_regclass($1) IS NOT NULL`, index).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("index %s still exists", index)
		}
	}
	for _, column := range []string{"bot_id", "team_bot_id"} {
		var exists bool
		if err := st.Pool().QueryRow(ctx,
			`SELECT EXISTS (
			    SELECT 1 FROM information_schema.columns
			     WHERE table_schema = current_schema() AND table_name = 'sessions' AND column_name = $1
			 )`, column).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("sessions.%s is missing", column)
		}
	}
}

// TestSessionBindingIsOwnership pins the projection: a selection naming the
// session's own Bot/TeamBot applies; Get reads the ownership columns.
func TestSessionBindingIsOwnership(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	account, _, _, bot := identityFixture(t, st)
	cleanupSessionBindingTenant(t, st, account.TenantID)

	team, err := st.CreateTeamBot(ctx, TeamBotRecord{
		TenantID: account.TenantID, ID: "team-" + account.Username, Name: "Team",
		Mission: "ship", Roster: []byte(`[{"botId":"` + bot.ID + `","role":"captain"}]`),
		Revision: 1, ContentHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := uuid.New()
	if err := st.CreateOwnedSession(ctx, SessionOwnership{
		TenantID: account.TenantID, SessionID: sessionID, BotID: bot.ID,
		AccountID: account.ID, CreatedByAccountID: account.ID, TeamBotID: team.ID,
	}); err != nil {
		t.Fatal(err)
	}
	applied, err := st.SetSessionBinding(ctx, account.TenantID, sessionID, bot.ID, team.ID)
	if err != nil || !applied {
		t.Fatalf("matching binding applied=%v err=%v", applied, err)
	}
	binding, found, err := st.GetSessionBinding(ctx, account.TenantID, sessionID)
	if err != nil || !found {
		t.Fatalf("get binding: found=%v err=%v", found, err)
	}
	if binding.BotID != bot.ID || binding.TeamBotID != team.ID {
		t.Fatalf("binding = %+v, want %s/%s", binding, bot.ID, team.ID)
	}
	// A team selection carries no bot id; the projection is still the ownership.
	applied, err = st.SetSessionBinding(ctx, account.TenantID, sessionID, "", team.ID)
	if err != nil || !applied {
		t.Fatalf("team-only binding applied=%v err=%v", applied, err)
	}
}

// TestSessionBindingNeverMovesOwnership pins the delegation rule: a child
// session may name a member Bot for its prompt/tools, but the ownership columns
// keep the parent Bot.
func TestSessionBindingNeverMovesOwnership(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	account, _, profile, bot := identityFixture(t, st)
	cleanupSessionBindingTenant(t, st, account.TenantID)
	member, err := st.CreateBot(ctx, BotRecord{
		TenantID: account.TenantID, ID: bot.ID + "-member", Name: "Member",
		ProfileID: profile.ID, ModelKind: "text",
		DefaultModel: []byte(`{"providerId":"magpie","modelId":"qwen"}`),
		Harness:      []byte(`{"tools":["bash"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := uuid.New()
	if err := st.CreateOwnedSession(ctx, SessionOwnership{
		TenantID: account.TenantID, SessionID: sessionID, BotID: bot.ID,
		AccountID: account.ID, CreatedByAccountID: account.ID,
	}); err != nil {
		t.Fatal(err)
	}
	applied, err := st.SetSessionBinding(ctx, account.TenantID, sessionID, member.ID, "")
	if err != nil {
		t.Fatalf("surface-only binding: %v", err)
	}
	if applied {
		t.Fatal("a selection naming another bot must not be applied to ownership")
	}
	binding, _, err := st.GetSessionBinding(ctx, account.TenantID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.BotID != bot.ID {
		t.Fatalf("ownership moved to %q, want %q", binding.BotID, bot.ID)
	}
}

// TestSessionBindingRefusesUnownedSession pins the fail-closed rule: a pre-A4
// session with no Bot owner is not runnable and cannot be claimed by a
// selection.
func TestSessionBindingRefusesUnownedSession(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	account, _, _, bot := identityFixture(t, st)
	cleanupSessionBindingTenant(t, st, account.TenantID)
	sessionID := uuid.New()
	if err := st.CreateSession(ctx, account.TenantID, sessionID); err != nil {
		t.Fatal(err)
	}
	if applied, err := st.SetSessionBinding(ctx, account.TenantID, sessionID, bot.ID, ""); err != nil || applied {
		t.Fatalf("unowned session binding applied=%v err=%v, want a no-op", applied, err)
	}
	if _, err := st.SetSessionBinding(ctx, account.TenantID, uuid.New(), bot.ID, ""); !errors.Is(err, ErrNoSession) {
		t.Fatalf("missing session binding err = %v, want ErrNoSession", err)
	}
	if _, err := st.SetSessionBinding(ctx, uuid.New(), sessionID, bot.ID, ""); !errors.Is(err, ErrNoSession) {
		t.Fatalf("foreign session binding err = %v, want ErrNoSession", err)
	}
}

// TestSessionBindingTeamSnapshotIsExact pins the team projection: a team id that
// differs from the session's mission snapshot is surface-only, not applied.
func TestSessionBindingTeamSnapshotIsExact(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	account, _, _, bot := identityFixture(t, st)
	cleanupSessionBindingTenant(t, st, account.TenantID)
	first, err := st.CreateTeamBot(ctx, TeamBotRecord{
		TenantID: account.TenantID, ID: "team-first-" + account.Username, Name: "First",
		Mission: "ship", Roster: []byte(`[{"botId":"` + bot.ID + `","role":"captain"}]`),
		Revision: 1, ContentHash: "hash-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.CreateTeamBot(ctx, TeamBotRecord{
		TenantID: account.TenantID, ID: "team-second-" + account.Username, Name: "Second",
		Mission: "ship", Roster: []byte(`[{"botId":"` + bot.ID + `","role":"captain"}]`),
		Revision: 1, ContentHash: "hash-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := uuid.New()
	if err := st.CreateOwnedSession(ctx, SessionOwnership{
		TenantID: account.TenantID, SessionID: sessionID, BotID: bot.ID,
		AccountID: account.ID, CreatedByAccountID: account.ID, TeamBotID: first.ID,
	}); err != nil {
		t.Fatal(err)
	}
	applied, err := st.SetSessionBinding(ctx, account.TenantID, sessionID, "", second.ID)
	if err != nil || applied {
		t.Fatalf("foreign team projection applied=%v err=%v", applied, err)
	}
	binding, _, err := st.GetSessionBinding(ctx, account.TenantID, sessionID)
	if err != nil || binding.TeamBotID != first.ID {
		t.Fatalf("binding = %+v err=%v, want the first team snapshot", binding, err)
	}
}
