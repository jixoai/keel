package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestCreateOwnedSessionWritesOwnershipAndOwnerRelation(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	account, _, _, bot := identityFixture(t, st)
	sessionID := uuid.New()
	ownership := SessionOwnership{
		TenantID: account.TenantID, SessionID: sessionID, BotID: bot.ID,
		AccountID: account.ID, CreatedByAccountID: account.ID,
	}
	if err := st.CreateOwnedSession(ctx, ownership); err != nil {
		t.Fatalf("CreateOwnedSession: %v", err)
	}
	got, err := st.GetSessionOwnership(ctx, account.TenantID, sessionID)
	if err != nil {
		t.Fatalf("GetSessionOwnership: %v", err)
	}
	if got.BotID != bot.ID || got.AccountID != account.ID || got.CreatedByAccountID != account.ID {
		t.Fatalf("ownership = %+v", got)
	}
	holder, err := st.HasResourceRelation(ctx, account.TenantID, "session", sessionID.String(),
		RelationOwner, RelationSubjectAccount, account.ID.String())
	if err != nil {
		t.Fatalf("HasResourceRelation: %v", err)
	}
	if !holder {
		t.Fatal("owner relation missing")
	}
	// Idempotent: repeating the same ownership is a success.
	if err := st.CreateOwnedSession(ctx, ownership); err != nil {
		t.Fatalf("repeat CreateOwnedSession: %v", err)
	}
}

func TestCreateOwnedSessionRejectsConflicts(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	account, _, _, bot := identityFixture(t, st)

	// A second account that does not own the bot.
	other, err := st.CreateAccount(ctx, AccountRecord{
		TenantID: account.TenantID, ID: uuid.New(), Username: "other-" + account.Username,
		DisplayName: "Other", Kind: "human",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateOwnedSession(ctx, SessionOwnership{
		TenantID: account.TenantID, SessionID: uuid.New(), BotID: bot.ID,
		AccountID: other.ID, CreatedByAccountID: other.ID,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong account err = %v, want ErrConflict", err)
	}

	// A live session cannot be re-owned.
	sessionID := uuid.New()
	if err := st.CreateOwnedSession(ctx, SessionOwnership{
		TenantID: account.TenantID, SessionID: sessionID, BotID: bot.ID,
		AccountID: account.ID, CreatedByAccountID: account.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateOwnedSession(ctx, SessionOwnership{
		TenantID: account.TenantID, SessionID: sessionID, BotID: bot.ID,
		AccountID: other.ID, CreatedByAccountID: other.ID,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("re-own err = %v, want ErrConflict", err)
	}

	// An archived bot refuses a new session; the existing one is untouched.
	if err := st.ArchiveBot(ctx, account.TenantID, bot.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateOwnedSession(ctx, SessionOwnership{
		TenantID: account.TenantID, SessionID: uuid.New(), BotID: bot.ID,
		AccountID: account.ID, CreatedByAccountID: account.ID,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("archived bot err = %v, want ErrConflict", err)
	}
	if _, err := st.GetSessionOwnership(ctx, account.TenantID, sessionID); err != nil {
		t.Fatalf("existing session lost after archived refusal: %v", err)
	}

	// A missing bot is ErrNotFound, not a new row.
	if err := st.CreateOwnedSession(ctx, SessionOwnership{
		TenantID: account.TenantID, SessionID: uuid.New(), BotID: "nope",
		AccountID: account.ID, CreatedByAccountID: account.ID,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing bot err = %v, want ErrNotFound", err)
	}
}

func TestCreateOwnedSessionTeamBotSnapshot(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	account, _, _, bot := identityFixture(t, st)
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
		t.Fatalf("CreateOwnedSession: %v", err)
	}
	got, err := st.GetSessionOwnership(ctx, account.TenantID, sessionID)
	if err != nil || got.TeamBotID != team.ID {
		t.Fatalf("team snapshot = %+v err=%v", got, err)
	}
	if err := st.ArchiveTeamBot(ctx, account.TenantID, team.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateOwnedSession(ctx, SessionOwnership{
		TenantID: account.TenantID, SessionID: uuid.New(), BotID: bot.ID,
		AccountID: account.ID, CreatedByAccountID: account.ID, TeamBotID: team.ID,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("archived team err = %v, want ErrConflict", err)
	}
}
