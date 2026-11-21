// Session-owner callback tests (change keel-bot-identity-access, A4.1): the
// production seam POST /v1/sessions uses when the body names a bot.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/httpapi"
	"github.com/jixoai/keel/internal/store"
)

func TestSessionOwnerCallback(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "session-owner-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	makeAccount := func(name string) store.AccountRecord {
		t.Helper()
		account, err := st.CreateAccount(ctx, store.AccountRecord{
			TenantID: tenant, ID: uuid.New(), Username: name + "-" + tenant.String()[:8],
			DisplayName: name, Kind: "human",
		})
		if err != nil {
			t.Fatal(err)
		}
		return account
	}
	owner, outsider := makeAccount("owner"), makeAccount("outsider")
	role, err := st.CreateRole(ctx, store.RoleRecord{TenantID: tenant, ID: uuid.New(), Name: "Role"})
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
		TenantID: tenant, ID: "support-1", Name: "Support", ProfileID: profile.ID,
		ModelKind: "text", DefaultModel: json.RawMessage(`{"providerId":"fake","modelId":"fake"}`),
		Harness: json.RawMessage(`{"tools":[],"context":{"permissions":[]}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutResourceRelation(ctx, store.ResourceRelation{
		TenantID: tenant, ResourceType: "bot", ResourceID: bot.ID,
		Relation: store.RelationOwner, SubjectType: store.RelationSubjectAccount,
		SubjectID: owner.ID.String(), CreatedBy: owner.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePermissionGrant(ctx, store.PermissionGrantRecord{
		TenantID: tenant, ID: uuid.New(), SubjectType: store.PermissionSubjectAccount,
		SubjectID: owner.ID, PermissionKey: access.KeyBotInvoke,
		Scope: string(access.ScopeTenant), GrantedBy: owner.ID,
	}); err != nil {
		t.Fatal(err)
	}
	bridge, err := accesswire.NewBridge(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := auth.WithPrincipal(ctx, auth.Principal{
		Kind: auth.PrincipalHuman, TenantID: tenant, AccountID: owner.ID,
	})
	outsiderCtx := auth.WithPrincipal(ctx, auth.Principal{
		Kind: auth.PrincipalHuman, TenantID: tenant, AccountID: outsider.ID,
	})
	callback := newSessionOwner(st, bridge)

	sessionID := uuid.New()
	if err := callback(ownerCtx, tenant, sessionID, bot.ID); err != nil {
		t.Fatalf("owner create: %v", err)
	}
	ownership, err := st.GetSessionOwnership(ctx, tenant, sessionID)
	if err != nil {
		t.Fatalf("ownership: %v", err)
	}
	if ownership.BotID != bot.ID || ownership.AccountID != owner.ID || ownership.CreatedByAccountID != owner.ID {
		t.Fatalf("ownership = %+v", ownership)
	}
	holder, err := st.HasResourceRelation(ctx, tenant, "session", sessionID.String(),
		store.RelationOwner, store.RelationSubjectAccount, owner.ID.String())
	if err != nil || !holder {
		t.Fatalf("owner relation = %v err=%v", holder, err)
	}

	// An account without bot.invoke is refused with the engine reason.
	var ownershipErr *httpapi.SessionOwnerError
	if err := callback(outsiderCtx, tenant, uuid.New(), bot.ID); !errors.As(err, &ownershipErr) ||
		ownershipErr.Status != http.StatusForbidden {
		t.Fatalf("outsider create err = %v", err)
	}
	// A header-named foreign tenant never becomes a principal.
	if err := callback(ownerCtx, uuid.New(), uuid.New(), bot.ID); !errors.As(err, &ownershipErr) ||
		ownershipErr.Status != http.StatusUnauthorized {
		t.Fatalf("tenant mismatch err = %v", err)
	}
	// An archived bot refuses a new session.
	if err := st.ArchiveBot(ctx, tenant, bot.ID); err != nil {
		t.Fatal(err)
	}
	if err := callback(ownerCtx, tenant, uuid.New(), bot.ID); !errors.As(err, &ownershipErr) ||
		ownershipErr.Status != http.StatusConflict {
		t.Fatalf("archived bot err = %v", err)
	}
}
