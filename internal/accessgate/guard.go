package accessgate

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
)

// Access is the authorization seam the gate needs. *accesswire.Bridge
// implements it; a test supplies its own. A nil Access makes the gate answer
// 503 on every non-public route (fail closed).
type Access interface {
	Principal(ctx context.Context, tenantID uuid.UUID) (access.Principal, error)
	Authorize(ctx context.Context, principal access.Principal, key string, resource access.Resource) (access.Decision, error)
	Effective(ctx context.Context, principal access.Principal) (access.Grants, error)
	Visible(ctx context.Context, principal access.Principal, key, resourceType string, candidateIDs []string) (access.VisibleResult, error)
	Denied(ctx context.Context, principal access.Principal, decision access.Decision, resource access.Resource)
}

// Gate is the default-deny HTTP middleware. It runs after the auth middleware
// has resolved the caller and before the route's handler, so no route can be
// reached without its declared decision.
type Gate struct {
	access Access
	next   http.Handler
	logger *slog.Logger
}

// New builds the gate around next.
func New(accessSeam Access, next http.Handler, logger *slog.Logger) *Gate {
	if logger == nil {
		logger = slog.Default()
	}
	return &Gate{access: accessSeam, next: next, logger: logger}
}

// ServeHTTP implements http.Handler.
func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	// /v1 and /v1/ are not product routes; the concrete paths are.
	if path == "/v1" || path == "/v1/" || !strings.HasPrefix(path, "/v1/") {
		g.next.ServeHTTP(w, r)
		return
	}
	match, ok := Match(r.Method, path)
	if !ok {
		// Unlisted path: fail closed without pretending a route exists. A
		// registered-but-unlisted route also lands here at runtime; the boot
		// coverage check (CheckRoutes) is what refuses to start.
		writeGateJSON(w, http.StatusNotFound, map[string]any{
			"error": "route not found", "code": "not_found",
		})
		return
	}
	rule := match.Rule
	if rule.Mode == ModePublic || rule.Mode == ModeOperator {
		g.next.ServeHTTP(w, r)
		return
	}
	if g.access == nil {
		writeGateJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "the authorization surface is not wired in this deployment",
			"code":  "access_unwired",
		})
		return
	}
	tenantID, ok := gateTenant(w, r)
	if !ok {
		return
	}
	principal, err := g.access.Principal(r.Context(), tenantID)
	if err != nil {
		writeGateJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "authentication required", "code": "invalid_session",
		})
		return
	}
	switch rule.Mode {
	case ModeKey:
		g.decide(w, r, principal, tenantID, rule.Key, access.Resource{TenantID: tenantID})
	case ModeHeldKey:
		g.requireHeld(w, r, principal, tenantID, rule.Key)
	case ModeTenantKey:
		g.requireTenantScope(w, r, principal, tenantID, rule.Key)
	case ModeResourceKey:
		resourceID, ok := g.resourceID(w, r, match)
		if !ok {
			return
		}
		resource := access.Resource{TenantID: tenantID, Type: rule.ResourceType, ID: resourceID}
		g.decide(w, r, principal, tenantID, rule.Key, resource)
	default:
		// Validate() refuses unknown modes at init; this is unreachable unless a
		// future mode is added without a gate branch. Fail closed.
		g.logger.Error("accessgate: unhandled mode", "mode", rule.Mode, "route", rule.String())
		writeGateJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "route permission mode is not implemented", "code": "access_misconfigured",
		})
	}
}

// decide runs the engine decision and writes the structured 403 on denial.
func (g *Gate) decide(w http.ResponseWriter, r *http.Request, principal access.Principal, tenantID uuid.UUID, key string, resource access.Resource) {
	decision, err := g.access.Authorize(r.Context(), principal, key, resource)
	if err != nil {
		writeGateJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "authorization is unavailable",
		})
		return
	}
	if decision.Denied() {
		WriteDenied(w, decision)
		return
	}
	g.next.ServeHTTP(w, r)
}

// requireHeld enforces a resource-scoped key at a collection/create edge: the
// key must be held at some scope, but there is no resource row yet to attach a
// relation to. The owner relation written by the create is what later
// single-resource calls use.
func (g *Gate) requireHeld(w http.ResponseWriter, r *http.Request, principal access.Principal, tenantID uuid.UUID, key string) {
	effective, err := g.access.Effective(r.Context(), principal)
	if err != nil {
		writeGateJSON(w, http.StatusInternalServerError, map[string]any{"error": "authorization is unavailable"})
		return
	}
	if _, held := effective.ScopeOf(key); held {
		g.next.ServeHTTP(w, r)
		return
	}
	decision := access.Decision{
		Allow: false, Reason: access.ReasonMissingGrant, Key: key,
		Detail: "the principal does not hold the collection key",
	}
	g.access.Denied(r.Context(), principal, decision, access.Resource{TenantID: tenantID})
	WriteDenied(w, decision)
}

// requireTenantScope enforces a resource-scoped key on a tenant-global
// resource (triggers). own/team cannot reach a tenant-global registration.
func (g *Gate) requireTenantScope(w http.ResponseWriter, r *http.Request, principal access.Principal, tenantID uuid.UUID, key string) {
	effective, err := g.access.Effective(r.Context(), principal)
	if err != nil {
		writeGateJSON(w, http.StatusInternalServerError, map[string]any{"error": "authorization is unavailable"})
		return
	}
	scope, held := effective.ScopeOf(key)
	if held && (scope == access.ScopeTenant || scope == access.ScopeAll) {
		g.next.ServeHTTP(w, r)
		return
	}
	detail := "the principal does not hold the tenant-scoped key"
	if held {
		detail = "scope " + string(scope) + " does not reach a tenant-global resource"
	}
	decision := access.Decision{
		Allow: false, Reason: access.ReasonMissingGrant, Key: key, Scope: scope, Detail: detail,
	}
	g.access.Denied(r.Context(), principal, decision, access.Resource{TenantID: tenantID})
	WriteDenied(w, decision)
}

// resourceID extracts the id declared by the rule (path or query).
func (g *Gate) resourceID(w http.ResponseWriter, r *http.Request, match MatchResult) (string, bool) {
	prefix, name, _ := strings.Cut(match.Rule.ResourceParam, ":")
	var id string
	switch prefix {
	case "path":
		id = strings.TrimSpace(match.Params[name])
	case "query":
		id = strings.TrimSpace(r.URL.Query().Get(name))
	}
	if id == "" {
		writeGateJSON(w, http.StatusBadRequest, map[string]any{
			"error": "the route requires " + match.Rule.ResourceParam, "code": "invalid_request",
		})
		return "", false
	}
	return id, true
}

func gateTenant(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := strings.TrimSpace(r.Header.Get("X-Tenant-ID"))
	if raw == "" {
		writeGateJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "X-Tenant-ID is required", "code": "invalid_session",
		})
		return uuid.Nil, false
	}
	tenantID, err := uuid.Parse(raw)
	if err != nil {
		writeGateJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "X-Tenant-ID is not a UUID", "code": "invalid_session",
		})
		return uuid.Nil, false
	}
	return tenantID, true
}

func WriteDenied(w http.ResponseWriter, decision access.Decision) {
	body := map[string]any{
		"error":       "forbidden",
		"code":        "forbidden",
		"reason":      decision.Reason.String(),
		"requiredKey": decision.Key,
	}
	if decision.Scope != "" {
		body["scope"] = string(decision.Scope)
	}
	writeGateJSON(w, http.StatusForbidden, body)
}

func writeGateJSON(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}
