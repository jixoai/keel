package mcphost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/mcp"
)

// fakeFleet is the narrow Fleet the registry reads. The manager implements it
// in production; the test controls the declaration and the prompt renderers
// independently of a live connection.
type fakeFleet struct {
	servers []*mcp.Server
	apps    []mcp.App
}

func (f *fakeFleet) Servers() []*mcp.Server    { return f.servers }
func (f *fakeFleet) DeclaredApps() []mcp.App   { return f.apps }
func (f *fakeFleet) PromptSection() string     { return "- weather: test fixture" }
func (f *fakeFleet) AppsPromptSection() string { return "- weather/dashboard: ui://weather/dashboard" }

// fakeAuditor records the audit events the registry emits.
type fakeAuditor struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (a *fakeAuditor) RecordMCPApp(_ context.Context, event AuditEvent) {
	a.mu.Lock()
	a.events = append(a.events, event)
	a.mu.Unlock()
}

func (a *fakeAuditor) snapshot() []AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AuditEvent(nil), a.events...)
}

func (a *fakeAuditor) results(action, result string) int {
	count := 0
	for _, event := range a.snapshot() {
		if event.Action == action && event.Result == result {
			count++
		}
	}
	return count
}

// appFixture builds one connected server declaring one app referenced by two
// tools (the same resource, so the catalogue must dedupe it).
func appFixture(server string, tools ...string) (*mcp.Server, mcp.App) {
	serverEntry := mcp.ServerEntry{Name: server, Config: map[string]any{"url": "https://mcp.example.com/mcp"}}
	connected := &mcp.Server{Entry: serverEntry, Client: mcp.NewClient(mcp.ClientOptions{}), Tools: []mcp.Tool{
		{Name: tools[0], Title: "Weather panel"},
	}}
	app := mcp.App{
		AppID:       mcp.AppIDFor(server, "ui://weather/dashboard"),
		Server:      server,
		ResourceURI: "ui://weather/dashboard",
	}
	for _, tool := range tools {
		app.Tools = append(app.Tools, mcp.AppTool{Server: server, Tool: tool, Visibility: []string{"model", "app"}})
	}
	return connected, app
}

func appDocument(html string, meta string) mcp.AppResource {
	resource := mcp.AppResource{
		URI:      "ui://weather/dashboard",
		MIMEType: mcp.AppMIMEType,
		Text:     html,
	}
	if meta != "" {
		resource.Meta = json.RawMessage(meta)
	}
	return resource
}

func newTestRegistry(t *testing.T, options Options) *Registry {
	t.Helper()
	if options.Logger == nil {
		options.Logger = discardTestLogger()
	}
	return New(options)
}

