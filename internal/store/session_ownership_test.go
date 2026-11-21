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

func TestCreateOwnedSessionClaimsAnUnownedSession(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	account, _, _, bot := identityFixture(t, st)
	sessionID := uuid.New()
	// The pre-A4 create path writes a tenant session with no owner columns.
	if err := st.CreateSession(ctx, account.TenantID, sessionID); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateOwnedSession(ctx, SessionOwnership{
		TenantID: account.TenantID, SessionID: sessionID, BotID: bot.ID,
		AccountID: account.ID, CreatedByAccountID: account.ID,
	}); err != nil {
		t.Fatalf("claim unowned session: %v", err)
	}
	got, err := st.GetSessionOwnership(ctx, account.TenantID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BotID != bot.ID || got.AccountID != account.ID || got.CreatedByAccountID != account.ID {
		t.Fatalf("claimed ownership = %+v", got)
	}
	// A claimed session cannot be re-owned by another account.
	other, err := st.CreateAccount(ctx, AccountRecord{
		TenantID: account.TenantID, ID: uuid.New(), Username: "claim-other-" + account.Username,
		DisplayName: "Other", Kind: "human",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateOwnedSession(ctx, SessionOwnership{
		TenantID: account.TenantID, SessionID: sessionID, BotID: bot.ID,
		AccountID: other.ID, CreatedByAccountID: other.ID,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("re-own claimed session err = %v, want ErrConflict", err)
	}
}

// TestCreateSessionLikeParentInheritsOwner pins the sub-agent spawn/fork
// contract: a child session gets the parent's Bot/account ownership and the
// same owner relation, so its single-resource routes authorize like the
// parent's. A legacy account-owned parent is inherited through its relation.
func TestCreateSessionLikeParentInheritsOwner(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	account, _, _, bot := identityFixture(t, st)

	parent := uuid.New()
	if err := st.CreateOwnedSession(ctx, SessionOwnership{
		TenantID: account.TenantID, SessionID: parent, BotID: bot.ID,
		AccountID: account.ID, CreatedByAccountID: account.ID,
	}); err != nil {
		t.Fatal(err)
	}
	child := uuid.New()
	if err := st.CreateSessionLikeParent(ctx, account.TenantID, parent, child); err != nil {
		t.Fatalf("inherit owned parent: %v", err)
	}
	got, err := st.GetSessionOwnership(ctx, account.TenantID, child)
	if err != nil {
		t.Fatal(err)
	}
	if got.BotID != bot.ID || got.AccountID != account.ID {
		t.Fatalf("child ownership = %+v, want bot %q account %s", got, bot.ID, account.ID)
	}
	holder, err := st.HasResourceRelation(ctx, account.TenantID, "session", child.String(),
		RelationOwner, RelationSubjectAccount, account.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if !holder {
		t.Fatal("child owner relation missing")
	}

	// Legacy account-owned parent: no bot columns, only the owner relation.
	legacy := uuid.New()
	if err := st.CreateAccountOwnedSession(ctx, account.TenantID, legacy, account.ID); err != nil {
		t.Fatal(err)
	}
	legacyChild := uuid.New()
	if err := st.CreateSessionLikeParent(ctx, account.TenantID, legacy, legacyChild); err != nil {
		t.Fatalf("inherit legacy parent: %v", err)
	}
	holder, err = st.HasResourceRelation(ctx, account.TenantID, "session", legacyChild.String(),
		RelationOwner, RelationSubjectAccount, account.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if !holder {
		t.Fatal("legacy child owner relation missing")
	}

	// A session with no ownership and no owner relation cannot have a child: the
	// child's owner would be unknowable.
	orphan := uuid.New()
	if err := st.CreateSession(ctx, account.TenantID, orphan); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSessionLikeParent(ctx, account.TenantID, orphan, uuid.New()); !errors.Is(err, ErrConflict) {
		t.Fatalf("orphan parent err = %v, want ErrConflict", err)
	}
}
