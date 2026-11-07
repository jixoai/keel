// MCP OAuth persistence: encryption at rest, pending browser steps, and the
// PostgreSQL refresh lock. Skipped when KEEL_TEST_DSN is unset, like the other
// store integration tests.

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func mcpOAuthTestStore(t *testing.T) (*Store, uuid.UUID) {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	st, err := Open(context.Background(), Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	tenant := uuid.New()
	if err := st.CreateTenant(context.Background(), tenant, "mcp-oauth-test"); err != nil {
		t.Fatal(err)
	}
	return st, tenant
}

func testSecretBox(t *testing.T) *SecretBox {
	t.Helper()
	key := bytes.Repeat([]byte{0x42}, 32)
	box, err := NewSecretBox(key)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func TestSecretBoxRoundTrip(t *testing.T) {
	box := testSecretBox(t)
	aad := MCPOAuthAAD(uuid.New(), "server")
	sealed, err := box.Seal([]byte("access-token-secret"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("access-token-secret")) {
		t.Error("ciphertext contains the plaintext")
	}
	plaintext, err := box.Open(sealed, aad)
	if err != nil || string(plaintext) != "access-token-secret" {
		t.Fatalf("open = %q, %v", plaintext, err)
	}
	// The AAD binds the payload to its tenant/server: another row cannot open it.
	if _, err := box.Open(sealed, MCPOAuthAAD(uuid.New(), "server")); err == nil {
		t.Error("payload opened with another tenant's AAD")
	}
	if _, err := box.Open(sealed, MCPOAuthAAD(uuid.New(), "other-server")); err == nil {
		t.Error("payload opened with another server's AAD")
	}
	other := bytes.Repeat([]byte{0x24}, 32)
	otherBox, _ := NewSecretBox(other)
	if _, err := otherBox.Open(sealed, aad); err == nil {
		t.Error("a different key decrypted the payload")
	}
	if _, err := box.Open(sealed[:4], aad); err == nil {
		t.Error("truncated payload decrypted")
	}
	// The envelope carries a version and key id for deliberate rotation.
	if sealed[0] != secretPayloadVersion {
		t.Errorf("version byte = %d", sealed[0])
	}
}

func TestSecretKeyFormats(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 32)
	inputs := []string{
		"1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a",
		"ERERERERERERERERERERERERERERERERERERERERERE=",
	}
	for _, input := range inputs {
		decoded, err := ParseSecretKey(input)
		if err != nil {
			t.Fatalf("parse %q: %v", input, err)
		}
		if len(decoded) != 32 {
			t.Errorf("len(%q) = %d", input, len(decoded))
		}
	}
	if _, err := ParseSecretKey("short"); err == nil {
		t.Error("short key accepted")
	}
	// A raw 32-character string is an ambiguous, likely weak passphrase.
	if _, err := ParseSecretKey(string(key)); err == nil {
		t.Error("raw 32-character key accepted")
	}
	if _, err := SecretKeyFromEnv(func(string) string { return "" }); !errors.Is(err, ErrSecretKeyMissing) {
		t.Errorf("missing key error = %v", err)
	}
}

func TestMCPOAuthCredentialsEncryptedAtRest(t *testing.T) {
	st, tenant := mcpOAuthTestStore(t)
	box := testSecretBox(t)
	ctx := context.Background()
	server := "server-" + uuid.NewString()
	payload := []byte(`{"tokens":{"access_token":"access-token-secret","refresh_token":"refresh-token-secret"}}`)
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	if err := st.SaveMCPOAuthCredentials(ctx, MCPOAuthCredentials{
		TenantID: tenant, Server: server, Payload: payload, Scopes: "read write", ExpiresAt: &expires,
	}, box); err != nil {
		t.Fatal(err)
	}

	// The row holds ciphertext, never the tokens.
	var raw []byte
	if err := st.pool.QueryRow(ctx,
		`SELECT payload FROM mcp_oauth_credentials WHERE tenant_id = $1 AND server = $2`,
		tenant, server).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("access-token-secret")) || bytes.Contains(raw, []byte("refresh-token-secret")) {
		t.Fatal("credentials stored in plaintext")
	}

	loaded, err := st.LoadMCPOAuthCredentials(ctx, tenant, server, box)
	if err != nil {
		t.Fatal(err)
	}
	if string(loaded.Payload) != string(payload) || loaded.Scopes != "read write" {
		t.Errorf("loaded = %+v", loaded)
	}
	if loaded.ExpiresAt == nil || !loaded.ExpiresAt.Equal(expires) {
		t.Errorf("expires at = %v", loaded.ExpiresAt)
	}
	if loaded.Version != 1 {
		t.Errorf("version = %d", loaded.Version)
	}

	otherBox, _ := NewSecretBox(bytes.Repeat([]byte{0x24}, 32))
	if _, err := st.LoadMCPOAuthCredentials(ctx, tenant, server, otherBox); err == nil {
		t.Error("a different key loaded the credentials")
	}
	if _, err := st.LoadMCPOAuthCredentials(ctx, tenant, server, nil); !errors.Is(err, ErrSecretKeyMissing) {
		t.Errorf("nil box error = %v", err)
	}
	if err := st.SaveMCPOAuthCredentials(ctx, MCPOAuthCredentials{TenantID: tenant, Server: server}, nil); !errors.Is(err, ErrSecretKeyMissing) {
		t.Errorf("nil box save error = %v", err)
	}

	otherTenant := uuid.New()
	if _, err := st.LoadMCPOAuthCredentials(ctx, otherTenant, server, box); !errors.Is(err, ErrMCPOAuthNotFound) {
		t.Errorf("other tenant error = %v", err)
	}
	if err := st.DeleteMCPOAuthCredentials(ctx, tenant, server); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LoadMCPOAuthCredentials(ctx, tenant, server, box); !errors.Is(err, ErrMCPOAuthNotFound) {
		t.Errorf("after delete error = %v", err)
	}
}

func TestMCPOAuthPendingLifecycle(t *testing.T) {
	st, tenant := mcpOAuthTestStore(t)
	ctx := context.Background()
	server := "server-" + uuid.NewString()
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Millisecond)
	// States are globally unique (the unauthenticated callback lookup relies on
	// it), so repeated runs must not reuse a fixed value.
	stateOne := uuid.NewString()
	stateTwo := uuid.NewString()
	first := MCPOAuthPending{
		TenantID: tenant, Server: server, State: stateOne, CodeVerifier: "verifier-1",
		RedirectURL: "https://app.example/callback", AuthorizationURL: "https://auth.example/authorize?state=" + stateOne,
		Scopes: "read", ExpiresAt: expires,
	}
	if err := st.StartMCPOAuthPending(ctx, first); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.GetMCPOAuthPending(ctx, tenant, server, stateOne)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CodeVerifier != "verifier-1" || loaded.RedirectURL != first.RedirectURL || loaded.Scopes != "read" {
		t.Errorf("pending = %+v", loaded)
	}
	if loaded.ConsumedAt != nil {
		t.Error("pending already consumed")
	}
	found, err := st.FindMCPOAuthPendingByState(ctx, stateOne)
	if err != nil || found.TenantID != tenant || found.Server != server {
		t.Fatalf("find = %+v, %v", found, err)
	}

	// A second browser step supersedes the first.
	second := first
	second.State = stateTwo
	second.CodeVerifier = "verifier-2"
	second.AuthorizationURL = "https://auth.example/authorize?state=" + stateTwo
	if err := st.StartMCPOAuthPending(ctx, second); err != nil {
		t.Fatal(err)
	}
	superseded, err := st.GetMCPOAuthPending(ctx, tenant, server, stateOne)
	if err != nil {
		t.Fatal(err)
	}
	if superseded.ConsumedAt == nil {
		t.Error("first pending was not superseded")
	}
	active, err := st.GetMCPOAuthPending(ctx, tenant, server, stateTwo)
	if err != nil || active.ConsumedAt != nil {
		t.Fatalf("second pending = %+v, %v", active, err)
	}

	if _, err := st.ConsumeMCPOAuthPending(ctx, tenant, server, stateTwo); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConsumeMCPOAuthPending(ctx, tenant, server, stateTwo); !errors.Is(err, ErrMCPOAuthNotFound) {
		t.Errorf("double consume error = %v", err)
	}
	if _, err := st.GetMCPOAuthPending(ctx, tenant, server, uuid.NewString()); !errors.Is(err, ErrMCPOAuthNotFound) {
		t.Errorf("missing pending error = %v", err)
	}
}

