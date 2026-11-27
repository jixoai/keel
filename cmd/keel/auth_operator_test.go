package main

// The serving edge accepts KEEL_ADMIN_KEY only on accessgate ModeOperator routes
// (change keel-plugin-telemetry E2E follow-up). Before this, the edge let the
// key reach only /v1/admin/*, so the documented tenant configuration API
// (PUT /v1/tenants/{id}/config) and /v1/metrics were unreachable with the
// operator credential even though the matrix and the handlers expected it.

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestOperatorAdminRouteFollowsTheAccessMatrix(t *testing.T) {
	tenant := uuid.NewString()
	cases := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodPut, "/v1/tenants/" + tenant + "/config", true},
		{http.MethodGet, "/v1/tenants/" + tenant + "/config", true},
		{http.MethodGet, "/v1/tenants/" + tenant + "/usage", true},
		{http.MethodPost, "/v1/admin/tenants", true},
		{http.MethodGet, "/v1/metrics", true},
		{http.MethodGet, "/v1/sessions", false},
		{http.MethodGet, "/v1/plugins", false},
		{http.MethodPost, "/v1/auth/login", false},
		{http.MethodDelete, "/v1/tenants/" + tenant + "/config", false},
	}
	for _, tc := range cases {
		if got := operatorAdminRoute(tc.method, tc.path); got != tc.want {
			t.Fatalf("operatorAdminRoute(%s %s) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestAdminBearerReachesOnlyOperatorRoutes(t *testing.T) {
	const key = "e2e-operator-key"
	t.Setenv("KEEL_ADMIN_KEY", key)
	controller := &authController{
		cfg:    authConfig{Mode: authModeLocalOnly, TenantID: uuid.New()},
		logger: slog.Default(),
	}
	tenant := uuid.NewString()

	call := func(method, path, token string) (int, bool) {
		t.Helper()
		reached := false
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusNoContent)
		})
		request := httptest.NewRequest(method, path, nil)
		request.RemoteAddr = "127.0.0.1:54321"
		request.Host = "127.0.0.1:54321"
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		recorder := httptest.NewRecorder()
		controller.serve(recorder, request, next)
		return recorder.Code, reached
	}

	if code, reached := call(http.MethodPut, "/v1/tenants/"+tenant+"/config", key); !reached || code != http.StatusNoContent {
		t.Fatalf("tenant config with the operator key = %d, reached=%v; want 204/true", code, reached)
	}
	if code, reached := call(http.MethodGet, "/v1/metrics", key); !reached || code != http.StatusNoContent {
		t.Fatalf("/v1/metrics with the operator key = %d, reached=%v; want 204/true", code, reached)
	}
	if code, reached := call(http.MethodGet, "/v1/sessions", key); reached || code != http.StatusUnauthorized {
		t.Fatalf("non-operator route with the operator key = %d, reached=%v; want 401/false", code, reached)
	}
	if code, reached := call(http.MethodPut, "/v1/tenants/"+tenant+"/config", "wrong-key"); reached || code != http.StatusUnauthorized {
		t.Fatalf("operator route with a wrong key = %d, reached=%v; want 401/false", code, reached)
	}
	if code, reached := call(http.MethodDelete, "/v1/tenants/"+tenant+"/config", key); reached || code != http.StatusUnauthorized {
		t.Fatalf("unlisted method with the operator key = %d, reached=%v; want 401/false", code, reached)
	}
}
