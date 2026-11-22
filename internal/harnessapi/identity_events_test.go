package harnessapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/store"
)

// identitySSEFrame is one parsed Server-Sent Events frame. Comment frames are
// keepalives; identity events carry `event: identity` and a JSON data line.
type identitySSEFrame struct {
	Event   string
	Data    string
	Comment string
}

// identitySSEClient reads GET /v1/identity/events without blocking the test.
type identitySSEClient struct {
	t      *testing.T
	resp   *http.Response
	frames chan identitySSEFrame
	done   chan struct{}
	once   sync.Once
}

func openIdentitySSE(t *testing.T, env *identityEnv, account store.AccountRecord, super bool) *identitySSEClient {
	t.Helper()
	headers := map[string]string{"X-Test-Account": account.ID.String()}
	if super {
		headers["X-Test-Super"] = "1"
	}
	return openIdentitySSEAt(t, env.server.URL, headers)
}

func openIdentitySSEAt(t *testing.T, baseURL string, headers map[string]string) *identitySSEClient {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/identity/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		var body strings.Builder
		_, _ = bufio.NewReader(resp.Body).WriteTo(&body)
		resp.Body.Close()
		t.Fatalf("identity stream = %d, want 200; body=%s", resp.StatusCode, body.String())
	}
	client := &identitySSEClient{
		t: t, resp: resp,
		frames: make(chan identitySSEFrame, 16),
		done:   make(chan struct{}),
	}
	go func() {
		defer close(client.frames)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 4096), 1<<20)
		frame := identitySSEFrame{}
		flush := func() {
			if frame.Event == "" && frame.Data == "" && frame.Comment == "" {
				return
			}
			select {
			case client.frames <- frame:
			case <-client.done:
			}
			frame = identitySSEFrame{}
		}
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				flush()
				continue
			}
			if strings.HasPrefix(line, ":") {
				frame.Comment = line
				continue
			}
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				frame.Event = value
			case "data":
				if frame.Data != "" {
					frame.Data += "\n"
				}
				frame.Data += value
			}
		}
	}()
	t.Cleanup(client.close)
	return client
}

func (c *identitySSEClient) close() {
	if c == nil {
		return
	}
	c.once.Do(func() {
		close(c.done)
		_ = c.resp.Body.Close()
	})
}

func (c *identitySSEClient) nextFrame(timeout time.Duration) (identitySSEFrame, bool) {
	select {
	case frame, ok := <-c.frames:
		return frame, ok
	case <-time.After(timeout):
		return identitySSEFrame{}, false
	}
}

// nextEvent skips heartbeat comments until an identity event arrives.
func (c *identitySSEClient) nextEvent(timeout time.Duration) (IdentityEvent, bool) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return IdentityEvent{}, false
		}
		frame, ok := c.nextFrame(remaining)
		if !ok {
			return IdentityEvent{}, false
		}
		if frame.Event != "identity" || frame.Data == "" {
			continue
		}
		var event IdentityEvent
		if err := json.Unmarshal([]byte(frame.Data), &event); err != nil {
			c.t.Fatalf("identity event data is not JSON: %q: %v", frame.Data, err)
		}
		return event, true
	}
}

