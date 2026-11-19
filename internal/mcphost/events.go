package mcphost

// MCP event routing (change feat-mcp-apps-events, P4.2–P4.4).
//
// The dispatcher classifies every server notification into exactly one of three
// destinations (design §4.1):
//
//   - transient:   progress and server logs never touch the catalogue or the
//                  event ledger; progress is routed per call by internal/mcp,
//                  logs go to the structured logger;
//   - catalogue:   list_changed / resources-updated mark the surface dirty and
//                  trigger one debounced full republish (generation++);
//   - call result: cancelled aborts the in-flight request in internal/mcp; the
//                  eventual tool.result is the only durable fact it produces.
//
// The ledger is deliberately absent from this file: notifications are not
// session facts, so no event kind is added and nothing here can write one
// (spec mcp-events「账本零新增 kind」).
//
// Failure semantics (spec「热更的 attempt 边界与失败保面」): a failed catalogue
// recompute keeps the previous tools (mcp.Manager.RefreshServer installs only a
// successful list), the registry keeps the previous app versions, and the
// failure is audited and visible on the server projection. A failed publish
// never empties the surface.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/mcp"
)

// DefaultDebounce is the list_changed coalescing window (design §4.4: 250ms).
const DefaultDebounce = 250 * time.Millisecond

// Dispatcher is the host seam internal/mcp calls: every notification and every
// legacy server-initiated request arrives here with the configured server name.
// *Events implements it.
type Dispatcher interface {
	Notification(ctx context.Context, server, method string, params json.RawMessage)
	ServerRequest(ctx context.Context, server string, request *mcp.Request) (mcp.Message, error)
}

// SurfaceChange is one successful (or failed) catalogue republish, as the hook
// event and the audit trail report it.
type SurfaceChange struct {
	// Servers are the servers whose catalogue was recomputed.
	Servers []string
	// Kinds are the changed categories (tools|prompts|resources), bounded.
	Kinds []string
	// Added and Removed are the tool-name changes, bounded by the catalogue.
	Added   []string
	Removed []string
	// Generation is the published surface generation after this change.
	Generation int64
	// Err is the recompute failure, nil when the surface advanced.
	Err error
}

// EventsOptions configures the dispatcher.
type EventsOptions struct {
	// Fleet is the connected surface the full recompute reads. It may be set
	// after construction with SetFleet when the manager is built later.
	Fleet Fleet
	// Registry, when set, publishes the app catalogue on every recompute and
	// owns the generation. Nil keeps an internal generation (a deployment
	// without the apps surface).
	Registry *Registry
	// Auditor, when set, receives the catalogue.refresh rows. Nil keeps the
	// structured log only.
	Auditor Auditor
	Logger  *slog.Logger
	Metrics *Metrics
	// Debounce coalesces list_changed bursts (default DefaultDebounce).
	Debounce time.Duration
	// After is the debounce timer (tests inject a manual channel). Nil uses
	// time.After.
	After func(time.Duration) <-chan time.Time
	// OnSurfaceChanged is the hook emitter (mcp.surface.changed). Nil means the
	// deployment has no hook consumer.
	OnSurfaceChanged func(SurfaceChange)
	// ListenFor decides which servers get a modern subscriptions/listen stream.
	// Nil means "only a modern era connection" (protocol version >= 2026-07-28):
	// legacy servers keep their GET stream, exactly as before P4.
	ListenFor func(server *mcp.Server) bool
	// ListenBackoff is the delay before the next listen attempt. Nil uses 1s
	// doubling to 30s.
	ListenBackoff func(attempt int) time.Duration
	// Sleep waits for d; false stops the listener (tests inject a fake clock).
	Sleep func(ctx context.Context, d time.Duration) bool
	// ListenOptions is passed to every subscriptions/listen.
	ListenOptions mcp.ListenOptions
}

