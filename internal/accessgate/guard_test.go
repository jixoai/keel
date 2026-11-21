package accessgate_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accessgate"
)

type call struct {
	key      string
	resource access.Resource
}

// fakeSeam answers from explicit allow sets; everything else is denied. It
// records every decision input so the test can assert the resource id came from
// the path or query parameter.
type fakeSeam struct {
	principal access.Principal
	grants    access.Grants
	allowKeys map[string]bool
	calls     []call
	denials   int
}

func (f *fakeSeam) Principal(context.Context, uuid.UUID) (access.Principal, error) {
	return f.principal, nil
}

func (f *fakeSeam) Authorize(_ context.Context, _ access.Principal, key string, resource access.Resource) (access.Decision, error) {
	f.calls = append(f.calls, call{key: key, resource: resource})
	if f.allowKeys[key] {
		return access.Decision{Allow: true, Reason: access.ReasonAllow, Key: key, Scope: access.ScopeTenant}, nil
	}
	return access.Decision{Allow: false, Reason: access.ReasonMissingGrant, Key: key}, nil
}

func (f *fakeSeam) Effective(context.Context, access.Principal) (access.Grants, error) {
	return f.grants, nil
}

func (f *fakeSeam) Visible(_ context.Context, _ access.Principal, key, _ string, candidates []string) (access.VisibleResult, error) {
	allow := f.allowKeys[key] || f.grants != nil && hasKey(f.grants, key)
	result := access.VisibleResult{IDs: nil, Scope: access.ScopeTenant}
	if allow {
		result.IDs = candidates
		result.Decision = access.Decision{Allow: true, Reason: access.ReasonAllow, Key: key}
	} else {
		result.Decision = access.Decision{Allow: false, Reason: access.ReasonMissingGrant, Key: key}
	}
	return result, nil
}

func (f *fakeSeam) Denied(context.Context, access.Principal, access.Decision, access.Resource) {
	f.denials++
}

func hasKey(grants access.Grants, key string) bool {
	_, ok := grants[key]
	return ok
}

func newGate(seam accessgate.Access) http.Handler {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	return accessgate.New(seam, next, nil)
}