// TestRegistryPublishesValidatedApps is the catalogue contract: appId is
// derived, the resource referenced by two tools appears once with both tool
// refs, the content hash is in the catalogue and in the URL, and the document
// is retrievable only by its current hash.
func TestRegistryPublishesValidatedApps(t *testing.T) {
	server, app := appFixture("weather", "dashboard", "forecast")
	fleet := &fakeFleet{servers: []*mcp.Server{server}, apps: []mcp.App{app}}
	html := "<html><body>weather</body></html>"
	calls := 0
	auditor := &fakeAuditor{}
	var sinkApps []AppSpec
	registry := newTestRegistry(t, Options{
		Auditor: auditor,
		ReadApp: func(context.Context, *mcp.Server, string) (mcp.AppResource, error) {
			calls++
			return appDocument(html, `{"ui":{"csp":{"connectDomains":["https://api.example.com"]},"permissions":{"clipboardWrite":true},"domain":"Weather.Example","prefersBorder":true}}`), nil
		},
		Sinks: Sinks{Apps: func(apps []AppSpec) error { sinkApps = apps; return nil }},
	})

	surface, err := registry.Refresh(context.Background(), fleet)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if calls != 1 {
		t.Fatalf("read calls = %d, want 1", calls)
	}
	if surface.Generation != 1 || registry.Generation() != 1 {
		t.Fatalf("generation = %d / %d", surface.Generation, registry.Generation())
	}
	if len(surface.Apps) != 1 {
		t.Fatalf("apps = %+v", surface.Apps)
	}
	spec := surface.Apps[0]
	if spec.AppID != app.AppID || spec.AppID != mcp.AppIDFor("weather", "ui://weather/dashboard") {
		t.Fatalf("appId = %q, want %q", spec.AppID, app.AppID)
	}
	if spec.Server != "weather" || spec.ResourceURI != "ui://weather/dashboard" || spec.MIMEType != mcp.AppMIMEType {
		t.Fatalf("spec identity = %+v", spec)
	}
	if len(spec.Tools) != 2 {
		t.Fatalf("tools = %+v, want the two referencing tools", spec.Tools)
	}
	if spec.Tools[0].Tool != "dashboard" || spec.Tools[1].Tool != "forecast" {
		t.Fatalf("tool order = %+v", spec.Tools)
	}
	if spec.Title != "Weather panel" {
		t.Fatalf("title = %q", spec.Title)
	}
	wantHash := contentHash([]byte(html))
	if spec.Hash != wantHash {
		t.Fatalf("hash = %q, want %q", spec.Hash, wantHash)
	}
	if spec.DocumentURL != "/v1/mcp/apps/"+spec.AppID+"/view?v="+wantHash {
		t.Fatalf("documentUrl = %q", spec.DocumentURL)
	}
	if !strings.Contains(spec.CSP, "connect-src 'self' https://api.example.com") ||
		!strings.Contains(spec.CSP, "frame-ancestors 'self'") {
		t.Fatalf("csp = %q", spec.CSP)
	}
	if spec.Permissions["clipboardWrite"] != true || spec.PrefersBorder == nil || !*spec.PrefersBorder {
		t.Fatalf("permissions/prefersBorder = %+v / %v", spec.Permissions, spec.PrefersBorder)
	}
	// `_meta.ui.domain` is parsed AND published: the catalogue is the single
	// machine-readable source, so a client never re-reads resource metadata.
	if spec.Domain != "weather.example" {
		t.Fatalf("domain = %q, want the lower-cased declared hostname", spec.Domain)
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal catalogue app: %v", err)
	}
	var catalogueFields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &catalogueFields); err != nil {
		t.Fatalf("decode catalogue app: %v", err)
	}
	if string(catalogueFields["domain"]) != `"weather.example"` {
		t.Fatalf("catalogue JSON domain = %s", catalogueFields["domain"])
	}
	if spec.CSPDomains == nil || len(spec.CSPDomains.ConnectDomains) != 1 {
		t.Fatalf("cspDomains = %+v", spec.CSPDomains)
	}
	if len(sinkApps) != 1 || sinkApps[0].AppID != spec.AppID {
		t.Fatalf("the Apps sink did not receive the catalogue: %+v", sinkApps)
	}
	if len(surface.Resources) != 1 || surface.Resources[0].Size != int64(len(html)) {
		t.Fatalf("resources = %+v", surface.Resources)
	}
	if len(surface.Prompt) != 2 || surface.Prompt[0].Name != "mcp_servers" || surface.Prompt[1].Name != "mcp_apps" {
		t.Fatalf("prompt sections = %+v", surface.Prompt)
	}
	if len(surface.Servers) != 1 {
		t.Fatalf("servers = %+v", surface.Servers)
	}
	state := surface.Servers[0]
	if state.Name != "weather" || state.Transport != "streamable-http" || !state.Connected || state.Apps != 1 || state.Tools != 1 {
		t.Fatalf("server state = %+v", state)
	}
	if state.Era != "legacy" {
		t.Fatalf("era = %q, want legacy for an unnegotiated client", state.Era)
	}

	if !registry.AppReady(spec.AppID) {
		t.Fatal("the published app is not ready")
	}
	document, ok := registry.Document(spec.AppID, wantHash)
	if !ok || string(document.Bytes) != html || document.CSP != spec.CSP {
		t.Fatalf("document lookup = %+v, %v", document, ok)
	}
	if _, ok := registry.Document(spec.AppID, ""); !ok {
		t.Fatal("the current-version lookup failed")
	}
	if _, ok := registry.Document(spec.AppID, strings.Repeat("0", 64)); ok {
		t.Fatal("a stale hash was served")
	}
	if _, ok := registry.Document("mcpapp_0000000000000000", ""); ok {
		t.Fatal("an unknown appId was served")
	}
	if auditor.results("document.fetch", "ok") != 1 || auditor.results("catalogue.publish", "ok") != 1 {
		t.Fatalf("audit = %+v", auditor.snapshot())
	}
}