func TestMCPOAuthRefreshLock(t *testing.T) {
	st, tenant := mcpOAuthTestStore(t)
	server := "server-" + uuid.NewString()
	ctx := context.Background()

	held := make(chan struct{})
	release := make(chan struct{})
	var running int32
	var maxRunning int32
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- st.WithMCPOAuthRefreshLock(ctx, tenant, server, 2*time.Second, func(context.Context) error {
			current := atomic.AddInt32(&running, 1)
			for {
				previous := atomic.LoadInt32(&maxRunning)
				if current <= previous || atomic.CompareAndSwapInt32(&maxRunning, previous, current) {
					break
				}
			}
			close(held)
			<-release
			atomic.AddInt32(&running, -1)
			return nil
		})
	}()
	<-held

	// A second process gives up after its bounded wait.
	start := time.Now()
	err := st.WithMCPOAuthRefreshLock(ctx, tenant, server, 200*time.Millisecond, func(context.Context) error {
		t.Error("the busy lock ran its callback")
		return nil
	})
	if !errors.Is(err, ErrMCPOAuthRefreshLockBusy) {
		t.Fatalf("busy error = %v", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("busy wait = %v, want at least the lock timeout", elapsed)
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}

	// After release the lock is available again.
	ran := false
	if err := st.WithMCPOAuthRefreshLock(ctx, tenant, server, time.Second, func(context.Context) error {
		ran = true
		return nil
	}); err != nil || !ran {
		t.Fatalf("second acquire = %v", err)
	}

	// Concurrent holders serialize.
	var wg sync.WaitGroup
	var concurrentMu sync.Mutex
	concurrent := 0
	maxConcurrent := 0
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := st.WithMCPOAuthRefreshLock(ctx, tenant, server, 2*time.Second, func(context.Context) error {
				concurrentMu.Lock()
				concurrent++
				if concurrent > maxConcurrent {
					maxConcurrent = concurrent
				}
				concurrentMu.Unlock()
				time.Sleep(5 * time.Millisecond)
				concurrentMu.Lock()
				concurrent--
				concurrentMu.Unlock()
				return nil
			}); err != nil {
				t.Errorf("lock: %v", err)
			}
		}()
	}
	wg.Wait()
	if maxConcurrent > 1 {
		t.Errorf("max concurrent holders = %d", maxConcurrent)
	}
}

