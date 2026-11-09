package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"
)

// committedUserMessage returns the stored payload of the committed
// user.message carrying requestID. The events table stores jsonb, so the
// read-back is the canonical form of what the edge wrote (Postgres decides key
// order and spacing); the assertion is on the content, not the bytes.
func committedUserMessage(t *testing.T, st *store.Store, tenant, session uuid.UUID, requestID string) (map[string]json.RawMessage, bool) {
	t.Helper()
	events, err := st.LoadEvents(context.Background(), tenant, session, 0, 100)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	for _, ev := range events {
		if ev.Kind != store.EventUserMessage {
			continue
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("decode user message: %v", err)
		}
		var got string
		_ = json.Unmarshal(payload["requestId"], &got)
		if got == requestID {
			return payload, true
		}
	}
	return nil, false
}

// TestInputSelectionPersistsVerbatim pins the ledger rule: a vetted selection
// is stored with the input exactly as sent — including fields this server's
// own types do not know about — while an absent selection leaves the payload
// without the key (old clients keep their byte-identical shape).
func TestInputSelectionPersistsVerbatim(t *testing.T) {
	hs, st := newTestServer(t, func(c *Config) {
		// The product-layer stand-in: it knows providerId/modelId and nothing
		// else, like a validator backed by the resolved catalog.
		c.ValidateSelection = func(raw json.RawMessage) error {
			var sel struct {
				ProviderID string `json:"providerId"`
				ModelID    string `json:"modelId"`
			}
			if err := json.Unmarshal(raw, &sel); err != nil {
				return err
			}
			if sel.ProviderID == "" || sel.ModelID == "" {
				return errors.New("selection requires providerId and modelId")
			}
			return nil
		}
	})
	hc := hs.Client()
	tenant := uuid.NewString()
	session := inputSetup(t, hs, tenant)
	tenantID := uuid.MustParse(tenant)
	url := hs.URL + "/v1/sessions/" + session.String() + "/input"

	// futureOption is a field no Go struct in this server models: a re-encode
	// through the product vocabulary would drop it.
	sent := `{"providerId":"s2api","modelId":"deepseek-v4.1-flash","reasoningLevel":"high","futureOption":{"b":1,"a":2}}`
	body := map[string]any{
		"text":      "hello",
		"requestId": "sel-verbatim",
		"selection": json.RawMessage(sent),
	}
	if code := doJSON(t, hc, "POST", url, tenant, body, nil); code != 201 {
		t.Fatalf("selection input = %d, want 201", code)
	}
	payload, ok := committedUserMessage(t, st, tenantID, session, "sel-verbatim")
	if !ok {
		t.Fatal("no committed user.message for the selection input")
	}
	var stored, want map[string]any
	if err := json.Unmarshal(payload["selection"], &stored); err != nil {
		t.Fatalf("stored selection: %v", err)
	}
	if err := json.Unmarshal([]byte(sent), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, want) {
		t.Fatalf("stored selection = %#v, want %#v", stored, want)
	}

	// A selection rides along with the delivery fact: the downgrade rule still
	// commits guide-as-queue, and the selection is stored either way.
	body = map[string]any{
		"text":      "steer",
		"requestId": "sel-guide",
		"delivery":  "guide",
		"selection": map[string]any{"providerId": "a6api", "modelId": "deepseek-v4.1-flash"},
	}
	if code := doJSON(t, hc, "POST", url, tenant, body, nil); code != 201 {
		t.Fatalf("selection+guide input = %d, want 201", code)
	}
	payload, ok = committedUserMessage(t, st, tenantID, session, "sel-guide")
	if !ok {
		t.Fatal("no committed user.message for the selection+guide input")
	}
	var delivery string
	if err := json.Unmarshal(payload["delivery"], &delivery); err != nil || delivery != "queue" {
		t.Fatalf("downgraded delivery = %q (err %v), want queue", delivery, err)
	}
	if _, present := payload["selection"]; !present {
		t.Fatal("selection was dropped from the downgraded guide input")
	}

	// No selection at all: the payload carries no key, exactly as before this
	// field existed (the idempotent-retry contract depends on it).
	body = map[string]any{"text": "plain", "requestId": "sel-absent"}
	if code := doJSON(t, hc, "POST", url, tenant, body, nil); code != 201 {
		t.Fatalf("plain input = %d, want 201", code)
	}
	payload, ok = committedUserMessage(t, st, tenantID, session, "sel-absent")
	if !ok {
		t.Fatal("no committed user.message for the plain input")
	}
	if _, present := payload["selection"]; present {
		t.Fatalf("absent selection was injected into the payload: %s", payload["selection"])
	}
}