// TestRegistryExcludesFailedApps is the degradation contract: a fetch failure
// and a type mismatch keep the app out of the catalogue (never a half-built
// surface), are diagnosed and audited, and the other app still publishes.
func TestRegistryExcludesFailedApps(t *testing.T) {
	server, good := appFixture("weather", "dashboard")
	brokenURI := "ui://weather/broken"
	broken := mcp.App{
		AppID:       mcp.AppIDFor("weather", brokenURI),
		Server:      "weather",
		ResourceURI: brokenURI,
		Tools:       []mcp.AppTool{{Server: "weather", Tool: "broken", Visibility: []string{"app"}}},
	}
	fleet := &fakeFleet{servers: []*mcp.Server{server}, apps: []mcp.App{good, broken}}
	auditor := &fakeAuditor{}
	registry := newTestRegistry(t, Options{
		Auditor: auditor,
		ReadApp: func(_ context.Context, _ *mcp.Server, uri string) (mcp.AppResource, error) {
			switch uri {
			case good.ResourceURI:
				return appDocument("<html>ok</html>", ""), nil
			case brokenURI:
				return mcp.AppResource{URI: brokenURI, MIMEType: "application/json", Text: `{"a":1}`}, nil
			default:
				return mcp.AppResource{}, errors.New("unexpected uri")
			}
		},
	})
	surface, err := registry.Refresh(context.Background(), fleet)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(surface.Apps) != 1 || surface.Apps[0].AppID != good.AppID {
		t.Fatalf("apps = %+v, want only the valid one", surface.Apps)
	}
	if registry.AppReady(broken.AppID) {
		t.Fatal("a failed app is ready")
	}
	diagnostics := registry.Diagnostics()
	if len(diagnostics) != 1 || diagnostics[0].Kind != "validate" || !strings.Contains(diagnostics[0].Message, "mimeType") {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
	if auditor.results("document.validate", "error") != 1 {
		t.Fatalf("audit = %+v", auditor.snapshot())
	}
	for _, event := range auditor.snapshot() {
		if strings.Contains(event.Detail, "<html>") || strings.Contains(event.Detail, `{"a":1}`) {
			t.Fatalf("the audit carried document content: %+v", event)
		}
	}
}

// TestRegistryKeepsPreviousVersionOnRefetchFailure is the "no blank area" rule:
// a failed re-fetch keeps the last valid version and diagnoses the failure; a
// successful content change produces a new hash and a new immutable URL.
func TestRegistryKeepsPreviousVersionOnRefetchFailure(t *testing.T) {
	server, app := appFixture("weather", "dashboard")
	fleet := &fakeFleet{servers: []*mcp.Server{server}, apps: []mcp.App{app}}
	html := "<html>v1</html>"
	failing := false
	calls := 0
	registry := newTestRegistry(t, Options{
		ReadApp: func(context.Context, *mcp.Server, string) (mcp.AppResource, error) {
			calls++
			if failing {
				return mcp.AppResource{}, errors.New("upstream timeout")
			}
			return appDocument(html, ""), nil
		},
	})
	first, err := registry.Refresh(context.Background(), fleet)
	if err != nil || len(first.Apps) != 1 {
		t.Fatalf("first refresh: %v %+v", err, first.Apps)
	}
	firstHash := first.Apps[0].Hash

	failing = true
	second, err := registry.Refresh(context.Background(), fleet)
	if err != nil || len(second.Apps) != 1 {
		t.Fatalf("failed refresh dropped the app: %v %+v", err, second.Apps)
	}
	if second.Apps[0].Hash != firstHash || second.Generation != 2 {
		t.Fatalf("kept version = %+v, want hash %s and generation 2", second.Apps[0], firstHash)
	}
	diagnostics := registry.Diagnostics()
	if len(diagnostics) != 1 || diagnostics[0].Kind != "fetch" {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
	if _, ok := registry.Document(app.AppID, firstHash); !ok {
		t.Fatal("the kept document is not servable")
	}

	// Recovery with new content: the hash and the URL change, and the old hash
	// is not served as new content.
	failing = false
	html = "<html>v2</html>"
	third, err := registry.Refresh(context.Background(), fleet)
	if err != nil || len(third.Apps) != 1 {
		t.Fatalf("third refresh: %v %+v", err, third.Apps)
	}
	secondHash := third.Apps[0].Hash
	if secondHash == firstHash {
		t.Fatal("the content change did not change the hash")
	}
	if !strings.HasSuffix(third.Apps[0].DocumentURL, secondHash) {
		t.Fatalf("documentUrl = %q", third.Apps[0].DocumentURL)
	}
	if _, ok := registry.Document(app.AppID, firstHash); ok {
		t.Fatal("the superseded hash is still served")
	}
	if _, ok := registry.Document(app.AppID, secondHash); !ok {
		t.Fatal("the new hash is not served")
	}
	if calls != 3 {
		t.Fatalf("read calls = %d, want 3", calls)
	}
}

// TestRegistryHonoursTTL pins the advisory cache hint: a fresh document inside
// its ttlMs is not re-read, and a zero TTL is always re-read.
func TestRegistryHonoursTTL(t *testing.T) {
	server, app := appFixture("weather", "dashboard")
	fleet := &fakeFleet{servers: []*mcp.Server{server}, apps: []mcp.App{app}}
	now := time.Unix(1_700_000_000, 0)
	ttl := int64(60_000)
	calls := 0
	registry := newTestRegistry(t, Options{
		Now: func() time.Time { return now },
		ReadApp: func(context.Context, *mcp.Server, string) (mcp.AppResource, error) {
			calls++
			doc := appDocument("<html>ttl</html>", "")
			doc.TTLMs = &ttl
			return doc, nil
		},
	})
	if _, err := registry.Refresh(context.Background(), fleet); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Refresh(context.Background(), fleet); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("a document inside its TTL was re-read: calls=%d", calls)
	}
	now = now.Add(61 * time.Second)
	if _, err := registry.Refresh(context.Background(), fleet); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("an expired document was not re-read: calls=%d", calls)
	}

	zero := int64(0)
	calls = 0
	always := newTestRegistry(t, Options{
		ReadApp: func(context.Context, *mcp.Server, string) (mcp.AppResource, error) {
			calls++
			doc := appDocument("<html>ttl</html>", "")
			doc.TTLMs = &zero
			return doc, nil
		},
	})
	always.Refresh(context.Background(), fleet)
	always.Refresh(context.Background(), fleet)
	if calls != 2 {
		t.Fatalf("a zero TTL was cached: calls=%d", calls)
	}
}