// Events is the process-wide MCP dispatcher: one instance per host, one listen
// stream per server (never per session; design §4.3).
type Events struct {
	options EventsOptions
	logger  *slog.Logger

	mu         sync.Mutex
	refreshMu  sync.Mutex
	fleet      Fleet
	dirty      map[string]map[string]bool
	timer      <-chan time.Time
	pendingGen int64
	generation int64
	errors     map[string]string
	lastChange SurfaceChange

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed bool
}

// NewEvents builds a dispatcher. Nothing runs until Start.
func NewEvents(options EventsOptions) *Events {
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.Debounce <= 0 {
		options.Debounce = DefaultDebounce
	}
	if options.After == nil {
		options.After = time.After
	}
	if options.ListenBackoff == nil {
		options.ListenBackoff = defaultListenBackoff
	}
	if options.Sleep == nil {
		options.Sleep = sleepContext
	}
	return &Events{
		options: options,
		logger:  options.Logger,
		fleet:   options.Fleet,
		dirty:   make(map[string]map[string]bool),
		errors:  make(map[string]string),
	}
}

// SetFleet installs the manager once it exists (the service factory builds the
// dispatcher before the connections so the callbacks it hands Connect can
// already point here).
func (e *Events) SetFleet(fleet Fleet) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.fleet = fleet
	e.mu.Unlock()
}

// Notification classifies one server notification. It runs on the MCP receive
// loop, so every path is non-blocking: the recompute is asynchronous and the
// log/metric paths are bounded.
func (e *Events) Notification(ctx context.Context, server, method string, params json.RawMessage) {
	if e == nil {
		return
	}
	switch method {
	case "notifications/progress":
		// Transient: internal/mcp already routed the frame to the call that owns
		// the token. Nothing durable, nothing catalogue.
		return
	case "notifications/message":
		e.logServerMessage(ctx, server, params)
		return
	case "notifications/tools/list_changed":
		e.MarkDirty(server, "tools")
		return
	case "notifications/prompts/list_changed":
		e.MarkDirty(server, "prompts")
		return
	case "notifications/resources/list_changed":
		e.MarkDirty(server, "resources")
		return
	case "notifications/resources/updated":
		if uri := resourceUpdatedURI(params); uri != "" && e.options.Registry != nil {
			e.options.Registry.InvalidateResource(server, uri)
		}
		e.MarkDirty(server, "resources")
		return
	case "notifications/cancelled":
		// internal/mcp aborted the in-flight call; the tool.result is the
		// durable outcome and it is written by the run path, not here.
		e.logger.Info("mcp request cancelled by server", "server", server)
		return
	case "notifications/subscriptions/acknowledged":
		// A tagged ack is consumed by the listen stream. An untagged one reached
		// the global callback: a protocol violation worth reporting.
		e.logger.Warn("mcp subscription acknowledgement outside a listen stream", "server", server)
		return
	default:
		e.logger.Warn("mcp notification ignored: unknown method", "server", server, "method", bounded(method, 128))
	}
}

// ServerRequest answers a legacy server-initiated request. P4 wires the seam but
// implements no handler: MRTR execution (sampling/elicitation/roots) is P5, and
// an unimplemented request must fail closed with -32601 rather than be silently
// dropped (spec「legacy 主动请求与能力声明一致」).
func (e *Events) ServerRequest(_ context.Context, server string, request *mcp.Request) (mcp.Message, error) {
	if e != nil {
		e.logger.Warn("mcp server request refused: not implemented",
			"server", server, "method", bounded(request.Method, 128))
	}
	return nil, fmt.Errorf("mcp: server request %q is not implemented", request.Method)
}

// MarkDirty records one changed category of one server and schedules the
// debounced full recompute. Bursts of any number of notifications within the
// window produce one republish.
func (e *Events) MarkDirty(server, kind string) {
	if e == nil || server == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dirty[server] == nil {
		e.dirty[server] = make(map[string]bool, 3)
	}
	e.dirty[server][kind] = true
	if e.timer != nil {
		return
	}
	e.pendingGen++
	generation := e.pendingGen
	timer := e.options.After(e.options.Debounce)
	e.timer = timer
	go func() {
		<-timer
		e.flushGeneration(context.Background(), generation)
	}()
}

