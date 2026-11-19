package mcphost

// The registry: the host's single source of truth for MCP apps. It reads the
// declared app catalogue of the connected fleet, fetches and validates each
// document, publishes the resulting surface (catalogue + prompt sections) and
// keeps the documents in the byte-bounded cache.
//
// Publication is all-or-nothing per app: an app enters the catalogue only after
// its document was fetched, validated, hashed and cached. A failed app keeps a
// previous valid version when one exists ("重取失败保留旧版本"), and otherwise
// stays out — the tool result keeps its text content, which is the spec's
// degradation contract (「降级路径与能力协商」).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/mcp"
)

// Defaults from design §3.2/§3.7. They are package variables (not constants) so
// a deployment can be sized, but the zero Options means exactly these.
const (
	DefaultCacheBytes       = 64 << 20
	DefaultFetchConcurrency = 4
	DefaultFetchTimeout     = 10 * time.Second
	// maxDiagnostics bounds the diagnostic list the catalogue/API reports.
	maxDiagnostics = 200
)

// Options configures one registry (one process tenant).
type Options struct {
	// TenantID scopes the catalogue. uuid.Nil means the deployment has no
	// tenant isolation (a single-tenant process); any other value means only
	// that tenant may read the catalogue.
	TenantID uuid.UUID
	// CacheBytes is the document cache bound (default 64 MiB).
	CacheBytes int64
	// MaxDocumentBytes is the per-document bound (default 2 MiB, the P1
	// validator's limit).
	MaxDocumentBytes int64
	// FetchConcurrency bounds simultaneous resources/read calls.
	FetchConcurrency int
	// FetchTimeout bounds one document fetch.
	FetchTimeout time.Duration
	// Now is the clock (tests). Nil uses time.Now.
	Now func() time.Time
	// Logger records diagnostics and audit events. Nil uses slog.Default.
	Logger *slog.Logger
	// Auditor, when set, receives one event per audited action (document
	// fetch/validation/cache/refusal). The durable table is P3.7; P2 keeps the
	// seam and the structured log.
	Auditor Auditor
	// AllowPrivateDomains admits loopback/private origins in an app's CSP. Off
	// by default: a View is browser JavaScript, and a private origin gives it a
	// path to the host's own network services. A dev fixture opts in.
	AllowPrivateDomains bool
	// Metrics records document fetch outcomes (P4.7). Nil is a no-op.
	Metrics *Metrics
	// ReadApp reads one app document. Nil uses server.Client.ReadAppResource.
	ReadApp func(ctx context.Context, server *mcp.Server, uri string) (mcp.AppResource, error)
	// Sinks receive the published surface. Every sink is optional.
	Sinks Sinks
}

// Auditor receives the app audit trail. Implementations must be safe for
// concurrent use and must not include message bodies, credentials or tokens
// (the registry only passes identifiers, a bounded detail and a result).
type Auditor interface {
	RecordMCPApp(ctx context.Context, event AuditEvent)
}

// AuditEvent is one audited MCP app action.
type AuditEvent struct {
	TenantID    uuid.UUID `json:"tenantId"`
	Server      string    `json:"server,omitempty"`
	AppID       string    `json:"appId,omitempty"`
	ResourceURI string    `json:"resourceUri,omitempty"`
	Action      string    `json:"action"`
	Result      string    `json:"result"`
	Detail      string    `json:"detail,omitempty"`
}

// Diagnostic is one reported degradation, never fatal.
type Diagnostic struct {
	At          time.Time `json:"at"`
	Server      string    `json:"server,omitempty"`
	AppID       string    `json:"appId,omitempty"`
	ResourceURI string    `json:"resourceUri,omitempty"`
	Kind        string    `json:"kind"`
	Message     string    `json:"message"`
}

// Fleet is the connected MCP surface the registry reads. *mcp.Manager
// implements it.
type Fleet interface {
	Servers() []*mcp.Server
	DeclaredApps() []mcp.App
	PromptSection() string
	AppsPromptSection() string
}