func do(t *testing.T, handler http.Handler, method, path string, tenant uuid.UUID, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if tenant != uuid.Nil {
		req.Header.Set("X-Tenant-ID", tenant.String())
	}
	for name, values := range header {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func tenantPrincipal() (uuid.UUID, access.Principal) {
	tenant := uuid.New()
	return tenant, access.Principal{TenantID: tenant, AccountID: uuid.New()}
}

func TestGatePublicAndUnknownPaths(t *testing.T) {
	handler := newGate(nil)
	if rec := do(t, handler, http.MethodGet, "/v1/health", uuid.Nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("health: %d", rec.Code)
	}
	if rec := do(t, handler, http.MethodGet, "/v1/ready", uuid.Nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("ready: %d", rec.Code)
	}
	// The MCP OAuth callback is registered method-less and is public: the signed
	// state is the authority.
	if rec := do(t, handler, http.MethodGet, "/v1/mcp/callback", uuid.Nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("mcp callback: %d", rec.Code)
	}
	// An unlisted path is fail-closed, not delegated to the product mux.
	if rec := do(t, handler, http.MethodGet, "/v1/no-such-route", uuid.Nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown route: %d", rec.Code)
	}
	if rec := do(t, handler, http.MethodPost, "/v1/admin/tenants", uuid.Nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("operator route without a session: %d", rec.Code)
	}
}

func TestGateRequiresTenantAndAccess(t *testing.T) {
	_, principal := tenantPrincipal()
	seam := &fakeSeam{principal: principal, allowKeys: map[string]bool{access.KeyAppRead: true}}
	handler := newGate(seam)
	if rec := do(t, handler, http.MethodGet, "/v1/capabilities", uuid.Nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing tenant: %d", rec.Code)
	}
	if rec := do(t, newGate(nil), http.MethodGet, "/v1/capabilities", principal.TenantID, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired access: %d", rec.Code)
	}
}

func TestGateTenantKey(t *testing.T) {
	tenant, principal := tenantPrincipal()
	seam := &fakeSeam{principal: principal, allowKeys: map[string]bool{access.KeyAppRead: true}}
	handler := newGate(seam)
	if rec := do(t, handler, http.MethodGet, "/v1/capabilities", tenant, nil); rec.Code != http.StatusOK {
		t.Fatalf("capabilities allow: %d", rec.Code)
	}
	if len(seam.calls) != 1 || seam.calls[0].key != access.KeyAppRead || seam.calls[0].resource.TenantID != tenant {
		t.Fatalf("decision input = %+v", seam.calls)
	}

	zero := &fakeSeam{principal: principal}
	rec := do(t, newGate(zero), http.MethodPut, "/v1/settings/secrets/OPENAI_KEY", tenant, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("zero grant secret write: %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["requiredKey"] != access.KeySecretWrite || body["reason"] != string(access.ReasonMissingGrant) {
		t.Fatalf("denial body = %v", body)
	}
	if zero.denials != 0 {
		// Authorize already wrote the denial; the gate must not double-audit.
		t.Fatalf("gate audited a handler-level denial twice")
	}
}

func TestGateHeldKey(t *testing.T) {
	tenant, principal := tenantPrincipal()
	seam := &fakeSeam{
		principal: principal,
		grants:    access.Grants{access.KeySessionRead: access.ScopeOwn},
	}
	handler := newGate(seam)
	if rec := do(t, handler, http.MethodGet, "/v1/sessions", tenant, nil); rec.Code != http.StatusOK {
		t.Fatalf("session list with own scope: %d", rec.Code)
	}
	rec := do(t, handler, http.MethodPost, "/v1/sessions", tenant, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("session create without session.write: %d", rec.Code)
	}
}

func TestGateTenantScopedTriggerKey(t *testing.T) {
	tenant, principal := tenantPrincipal()
	own := &fakeSeam{principal: principal, grants: access.Grants{access.KeyTriggerRead: access.ScopeOwn}}
	if rec := do(t, newGate(own), http.MethodGet, "/v1/triggers", tenant, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("own-scoped trigger read: %d", rec.Code)
	}
	tenantScoped := &fakeSeam{principal: principal, grants: access.Grants{access.KeyTriggerRead: access.ScopeTenant}}
	if rec := do(t, newGate(tenantScoped), http.MethodGet, "/v1/triggers", tenant, nil); rec.Code != http.StatusOK {
		t.Fatalf("tenant-scoped trigger read: %d", rec.Code)
	}
}

func TestGateResourceKeyExtractsID(t *testing.T) {
	tenant, principal := tenantPrincipal()
	sessionID := uuid.New()
	seam := &fakeSeam{principal: principal, allowKeys: map[string]bool{access.KeySessionRead: true}}
	handler := newGate(seam)
	if rec := do(t, handler, http.MethodGet, "/v1/sessions/"+sessionID.String()+"/events", tenant, nil); rec.Code != http.StatusOK {
		t.Fatalf("events allow: %d", rec.Code)
	}
	if len(seam.calls) != 1 {
		t.Fatalf("decisions = %+v", seam.calls)
	}
	resource := seam.calls[0].resource
	if resource.Type != "session" || resource.ID != sessionID.String() || resource.TenantID != tenant {
		t.Fatalf("resource = %+v", resource)
	}

	// A query-parameter resource (realtime bridge) is extracted the same way.
	realtime := &fakeSeam{principal: principal, allowKeys: map[string]bool{access.KeySessionRead: true}}
	if rec := do(t, newGate(realtime), http.MethodGet, "/v1/realtime?session_id="+sessionID.String(), tenant, nil); rec.Code != http.StatusOK {
		t.Fatalf("realtime allow: %d", rec.Code)
	}
	if realtime.calls[0].resource.ID != sessionID.String() {
		t.Fatalf("realtime resource = %+v", realtime.calls[0].resource)
	}
	missing := &fakeSeam{principal: principal, allowKeys: map[string]bool{access.KeySessionRead: true}}
	if rec := do(t, newGate(missing), http.MethodGet, "/v1/realtime", tenant, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("realtime without session_id: %d", rec.Code)
	}
}

func TestGateResourceDeniedForForeignSession(t *testing.T) {
	tenant, principal := tenantPrincipal()
	sessionID := uuid.New()
	seam := &fakeSeam{principal: principal, allowKeys: map[string]bool{}}
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/v1/sessions/" + sessionID.String() + "/events"},
		{http.MethodGet, "/v1/sessions/" + sessionID.String() + "/events/stream"},
		{http.MethodPost, "/v1/sessions/" + sessionID.String() + "/input"},
		{http.MethodPost, "/v1/sessions/" + sessionID.String() + "/cancel"},
		{http.MethodDelete, "/v1/sessions/" + sessionID.String()},
		{http.MethodPost, "/v1/sessions/" + sessionID.String() + "/fork"},
	} {
		rec := do(t, newGate(seam), probe.method, probe.path, tenant, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s: %d, want 403", probe.method, probe.path, rec.Code)
		}
	}
}

func TestMatrixValidate(t *testing.T) {
	if err := accessgate.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRoutesRefusesUndeclaredRoute(t *testing.T) {
	if err := accessgate.CheckRoutes([]string{"GET /v1/unlisted"}); err == nil {
		t.Fatal("CheckRoutes accepted a route with no declaration")
	}
	// The product subtree mount is the one /v1 pattern exempt from a rule: its
	// concrete inner routes are enumerated separately.
	if err := accessgate.CheckRoutes([]string{"/v1/"}); err != nil {
		t.Fatalf("delegated mount: %v", err)
	}
}

// TestBotOperatorPresetSatisfiesTriggerRoute joins the preset and the route:
// bot-operator's trigger grants must reach tenant scope, because GET
// /v1/triggers is a tenant-global route (no relation tuples to authorize).
func TestBotOperatorPresetSatisfiesTriggerRoute(t *testing.T) {
	preset, ok := access.LookupBuiltinGroup("bot-operator")
	if !ok {
		t.Fatal("bot-operator preset missing")
	}
	grants := access.Grants{}
	for _, grant := range preset.Grants {
		if grant.Key == access.KeyTriggerRead || grant.Key == access.KeyTriggerManage {
			grants[grant.Key] = grant.Scope
		}
	}
	tenant, principal := tenantPrincipal()
	seam := &fakeSeam{principal: principal, grants: grants}
	if rec := do(t, newGate(seam), http.MethodGet, "/v1/triggers", tenant, nil); rec.Code != http.StatusOK {
		t.Fatalf("bot-operator GET /v1/triggers: %d, want 200", rec.Code)
	}
}