// pending reports the currently dirty servers and kinds (tests and shutdown).
func (e *Events) pending() map[string][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return copyDirtyLocked(e.dirty)
}

func copyDirtyLocked(dirty map[string]map[string]bool) map[string][]string {
	out := make(map[string][]string, len(dirty))
	for server, kinds := range dirty {
		list := make([]string, 0, len(kinds))
		for _, kind := range []string{"tools", "prompts", "resources"} {
			if kinds[kind] {
				list = append(list, kind)
			}
		}
		out[server] = list
	}
	return out
}

// flushGeneration runs the debounced recompute, unless Flush or a newer timer
// already superseded it.
func (e *Events) flushGeneration(ctx context.Context, generation int64) {
	e.mu.Lock()
	if e.closed || e.pendingGen != generation || e.timer == nil {
		e.mu.Unlock()
		return
	}
	dirty := copyDirtyLocked(e.dirty)
	e.dirty = make(map[string]map[string]bool)
	e.timer = nil
	e.mu.Unlock()
	e.refresh(ctx, dirty)
}

// Flush runs any pending debounced recompute synchronously. A no-op when
// nothing is dirty; it is the shutdown-safe/deterministic-test form.
func (e *Events) Flush(ctx context.Context) {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.closed || len(e.dirty) == 0 {
		e.mu.Unlock()
		return
	}
	dirty := copyDirtyLocked(e.dirty)
	e.dirty = make(map[string]map[string]bool)
	e.timer = nil
	e.pendingGen++
	e.mu.Unlock()
	e.refresh(ctx, dirty)
}

// RefreshNow recomputes the named servers' catalogues immediately (the
// reconnect/resubscribe path: changes during the gap have no replay, so the
// only recovery is a full recompute). An empty list recomputes everything.
func (e *Events) RefreshNow(ctx context.Context, servers []string) {
	if e == nil {
		return
	}
	fleet := e.currentFleet()
	dirty := make(map[string][]string, len(servers))
	if len(servers) == 0 && fleet != nil {
		for _, server := range fleet.Servers() {
			dirty[server.Name()] = []string{"tools", "prompts", "resources"}
		}
	}
	for _, server := range servers {
		dirty[server] = []string{"tools", "prompts", "resources"}
	}
	e.refresh(ctx, dirty)
}

func (e *Events) currentFleet() Fleet {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.fleet
}

// NoteReconnect is the legacy GET-stream counterpart of the listen
// supervisor's reconnect: it counts the reconnect and recomputes the whole
// catalogue (the stream has no replay).
func (e *Events) NoteReconnect(ctx context.Context, server string) {
	if e == nil {
		return
	}
	e.options.Metrics.ListenReconnect()
	e.RefreshNow(ctx, []string{server})
}