func TestIdentityEventsHubIsTenantScopedAndBounded(t *testing.T) {
	hub := NewIdentityEvents()
	tenantA, tenantB := uuid.New(), uuid.New()
	subA, err := hub.Subscribe(tenantA)
	if err != nil {
		t.Fatal(err)
	}
	subB, err := hub.Subscribe(tenantB)
	if err != nil {
		t.Fatal(err)
	}
	defer subA.Close()
	defer subB.Close()

	hub.Publish(tenantA, IdentityEventAccounts)
	select {
	case event := <-subA.Events():
		if event.Kind != IdentityEventAccounts {
			t.Fatalf("tenant A kind = %q, want %q", event.Kind, IdentityEventAccounts)
		}
		if event.At.IsZero() {
			t.Fatal("tenant A event has no timestamp")
		}
	case <-time.After(time.Second):
		t.Fatal("tenant A did not receive its own invalidation")
	}
	select {
	case event := <-subB.Events():
		t.Fatalf("tenant B received tenant A's event: %+v", event)
	case <-time.After(80 * time.Millisecond):
	}

	// An unknown kind is a code mistake, not a new wire vocabulary.
	hub.Publish(tenantA, IdentityEventKind("credentials"))
	select {
	case event := <-subA.Events():
		t.Fatalf("unknown kind was published: %+v", event)
	case <-time.After(80 * time.Millisecond):
	}

	// Closing removes the subscriber; the tenant count follows.
	subA.Close()
	subB.Close()
	if got := hub.Subscribers(); got != 0 {
		t.Fatalf("subscribers after close = %d, want 0", got)
	}

	// Bounded: two total and one per tenant. The second tenant has its own slot.
	limit := NewIdentityEvents()
	limit.maxSubs = 2
	limit.maxTenant = 1
	first, err := limit.Subscribe(tenantA)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := limit.Subscribe(tenantA); !errors.Is(err, ErrIdentityEventStreamFull) {
		t.Fatalf("second same-tenant subscribe err = %v, want ErrIdentityEventStreamFull", err)
	}
	second, err := limit.Subscribe(tenantB)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := limit.Subscribe(uuid.New()); !errors.Is(err, ErrIdentityEventStreamFull) {
		t.Fatalf("over-budget subscribe err = %v, want ErrIdentityEventStreamFull", err)
	}
}

func TestIdentityEventStreamIsTenantScopedAtTheRoute(t *testing.T) {
	env := newIdentityEnv(t)
	hub := NewIdentityEvents()
	tenantB := env.otherTenant
	makeStreamServer := func(tenant uuid.UUID) *httptest.Server {
		seam := &fakeAccess{principal: access.Principal{TenantID: tenant, AccountID: env.alice.ID}}
		api := New(Config{Identity: env.st, Access: seam, IdentityEvents: hub, Logger: testLogger()})
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Set("X-Tenant-ID", tenant.String())
			api.ServeHTTP(w, r)
		}))
	}
	serverA := makeStreamServer(env.tenant)
	t.Cleanup(serverA.Close)
	serverB := makeStreamServer(tenantB)
	t.Cleanup(serverB.Close)
	clientA := openIdentitySSEAt(t, serverA.URL, nil)
	clientB := openIdentitySSEAt(t, serverB.URL, nil)

	hub.Publish(env.tenant, IdentityEventAccounts)
	event, ok := clientA.nextEvent(time.Second)
	if !ok {
		t.Fatal("tenant A's subscriber did not receive its own fact")
	}
	if event.Kind != IdentityEventAccounts {
		t.Fatalf("tenant A kind = %q", event.Kind)
	}
	if event, ok := clientB.nextEvent(150 * time.Millisecond); ok {
		t.Fatalf("tenant B's subscriber received tenant A's fact: %+v", event)
	}
	if got := hub.TenantSubscribers(env.tenant); got != 1 {
		t.Fatalf("tenant A subscribers = %d, want 1", got)
	}
	if got := hub.TenantSubscribers(tenantB); got != 1 {
		t.Fatalf("tenant B subscribers = %d, want 1", got)
	}
}

func TestIdentityEventStreamDeliversMutationWithinTwoSeconds(t *testing.T) {
	env := newIdentityEnv(t)
	client := openIdentitySSE(t, env, env.alice, false)

	// A real handler write (permission-group create: one row plus its audit row;
	// no Argon2 hashing to distort the measurement under a loaded test host).
	start := time.Now()
	status, body := env.asAlice(http.MethodPost, "/v1/permission-groups", map[string]any{
		"name": "realtime-latency",
	})
	wantStatus(t, status, body, http.StatusCreated, "create permission group")

	event, ok := client.nextEvent(2 * time.Second)
	if !ok {
		t.Fatal("no identity event within 2s after a real permission-group create")
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("mutation -> event took %s, want <= 2s", elapsed)
	}
	if event.Kind != IdentityEventPermissions {
		t.Fatalf("event kind = %q, want %q", event.Kind, IdentityEventPermissions)
	}
	if event.At.IsZero() {
		t.Fatal("event has no timestamp")
	}
	t.Logf("identity event latency: %s (kind=%s)", elapsed, event.Kind)
}

