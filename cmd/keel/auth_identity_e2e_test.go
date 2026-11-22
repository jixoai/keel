// Identity and notification journeys over the real auth edge
// (change keel-bot-identity-access A4.2b2).
//
// These tests mount the real product API behind the real auth controller, so the
// edge policy (auth=required, cookie sessions, CSRF, must-change gate) and the
// handler semantics are exercised together. They need KEEL_TEST_DSN and are
// skipped otherwise.
package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/harnessapi"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/notify"
	"github.com/jixoai/keel/internal/store"
)

// okCommitter accepts the notification-ack fact without a kernel: these tests
// exercise the HTTP token authority, not the ledger writer.
type okCommitter struct{}

func (okCommitter) Commit(context.Context, kernel.CommitRequest) (kernel.Receipt, error) {
	return kernel.Receipt{}, nil
}

// TestNotificationTokenRouteClassification pins the exact anonymous allowlist:
// only the two token-addressed notification paths bypass the session edge, and
// only with a UUID-shaped id.
func TestNotificationTokenRouteClassification(t *testing.T) {
	id := uuid.NewString()
	for _, path := range []string{
		"/v1/notifications/" + id + "/open",
		"/v1/notifications/" + id + "/pixel",
	} {
		if !anonymousAuthRoute(path) || !notificationTokenRoute(path) {
			t.Fatalf("%s is not an anonymous token route", path)
		}
	}
	for _, path := range []string{
		"/v1/notifications",
		"/v1/notifications/" + id,
		"/v1/notifications/" + id + "/ack",
		"/v1/notifications/" + id + "/open/",
		"/v1/notifications/not-a-uuid/open",
		"/v1/notifications/" + id + "/opener",
		"/v1/notificationsx/" + id + "/open",
	} {
		if anonymousAuthRoute(path) {
			t.Fatalf("%s unexpectedly bypassed the auth edge", path)
		}
	}
}