// Registry is one tenant's published app surface.
type Registry struct {
	options Options
	now     func() time.Time
	logger  *slog.Logger

	refreshMu sync.Mutex

	mu         sync.RWMutex
	generation int64
	published  bool
	surface    Surface
	apps       map[string]appEntry
	ready      map[string]bool
	// stale marks appIds whose document a notifications/resources/updated
	// invalidated: their TTL reuse is bypassed on the next Refresh (a content
	// change is not a cache hit).
	stale    map[string]bool
	cache    *docCache
	diagnose []Diagnostic
}

// New builds a registry. It publishes nothing until Refresh runs; until then
// Surface reports ok=false and every app is unpublished.
func New(options Options) *Registry {
	if options.CacheBytes <= 0 {
		options.CacheBytes = DefaultCacheBytes
	}
	if options.MaxDocumentBytes <= 0 {
		options.MaxDocumentBytes = mcp.AppResourceMaxBytes
	}
	if options.FetchConcurrency <= 0 {
		options.FetchConcurrency = DefaultFetchConcurrency
	}
	if options.FetchTimeout <= 0 {
		options.FetchTimeout = DefaultFetchTimeout
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &Registry{
		options: options,
		now:     options.Now,
		logger:  options.Logger,
		apps:    make(map[string]appEntry),
		ready:   make(map[string]bool),
		stale:   make(map[string]bool),
		cache:   newDocCache(options.CacheBytes),
	}
}

// TenantID is the scope this registry publishes under.
func (r *Registry) TenantID() uuid.UUID {
	if r == nil {
		return uuid.Nil
	}
	return r.options.TenantID
}

// Visible reports whether tenantID may read this catalogue. A registry built
// with the zero tenant is process-global (a single-tenant deployment); any
// other scope answers only its own tenant.
func (r *Registry) Visible(tenantID uuid.UUID) bool {
	if r == nil {
		return false
	}
	scope := r.options.TenantID
	return scope == uuid.Nil || scope == tenantID
}

// AppReady is the mcp.ConnectOptions.AppReadiness predicate: only an app that
// is in the published catalogue may carry an app binding or a prompt line.
func (r *Registry) AppReady(appID string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ready[appID]
}

// Surface returns the current published surface. The value is immutable: the
// registry never mutates a surface it handed out (a change publishes a new
// value), so callers may read the slices without copying.
func (r *Registry) Surface() (Surface, bool) {
	if r == nil {
		return Surface{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.published {
		return Surface{}, false
	}
	return r.surface, true
}

// Generation is the monotonic change identity of the published surface.
func (r *Registry) Generation() int64 {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.generation
}

// App returns one published app's specification. The catalogue is the only
// binding authority: the bridge derives the server from here, never from a
// client-supplied name.
func (r *Registry) App(appID string) (AppSpec, bool) {
	if r == nil {
		return AppSpec{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.apps[appID]
	if !ok {
		return AppSpec{}, false
	}
	return entry.spec, true
}

// Document returns the published document of one app. An empty hash selects the
// current version; a non-empty hash must match it exactly (a stale hash is not
// served as new content). The returned bytes are immutable.
func (r *Registry) Document(appID, hash string) (Document, bool) {
	if r == nil {
		return Document{}, false
	}
	r.mu.RLock()
	entry, ok := r.apps[appID]
	hashMatches := ok && (hash == "" || hash == entry.spec.Hash)
	r.mu.RUnlock()
	if !ok || !hashMatches {
		return Document{}, false
	}
	return r.cache.get(entry.docID)
}

// Diagnostics returns the recent degradations (bounded, newest last).
func (r *Registry) Diagnostics() []Diagnostic {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Diagnostic(nil), r.diagnose...)
}

// InvalidateResource marks the app documents of one (server, uri) stale so the
// next Refresh re-reads them even when their TTL would have allowed a reuse.
// This is the notifications/resources/updated landing point (design §4.1); an
// unknown URI is a no-op.
func (r *Registry) InvalidateResource(server, uri string) {
	if r == nil || uri == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for appID, entry := range r.apps {
		if entry.spec.Server == server && entry.spec.ResourceURI == uri {
			r.stale[appID] = true
		}
	}
}

// isStale reports whether an app's cached document was invalidated.
func (r *Registry) isStale(appID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.stale[appID]
}

// clearStale drops the invalidation mark of a successfully re-read app.
func (r *Registry) clearStale(appID string) {
	r.mu.Lock()
	delete(r.stale, appID)
	r.mu.Unlock()
}

// Limits reports the byte bounds this registry enforces: one document, and the
// whole cache. The API surface serves the same numbers it applies, so a client
// never estimates them.
func (r *Registry) Limits() (documentBytes, cacheBytes int64) {
	if r == nil {
		return mcp.AppResourceMaxBytes, DefaultCacheBytes
	}
	return r.options.MaxDocumentBytes, r.options.CacheBytes
}

// Refresh re-reads the declared app catalogue, fetches what is missing or
// stale, publishes the new surface and invokes the sinks. Individual app
// failures are diagnostics, not errors; a returned error means the publication
// itself failed to reach a sink.
func (r *Registry) Refresh(ctx context.Context, fleet Fleet) (Surface, error) {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()

	now := r.now()
	servers := fleet.Servers()
	declared := fleet.DeclaredApps()
	byName := make(map[string]*mcp.Server, len(servers))
	for _, server := range servers {
		byName[server.Entry.Name] = server
	}
	declared = dedupeApps(declared)

	// Fetch phase. Each app either reuses a still-fresh document, fetches a
	// new one, or keeps the previous valid version when the fetch fails.
	outcomes := make([]resolveResult, len(declared))
	work := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < r.options.FetchConcurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range work {
				outcomes[index] = r.resolve(ctx, declared[index], byName, now)
			}
		}()
	}
	for index := range declared {
		work <- index
	}
	close(work)
	wg.Wait()

	// Publish phase: install the catalogue, then build the prompt sections
	// from the gated catalogue so the mcp_apps lines and the renderable
	// catalogue cannot disagree.
	newApps := make(map[string]appEntry, len(declared))
	newSpecs := make([]AppSpec, 0, len(declared))
	newReady := make(map[string]bool, len(declared))
	protected := make(map[string]bool, len(declared))
	for index, app := range declared {
		result := outcomes[index]
		if !result.keepDoc {
			continue
		}
		docID := documentKey(app.AppID, result.doc.Hash)
		newApps[app.AppID] = appEntry{spec: result.spec, docID: docID}
		newSpecs = append(newSpecs, result.spec)
		newReady[app.AppID] = true
		protected[docID] = true
	}
	sort.Slice(newSpecs, func(i, j int) bool { return newSpecs[i].AppID < newSpecs[j].AppID })

	r.mu.Lock()
	r.apps = newApps
	r.ready = newReady
	r.mu.Unlock()
	// The cache is the document store: retain keeps exactly this generation's
	// documents pinned, and every superseded version becomes evictable.
	r.cache.retain(protected)

	r.mu.Lock()
	r.generation++
	generation := r.generation
	r.mu.Unlock()

	surface := r.buildSurface(fleet, servers, newSpecs, generation)

	r.mu.Lock()
	r.surface = surface
	r.published = true
	r.mu.Unlock()

	r.record(ctx, AuditEvent{
		Action: "catalogue.publish",
		Result: "ok",
		Detail: fmt.Sprintf("generation=%d apps=%d declared=%d", surface.Generation, len(surface.Apps), len(declared)),
	})

	return surface, r.emit(ctx, surface)
}

// resolveResult is one app's resolved catalogue entry.
type resolveResult struct {
	spec    AppSpec
	doc     Document
	keepDoc bool
}

// resolve produces one app's catalogue entry: reuse (TTL), fetch, or keep the
// previous version.
func (r *Registry) resolve(ctx context.Context, app mcp.App, byName map[string]*mcp.Server, now time.Time) resolveResult {
	server := byName[app.Server]
	if server == nil || server.Err != nil {
		r.options.Metrics.AppFetch("error")
		message := "the server is not connected"
		if server != nil && server.Err != nil {
			message = server.Err.Error()
		}
		r.diagnoseApp(ctx, app, "fetch", "the app is not published: "+message)
		return resolveResult{}
	}

	// A fresh, still-cached previous version is reused without a fetch. TTLMs
	// is advisory (2026-07-28); a server that declares none is re-read.
	r.mu.RLock()
	previous, hasPrevious := r.apps[app.AppID]
	r.mu.RUnlock()
	var previousDoc Document
	hasPreviousDoc := false
	if hasPrevious {
		previousDoc, hasPreviousDoc = r.cache.get(previous.docID)
	}
	if hasPrevious && hasPreviousDoc {
		if !r.isStale(app.AppID) && previous.spec.TTLMs != nil && now.Before(previous.spec.FetchedAt.Add(time.Duration(*previous.spec.TTLMs)*time.Millisecond)) {
			r.options.Metrics.AppFetch("reused")
			return resolveResult{spec: withTools(previous.spec, app), doc: previousDoc, keepDoc: true}
		}
	}

	resource, err := r.read(ctx, server, app.ResourceURI)
	if err != nil {
		r.options.Metrics.AppFetch("error")
		r.diagnoseApp(ctx, app, "fetch", "resources/read failed: "+err.Error())
		return r.keepPrevious(previous, hasPrevious, previousDoc, hasPreviousDoc, app)
	}
	bytes, err := documentBytes(resource, r.options.MaxDocumentBytes)
	if err != nil {
		r.options.Metrics.AppFetch("error")
		r.diagnoseApp(ctx, app, "validate", err.Error())
		return r.keepPrevious(previous, hasPrevious, previousDoc, hasPreviousDoc, app)
	}
	hash := contentHash(bytes)
	docID := documentKey(app.AppID, hash)

	meta, metaNotes := parseResourceMeta(resource.Meta)
	cspHeader, cspNotes := BuildCSP(meta.CSP, CSPOptions{AllowPrivateDomains: r.options.AllowPrivateDomains})
	// One diagnostic budget per app across metadata parsing and CSP building:
	// a hostile block cannot amplify its notes into the catalogue.
	notes := boundDiagnostics(append(metaNotes, cspNotes...), maxAppDiagnostics)
	if len(resource.Diagnostics) > 0 {
		notes = boundDiagnostics(append(notes, resource.Diagnostics...), maxAppDiagnostics)
	}

	doc := Document{
		AppID:     app.AppID,
		Server:    app.Server,
		Hash:      hash,
		MIMEType:  resource.MIMEType,
		CSP:       cspHeader,
		Bytes:     bytes,
		FetchedAt: now,
	}
	// Store the new version first: the previous one stays pinned until the new
	// one is protected, so a failed put (cache full) can still serve the old
	// version instead of leaving an unprotected document that a concurrent put
	// may evict. Only after the new version is in the cache does the old one
	// become evictable.
	if !r.cache.put(docID, doc, true) {
		r.options.Metrics.AppFetch("error")
		message := fmt.Sprintf("the document cannot be cached within the %d-byte budget", r.options.CacheBytes)
		r.diagnoseApp(ctx, app, "cache", message)
		return r.keepPrevious(previous, hasPrevious, previousDoc, hasPreviousDoc, app)
	}
	r.clearStale(app.AppID)
	r.options.Metrics.AppFetch("ok")
	if hasPrevious && previous.docID != docID {
		r.cache.protect(previous.docID, false)
	}

	spec := AppSpec{
		AppID:         app.AppID,
		Server:        app.Server,
		Title:         titleFor(app, server),
		ResourceURI:   app.ResourceURI,
		MIMEType:      resource.MIMEType,
		CSP:           cspHeader,
		CSPDomains:    meta.CSP,
		Permissions:   meta.Permissions,
		PrefersBorder: meta.PrefersBorder,
		Hash:          hash,
		FetchedAt:     now,
		TTLMs:         resource.TTLMs,
		DocumentURL:   documentURL(app.AppID, hash),
		Diagnostics:   notes,
	}
	spec = withTools(spec, app)

	r.record(ctx, AuditEvent{
		Server:      app.Server,
		AppID:       app.AppID,
		ResourceURI: safeURI(app.ResourceURI),
		Action:      "document.fetch",
		Result:      "ok",
		Detail:      fmt.Sprintf("hash=%s bytes=%d", hash, len(bytes)),
	})
	return resolveResult{spec: spec, doc: doc, keepDoc: true}
}

// withTools refreshes the tool projection of a reused specification (a tool may
// have been added to a resource that did not change).
func withTools(spec AppSpec, app mcp.App) AppSpec {
	spec.Tools, spec.Visibility = appTools(app)
	return spec
}

// keepPrevious is the degradation rule: a failed refresh keeps the last valid
// version instead of blanking the app ("重取失败保留旧版本，不出现空白").
func (r *Registry) keepPrevious(previous appEntry, hasPrevious bool, previousDoc Document, hasPreviousDoc bool, app mcp.App) resolveResult {
	if !hasPrevious || !hasPreviousDoc || !r.cache.cached(previous.docID) {
		return resolveResult{}
	}
	return resolveResult{spec: withTools(previous.spec, app), doc: previousDoc, keepDoc: true}
}

// read fetches one document through the seam (default: the server's client).
func (r *Registry) read(ctx context.Context, server *mcp.Server, uri string) (mcp.AppResource, error) {
	read := r.options.ReadApp
	if read == nil {
		if server.Client == nil {
			return mcp.AppResource{}, errors.New("the server has no client")
		}
		read = func(ctx context.Context, server *mcp.Server, uri string) (mcp.AppResource, error) {
			return server.Client.ReadAppResource(ctx, uri)
		}
	}
	fetchCtx, cancel := context.WithTimeout(ctx, r.options.FetchTimeout)
	defer cancel()
	return read(fetchCtx, server, uri)
}

// buildSurface assembles the published value. It calls the fleet's prompt
// renderers, which read the registry's readiness gate: call it only after the
// new catalogue is installed.
func (r *Registry) buildSurface(fleet Fleet, servers []*mcp.Server, specs []AppSpec, generation int64) Surface {
	appCounts := make(map[string]int, len(servers))
	for _, spec := range specs {
		appCounts[spec.Server]++
	}
	states := make([]ServerState, 0, len(servers))
	for _, server := range servers {
		state := ServerState{
			Name:           server.Entry.Name,
			Transport:      transportName(server.Entry),
			Connected:      server.Err == nil && server.Client != nil,
			Reconnects:     server.Reconnects,
			Tools:          len(server.Tools),
			Apps:           appCounts[server.Entry.Name],
			Diagnostics:    append([]string(nil), server.Diagnostics...),
			CatalogueError: server.CatalogueError,
		}
		if server.Err != nil {
			state.Error = server.Err.Error()
		}
		if server.Client != nil {
			state.ProtocolVersion = server.Client.ProtocolVersion()
			state.Era = eraOf(state.ProtocolVersion)
			if capabilities, err := json.Marshal(server.Client.ServerCapabilities()); err == nil {
				state.Capabilities = capabilities
			}
		} else {
			state.Era = eraOf("")
		}
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Name < states[j].Name })

	resources := make([]ResourceSpec, 0, len(specs))
	for _, spec := range specs {
		doc, ok := r.cachedDocument(spec)
		if !ok {
			continue
		}
		resources = append(resources, ResourceSpec{
			Server:   spec.Server,
			URI:      spec.ResourceURI,
			Name:     spec.Title,
			MIMEType: spec.MIMEType,
			Size:     int64(len(doc.Bytes)),
			Meta:     resourceMetaFor(spec),
		})
	}

	sections := make([]Section, 0, 2)
	if content := fleet.PromptSection(); content != "" {
		sections = append(sections, Section{Name: "mcp_servers", Content: content})
	}
	if content := fleet.AppsPromptSection(); content != "" {
		sections = append(sections, Section{Name: "mcp_apps", Content: content})
	}

	return Surface{
		Generation: generation,
		Servers:    states,
		Resources:  resources,
		Apps:       specs,
		Prompt:     sections,
	}
}

func (r *Registry) cachedDocument(spec AppSpec) (Document, bool) {
	return r.cache.get(documentKey(spec.AppID, spec.Hash))
}

// resourceMetaFor derives the resource metadata projection from an AppSpec.
func resourceMetaFor(spec AppSpec) UIResourceMeta {
	return UIResourceMeta{
		CSP:           spec.CSPDomains,
		Permissions:   spec.Permissions,
		PrefersBorder: spec.PrefersBorder,
	}
}

// emit invokes the sinks in their fixed order. A nil sink is "not served".
func (r *Registry) emit(ctx context.Context, surface Surface) error {
	sinks := r.options.Sinks
	if sinks.Tools != nil {
		if err := sinks.Tools(surface.Tools); err != nil {
			return err
		}
	}
	if sinks.Prompt != nil {
		if err := sinks.Prompt(surface.Prompt); err != nil {
			return err
		}
	}
	if sinks.Apps != nil {
		if err := sinks.Apps(surface.Apps); err != nil {
			return err
		}
	}
	if sinks.Resources != nil {
		if err := sinks.Resources(surface.Resources); err != nil {
			return err
		}
	}
	if sinks.Changed != nil {
		sinks.Changed(surface)
	}
	_ = ctx
	return nil
}

// diagnoseApp records one degradation for an app and audits it.
func (r *Registry) diagnoseApp(ctx context.Context, app mcp.App, kind, message string) {
	r.addDiagnostic(Diagnostic{
		At:          r.now(),
		Server:      app.Server,
		AppID:       app.AppID,
		ResourceURI: safeURI(app.ResourceURI),
		Kind:        kind,
		Message:     bounded(message, 300),
	})
	result := "error"
	if kind == "cache" {
		result = "refused"
	}
	// The audit detail keeps the reason but never a raw query string: a
	// server-supplied URI travels through the safe form only.
	r.record(ctx, AuditEvent{
		Server:      app.Server,
		AppID:       app.AppID,
		ResourceURI: safeURI(app.ResourceURI),
		Action:      "document." + kind,
		Result:      result,
		Detail:      sanitizeDetail(message, app.ResourceURI),
	})
}

func (r *Registry) addDiagnostic(diagnostic Diagnostic) {
	r.mu.Lock()
	r.diagnose = append(r.diagnose, diagnostic)
	if len(r.diagnose) > maxDiagnostics {
		r.diagnose = append([]Diagnostic(nil), r.diagnose[len(r.diagnose)-maxDiagnostics:]...)
	}
	r.mu.Unlock()
}

// record writes one audit event to the structured log and, when wired, to the
// Auditor seam.
func (r *Registry) record(ctx context.Context, event AuditEvent) {
	event.TenantID = r.options.TenantID
	level := slog.LevelInfo
	if event.Result != "ok" {
		level = slog.LevelWarn
	}
	r.logger.Log(ctx, level, "mcp app audit",
		"tenant", event.TenantID.String(),
		"server", event.Server,
		"app", event.AppID,
		"resource", event.ResourceURI,
		"action", event.Action,
		"result", event.Result,
		"detail", event.Detail)
	if r.options.Auditor != nil {
		r.options.Auditor.RecordMCPApp(ctx, event)
	}
}

// dedupeApps keeps one entry per derived appId, in appId order, and derives the
// identity itself: the registry never trusts a caller-supplied appId (the
// catalogue rule is "appId = derived identity", design §3.1). The manager
// already dedupes by (server, resourceUri); this is the registry's own guard
// against a future caller handing it a repeated or mislabelled app.
func dedupeApps(apps []mcp.App) []mcp.App {
	seen := make(map[string]bool, len(apps))
	out := make([]mcp.App, 0, len(apps))
	for _, app := range apps {
		if app.Server == "" || app.ResourceURI == "" {
			continue
		}
		app.AppID = mcp.AppIDFor(app.Server, app.ResourceURI)
		if seen[app.AppID] {
			continue
		}
		seen[app.AppID] = true
		out = append(out, app)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AppID < out[j].AppID })
	return out
}

