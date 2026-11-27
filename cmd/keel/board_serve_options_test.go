package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/store"
)

func TestBoardProviderHostsEnv(t *testing.T) {
	t.Setenv("KEEL_BOARD_PROVIDER_HOSTS", " api.example.com , api2.example.com ")
	if got, want := boardProviderHosts(), []string{"api.example.com", "api2.example.com"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hosts = %v, want %v", got, want)
	}
	t.Setenv("KEEL_BOARD_PROVIDER_HOSTS", "")
	if got := boardProviderHosts(); got != nil {
		t.Fatalf("empty hosts = %v", got)
	}
}

// TestBoardServeOptionsReachTheRuntimeProviderGate covers P2: the serve
// configure hook (not just Options injection) must carry
// KEEL_BOARD_PROVIDER_HOSTS into the running surface's provider validation.
func TestBoardServeOptionsReachTheRuntimeProviderGate(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "board-provider-"+tenant.String()[:8]); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := accesswire.EnsureBuiltinGroups(ctx, st, tenant); err != nil {
		t.Fatalf("groups: %v", err)
	}
	if _, err := st.SetTenantConfig(ctx, tenant, json.RawMessage(`{"board":{"enabled":true,"sync":{"enabled":true}}}`)); err != nil {
		t.Fatalf("tenant config: %v", err)
	}
	account, err := st.CreateAccount(ctx, store.AccountRecord{
		TenantID: tenant, ID: uuid.New(), Username: "provider-" + tenant.String()[:8],
		DisplayName: "Provider", Kind: "human",
	})
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	for _, key := range []string{access.KeyBoardManage, access.KeySecretWrite} {
		if _, err := st.CreatePermissionGrant(ctx, store.PermissionGrantRecord{
			TenantID: tenant, ID: uuid.New(), SubjectType: store.PermissionSubjectAccount,
			SubjectID: account.ID, PermissionKey: key, Scope: store.PermissionScopeTenant,
			GrantedBy: account.ID,
		}); err != nil {
			t.Fatalf("grant %s: %v", key, err)
		}
	}
	bridge, err := accesswire.NewBridge(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("access bridge: %v", err)
	}
	t.Setenv("KEEL_BOARD_ENABLED", "1")
	t.Setenv("KEEL_BOARD_PROVIDER_HOSTS", "api.example.com")
	schema := fmt.Sprintf("board_serve_%s", strings.ReplaceAll(uuid.NewString()[:8], "-", ""))
	t.Setenv("KEEL_BOARD_SCHEMA", schema)
	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = st.Pool().Exec(dropCtx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`)
	})
	wiring, err := newBoardWiringWithOptions(ctx, dsn, tenant, bridge, st,
		slog.New(slog.NewTextHandler(io.Discard, nil)), boardServeOptions(nil, t.TempDir()))
	if err != nil {
		t.Fatalf("board wiring: %v", err)
	}
	if wiring.Surface == nil {
		t.Fatal("no surface")
	}
	t.Cleanup(wiring.Surface.Close)

	body, _ := json.Marshal(map[string]any{
		"key": "slack", "platform": "slack",
		"secretRef": map[string]string{"signingSecret": "SLACK_SIGNING", "botToken": "SLACK_BOT"},
		"settings":  map[string]any{"botUserId": "U_APP", "apiBaseUrl": "https://api.example.com/api"},
		"enabled":   true,
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/board/sync/entries", bytes.NewReader(body))
	request.Header.Set("content-type", "application/json")
	request = request.WithContext(auth.WithPrincipal(request.Context(), auth.Principal{
		Kind: auth.PrincipalHuman, TenantID: tenant, AccountID: account.ID,
		Username: account.Username, DisplayName: account.DisplayName,
	}))
	recorder := httptest.NewRecorder()
	wiring.API.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("allowlisted provider host = %d %s", recorder.Code, recorder.Body.String())
	}
}