// refresh performs one full recompute: per-server tools/list (only for servers
// whose tools changed), then one registry publish, then generation, hook and
// audit. The publish is whole-surface: the registry composes tools, resources,
// apps and prompt sections together.
func (e *Events) refresh(ctx context.Context, dirty map[string][]string) {
	// One recompute at a time: a reconnect-triggered refresh and a debounced
	// list_changed must not interleave generation and audit sequences.
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()
	fleet := e.currentFleet()
	if fleet == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	change := SurfaceChange{Servers: sortedServers(dirty)}
	change.Kinds = unionKinds(dirty)
	var firstErr error
	for _, server := range change.Servers {
		if !contains(dirty[server], "tools") {
			continue
		}
		diff, err := e.refreshServerTools(ctx, fleet, server)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			e.recordServerError(server, err)
			// The per-server failure is an audited fact, not just a log line; the
			// aggregate row below records the publish outcome.
			e.audit(ctx, AuditEvent{Server: server, Action: "catalogue.list", Result: "error",
				Detail: bounded(err.Error(), 300)})
			continue
		}
		e.clearServerError(server)
		change.Added = append(change.Added, diff.Added...)
		change.Removed = append(change.Removed, diff.Removed...)
	}
	change.Added = boundedUnique(change.Added, 256)
	change.Removed = boundedUnique(change.Removed, 256)

	generation, err := e.publish(ctx, fleet)
	if err != nil && firstErr == nil {
		firstErr = err
	}
	change.Generation = generation
	change.Err = firstErr
	e.mu.Lock()
	e.lastChange = change
	e.mu.Unlock()

	if e.options.Metrics != nil {
		if firstErr == nil {
			e.options.Metrics.SurfaceGeneration(generation)
		}
		connected, disconnected := 0, 0
		for _, server := range fleet.Servers() {
			if server.Err == nil && server.Client != nil {
				connected++
			} else {
				disconnected++
			}
		}
		e.options.Metrics.ServerStates(connected, disconnected)
	}

	result := "ok"
	if firstErr != nil {
		result = "error"
	}
	e.audit(ctx, AuditEvent{
		Action: "catalogue.refresh",
		Result: result,
		Detail: bounded(fmt.Sprintf("generation=%d servers=%v kinds=%v added=%d removed=%d err=%v",
			generation, change.Servers, change.Kinds, len(change.Added), len(change.Removed), firstErr), 512),
	})
	if firstErr == nil && e.options.OnSurfaceChanged != nil {
		e.options.OnSurfaceChanged(change)
	}
}

// refreshServerTools refreshes one server's catalogue through the manager seam.
func (e *Events) refreshServerTools(ctx context.Context, fleet Fleet, server string) (mcp.ToolDiff, error) {
	refresher, ok := fleet.(interface {
		RefreshServer(context.Context, string) (mcp.ToolDiff, error)
	})
	if !ok {
		return mcp.ToolDiff{}, nil
	}
	return refresher.RefreshServer(ctx, server)
}

// publish refreshes the app catalogue (one generation) and returns the current
// generation. Without a registry the dispatcher owns a simple counter.
func (e *Events) publish(ctx context.Context, fleet Fleet) (int64, error) {
	if e.options.Registry != nil {
		surface, err := e.options.Registry.Refresh(ctx, fleet)
		return surface.Generation, err
	}
	e.mu.Lock()
	e.generation++
	generation := e.generation
	e.mu.Unlock()
	return generation, nil
}