// TestRegistryCacheBudgetDropsApps pins the byte budget: an app whose document
// does not fit is refused with a cache diagnostic and an audit "refused", while
// the apps that fit still publish.
func TestRegistryCacheBudgetDropsApps(t *testing.T) {
	serverA, appA := appFixture("weather", "dashboard")
	serverB := &mcp.Server{Entry: mcp.ServerEntry{Name: "maps", Config: map[string]any{"url": "https://maps.example.com/mcp"}}}
	uriB := "ui://maps/panel"
	appB := mcp.App{
		AppID:       mcp.AppIDFor("maps", uriB),
		Server:      "maps",
		ResourceURI: uriB,
		Tools:       []mcp.AppTool{{Server: "maps", Tool: "panel", Visibility: []string{"app"}}},
	}
	fleet := &fakeFleet{servers: []*mcp.Server{serverA, serverB}, apps: []mcp.App{appA, appB}}
	auditor := &fakeAuditor{}
	registry := newTestRegistry(t, Options{
		CacheBytes:       100,
		MaxDocumentBytes: 200,
		Auditor:          auditor,
		ReadApp: func(_ context.Context, server *mcp.Server, uri string) (mcp.AppResource, error) {
			return mcp.AppResource{URI: uri, MIMEType: mcp.AppMIMEType, Text: strings.Repeat("x", 80)}, nil
		},
	})
	surface, err := registry.Refresh(context.Background(), fleet)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(surface.Apps) != 1 {
		t.Fatalf("apps = %+v, want exactly the one that fits", surface.Apps)
	}
	if auditor.results("document.cache", "refused") != 1 {
		t.Fatalf("audit = %+v", auditor.snapshot())
	}
	found := false
	for _, diagnostic := range registry.Diagnostics() {
		if diagnostic.Kind == "cache" {
			found = true
		}
	}
	if !found {
		t.Fatalf("diagnostics = %+v", registry.Diagnostics())
	}
}