// TestInputSelectionFailClosed: every selection the server cannot vet is a 400
// the moment it arrives, and nothing reaches the log.
func TestInputSelectionFailClosed(t *testing.T) {
	hs, st := newTestServer(t, func(c *Config) {
		c.ValidateSelection = func(raw json.RawMessage) error {
			var sel struct {
				ModelID string `json:"modelId"`
			}
			if err := json.Unmarshal(raw, &sel); err != nil {
				return err
			}
			if sel.ModelID == "" {
				return errors.New(`selection "modelId" is required`)
			}
			return nil
		}
	})
	hc := hs.Client()
	tenant := uuid.NewString()
	session := inputSetup(t, hs, tenant)
	tenantID := uuid.MustParse(tenant)
	url := hs.URL + "/v1/sessions/" + session.String() + "/input"

	cases := []struct {
		name     string
		request  map[string]any
		contains string
	}{
		{
			"validator rejects",
			map[string]any{"text": "hi", "requestId": "sel-invalid", "selection": map[string]any{"providerId": "s2api"}},
			`selection "modelId" is required`,
		},
		{
			"string is not an object",
			map[string]any{"text": "hi", "requestId": "sel-string", "selection": "s2api/deepseek"},
			"selection must be a JSON object",
		},
		{
			"array is not an object",
			map[string]any{"text": "hi", "requestId": "sel-array", "selection": []string{"s2api"}},
			"selection must be a JSON object",
		},
		{
			"null is not an object",
			map[string]any{"text": "hi", "requestId": "sel-null", "selection": nil},
			"selection must be a JSON object",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out struct {
				Error string `json:"error"`
			}
			if code := doJSON(t, hc, "POST", url, tenant, tc.request, &out); code != 400 {
				t.Fatalf("status = %d, want 400", code)
			}
			if !strings.Contains(out.Error, tc.contains) {
				t.Fatalf("error = %q, want it to contain %q", out.Error, tc.contains)
			}
		})
	}

	// Refusal happens before the commit: none of the refused request ids is in
	// the log.
	events, err := st.LoadEvents(context.Background(), tenantID, session, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Kind != store.EventUserMessage {
			continue
		}
		var payload struct {
			RequestID string `json:"requestId"`
		}
		_ = json.Unmarshal(ev.Payload, &payload)
		if strings.HasPrefix(payload.RequestID, "sel-") {
			t.Fatalf("a refused selection reached the log: %s", payload.RequestID)
		}
	}
}

// TestInputSelectionRefusedWithoutValidator: the shell with no wired product
// validator accepts nothing but absent fields — it must not store a selection
// it cannot honour.
func TestInputSelectionRefusedWithoutValidator(t *testing.T) {
	hs, st := newTestServer(t)
	hc := hs.Client()
	tenant := uuid.NewString()
	session := inputSetup(t, hs, tenant)
	tenantID := uuid.MustParse(tenant)

	var out struct {
		Error string `json:"error"`
	}
	body := map[string]any{"text": "hi", "requestId": "sel-unwired",
		"selection": map[string]any{"providerId": "s2api", "modelId": "deepseek-v4.1-flash"}}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+session.String()+"/input", tenant, body, &out); code != 400 {
		t.Fatalf("unwired selection = %d, want 400", code)
	}
	if !strings.Contains(out.Error, "selection is not supported by this server") {
		t.Fatalf("error = %q", out.Error)
	}
	events, err := st.LoadEvents(context.Background(), tenantID, session, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		var payload struct {
			RequestID string `json:"requestId"`
		}
		_ = json.Unmarshal(ev.Payload, &payload)
		if payload.RequestID == "sel-unwired" {
			t.Fatal("the refused selection reached the log")
		}
	}
}