// Generation is the last published generation (registry-backed when wired).
func (e *Events) Generation() int64 {
	if e == nil {
		return 0
	}
	if e.options.Registry != nil {
		return e.options.Registry.Generation()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.generation
}

// LastChange is the most recent recompute outcome.
func (e *Events) LastChange() SurfaceChange {
	if e == nil {
		return SurfaceChange{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastChange
}

// Errors reports the last per-server recompute failures (visible through the
// server projection and this accessor).
func (e *Events) Errors() map[string]string {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]string, len(e.errors))
	for server, message := range e.errors {
		out[server] = message
	}
	return out
}

func (e *Events) recordServerError(server string, err error) {
	e.mu.Lock()
	e.errors[server] = bounded(err.Error(), 512)
	e.mu.Unlock()
}

func (e *Events) clearServerError(server string) {
	e.mu.Lock()
	delete(e.errors, server)
	e.mu.Unlock()
}

// logServerMessage writes one notifications/message row. The level is a closed
// set (unknown → info), the logger name and the data payload are bounded, and
// no message body is ever copied here: this is server-side logging, not
// chat content.
func (e *Events) logServerMessage(ctx context.Context, server string, params json.RawMessage) {
	var payload struct {
		Level  string          `json:"level"`
		Logger string          `json:"logger"`
		Data   json.RawMessage `json:"data"`
	}
	if len(params) == 0 || json.Unmarshal(params, &payload) != nil {
		e.logger.Warn("mcp server log without a decodable payload", "server", server)
		return
	}
	level := slog.LevelInfo
	switch payload.Level {
	case "debug":
		level = slog.LevelDebug
	case "info", "":
		level = slog.LevelInfo
	case "warning", "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	data := ""
	if len(payload.Data) > 0 {
		data = bounded(string(payload.Data), 256)
	}
	e.logger.Log(ctx, level, "mcp server log",
		"server", server,
		"mcp_level", bounded(payload.Level, 16),
		"mcp_logger", bounded(payload.Logger, 64),
		"data", data)
}

func (e *Events) audit(ctx context.Context, event AuditEvent) {
	if event.TenantID == uuid.Nil && e.options.Registry != nil {
		event.TenantID = e.options.Registry.TenantID()
	}
	level := slog.LevelInfo
	if event.Result != "ok" {
		level = slog.LevelWarn
	}
	e.logger.Log(ctx, level, "mcp catalogue audit",
		"server", event.Server,
		"action", event.Action,
		"result", event.Result,
		"detail", event.Detail)
	if e.options.Auditor != nil {
		e.options.Auditor.RecordMCPApp(ctx, event)
	}
}

// --- listen supervision ---------------------------------------------------

// Start launches one subscriptions/listen supervisor per server the predicate
// selects. Legacy servers keep the GET stream (their notifications already
// reach Notification through the injected client callback).
func (e *Events) Start(ctx context.Context) {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.cancel != nil || e.closed {
		e.mu.Unlock()
		return
	}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.mu.Unlock()
	fleet := e.currentFleet()
	if fleet == nil {
		return
	}
	// Publish the boot state onto the gauges: the initial surface generation is
	// the registry's, not zero.
	if e.options.Metrics != nil {
		e.options.Metrics.SurfaceGeneration(e.Generation())
		connected, disconnected := 0, 0
		for _, server := range fleet.Servers() {
			if server.Err == nil && server.Client != nil {
				connected++
			} else {
				disconnected++
			}
		}
		e.options.Metrics.ServerStates(connected, disconnected)
	}
	for _, server := range fleet.Servers() {
		if !e.listenFor(server) {
			continue
		}
		server := server
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.supervise(e.ctx, server)
		}()
	}
}

// Stop cancels every listener and waits for the goroutines.
func (e *Events) Stop() {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	cancel := e.cancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	e.wg.Wait()
}

func (e *Events) listenFor(server *mcp.Server) bool {
	if server == nil || server.Client == nil || server.Err != nil {
		return false
	}
	if e.options.ListenFor != nil {
		return e.options.ListenFor(server)
	}
	return server.Client.ProtocolVersion() >= "2026-07-28"
}

// supervise keeps one server's listen stream alive: subscribe, forward frames,
// and on any end back off and subscribe again (the server keeps no state, so
// the only recovery is a new listen plus a full recompute).
func (e *Events) supervise(ctx context.Context, server *mcp.Server) {
	attempt := 0
	iterations := 0
	for ctx.Err() == nil {
		// Every loop after the first is a reconnect: either the stream ended
		// (the server keeps no state, so a new listen is the only recovery) or
		// the previous attempt failed.
		if iterations > 0 {
			e.options.Metrics.ListenReconnect()
		}
		iterations++
		params := mcp.ListenParams{Notifications: mcp.ListenFilter{
			ToolsListChanged:      true,
			PromptsListChanged:    true,
			ResourcesListChanged:  true,
			ResourceSubscriptions: e.resourceSubscriptions(server),
		}}
		handle, err := server.Client.Listen(ctx, params, e.options.ListenOptions)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			e.audit(ctx, AuditEvent{Server: server.Name(), Action: "subscription.listen", Result: "error",
				Detail: bounded(err.Error(), 300)})
			e.logger.Warn("mcp listen failed", "server", server.Name(), "err", err)
			if !e.options.Sleep(ctx, e.options.ListenBackoff(attempt)) {
				return
			}
			attempt++
			continue
		}
		for _, missing := range handle.Ack.Missing {
			// A category requested but not accepted is a diagnostic, never a
			// silent assumption (spec「订阅主体与 modern listen 契约」).
			e.logger.Warn("mcp listen category not accepted", "server", server.Name(), "category", missing)
		}
		e.audit(ctx, AuditEvent{Server: server.Name(), Action: "subscription.listen", Result: "ok",
			Detail: fmt.Sprintf("subscription=%s", bounded(handle.Ack.SubscriptionID, 64))})
		// A subscription that just (re)established has no replay: recompute the
		// whole catalogue once so changes during the gap are picked up.
		e.RefreshNow(ctx, []string{server.Name()})
		attempt = 0
		if e.consume(ctx, server, handle) {
			return
		}
		if !e.options.Sleep(ctx, e.options.ListenBackoff(0)) {
			return
		}
	}
}

