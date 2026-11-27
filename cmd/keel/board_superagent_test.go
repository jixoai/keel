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
	"github.com/jixoai/keel/internal/boardintegration"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/store"

	"github.com/jixoai/keel-board/protocol"
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

// TestBoardChannelOperatorACLUsesRealGroupMembership pins the production
// operator seam: the mirror grants tenant-admin/bot-operator owner on the queue
// channel, and a real group member can then pass the channel-level editor check
// the bridge applies. A board.write holder with no relation is refused.
func TestBoardChannelOperatorACLUsesRealGroupMembership(t *testing.T) {
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
	if err := st.CreateTenant(ctx, tenant, "board-acl-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if err := accesswire.EnsureBuiltinGroups(ctx, st, tenant); err != nil {
		t.Fatal(err)
	}
	account := func(name string, keys ...string) store.AccountRecord {
		t.Helper()
		record, err := st.CreateAccount(ctx, store.AccountRecord{
			TenantID: tenant, ID: uuid.New(), Username: name + "-" + tenant.String()[:8],
			DisplayName: name, Kind: "human",
		})
		if err != nil {
			t.Fatalf("account %s: %v", name, err)
		}
		for _, key := range keys {
			if _, err := st.CreatePermissionGrant(ctx, store.PermissionGrantRecord{
				TenantID: tenant, ID: uuid.New(), SubjectType: store.PermissionSubjectAccount,
				SubjectID: record.ID, PermissionKey: key, Scope: store.PermissionScopeTenant,
				GrantedBy: record.ID,
			}); err != nil {
				t.Fatalf("grant %s: %v", key, err)
			}
		}
		return record
	}
	operator := account("operator")
	admin := account("admin")
	writer := account("writer", access.KeyBoardWrite)
	for _, tc := range []struct {
		name   string
		record store.AccountRecord
		group  string
	}{
		{"tenant-admin", admin, "tenant-admin"},
		{"bot-operator", operator, "bot-operator"},
	} {
		if _, err := st.AddPermissionGroupMember(ctx, tenant, access.BuiltinGroupID(tenant, tc.group), tc.record.ID, tc.record.ID); err != nil {
			t.Fatalf("%s membership: %v", tc.name, err)
		}
	}
	bridge, err := accesswire.NewBridge(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := boardintegration.NewAuthorizer(bridge, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := (boardChannelACL{store: st}).EnsureOperator(ctx, tenant, "superagent"); err != nil {
		t.Fatalf("operator ACL: %v", err)
	}
	for _, tc := range []struct {
		name   string
		record store.AccountRecord
		allow  bool
	}{
		{"tenant-admin", admin, true},
		{"bot-operator", operator, true},
		{"board.write no relation", writer, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			principalCtx := auth.WithPrincipal(ctx, auth.Principal{
				Kind: auth.PrincipalHuman, TenantID: tenant, AccountID: tc.record.ID,
			})
			actor := protocol.Actor{
				ID:        boardintegration.AccountActorID(tc.record.ID),
				Principal: boardintegration.AccountActorID(tc.record.ID),
			}
			err := authorizer.Authorize(principalCtx, tenant.String(), actor, "superagent", protocol.PermissionLabel)
			if tc.allow && err != nil {
				t.Fatalf("%s denied: %v", tc.name, err)
			}
			if !tc.allow && err == nil {
				t.Fatalf("%s was allowed without a channel relation", tc.name)
			}
		})
	}
}
