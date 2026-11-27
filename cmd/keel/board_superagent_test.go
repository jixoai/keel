package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/store"
)

// TestBoardCommandGateNeedsSupervisorTaskWrite pins the P1 review finding: a
// caller with board.write but no task.write must not reach a mirrored task's
// supervisor command. The bridge and the orchestrator share this gate.
func TestBoardCommandGateNeedsSupervisorTaskWrite(t *testing.T) {
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
	if err := st.CreateTenant(ctx, tenant, "board-gate-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	account, err := st.CreateAccount(ctx, store.AccountRecord{
		TenantID: tenant, ID: uuid.New(), Username: "gate-" + tenant.String()[:8],
		DisplayName: "Gate", Kind: "human",
	})
	if err != nil {
		t.Fatal(err)
	}
	grant := func(key string) {
		t.Helper()
		if _, err := st.CreatePermissionGrant(ctx, store.PermissionGrantRecord{
			TenantID: tenant, ID: uuid.New(), SubjectType: store.PermissionSubjectAccount,
			SubjectID: account.ID, PermissionKey: key,
			Scope: string(access.ScopeTenant), GrantedBy: account.ID,
		}); err != nil {
			t.Fatalf("grant %s: %v", key, err)
		}
	}
	grant(access.KeyBoardWrite)

	bridge, err := accesswire.NewBridge(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	principalCtx := auth.WithPrincipal(ctx, auth.Principal{
		Kind: auth.PrincipalHuman, TenantID: tenant, AccountID: account.ID,
	})
	gate := superagentOperator{bridge: bridge}
	if err := gate.AuthorizeTaskWrite(principalCtx, tenant); !errors.Is(err, collab.ErrPermissionDenied) {
		t.Fatalf("board.write-only gate = %v, want permission denied", err)
	}
	grant(access.KeyTaskWrite)
	if err := gate.AuthorizeTaskWrite(principalCtx, tenant); err != nil {
		t.Fatalf("board.write+task.write gate = %v", err)
	}
}
