package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"
)

func TestCreateSessionWithBotIDUsesSessionOwner(t *testing.T) {
	var (
		calledTenant  uuid.UUID
		calledSession uuid.UUID
		calledBot     string
	)
	hs, _ := newTestServer(t, func(cfg *Config) {
		cfg.SessionOwner = func(_ context.Context, tenantID, sessionID uuid.UUID, botID string) error {
			calledTenant, calledSession, calledBot = tenantID, sessionID, botID
			return nil
		}
	})
	hc := hs.Client()
	tenant := uuid.NewString()
	if code := createTestTenant(t, hc, hs.URL, tenant, "t"); code != 201 {
		t.Fatalf("create tenant: %d", code)
	}
	var sess struct {
		SessionID string `json:"sessionId"`
		BotID     string `json:"botId"`
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions", tenant,
		map[string]string{"botId": "support-1"}, &sess); code != http.StatusCreated {
		t.Fatalf("bot-owned create: %d", code)
	}
	if calledBot != "support-1" || calledTenant.String() != tenant {
		t.Fatalf("callback args = %s %s %s", calledTenant, calledSession, calledBot)
	}
	if calledSession.String() != sess.SessionID || sess.BotID != "support-1" {
		t.Fatalf("answer = %+v (callback session %s)", sess, calledSession)
	}
}

func TestCreateSessionWithBotIDFailures(t *testing.T) {
	hs, _ := newTestServer(t, func(cfg *Config) {
		cfg.SessionOwner = func(_ context.Context, _, _ uuid.UUID, botID string) error {
			switch botID {
			case "denied":
				return &SessionOwnerError{Status: http.StatusForbidden, Code: "forbidden",
					Message: "forbidden", Reason: "missing_relation"}
			case "missing":
				return store.ErrNotFound
			case "archived":
				return &SessionOwnerError{Status: http.StatusConflict, Code: "bot_archived",
					Message: "an archived bot refuses new sessions"}
			default:
				return nil
			}
		}
	})
	hc := hs.Client()
	tenant := uuid.NewString()
	if code := createTestTenant(t, hc, hs.URL, tenant, "t"); code != 201 {
		t.Fatalf("create tenant: %d", code)
	}
	var denied struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions", tenant,
		map[string]string{"botId": "denied"}, &denied); code != http.StatusForbidden {
		t.Fatalf("denied create: %d", code)
	}
	if denied.Code != "forbidden" || denied.Reason != "missing_relation" {
		t.Fatalf("denied body = %+v", denied)
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions", tenant,
		map[string]string{"botId": "missing"}, nil); code != http.StatusNotFound {
		t.Fatalf("missing bot create: %d", code)
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions", tenant,
		map[string]string{"botId": "archived"}, nil); code != http.StatusConflict {
		t.Fatalf("archived bot create: %d", code)
	}
	// A session that names no bot keeps the pre-A4 unowned path.
	var plain struct {
		SessionID string `json:"sessionId"`
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions", tenant, map[string]string{}, &plain); code != http.StatusCreated {
		t.Fatalf("plain create: %d", code)
	}
}

func TestCreateSessionWithBotIDUnwired(t *testing.T) {
	hs, _ := newTestServer(t)
	hc := hs.Client()
	tenant := uuid.NewString()
	if code := createTestTenant(t, hc, hs.URL, tenant, "t"); code != 201 {
		t.Fatalf("create tenant: %d", code)
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions", tenant,
		map[string]string{"botId": "support-1"}, nil); code != http.StatusNotImplemented {
		t.Fatalf("unwired bot-owned create: %d, want 501", code)
	}
	// The plain path is unaffected by a missing owner seam.
	var plain struct {
		SessionID string `json:"sessionId"`
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions", tenant, map[string]string{}, &plain); code != http.StatusCreated {
		t.Fatalf("plain create: %d", code)
	}
}
