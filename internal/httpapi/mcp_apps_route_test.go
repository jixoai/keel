package httpapi_test

// Routing composition (P2-5): the product-layer MCP app routes live on the
// AgentAPI mounted under /v1/, while /v1/mcp/servers/{name}/signin is an edge
// route. This test drives the composed mux (not the AgentAPI alone) so the
// precedence rule — specific edge routes win, the rest reaches the product — is
// pinned for the two paths that share the /v1/mcp prefix.

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentapi"
	"github.com/jixoai/keel/internal/httpapi"
	"github.com/jixoai/keel/internal/mcphost"
)

const (
	edgeAppID   = "mcpapp_0123456789abcdef"
	edgeAppHash = "abababababababababababababababababababababababababababababababab"
	edgeAppHTML = "<html><body>edge panel</body></html>"
)

// edgeMCPApps is the AgentAPI read model used through the composed mux.
type edgeMCPApps struct {
	tenant uuid.UUID
}

func (f edgeMCPApps) Visible(tenantID uuid.UUID) bool { return f.tenant == tenantID }

func (f edgeMCPApps) Surface() (mcphost.Surface, bool) {
	csp, _ := mcphost.BuildCSP(nil, mcphost.CSPOptions{})
	return mcphost.Surface{
		Generation: 3,
		Apps: []mcphost.AppSpec{{
			AppID:       edgeAppID,
			Server:      "edge",
			ResourceURI: "ui://edge/panel",
			MIMEType:    "text/html;profile=mcp-app",
			CSP:         csp,
			Hash:        edgeAppHash,
			DocumentURL: "/v1/mcp/apps/" + edgeAppID + "/view?v=" + edgeAppHash,
		}},
	}, true
}

func (f edgeMCPApps) App(appID string) (mcphost.AppSpec, bool) {
	surface, _ := f.Surface()
	for _, app := range surface.Apps {
		if app.AppID == appID {
			return app, true
		}
	}
	return mcphost.AppSpec{}, false
}

func (f edgeMCPApps) Document(appID, hash string) (mcphost.Document, bool) {
	if appID != edgeAppID || (hash != "" && hash != edgeAppHash) {
		return mcphost.Document{}, false
	}
	csp, _ := mcphost.BuildCSP(nil, mcphost.CSPOptions{})
	return mcphost.Document{
		AppID:    edgeAppID,
		Server:   "edge",
		Hash:     edgeAppHash,
		MIMEType: "text/html;profile=mcp-app",
		CSP:      csp,
		Bytes:    []byte(edgeAppHTML),
	}, true
}

func (f edgeMCPApps) Diagnostics() []mcphost.Diagnostic { return nil }

func (f edgeMCPApps) Limits() (int64, int64) { return 2 << 20, 64 << 20 }

// recordingProduct records every path the product handler received.
type recordingProduct struct {
	next  http.Handler
	mu    sync.Mutex
	paths []string
}

func (p *recordingProduct) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.paths = append(p.paths, r.URL.Path)
	p.mu.Unlock()
	p.next.ServeHTTP(w, r)
}

func (p *recordingProduct) saw(path string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, seen := range p.paths {
		if seen == path {
			return true
		}
	}
	return false
}

func TestProductMCPAppRoutesThroughEdgeMux(t *testing.T) {
	tenant := uuid.New()
	product := &recordingProduct{next: agentapi.New(agentapi.Config{
		MCPApps: edgeMCPApps{tenant: tenant},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})}
	server := httptest.NewServer(httpapi.New(httpapi.Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Product: product}))
	t.Cleanup(server.Close)

	get := func(path string, tenantID uuid.UUID) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tenantID != uuid.Nil {
			request.Header.Set("X-Tenant-ID", tenantID.String())
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { response.Body.Close() })
		return response
	}

	// The catalogue reaches the product handler through the /v1/ catch-all.
	catalog := get("/v1/mcp/apps", tenant)
	if catalog.StatusCode != http.StatusOK {
		t.Fatalf("catalog = %d", catalog.StatusCode)
	}
	var body struct {
		Generation int64             `json:"generation"`
		Apps       []mcphost.AppSpec `json:"apps"`
	}
	if err := json.NewDecoder(catalog.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Generation != 3 || len(body.Apps) != 1 || body.Apps[0].AppID != edgeAppID {
		t.Fatalf("catalog = %+v", body)
	}
	if !product.saw("/v1/mcp/apps") {
		t.Fatal("the edge mux answered the catalogue itself instead of forwarding it")
	}

	// The deeper document route also forwards, with the security headers intact.
	view := get("/v1/mcp/apps/"+edgeAppID+"/view?v="+edgeAppHash, tenant)
	if view.StatusCode != http.StatusOK {
		t.Fatalf("view = %d", view.StatusCode)
	}
	if got := view.Header.Get("Content-Security-Policy"); got == "" {
		t.Fatal("the edge mux dropped the document CSP")
	}
	if got := view.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}
	if !product.saw("/v1/mcp/apps/" + edgeAppID + "/view") {
		t.Fatal("the document route did not reach the product handler")
	}

	// The edge's own OAuth route keeps precedence over the product catch-all.
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/mcp/servers/edge/signin", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Tenant-ID", tenant.String())
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if product.saw("/v1/mcp/servers/edge/signin") {
		t.Fatal("the sign-in route was forwarded to the product instead of being served by the edge")
	}
	if response.StatusCode == http.StatusOK {
		t.Fatalf("sign-in without OAuth configuration = %d, want the edge's refusal", response.StatusCode)
	}

	// The tenant boundary is the AgentAPI's: without the header the product
	// answers 401 before any catalogue byte, through the same composition.
	anonymous := get("/v1/mcp/apps", uuid.Nil)
	if anonymous.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous catalog = %d, want 401", anonymous.StatusCode)
	}
}
