package agentapi

// MCP Apps HTTP surface (change feat-mcp-apps-events, P2.4): the server list,
// the app catalogue and the app document host. The data comes from the host's
// mcphost.Registry (the single machine-readable source of the binding); this
// file owns the HTTP contract only:
//
//   - every route requires the tenant header, and the tenant boundary is
//     checked before any catalogue or document byte is read (`tenantFrom`, the
//     same predicate the rest of the product API uses);
//   - an unknown appId answers 404 with a generic body: it must not be a probe
//     for the server list;
//   - the document response carries the four security headers the spec
//     requires (CSP, nosniff, Referrer-Policy, frame-ancestors inside the CSP)
//     plus an immutable cache identity keyed on the content hash;
//   - a deployment with no registry answers 501 explicitly, so a client can
//     downgrade instead of guessing.

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/mcphost"
)

// MCPAppsSurface is the read model the app routes consume. *mcphost.Registry
// implements it; tests substitute a fake.
type MCPAppsSurface interface {
	// Visible reports whether the request's tenant may read this catalogue.
	Visible(tenantID uuid.UUID) bool
	// Surface returns the published catalogue (ok=false before the first
	// publish).
	Surface() (mcphost.Surface, bool)
	// Document returns one published app document by app identity; an empty
	// hash selects the current version and a non-empty one must match it.
	Document(appID, hash string) (mcphost.Document, bool)
	// Diagnostics returns the recent degradations.
	Diagnostics() []mcphost.Diagnostic
	// Limits reports the byte bounds the surface enforces (one document, the
	// whole cache), so the catalogue serves the numbers it applies.
	Limits() (documentBytes, cacheBytes int64)
}

func (s *Server) mcpAppRoutes() {
	s.mux.HandleFunc("GET /v1/mcp/servers", s.listMCPServers)
	s.mux.HandleFunc("GET /v1/mcp/apps", s.listMCPApps)
	s.mux.HandleFunc("GET /v1/mcp/apps/{appId}/view", s.mcpAppView)
}

// mcpServersResponse is GET /v1/mcp/servers.
type mcpServersResponse struct {
	Generation  int64                 `json:"generation"`
	Servers     []mcphost.ServerState `json:"servers"`
	Diagnostics []mcphost.Diagnostic  `json:"diagnostics,omitempty"`
}