// appTools projects the tool references and their visibility union (deterministic
// order: model before app).
func appTools(app mcp.App) ([]ToolRef, []string) {
	tools := make([]ToolRef, 0, len(app.Tools))
	union := make([]string, 0, 2)
	hasModel, hasApp := false, false
	for _, tool := range app.Tools {
		visibility := append([]string(nil), tool.Visibility...)
		tools = append(tools, ToolRef{Tool: tool.Tool, Visibility: visibility})
		for _, value := range visibility {
			if value == mcp.VisibilityModel {
				hasModel = true
			}
			if value == mcp.VisibilityApp {
				hasApp = true
			}
		}
	}
	if hasModel {
		union = append(union, mcp.VisibilityModel)
	}
	if hasApp {
		union = append(union, mcp.VisibilityApp)
	}
	return tools, union
}

// titleFor derives a display title: the first referencing tool's title, or the
// resource URI. A resource's own title would need resources/list (P3).
func titleFor(app mcp.App, server *mcp.Server) string {
	for _, ref := range app.Tools {
		if server != nil {
			if tool, ok := server.Tool(ref.Tool); ok && tool.Title != "" {
				return tool.Title
			}
		}
	}
	return app.ResourceURI
}

// documentBytes extracts the document bytes and enforces the MCP Apps document
// contract again at the registry boundary (defense in depth: a custom ReadApp
// seam must satisfy the same rules as the client validator).
func documentBytes(resource mcp.AppResource, max int64) ([]byte, error) {
	if resource.MIMEType != mcp.AppMIMEType {
		return nil, fmt.Errorf("declared mimeType %q, want %q", resource.MIMEType, mcp.AppMIMEType)
	}
	if len(resource.Text) > 0 && len(resource.Blob) > 0 {
		return nil, errors.New("the document carries both text and blob")
	}
	var bytes []byte
	if len(resource.Blob) > 0 {
		bytes = resource.Blob
	} else {
		bytes = []byte(resource.Text)
	}
	if len(bytes) == 0 {
		return nil, errors.New("the document is empty")
	}
	if int64(len(bytes)) > max {
		return nil, fmt.Errorf("the document is %d bytes, over the %d-byte limit", len(bytes), max)
	}
	return bytes, nil
}

// contentHash is the document identity used in the URL and the cache key.
func contentHash(bytes []byte) string {
	sum := sha256.Sum256(bytes)
	return hex.EncodeToString(sum[:])
}

// safeURI strips a URI's query and fragment and bounds it: the catalog and the
// audit trail must never carry a full URL query string (spec「审计」).
func safeURI(uri string) string {
	if index := strings.IndexAny(uri, "?#"); index >= 0 {
		uri = uri[:index]
	}
	return bounded(uri, 256)
}

// sanitizeDetail replaces the raw resource URI inside a message with its safe
// form and bounds the result: transport/validator errors quote the URI, and a
// query string must not reach the audit trail.
func sanitizeDetail(message, uri string) string {
	if uri != "" {
		message = strings.ReplaceAll(message, uri, safeURI(uri))
	}
	return bounded(message, 300)
}

// bounded removes control characters and cuts at a rune boundary.
func bounded(value string, max int) string {
	var builder strings.Builder
	count := 0
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r == '\u2028' || r == '\u2029' {
			continue
		}
		size := len(string(r))
		if count+size > max {
			builder.WriteString("…")
			break
		}
		builder.WriteRune(r)
		count += size
	}
	return builder.String()
}