// TestAdminPasswordResetE2E pins the A4.2b2 reset semantics end to end: an
// administrator resets a credential over HTTP, the target's old cookie dies,
// the next login is marked must-change, and only a real password change clears
// that state.
func TestAdminPasswordResetE2E(t *testing.T) {
	env := newAuthTestEnvWithProduct(t, authModeRequired, uuid.Nil, func(st *store.Store) http.Handler {
		bridge, err := accesswire.NewBridge(st, discardLogger())
		if err != nil {
			t.Fatalf("access bridge: %v", err)
		}
		return harnessapi.New(harnessapi.Config{Identity: st, Access: bridge, Logger: discardLogger()})
	})
	adminID, csrf := env.bootstrapAdmin("bootstrap-password-1")
	username := "target-" + env.tenantID.String()[:8]

	status, body, _ := env.do(env.client, http.MethodPost, "/v1/accounts", map[string]any{
		"username": username, "displayName": "Target", "kind": "human",
		"password": "target-password-1",
	}, csrf)
	if status != http.StatusCreated {
		t.Fatalf("create target account: %d %v", status, body)
	}
	created, _ := body["account"].(map[string]any)
	targetID, err := uuid.Parse(created["accountId"].(string))
	if err != nil {
		t.Fatalf("target account id: %v", err)
	}

	oldClient := env.newClient()
	if status, body := env.login(oldClient, username, "target-password-1"); status != http.StatusOK {
		t.Fatalf("target login: %d %v", status, body)
	}
	if status, body, _ := env.do(oldClient, http.MethodGet, "/v1/sessions", nil, nil); status != http.StatusOK {
		t.Fatalf("target session before reset: %d %v", status, body)
	}

	status, body, _ = env.do(env.client, http.MethodPost,
		"/v1/accounts/"+targetID.String()+"/password-reset",
		map[string]any{"password": "reset-password-2"}, csrf)
	if status != http.StatusOK {
		t.Fatalf("admin reset: %d %v", status, body)
	}
	if body["mustChangePassword"] != true || body["sessionsRevoked"] != true {
		t.Fatalf("reset response = %v", body)
	}

	// The pre-reset cookie is dead at the edge, not just at the handler.
	if status, body, _ := env.do(oldClient, http.MethodGet, "/v1/sessions", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("old cookie after reset = %d, want 401; body=%v", status, body)
	}

	// The next login succeeds but is restricted to the password-change surface.
	newClient := env.newClient()
	status, loginBody := env.login(newClient, username, "reset-password-2")
	if status != http.StatusOK || loginBody["mustChangePassword"] != true {
		t.Fatalf("login after reset: %d %v", status, loginBody)
	}
	if status, body, _ := env.do(newClient, http.MethodGet, "/v1/sessions", nil, nil); status != http.StatusForbidden || body["code"] != "password_change_required" {
		t.Fatalf("must-change gate = %d %v", status, body)
	}
	status, changed := env.changePassword(newClient, "reset-password-2", "final-password-3")
	if status != http.StatusOK || changed["mustChangePassword"] != false {
		t.Fatalf("forced change: %d %v", status, changed)
	}
	if status, body, _ := env.do(newClient, http.MethodGet, "/v1/sessions", nil, nil); status != http.StatusOK {
		t.Fatalf("target session after change: %d %v", status, body)
	}

	// The audit fact is password.reset (not password.changed) with the admin as
	// actor; the account no longer reports must-change after the real change.
	events, err := env.st.ListAuthEvents(context.Background(), env.tenantID, store.AuthEventFilter{
		AccountID: targetID, Kind: store.AuthEventPasswordReset,
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Result != store.AuthResultOK || events[0].Kind != store.AuthEventPasswordReset {
		t.Fatalf("reset audit rows = %+v", events)
	}
	if events[0].ActorAccountID == nil || *events[0].ActorAccountID != adminID {
		t.Fatalf("reset audit actor = %v, want %s", events[0].ActorAccountID, adminID)
	}
	if detail := events[0].Detail; detail["by"] != "admin" || detail["must_change_password"] != true {
		t.Fatalf("reset audit detail = %v", detail)
	}
	if must, err := env.st.AccountMustChangePassword(context.Background(), env.tenantID, targetID); err != nil || must {
		t.Fatalf("must change after a real change = %v, err=%v", must, err)
	}
}

// TestNotificationTokenRoutesE2E pins the public capability routes at the auth
// edge: no session is needed when the single-use token is valid (and no super
// admin need exist), while missing/wrong tokens answer 401 without revealing
// whether the notification exists.
func TestNotificationTokenRoutesE2E(t *testing.T) {
	var notifier *notify.Service
	env := newAuthTestEnvWithProduct(t, authModeRequired, uuid.Nil, func(st *store.Store) http.Handler {
		notifier = notify.NewService(notify.Options{
			Store: notify.NewPostgresStore(st), Committer: okCommitter{}, Logger: discardLogger(),
			URLTTL: 30 * time.Minute,
		})
		return harnessapi.New(harnessapi.Config{Notifications: notifier, Logger: discardLogger()})
	})
	ctx := context.Background()
	push := func(consumers ...notify.ConsumerSpec) notify.View {
		t.Helper()
		view, err := notifier.Push(ctx, notify.PushRequest{
			TenantID: env.tenantID, SessionID: uuid.New(), Text: "please look",
			Consumers: consumers,
		})
		if err != nil {
			t.Fatal(err)
		}
		return view
	}

	view := push(
		notify.ConsumerSpec{ID: "mail", Kind: notify.ConsumerURL},
		notify.ConsumerSpec{ID: "pixel", Kind: notify.ConsumerImage},
	)
	anonymous := env.newClient()

	// No super admin exists yet: a valid token still opens (the edge must not
	// shadow the capability route with bootstrap_required).
	status, body, _ := env.do(anonymous, http.MethodGet, view.URLs["mail"], nil, nil)
	if status != http.StatusOK || body == nil {
		t.Fatalf("open with a valid token = %d %v", status, body)
	}
	// Wrong/missing tokens are 401, whether or not the notification exists.
	for _, path := range []string{
		"/v1/notifications/" + view.NotificationID + "/open",
		"/v1/notifications/" + view.NotificationID + "/open?token=wrong-token",
		"/v1/notifications/" + uuid.NewString() + "/open?token=wrong-token",
	} {
		status, body, _ = env.do(anonymous, http.MethodGet, path, nil, nil)
		if status != http.StatusUnauthorized || body["code"] != "invalid_token" {
			t.Fatalf("%s = %d %v, want 401 invalid_token", path, status, body)
		}
	}

	// A real super admin makes the edge's ordinary 401 (not 503) observable.
	env.bootstrapAdmin("bootstrap-password-1")
	if status, body, _ := env.do(anonymous, http.MethodGet, "/v1/notifications", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("protected notification list without a session = %d %v, want 401", status, body)
	}

	pixel := push(notify.ConsumerSpec{ID: "pixel2", Kind: notify.ConsumerImage})
	status, body, _ = env.do(anonymous, http.MethodGet, pixel.URLs["pixel2"], nil, nil)
	if status != http.StatusOK || body["tracked"] != true {
		t.Fatalf("pixel with a valid token = %d %v", status, body)
	}
}