func TestIdentityEventStreamPublishesOnlySucceededWrites(t *testing.T) {
	env := newIdentityEnv(t)
	// The alice stream represents the tenant's invalidation readers. The limited
	// account's refused write and alice's failed validation must not reach it.
	client := openIdentitySSE(t, env, env.alice, false)

	status, body := env.do(http.MethodPost, "/v1/bots", env.limited, false, map[string]any{
		"botId": "denied-bot", "name": "Denied", "profileId": env.aliceProfile.ID.String(),
		"modelKind":    "text",
		"defaultModel": map[string]any{"providerId": "fake", "modelId": "fake"},
		"harness":      map[string]any{"tools": []string{}, "context": map[string]any{"permissions": []string{}}},
	})
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "limited creates a bot")

	// A failed validation (human without a password) must not publish either.
	status, body = env.asAlice(http.MethodPost, "/v1/accounts", map[string]any{
		"username": "no-password", "displayName": "No", "kind": "human",
	})
	wantStatus(t, status, body, http.StatusBadRequest, "human without password")

	if event, ok := client.nextEvent(250 * time.Millisecond); ok {
		t.Fatalf("refused/failed writes published an event: %+v", event)
	}

	// The stream is still live; a real mutation publishes immediately.
	status, body = env.asAlice(http.MethodPost, "/v1/accounts", map[string]any{
		"username": "after-failure", "displayName": "After", "kind": "human",
		"password": "initial-password-123",
	})
	wantStatus(t, status, body, http.StatusCreated, "create account after refusals")
	event, ok := client.nextEvent(2 * time.Second)
	if !ok {
		t.Fatal("the stream stopped publishing after a refused write")
	}
	if event.Kind != IdentityEventAccounts {
		t.Fatalf("event kind = %q, want %q", event.Kind, IdentityEventAccounts)
	}

	// A store-level failure (duplicate username, 409) must not publish either.
	// A bot account needs no password, so this case does not pay the Argon2 cost
	// the user account above already exercised.
	status, body = env.asAlice(http.MethodPost, "/v1/accounts", map[string]any{
		"username": "after-failure", "displayName": "Duplicate", "kind": "bot",
	})
	wantStatus(t, status, body, http.StatusConflict, "duplicate account")
	if event, ok := client.nextEvent(250 * time.Millisecond); ok {
		t.Fatalf("a failed store write published an event: %+v", event)
	}
}

func TestIdentityEventStreamHeartbeatCleanupAndReconnect(t *testing.T) {
	env := newIdentityEnv(t)
	client := openIdentitySSE(t, env, env.alice, false)

	// The first comment is `: connected`; the periodic keepalive follows. Both
	// prove the connection is alive before any mutation.
	deadline := time.Now().Add(time.Second)
	heartbeatSeen := false
	for time.Now().Before(deadline) {
		frame, ok := client.nextFrame(time.Until(deadline))
		if !ok {
			break
		}
		if strings.Contains(frame.Comment, "heartbeat") {
			heartbeatSeen = true
			break
		}
	}
	if !heartbeatSeen {
		t.Fatal("no heartbeat frame within 1s")
	}

	// Disconnect cleanup: a closed connection leaves no subscriber behind.
	client.close()
	if !waitFor(2*time.Second, func() bool {
		return env.api.identityEvents.TenantSubscribers(env.tenant) == 0
	}) {
		t.Fatalf("subscriber count after disconnect = %d, want 0",
			env.api.identityEvents.TenantSubscribers(env.tenant))
	}

	// A fresh subscriber (the browser's reconnect) receives the next fact.
	reconnected := openIdentitySSE(t, env, env.alice, false)
	status, body := env.asAlice(http.MethodPost, "/v1/accounts", map[string]any{
		"username": "after-reconnect", "displayName": "Reconnect", "kind": "human",
		"password": "initial-password-123",
	})
	wantStatus(t, status, body, http.StatusCreated, "create account after reconnect")
	event, ok := reconnected.nextEvent(2 * time.Second)
	if !ok {
		t.Fatal("reconnected stream received no event")
	}
	if event.Kind != IdentityEventAccounts {
		t.Fatalf("event kind = %q, want %q", event.Kind, IdentityEventAccounts)
	}
}