// consume forwards one established stream's frames until it ends or the
// context closes. It reports whether the supervisor must stop entirely.
func (e *Events) consume(ctx context.Context, server *mcp.Server, handle *mcp.ListenHandle) (stop bool) {
	defer handle.Close()
	for {
		select {
		case event := <-handle.Events:
			e.Notification(ctx, server.Name(), event.Method, event.Params)
		case err := <-handle.Done:
			e.audit(ctx, AuditEvent{Server: server.Name(), Action: "subscription.listen", Result: "closed",
				Detail: bounded(fmt.Sprint(err), 300)})
			return false
		case <-ctx.Done():
			return true
		}
	}
}

// resourceSubscriptions is the minimal resource subscription set: only the
// app documents of this server (empty when the registry knows none).
func (e *Events) resourceSubscriptions(server *mcp.Server) []string {
	if e.options.Registry == nil || server == nil {
		return nil
	}
	surface, ok := e.options.Registry.Surface()
	if !ok {
		return nil
	}
	var uris []string
	for _, app := range surface.Apps {
		if app.Server == server.Name() {
			uris = append(uris, app.ResourceURI)
		}
	}
	return uris
}

func defaultListenBackoff(attempt int) time.Duration {
	delay := time.Second
	for i := 0; i < attempt && delay < 30*time.Second; i++ {
		delay *= 2
	}
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return delay
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

// resourceUpdatedURI extracts the uri of notifications/resources/updated.
func resourceUpdatedURI(params json.RawMessage) string {
	var payload struct {
		URI string `json:"uri"`
	}
	if len(params) == 0 || json.Unmarshal(params, &payload) != nil {
		return ""
	}
	return payload.URI
}

// sortedServers orders the dirty map for deterministic audit/hook payloads.
func sortedServers(dirty map[string][]string) []string {
	servers := make([]string, 0, len(dirty))
	for server := range dirty {
		servers = append(servers, server)
	}
	for i := 1; i < len(servers); i++ {
		for j := i; j > 0 && servers[j] < servers[j-1]; j-- {
			servers[j], servers[j-1] = servers[j-1], servers[j]
		}
	}
	return servers
}

func unionKinds(dirty map[string][]string) []string {
	seen := make(map[string]bool, 3)
	var out []string
	for _, kind := range []string{"tools", "prompts", "resources"} {
		for _, kinds := range dirty {
			if contains(kinds, kind) {
				if !seen[kind] {
					seen[kind] = true
					out = append(out, kind)
				}
				break
			}
		}
	}
	return out
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// boundedUnique sorts, dedupes and bounds a tool-name list for the hook/audit
// payload (a hostile server cannot grow the payload without limit).
func boundedUnique(values []string, max int) []string {
	if len(values) == 0 {
		return nil
	}
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	out := sorted[:0]
	last := ""
	for _, value := range sorted {
		value = bounded(value, 128)
		if value == last {
			continue
		}
		out = append(out, value)
		last = value
		if len(out) >= max {
			break
		}
	}
	return out
}

// Compile-time assertion: the dispatcher is what the manager injects.
var _ Dispatcher = (*Events)(nil)