func TestMCPOAuthRefreshLockCallbackError(t *testing.T) {
	st, tenant := mcpOAuthTestStore(t)
	server := "server-" + uuid.NewString()
	expected := errors.New("callback failed")
	err := st.WithMCPOAuthRefreshLock(context.Background(), tenant, server, time.Second, func(context.Context) error {
		return expected
	})
	if !errors.Is(err, expected) {
		t.Fatalf("error = %v", err)
	}
	// The lock was released despite the error.
	ran := false
	if err := st.WithMCPOAuthRefreshLock(context.Background(), tenant, server, time.Second, func(context.Context) error {
		ran = true
		return nil
	}); err != nil || !ran {
		t.Fatalf("lock after error = %v", err)
	}
}

// TestMCPOAuthRefreshLockDoesNotExhaustMainPool reproduces the P0 deadlock:
// with MaxConns=2, two lock holders used to take both main-pool connections and
// then block loading state on the same pool. Lock connections now come from a
// separate pool, so every refresh completes.
func TestMCPOAuthRefreshLockDoesNotExhaustMainPool(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := Open(ctx, Config{
		DSN:               dsn,
		MaxConns:          2,
		MinConns:          1,
		MCPOAuthLockConns: 2,
		AcquireTimeout:    2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box := testSecretBox(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		tenant := uuid.New()
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- st.WithMCPOAuthRefreshLock(ctx, tenant, "shared-server", 5*time.Second, func(lockCtx context.Context) error {
				// The callback uses the main pool while the lock connection is held.
				_, err := st.LoadMCPOAuthCredentials(lockCtx, tenant, "shared-server", box)
				if err != nil && !errors.Is(err, ErrMCPOAuthNotFound) {
					return err
				}
				return st.SaveMCPOAuthCredentials(lockCtx, MCPOAuthCredentials{
					TenantID: tenant, Server: "shared-server", Payload: []byte(`{"tokens":{"access_token":"x"}}`),
				}, box)
			})
		}()
	}
	completed := make(chan struct{})
	go func() { wg.Wait(); close(completed) }()
	select {
	case <-completed:
	case <-ctx.Done():
		t.Fatal("refresh locks deadlocked while callbacks used the main pool")
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("lock: %v", err)
		}
	}
}