// TestRegistryMaxDocumentBytes pins the per-document bound: an oversized
// document is refused before it enters the catalogue.
func TestRegistryMaxDocumentBytes(t *testing.T) {
	server, app := appFixture("weather", "dashboard")
	fleet := &fakeFleet{servers: []*mcp.Server{server}, apps: []mcp.App{app}}
	registry := newTestRegistry(t, Options{
		MaxDocumentBytes: 16,
		ReadApp: func(_ context.Context, _ *mcp.Server, uri string) (mcp.AppResource, error) {
			return mcp.AppResource{URI: uri, MIMEType: mcp.AppMIMEType, Text: strings.Repeat("x", 17)}, nil
		},
	})
	surface, err := registry.Refresh(context.Background(), fleet)
	if err != nil {
		t.Fatal(err)
	}
	if len(surface.Apps) != 0 {
		t.Fatalf("an oversized document was published: %+v", surface.Apps)
	}
	if diagnostics := registry.Diagnostics(); len(diagnostics) != 1 || diagnostics[0].Kind != "validate" {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
}

// TestRegistryTenantVisibility pins the tenant boundary primitive the HTTP
// layer uses: a scoped registry answers only its tenant; an unscoped one is
// process-global.
func TestRegistryTenantVisibility(t *testing.T) {
	tenant := uuid.New()
	scoped := newTestRegistry(t, Options{TenantID: tenant})
	if !scoped.Visible(tenant) || scoped.Visible(uuid.New()) {
		t.Fatal("scoped registry visibility is wrong")
	}
	global := newTestRegistry(t, Options{})
	if !global.Visible(tenant) || !global.Visible(uuid.New()) {
		t.Fatal("unscoped registry should be process-global")
	}
	var nilRegistry *Registry
	if nilRegistry.Visible(tenant) || nilRegistry.AppReady("x") {
		t.Fatal("a nil registry must be invisible")
	}
}

// TestRegistryUnconnectedServer pins the "no apps server" degradation: an app
// declared by a server that failed to connect is diagnosed as a fetch failure
// and never published.
func TestRegistryUnconnectedServer(t *testing.T) {
	failedServer := &mcp.Server{Entry: mcp.ServerEntry{Name: "down"}, Err: errors.New("connection refused")}
	app := mcp.App{
		AppID:       mcp.AppIDFor("down", "ui://down/panel"),
		Server:      "down",
		ResourceURI: "ui://down/panel",
		Tools:       []mcp.AppTool{{Server: "down", Tool: "panel"}},
	}
	fleet := &fakeFleet{servers: []*mcp.Server{failedServer}, apps: []mcp.App{app}}
	registry := newTestRegistry(t, Options{ReadApp: func(context.Context, *mcp.Server, string) (mcp.AppResource, error) {
		t.Fatal("a failed server must not be read")
		return mcp.AppResource{}, nil
	}})
	surface, err := registry.Refresh(context.Background(), fleet)
	if err != nil {
		t.Fatal(err)
	}
	if len(surface.Apps) != 0 {
		t.Fatalf("apps = %+v", surface.Apps)
	}
	if len(surface.Servers) != 1 || surface.Servers[0].Connected || surface.Servers[0].Error == "" {
		t.Fatalf("server state = %+v", surface.Servers)
	}
	if diagnostics := registry.Diagnostics(); len(diagnostics) != 1 || diagnostics[0].Kind != "fetch" {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
}

// discardTestLogger keeps the diagnostic/audit log out of test output.
func discardTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRegistryContentChangeReclaimsCache pins the byte accounting across a
// content change: once the new version is published the old one is no longer
// pinned, so the next insert evicts it and the cache total really drops (the
// cache owns the bytes).
func TestRegistryContentChangeReclaimsCache(t *testing.T) {
	server, app := appFixture("weather", "dashboard")
	secondServer := &mcp.Server{Entry: mcp.ServerEntry{Name: "maps", Config: map[string]any{"url": "https://maps.example.com/mcp"}}}
	secondURI := "ui://maps/panel"
	secondApp := mcp.App{
		AppID:       mcp.AppIDFor("maps", secondURI),
		Server:      "maps",
		ResourceURI: secondURI,
		Tools:       []mcp.AppTool{{Server: "maps", Tool: "panel", Visibility: []string{"app"}}},
	}
	html := strings.Repeat("a", 80)
	registry := newTestRegistry(t, Options{
		CacheBytes:       160,
		MaxDocumentBytes: 200,
		ReadApp: func(_ context.Context, _ *mcp.Server, uri string) (mcp.AppResource, error) {
			if uri == secondURI {
				return mcp.AppResource{URI: uri, MIMEType: mcp.AppMIMEType, Text: strings.Repeat("c", 80)}, nil
			}
			return mcp.AppResource{URI: uri, MIMEType: mcp.AppMIMEType, Text: html}, nil
		},
	})
	first, err := registry.Refresh(context.Background(), &fakeFleet{servers: []*mcp.Server{server}, apps: []mcp.App{app}})
	if err != nil || len(first.Apps) != 1 {
		t.Fatalf("first refresh: %v %+v", err, first.Apps)
	}
	firstHash := first.Apps[0].Hash
	oldKey := documentKey(app.AppID, firstHash)
	if got := registry.cache.totalBytes(); got != 80 {
		t.Fatalf("cache total = %d, want 80", got)
	}

	html = strings.Repeat("b", 80)
	second, err := registry.Refresh(context.Background(), &fakeFleet{servers: []*mcp.Server{server}, apps: []mcp.App{app}})
	if err != nil || len(second.Apps) != 1 {
		t.Fatalf("second refresh: %v %+v", err, second.Apps)
	}
	if second.Apps[0].Hash == firstHash {
		t.Fatal("the content change did not produce a new hash")
	}
	if !registry.cache.cached(oldKey) {
		t.Fatal("the superseded version was dropped before it was evicted")
	}

	// A third document that needs the superseded version's bytes: eviction
	// reclaims it, and the total stays under the bound.
	fleet := &fakeFleet{servers: []*mcp.Server{server, secondServer}, apps: []mcp.App{app, secondApp}}
	if _, err := registry.Refresh(context.Background(), fleet); err != nil {
		t.Fatal(err)
	}
	if registry.cache.cached(oldKey) {
		t.Fatal("the superseded version survived eviction")
	}
	if got := registry.cache.evictionCount(); got != 1 {
		t.Fatalf("evictions = %d, want 1", got)
	}
	if got := registry.cache.totalBytes(); got != 160 {
		t.Fatalf("cache total = %d, want 160", got)
	}
}

// TestRegistryKeepsPreviousVersionWhenCacheIsFull is the P2-4 regression: the
// new version is stored before the old one is unpinned, so a full cache keeps
// serving the previous document instead of leaving an unprotected version a
// concurrent put could evict.
func TestRegistryKeepsPreviousVersionWhenCacheIsFull(t *testing.T) {
	server, app := appFixture("weather", "dashboard")
	html := strings.Repeat("a", 80)
	registry := newTestRegistry(t, Options{
		CacheBytes:       100,
		MaxDocumentBytes: 200,
		ReadApp: func(context.Context, *mcp.Server, string) (mcp.AppResource, error) {
			return mcp.AppResource{URI: app.ResourceURI, MIMEType: mcp.AppMIMEType, Text: html}, nil
		},
	})
	first, err := registry.Refresh(context.Background(), &fakeFleet{servers: []*mcp.Server{server}, apps: []mcp.App{app}})
	if err != nil || len(first.Apps) != 1 {
		t.Fatalf("first refresh: %v %+v", err, first.Apps)
	}
	firstHash := first.Apps[0].Hash

	// A content change cannot fit next to the pinned version (80 + 80 > 100):
	// the old version stays current and is still servable.
	html = strings.Repeat("b", 80)
	second, err := registry.Refresh(context.Background(), &fakeFleet{servers: []*mcp.Server{server}, apps: []mcp.App{app}})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Apps) != 1 || second.Apps[0].Hash != firstHash {
		t.Fatalf("apps = %+v, want the previous version kept", second.Apps)
	}
	if document, ok := registry.Document(app.AppID, firstHash); !ok || len(document.Bytes) == 0 {
		t.Fatal("the previous version is not servable")
	}
	if !registry.AppReady(app.AppID) {
		t.Fatal("the app fell out of the catalogue on a cache-full refresh")
	}
	diagnostics := registry.Diagnostics()
	if len(diagnostics) != 1 || diagnostics[0].Kind != "cache" {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
}

// TestRegistryAppLookup pins the catalogue identity read the app bridge uses:
// only a published app resolves, and the returned spec carries the derived
// server (never a client-supplied one).
func TestRegistryAppLookup(t *testing.T) {
	server, app := appFixture("weather", "dashboard")
	fleet := &fakeFleet{servers: []*mcp.Server{server}, apps: []mcp.App{app}}
	registry := newTestRegistry(t, Options{
		ReadApp: func(context.Context, *mcp.Server, string) (mcp.AppResource, error) {
			return appDocument("<html></html>", ""), nil
		},
	})
	if _, ok := registry.App(app.AppID); ok {
		t.Fatal("an unpublished app resolved")
	}
	if _, err := registry.Refresh(context.Background(), fleet); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	spec, ok := registry.App(app.AppID)
	if !ok {
		t.Fatal("the published app did not resolve")
	}
	if spec.Server != "weather" || spec.AppID != app.AppID {
		t.Fatalf("spec = %+v", spec)
	}
	if _, ok := registry.App("mcpapp_0000000000000000"); ok {
		t.Fatal("an unknown app id resolved")
	}
}