func waitFor(timeout time.Duration, ready func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ready()
}

func TestIdentityEventStreamPublishesEveryProjection(t *testing.T) {
	env := newIdentityEnv(t)
	sessionID := uuid.New()
	if err := env.st.CreateOwnedSession(context.Background(), store.SessionOwnership{
		TenantID: env.tenant, SessionID: sessionID, BotID: env.aliceBot.ID,
		AccountID: env.alice.ID, CreatedByAccountID: env.alice.ID,
	}); err != nil {
		t.Fatal(err)
	}
	client := openIdentitySSE(t, env, env.alice, false)

	steps := []struct {
		name string
		want IdentityEventKind
		run  func() (int, map[string]any)
	}{
		{
			name: "permission group", want: IdentityEventPermissions,
			run: func() (int, map[string]any) {
				return env.asAlice(http.MethodPost, "/v1/permission-groups", map[string]any{"name": "realtime-group"})
			},
		},
		{
			name: "bot", want: IdentityEventBots,
			run: func() (int, map[string]any) {
				return env.asAlice(http.MethodPost, "/v1/bots", map[string]any{
					"botId": "realtime-bot", "name": "Realtime", "profileId": env.aliceProfile.ID.String(),
					"modelKind":    "text",
					"defaultModel": map[string]any{"providerId": "fake", "modelId": "fake"},
					"harness":      map[string]any{"tools": []string{}, "context": map[string]any{"permissions": []string{}}},
				})
			},
		},
		{
			name: "team bot", want: IdentityEventTeams,
			run: func() (int, map[string]any) {
				return env.asAlice(http.MethodPost, "/v1/team-bots", map[string]any{
					"teamBotId": "realtime-team", "name": "Realtime Team", "mission": "m",
					"roster": []map[string]any{{"botId": env.aliceBot.ID, "role": "captain"}},
				})
			},
		},
		{
			name: "session share", want: IdentityEventSessions,
			run: func() (int, map[string]any) {
				return env.asAlice(http.MethodPost, "/v1/sessions/"+sessionID.String()+"/shares", map[string]any{
					"subjectType": "account", "subjectId": env.bob.ID.String(), "relation": "viewer",
				})
			},
		},
	}
	for _, step := range steps {
		status, body := step.run()
		wantStatus(t, status, body, http.StatusCreated, step.name)
		event, ok := client.nextEvent(2 * time.Second)
		if !ok {
			t.Fatalf("%s: no identity event within 2s", step.name)
		}
		if event.Kind != step.want {
			t.Fatalf("%s: event kind = %q, want %q", step.name, event.Kind, step.want)
		}
	}
}

func TestIdentityEventStreamRequiresAuthenticationAndAppRead(t *testing.T) {
	env := newIdentityEnv(t)

	// No edge principal: the stream answers the same 401 as every identity route.
	unauth := httptest.NewServer(env.api)
	defer unauth.Close()
	req, err := http.NewRequest(http.MethodGet, unauth.URL+"/v1/identity/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", env.tenant.String())
	res, err := unauth.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated stream = %d, want 401", res.StatusCode)
	}

	// Authenticated but without app.read: 403 before the hub is touched.
	denied := &fakeAccess{decisions: map[string]access.Decision{
		access.KeyAppRead: {
			Allow: false, Key: access.KeyAppRead, Reason: access.ReasonMissingGrant,
			Detail: "app.read is not held",
		},
	}}
	api := New(Config{Identity: env.st, Access: denied, Logger: testLogger()})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := auth.Principal{
			Kind: auth.PrincipalHuman, TenantID: env.tenant, AccountID: env.alice.ID,
		}
		r.Header.Set("X-Tenant-ID", env.tenant.String())
		api.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	}))
	defer server.Close()
	req, err = http.NewRequest(http.MethodGet, server.URL+"/v1/identity/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("stream without app.read = %d, want 403", res.StatusCode)
	}
	if api.identityEvents.Subscribers() != 0 {
		t.Fatalf("a denied stream subscribed %d readers", api.identityEvents.Subscribers())
	}
}