// listMCPServers answers the MCP fleet projection: connection state, protocol
// version/era, capabilities, tool and app counts, and the recent diagnostics.
func (s *Server) listMCPServers(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.MCPApps == nil {
		writeErr(w, http.StatusNotImplemented, errMCPAppsUnwired)
		return
	}
	body := mcpServersResponse{Servers: []mcphost.ServerState{}}
	if s.cfg.MCPApps.Visible(tenantID) {
		if surface, published := s.cfg.MCPApps.Surface(); published {
			body.Generation = surface.Generation
			if surface.Servers != nil {
				body.Servers = surface.Servers
			}
			body.Diagnostics = s.cfg.MCPApps.Diagnostics()
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// mcpAppsResponse is GET /v1/mcp/apps: the app catalogue plus the two
// deployment inputs a View needs (the optional sandbox origin and the host
// theme palettes mapped onto the apps standard variables).
type mcpAppsResponse struct {
	Generation    int64                        `json:"generation"`
	Apps          []mcphost.AppSpec            `json:"apps"`
	Diagnostics   []mcphost.Diagnostic         `json:"diagnostics,omitempty"`
	SandboxOrigin string                       `json:"sandboxOrigin,omitempty"`
	Theme         map[string]map[string]string `json:"theme,omitempty"`
	Limits        map[string]int               `json:"limits,omitempty"`
}

// listMCPApps answers the app catalogue. The catalogue is the single source of
// the tool→app binding: clients must not recompute it from tool results.
func (s *Server) listMCPApps(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.MCPApps == nil {
		writeErr(w, http.StatusNotImplemented, errMCPAppsUnwired)
		return
	}
	documentBytes, cacheBytes := s.cfg.MCPApps.Limits()
	body := mcpAppsResponse{
		Apps: []mcphost.AppSpec{},
		Limits: map[string]int{
			"documentBytes": int(documentBytes),
			"cacheBytes":    int(cacheBytes),
		},
	}
	if s.cfg.MCPApps.Visible(tenantID) {
		if surface, published := s.cfg.MCPApps.Surface(); published {
			body.Generation = surface.Generation
			if surface.Apps != nil {
				body.Apps = surface.Apps
			}
			body.Diagnostics = s.cfg.MCPApps.Diagnostics()
		}
		body.SandboxOrigin = s.cfg.MCPAppsSandboxOrigin
		body.Theme = mcphost.ThemePalettes(s.cfg.MCPAppsTheme)
	}
	writeJSON(w, http.StatusOK, body)
}

// errMCPAppsUnwired is the explicit "this deployment serves no app surface"
// failure. It is not a 404: the route exists, the capability is off.
var errMCPAppsUnwired = errorString("MCP apps are not wired in this deployment")

type errorString string

func (e errorString) Error() string { return string(e) }

// mcpAppView serves one app document. The response is the HTML the View runs;
// the hash in ?v= is the cache identity, so a stale URL can never be served as
// new content.
func (s *Server) mcpAppView(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.MCPApps == nil {
		writeErr(w, http.StatusNotImplemented, errMCPAppsUnwired)
		return
	}
	appID := r.PathValue("appId")
	// A shape check before the lookup: only derived identities exist, so any
	// other string is a client bug (or a probe) and answers the same 404.
	if !validAppID(appID) || !s.cfg.MCPApps.Visible(tenantID) {
		writeErr(w, http.StatusNotFound, errAppNotFound)
		return
	}
	hash := strings.TrimSpace(r.URL.Query().Get("v"))
	if hash != "" && !validContentHash(hash) {
		writeErr(w, http.StatusNotFound, errAppNotFound)
		return
	}
	document, found := s.cfg.MCPApps.Document(appID, hash)
	// A document without its content identity, media type or CSP is not
	// servable: the registry never publishes one, and the route must not ship
	// an HTML body without the headers the spec requires.
	if !found || len(document.Bytes) == 0 || document.CSP == "" || document.MIMEType != mcpAppMIMEType {
		writeErr(w, http.StatusNotFound, errAppNotFound)
		return
	}
	// The four security headers the spec requires (the CSP carries
	// frame-ancestors 'self'). They are set before the body so an error after
	// this point cannot ship a document without them.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-MCP-App-Mime", document.MIMEType)
	w.Header().Set("X-MCP-App-Hash", document.Hash)
	w.Header().Set("Content-Security-Policy", document.CSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// A hash-named URL is immutable; the bare URL is content that moves under
	// the client's feet and must not be cached by any intermediary.
	if hash != "" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(document.Bytes)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(document.Bytes)
}

// mcpAppMIMEType is the only media type an app document may declare
// (mcp.AppMIMEType; restated here so the HTTP contract does not depend on the
// MCP package's constant staying exported).
const mcpAppMIMEType = "text/html;profile=mcp-app"

// errAppNotFound is the one answer for every unusable app identity: wrong
// shape, unknown id, stale hash, and foreign tenant all look the same.
var errAppNotFound = errorString("app not found")

// validAppID reports whether the path value has the derived appId shape
// (mcpapp_ + 16 lowercase hex): the only identities the registry can hold.
func validAppID(appID string) bool {
	const prefix = "mcpapp_"
	if !strings.HasPrefix(appID, prefix) || len(appID) != len(prefix)+16 {
		return false
	}
	for _, r := range appID[len(prefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// validContentHash reports whether the query value has the sha256 hex shape.
func validContentHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, r := range hash {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// Compile-time assertion: the registry satisfies the read model.
var _ MCPAppsSurface = (*mcphost.Registry)(nil)