// TestMCPOAuthPendingAtomicConsume proves exactly one concurrent completion can
// spend a pending browser step.
func TestMCPOAuthPendingAtomicConsume(t *testing.T) {
	st, tenant := mcpOAuthTestStore(t)
	ctx := context.Background()
	server := "server-" + uuid.NewString()
	state := uuid.NewString()
	if err := st.StartMCPOAuthPending(ctx, MCPOAuthPending{
		TenantID: tenant, Server: server, State: state, CodeVerifier: "verifier",
		RedirectURL: "https://app.example/callback", AuthorizationURL: "https://auth.example/authorize",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var winners int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.ConsumeMCPOAuthPending(ctx, tenant, server, state)
			if err == nil {
				atomic.AddInt32(&winners, 1)
				return
			}
			if !errors.Is(err, ErrMCPOAuthNotFound) {
				t.Errorf("consume: %v", err)
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1", winners)
	}
}

// TestMCPOAuthPendingSingleActive proves concurrent starts leave exactly one
// active browser step (serialized by the advisory lock and unique index).
func TestMCPOAuthPendingSingleActive(t *testing.T) {
	st, tenant := mcpOAuthTestStore(t)
	ctx := context.Background()
	server := "server-" + uuid.NewString()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := st.StartMCPOAuthPending(ctx, MCPOAuthPending{
				TenantID: tenant, Server: server, State: uuid.NewString(), CodeVerifier: "verifier",
				RedirectURL: "https://app.example/callback", AuthorizationURL: "https://auth.example/authorize",
				ExpiresAt: time.Now().Add(5 * time.Minute),
			}); err != nil {
				t.Errorf("start: %v", err)
			}
		}()
	}
	wg.Wait()
	var active int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM mcp_oauth_pending WHERE tenant_id=$1 AND server=$2 AND consumed_at IS NULL`,
		tenant, server).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Errorf("active pendings = %d, want 1", active)
	}
}

// insertMCPOAuthPending writes a pending row directly, so retention tests can
// date the terminal state in the past (Start/Consume always stamp now()).
func insertMCPOAuthPending(t *testing.T, st *Store, tenant uuid.UUID, server, state string, expires time.Time, consumedAt *time.Time) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(),
		`INSERT INTO mcp_oauth_pending
		 (tenant_id, server, state, code_verifier, redirect_uri, authorization_url, expires_at, consumed_at)
		 VALUES ($1, $2, $3, 'verifier', 'https://app.example/callback', 'https://auth.example/authorize', $4, $5)`,
		tenant, server, state, expires, consumedAt); err != nil {
		t.Fatalf("insert pending %s: %v", server, err)
	}
}

// TestMCPOAuthPendingPrune removes terminal rows older than the retention
// cutoff: consumed (finished or superseded) and expired ones. An unexpired,
// unconsumed browser step must survive because a callback may still arrive for
// it, and a recently terminal row stays readable inside the retention window.
func TestMCPOAuthPendingPrune(t *testing.T) {
	st, tenant := mcpOAuthTestStore(t)
	ctx := context.Background()
	prefix := "prune-" + uuid.NewString()
	now := time.Now()
	cutoff := now.Add(-time.Hour)

	expiredState := uuid.NewString()
	insertMCPOAuthPending(t, st, tenant, prefix+"-expired", expiredState, now.Add(-2*time.Hour), nil)
	consumedState := uuid.NewString()
	consumedAt := now.Add(-2 * time.Hour)
	insertMCPOAuthPending(t, st, tenant, prefix+"-consumed", consumedState, now.Add(time.Hour), &consumedAt)
	recentState := uuid.NewString()
	recentConsumedAt := now.Add(-time.Minute)
	insertMCPOAuthPending(t, st, tenant, prefix+"-recent", recentState, now.Add(time.Hour), &recentConsumedAt)
	activeState := uuid.NewString()
	if err := st.StartMCPOAuthPending(ctx, MCPOAuthPending{
		TenantID: tenant, Server: prefix + "-active", State: activeState, CodeVerifier: "verifier",
		RedirectURL: "https://app.example/callback", AuthorizationURL: "https://auth.example/authorize",
		ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	removed, err := st.PruneMCPOAuthPending(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	// The test database is shared with other packages: other stale rows may
	// also be swept, so only this test's rows are asserted individually.
	if removed < 2 {
		t.Fatalf("pruned rows = %d, want at least this test's 2 (expired + consumed)", removed)
	}
	for _, gone := range []struct{ server, state string }{
		{prefix + "-expired", expiredState},
		{prefix + "-consumed", consumedState},
	} {
		if _, err := st.GetMCPOAuthPending(ctx, tenant, gone.server, gone.state); !errors.Is(err, ErrMCPOAuthNotFound) {
			t.Errorf("%s survived the prune: %v", gone.server, err)
		}
	}
	if _, err := st.GetMCPOAuthPending(ctx, tenant, prefix+"-active", activeState); err != nil {
		t.Fatalf("unexpired pending was pruned: %v", err)
	}
	if _, err := st.GetMCPOAuthPending(ctx, tenant, prefix+"-recent", recentState); err != nil {
		t.Fatalf("recently consumed pending was pruned inside the retention window: %v", err)
	}
	if _, err := st.PruneMCPOAuthPending(ctx, cutoff); err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if _, err := st.GetMCPOAuthPending(ctx, tenant, prefix+"-active", activeState); err != nil {
		t.Fatalf("unexpired pending was pruned by the second sweep: %v", err)
	}
}

// TestMCPOAuthPendingPruneConcurrent runs several pruners while browser steps
// are created, superseded, and inserted with past expiry concurrently: no
// sweep may fail, and the unexpired, unconsumed steps a callback could still
// use must survive.
func TestMCPOAuthPendingPruneConcurrent(t *testing.T) {
	st, tenant := mcpOAuthTestStore(t)
	ctx := context.Background()
	prefix := "prune-race-" + uuid.NewString()
	cutoff := time.Now().Add(-time.Hour)
	activeState := uuid.NewString()
	if err := st.StartMCPOAuthPending(ctx, MCPOAuthPending{
		TenantID: tenant, Server: prefix, State: activeState, CodeVerifier: "verifier",
		RedirectURL: "https://app.example/callback", AuthorizationURL: "https://auth.example/authorize",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if _, err := st.PruneMCPOAuthPending(ctx, cutoff); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			server := fmt.Sprintf("%s-churn-%d", prefix, i)
			for j := 0; j < 5; j++ {
				expiry := time.Now().Add(time.Hour)
				if j%2 == 1 {
					expiry = time.Now().Add(-2 * time.Hour)
				}
				if err := st.StartMCPOAuthPending(ctx, MCPOAuthPending{
					TenantID: tenant, Server: server, State: uuid.NewString(), CodeVerifier: "verifier",
					RedirectURL: "https://app.example/callback", AuthorizationURL: "https://auth.example/authorize",
					ExpiresAt: expiry,
				}); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				// One row per server: the partial unique index allows a single
				// unconsumed row per (tenant, server), even when it is expired.
				server := fmt.Sprintf("%s-old-%d-%d", prefix, i, j)
				if _, err := st.pool.Exec(ctx,
					`INSERT INTO mcp_oauth_pending
					 (tenant_id, server, state, code_verifier, redirect_uri, authorization_url, expires_at)
					 VALUES ($1, $2, $3, 'verifier', 'https://app.example/callback', 'https://auth.example/authorize', $4)`,
					tenant, server, uuid.NewString(), time.Now().Add(-2*time.Hour)); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent prune/start: %v", err)
	}

	if _, err := st.GetMCPOAuthPending(ctx, tenant, prefix, activeState); err != nil {
		t.Fatalf("active pending was pruned during churn: %v", err)
	}
	if _, err := st.PruneMCPOAuthPending(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	var dead, active int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE (consumed_at IS NOT NULL AND consumed_at <= $3) OR expires_at <= $3),
		        count(*) FILTER (WHERE consumed_at IS NULL)
		 FROM mcp_oauth_pending WHERE tenant_id = $1 AND server LIKE $2`,
		tenant, prefix+"%", cutoff).Scan(&dead, &active); err != nil {
		t.Fatal(err)
	}
	if dead != 0 {
		t.Errorf("dead pending rows older than the cutoff after a final prune = %d, want 0", dead)
	}
	// One surviving active step per churn server plus the untouched one.
	if active != 5 {
		t.Errorf("active pending rows = %d, want 5", active)
	}
}

