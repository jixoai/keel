package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/kernel"
)

type stubLoadouts struct {
	pin kernel.RunPin
	err error
}

func (s stubLoadouts) RunPin(context.Context, kernel.Scope) (kernel.RunPin, error) {
	if s.err != nil {
		return kernel.RunPin{}, s.err
	}
	return s.pin, nil
}

func doLoadoutRequest(t *testing.T, cfg Config, sessionID, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	srv := New(cfg)
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sessionID+"/run/loadout", nil)
	if tenant != "" {
		req.Header.Set("X-Tenant-ID", tenant)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestRunLoadoutEndpoint(t *testing.T) {
	session := uuid.NewString()
	tenant := uuid.NewString()
	stub := stubLoadouts{
		pin: kernel.RunPin{
			LoadoutID: "default", LoadoutRevision: 7,
			LoadoutGeneration: 3, LoadoutOwnerEpoch: 42,
		},
	}
	rec := doLoadoutRequest(t, Config{Loadouts: stub}, session, tenant)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["loadoutId"] != "default" || body["revision"] != float64(7) ||
		body["generation"] != float64(3) || body["ownerEpoch"] != float64(42) {
		t.Fatalf("body = %v", body)
	}

	// No run/loadout for the session answers 404, not an empty 200.
	rec = doLoadoutRequest(t, Config{Loadouts: stubLoadouts{err: kernel.ErrNoRunnable}}, session, tenant)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing run: status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = doLoadoutRequest(t, Config{Loadouts: stubLoadouts{err: kernel.ErrNotFound}}, session, tenant)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing session: status = %d body=%s", rec.Code, rec.Body.String())
	}

	// The seam is optional: without it the endpoint is explicitly unavailable.
	rec = doLoadoutRequest(t, Config{}, session, tenant)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired: status = %d", rec.Code)
	}

	// Tenant header and session id validation keep the edge contract.
	rec = doLoadoutRequest(t, Config{Loadouts: stub}, session, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing tenant: status = %d", rec.Code)
	}
	rec = doLoadoutRequest(t, Config{Loadouts: stub}, "not-a-uuid", tenant)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad session id: status = %d", rec.Code)
	}
}
