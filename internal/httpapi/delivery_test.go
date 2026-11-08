package httpapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

// committedDelivery reads the delivery fact of a committed user.message. An
// absent field (old clients) comes back as "".
func committedDelivery(t *testing.T, st *store.Store, tenant, session uuid.UUID, requestID string) string {
	t.Helper()
	events, err := st.LoadEvents(context.Background(), tenant, session, 0, 100)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	for _, ev := range events {
		if ev.Kind != store.EventUserMessage {
			continue
		}
		var payload struct {
			RequestID string `json:"requestId"`
			Delivery  string `json:"delivery"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("decode user message: %v", err)
		}
		if payload.RequestID == requestID {
			return payload.Delivery
		}
	}
	t.Fatalf("no committed user.message with requestId %q", requestID)
	return ""
}

// TestInputDeliveryDowngradesWithoutALiveAttempt pins the downgrade rule at the
// edge: a guide input only stays a guide while an attempt is actually running.
// Without one it is committed as queue input, so the event itself carries the
// effective mode (nothing is downgraded silently).
func TestInputDeliveryDowngradesWithoutALiveAttempt(t *testing.T) {
	hs, st := newTestServer(t)
	hc := hs.Client()
	tenant := uuid.NewString()
	session := inputSetup(t, hs, tenant)
	tenantID := uuid.MustParse(tenant)

	post := func(requestID, delivery string) int {
		body := map[string]string{"text": "hello", "requestId": requestID}
		if delivery != "" {
			body["delivery"] = delivery
		}
		return doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+session.String()+"/input", tenant, body, nil)
	}

	// No run row at all: nothing is running, so the guide becomes queue input.
	if code := post("guide-idle", "guide"); code != 201 {
		t.Fatalf("idle guide input = %d", code)
	}
	if got := committedDelivery(t, st, tenantID, session, "guide-idle"); got != kernel.DeliveryQueue {
		t.Fatalf("idle guide committed as %q, want %q", got, kernel.DeliveryQueue)
	}

	// An explicit queue is kept as such.
	if code := post("explicit-queue", "queue"); code != 201 {
		t.Fatalf("explicit queue input = %d", code)
	}
	if got := committedDelivery(t, st, tenantID, session, "explicit-queue"); got != kernel.DeliveryQueue {
		t.Fatalf("explicit queue committed as %q", got)
	}

	// No delivery field at all: the payload stays byte-identical to the
	// pre-delivery contract (a client's idempotent retry must not conflict).
	if code := post("absent", ""); code != 201 {
		t.Fatalf("absent delivery input = %d", code)
	}
	if got := committedDelivery(t, st, tenantID, session, "absent"); got != "" {
		t.Fatalf("absent delivery committed as %q, want no field", got)
	}

	// A live attempt: the guide stays a guide.
	ctx := context.Background()
	pg, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	k := kernel.New(pg, kernel.Options{})
	lease, err := k.ClaimScope(ctx, kernel.Scope{TenantID: tenantID, SessionID: session}, "delivery-test", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	defer func() {
		if err := k.Release(context.Background(), lease); err != nil {
			t.Logf("release: %v", err)
		}
	}()
	if code := post("guide-live", "guide"); code != 201 {
		t.Fatalf("live guide input = %d", code)
	}
	if got := committedDelivery(t, st, tenantID, session, "guide-live"); got != kernel.DeliveryGuide {
		t.Fatalf("live guide committed as %q, want %q", got, kernel.DeliveryGuide)
	}

	// An unknown mode is refused before anything is written.
	if code := post("bogus", "steer"); code != 400 {
		t.Fatalf("bogus delivery = %d, want 400", code)
	}
	events, err := st.LoadEvents(ctx, tenantID, session, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		var payload struct {
			RequestID string `json:"requestId"`
		}
		_ = json.Unmarshal(ev.Payload, &payload)
		if payload.RequestID == "bogus" {
			t.Fatal("the refused delivery reached the log")
		}
	}
}