// TestMCPOAuthLockPoolBudgetAndTimeouts pins the lock-pool design: a separate
// pool with no minimum connections (created on demand), capped by
// MCPOAuthLockConns, with statement_timeout=0 so lock_timeout alone bounds a
// wait. The main pool keeps the runtime-wide statement timeout.
func TestMCPOAuthLockPoolBudgetAndTimeouts(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := Open(ctx, Config{DSN: dsn, MCPOAuthLockConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	before := st.lockPool.Stat()
	if before.MaxConns() != 2 {
		t.Errorf("lock pool max conns = %d, want the configured 2", before.MaxConns())
	}
	// MinConns is zero (store.go), so no connection exists before the first
	// refresh: the pool's budget is spent on demand, never pre-warmed.
	if before.TotalConns() != 0 {
		t.Errorf("lock pool opened %d connections before first use, want 0", before.TotalConns())
	}
	if mainMax := st.Pool().Stat().MaxConns(); mainMax != 24 {
		t.Errorf("main pool max conns = %d, want the default 24 (separate budget)", mainMax)
	}

	var held int32
	err = st.WithMCPOAuthRefreshLock(ctx, uuid.New(), "budget-server", time.Second, func(lockCtx context.Context) error {
		held = st.lockPool.Stat().TotalConns()
		conn, err := st.lockPool.Acquire(lockCtx)
		if err != nil {
			return err
		}
		defer conn.Release()
		var lockTimeout, mainTimeout int
		if err := conn.QueryRow(lockCtx,
			`SELECT setting::int FROM pg_settings WHERE name = 'statement_timeout'`).Scan(&lockTimeout); err != nil {
			return err
		}
		if lockTimeout != 0 {
			t.Errorf("lock connection statement_timeout = %d, want 0", lockTimeout)
		}
		if err := st.Pool().QueryRow(lockCtx,
			`SELECT setting::int FROM pg_settings WHERE name = 'statement_timeout'`).Scan(&mainTimeout); err != nil {
			return err
		}
		if mainTimeout != 5000 {
			t.Errorf("main pool statement_timeout = %d, want 5000", mainTimeout)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if held != 1 {
		t.Errorf("lock pool connections held by one refresh = %d, want 1", held)
	}
	if after := st.lockPool.Stat(); after.TotalConns() > after.MaxConns() {
		t.Errorf("lock pool exceeded its budget: %d > %d", after.TotalConns(), after.MaxConns())
	}
}

// TestMCPOAuthCredentialsBoundToRow proves the AAD: a ciphertext copied to
// another tenant/server row does not decrypt there.
func TestMCPOAuthCredentialsBoundToRow(t *testing.T) {
	st, tenant := mcpOAuthTestStore(t)
	box := testSecretBox(t)
	ctx := context.Background()
	server := "server-" + uuid.NewString()
	if err := st.SaveMCPOAuthCredentials(ctx, MCPOAuthCredentials{
		TenantID: tenant, Server: server, Payload: []byte(`{"tokens":{"access_token":"secret"}}`),
	}, box); err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	if err := st.pool.QueryRow(ctx,
		`SELECT payload FROM mcp_oauth_credentials WHERE tenant_id=$1 AND server=$2`,
		tenant, server).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	// Simulate someone with database write access moving the ciphertext.
	otherTenant := uuid.New()
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO mcp_oauth_credentials (tenant_id, server, payload) VALUES ($1,$2,$3)`,
		otherTenant, server, sealed); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LoadMCPOAuthCredentials(ctx, otherTenant, server, box); err == nil {
		t.Fatal("ciphertext decrypted for another tenant")
	}
}
