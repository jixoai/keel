package main

// The Bot invoke / TeamBot start seam (A4.2b3a): creating the session writes
// Bot/Account/created-by/TeamBot ownership, and the committed user.message names
// the same Bot/TeamBot in its selection fact so the attempt resolves that
// Bot's Role/Harness prompt, tool whitelist and default model.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentapi"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

type captureCommitter struct {
	requests []kernel.CommitRequest
}

func (c *captureCommitter) Commit(_ context.Context, request kernel.CommitRequest) (kernel.Receipt, error) {
	c.requests = append(c.requests, request)
	return kernel.Receipt{Seq: int64(len(c.requests)), Kind: request.Kind}, nil
}

func (c *captureCommitter) Lookup(context.Context, kernel.Scope, kernel.KeyRef) (kernel.Receipt, bool, error) {
	return kernel.Receipt{}, false, nil
}

func starterFixture(t *testing.T) (*store.Store, uuid.UUID, uuid.UUID, store.BotRecord, store.TeamBotRecord) {
	t.Helper()
	st := testStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "starter-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	account, err := st.CreateAccount(ctx, store.AccountRecord{
		TenantID: tenant, ID: uuid.New(), Username: "owner-" + tenant.String()[:8],
		DisplayName: "Owner", Kind: "human",
	})
	if err != nil {
		t.Fatal(err)
	}
	role, err := st.CreateRole(ctx, store.RoleRecord{TenantID: tenant, ID: uuid.New(), Name: "Captain"})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := st.CreateProfile(ctx, store.ProfileRecord{
		TenantID: tenant, ID: uuid.New(), AccountID: account.ID, RoleID: role.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	bot, err := st.CreateBot(ctx, store.BotRecord{
		TenantID: tenant, ID: "captain-" + tenant.String()[:8], Name: "Captain",
		ProfileID: profile.ID, ModelKind: "text",
		DefaultModel: json.RawMessage(`{"providerId":"magpie","modelId":"qwen"}`),
		Harness:      json.RawMessage(`{"tools":["bash"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	team, err := st.CreateTeamBot(ctx, store.TeamBotRecord{
		TenantID: tenant, ID: "team-" + tenant.String()[:8], Name: "Team",
		Mission: "Resolve tickets", Roster: []byte(`[{"botId":"` + bot.ID + `","role":"captain"}]`),
		Revision: 1, ContentHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM events WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM sessions WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM team_bots WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM bots WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM profiles WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM roles WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM accounts WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM tenants WHERE tenant_id = $1`, tenant)
	})
	return st, tenant, account.ID, bot, team
}

func TestBotSessionStarterWritesOwnershipAndSelection(t *testing.T) {
	st, tenant, accountID, bot, _ := starterFixture(t)
	committer := &captureCommitter{}
	starter := agentext.NewTurnStarter(st, committer, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	start := newBotSessionStarter(st, starter)
	ctx := context.Background()
	result, err := start(ctx, agentapi.BotSessionRequest{
		TenantID: tenant, BotID: bot.ID, AccountID: accountID,
		ActorAccountID: accountID, Text: "hello", RequestID: "req-bot",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ownership, err := st.GetSessionOwnership(ctx, tenant, result.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if ownership.BotID != bot.ID || ownership.AccountID != accountID ||
		ownership.CreatedByAccountID != accountID {
		t.Fatalf("ownership = %+v", ownership)
	}
	if len(committer.requests) != 1 {
		t.Fatalf("commits = %d", len(committer.requests))
	}
	var payload struct {
		Selection map[string]string `json:"selection"`
	}
	if err := json.Unmarshal(committer.requests[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Selection["botId"] != bot.ID || payload.Selection["teamBotId"] != "" {
		t.Fatalf("selection = %+v", payload.Selection)
	}
}

func TestTeamBotSessionStarterWritesMissionSnapshotAndTeamSelection(t *testing.T) {
	st, tenant, accountID, bot, team := starterFixture(t)
	committer := &captureCommitter{}
	starter := agentext.NewTurnStarter(st, committer, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	start := newBotSessionStarter(st, starter)
	ctx := context.Background()
	result, err := start(ctx, agentapi.BotSessionRequest{
		TenantID: tenant, BotID: bot.ID, AccountID: accountID,
		ActorAccountID: accountID, TeamBotID: team.ID, Mission: team.Mission,
		Text: "go", RequestID: "req-team",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ownership, err := st.GetSessionOwnership(ctx, tenant, result.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if ownership.BotID != bot.ID || ownership.TeamBotID != team.ID {
		t.Fatalf("ownership = %+v", ownership)
	}
	var payload struct {
		Selection   map[string]string `json:"selection"`
		TeamMission string            `json:"teamMission"`
	}
	if err := json.Unmarshal(committer.requests[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Selection["teamBotId"] != team.ID || payload.Selection["botId"] != "" {
		t.Fatalf("selection = %+v", payload.Selection)
	}
	if payload.TeamMission != team.Mission {
		t.Fatalf("teamMission = %q", payload.TeamMission)
	}
}

func TestBotSessionStarterRefusesArchivedBot(t *testing.T) {
	st, tenant, accountID, bot, _ := starterFixture(t)
	if err := st.ArchiveBot(context.Background(), tenant, bot.ID); err != nil {
		t.Fatal(err)
	}
	committer := &captureCommitter{}
	starter := agentext.NewTurnStarter(st, committer, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	start := newBotSessionStarter(st, starter)
	_, err := start(context.Background(), agentapi.BotSessionRequest{
		TenantID: tenant, BotID: bot.ID, AccountID: accountID,
		ActorAccountID: accountID, Text: "hello", RequestID: "req-archived",
	})
	if err == nil {
		t.Fatal("an archived bot started a session")
	}
	if len(committer.requests) != 0 {
		t.Fatal("an archived bot committed a turn")
	}
}
