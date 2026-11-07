package externalgw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/gateway"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/servicegraph"
)

// Backend connection states (BackendStatus.State).
const (
	backendConnecting = "connecting"
	backendReady      = "ready"
	backendDegraded   = "degraded"
	backendStopped    = "stopped"
)

// DefaultProjectionBudget bounds the commit attempts of one projected backend
// event. A projection that cannot be committed is dropped with a diagnostic;
// it never blocks the connection forever (the session keeps working, the
// missing projection is visible in logs/metrics).
const DefaultProjectionBudget = 3

// AgentService is the loadout's AgentBackend service: it owns every external
// agent connection, the session bookkeeping, and the event projection into the
// contract kernel. Its lifecycle follows the service graph (Start/Stop and
// effects in reverse order).
type AgentService struct {
	opts     Options
	config   Config
	log      *slog.Logger
	backends map[string]*backendRuntime

	// openLocks serializes Open per session scope inside this process (see the
	// openLocks type); the kernel's conditional upsert is the cross-process
	// guard.
	openLocks openLocks

	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
	wg     sync.WaitGroup
}

// openLocks is a keyed mutex set for the open path. Without it two concurrent
// Opens of one scope would both create a backend session before either marked
// the row, leaving an orphan backend session behind (the loser's handle is
// never recorded, so nothing would ever close it).
type openLocks struct {
	mu      sync.Mutex
	entries map[kernel.Scope]*openLockEntry
}

type openLockEntry struct {
	mu   sync.Mutex
	refs int
}

// lock acquires the scope's mutex and returns the release function.
func (l *openLocks) lock(scope kernel.Scope) func() {
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[kernel.Scope]*openLockEntry)
	}
	entry := l.entries[scope]
	if entry == nil {
		entry = &openLockEntry{}
		l.entries[scope] = entry
	}
	entry.refs++
	l.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.entries, scope)
		}
		l.mu.Unlock()
	}
}

var _ servicegraph.Instance = (*AgentService)(nil)

// NewAgentService resolves the tenant document and builds the service. It does
// not connect; Start owns every side effect.
func NewAgentService(doc host.ExternalConfig, opts Options) (*AgentService, error) {
	if opts.Committer == nil || opts.Sessions == nil {
		return nil, fmt.Errorf("externalgw: agent service requires the kernel committer and external-session seams")
	}
	config, err := ResolveConfig(doc, opts)
	if err != nil {
		return nil, err
	}
	if opts.Restart.MaxRestarts == 0 {
		opts.Restart = gateway.RestartPolicy{MaxRestarts: -1} // supervised until shutdown
	}
	service := &AgentService{
		opts:     opts,
		config:   config,
		log:      opts.logger(),
		backends: make(map[string]*backendRuntime, len(config.Backends)),
	}
	for _, backend := range config.Backends {
		runtime := &backendRuntime{
			name:     backend.Name,
			cfg:      backend,
			svc:      service,
			state:    backendConnecting,
			sessions: make(map[string]*sessionLink),
		}
		service.backends[backend.Name] = runtime
	}
	return service, nil
}

// Start launches every backend supervisor and registers one cleanup effect per
// backend. A provider that cannot connect does not fail activation: the
// service stays ACTIVE-degraded, the provider is retried with backoff, and its
// sessions answer backend-unavailable until it recovers.
func (s *AgentService) Start(ctx context.Context, effects *servicegraph.EffectScope) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	base, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.ctx, s.cancel = base, cancel
	s.mu.Unlock()

	for _, runtime := range s.backends {
		runtime := runtime
		// First attempt synchronously (bounded), so a healthy provider is
		// ready before boot logs "loadout active" and a broken one shows up in
		// the diagnostics immediately.
		attemptCtx, attemptCancel := context.WithTimeout(base, s.opts.connectTimeout())
		_ = runtime.tryConnect(attemptCtx)
		attemptCancel()

		runtimeCtx, runtimeCancel := context.WithCancel(base)
		runtime.cancel = runtimeCancel
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			runtime.supervise(runtimeCtx)
		}()
		if effects != nil {
			name := runtime.name
			if err := effects.Register("external-agent["+name+"]", func() error {
				runtime.shutdown(context.WithoutCancel(context.Background()))
				return nil
			}); err != nil {
				// The scope refused the registration; release this runtime here
				// so a failed Start leaves no supervisor behind (Start must not
				// leak effects outside the scope).
				runtime.shutdown(context.WithoutCancel(context.Background()))
				return err
			}
		}
	}
	for _, runtime := range s.backends {
		if runtime.ready() {
			s.log.Info("external agent backend ready", "backend", runtime.name, "transport", runtime.cfg.Transport)
		} else {
			s.log.Warn("external agent backend not ready; retrying with backoff",
				"backend", runtime.name, "transport", runtime.cfg.Transport, "error", runtime.lastError())
		}
	}
	return nil
}

// Stop cancels every supervisor, closes every connection (which takes the
// process group down for stdio providers) and waits for the goroutines with a
// bound so shutdown cannot hang on a wedged provider.
func (s *AgentService) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, runtime := range s.backends {
		runtime.shutdown(ctx)
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(15 * time.Second):
		s.log.Warn("external agent supervisors did not stop within 15s")
		return nil
	}
}

func (s *AgentService) enabled() bool {
	return s != nil && len(s.backends) > 0
}

// BackendStatuses returns one availability snapshot per configured backend.
func (s *AgentService) Backends() []BackendStatus {
	if s == nil {
		return nil
	}
	names := make([]string, 0, len(s.backends))
	for name := range s.backends {
		names = append(names, name)
	}
	sortStrings(names)
	out := make([]BackendStatus, 0, len(names))
	for _, name := range names {
		out = append(out, s.backends[name].status())
	}
	return out
}

// WaitReady waits until every backend has an active connection or ctx ends.
// It is a test/smoke helper; production code observes Backends().
func (s *AgentService) WaitReady(ctx context.Context) error {
	if s == nil {
		return ErrBackendUnavailable
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		allReady := true
		for _, backend := range s.backends {
			if !backend.ready() {
				allReady = false
				break
			}
		}
		if allReady {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Open opens an external session: the backend gets a session, Keel persists the
// marks (opening -> active) and subscribes to the event stream so every backend
// event is projected into Keel's log. A session that cannot be recorded is
// closed on the backend, so no orphan backend session is left behind.
func (s *AgentService) Open(ctx context.Context, scope kernel.Scope, backend string, request gateway.SessionOpenRequest) (SessionView, error) {
	if s == nil || !s.enabled() {
		return SessionView{}, fmt.Errorf("%w: no external backends are configured", ErrBackendUnavailable)
	}
	if scope.IsZero() {
		return SessionView{}, fmt.Errorf("externalgw: tenant and session are required")
	}
	runtime, ok := s.backends[backend]
	if !ok {
		return SessionView{}, fmt.Errorf("%w: %s", ErrBackendUnknown, backend)
	}
	// The critical section covers "check the marks → open on the backend → mark
	// the row", so a retry or a double-click cannot leave a second, unrecorded
	// backend session behind. The kernel upsert is the cross-process guard
	// (a different ref or backend is refused), this lock keeps the fast path
	// from ever creating the orphan.
	release := s.openLocks.lock(scope)
	defer release()

	// A session may only be promoted while it is pristine; a session that is
	// already external belongs to a backend session and is not reopened under a
	// second backend handle.
	if existing, err := s.opts.Sessions.ExternalSession(ctx, scope); err == nil {
		if existing.StateOwner == kernel.StateOwnerExternal {
			return SessionView{}, fmt.Errorf("%w: %s/%s", ErrAlreadyExternal, scope.TenantID, scope.SessionID)
		}
	} else if !errors.Is(err, kernel.ErrNotFound) {
		return SessionView{}, err
	}
	if err := s.checkSessionLimit(ctx, scope.TenantID, runtime); err != nil {
		return SessionView{}, err
	}
	client, err := runtime.clientOrErr()
	if err != nil {
		return SessionView{}, err
	}
	session, err := client.Open(ctx, request)
	if err != nil {
		return SessionView{}, runtime.mapError("session.open", err)
	}
	// The tenant policy decides whether an invisible-usage session may exist at
	// all; it is never silently admitted.
	if session.Metering == gateway.MeteringNone && s.config.Unmetered != host.ExternalUnmeteredAllow {
		_ = client.Close(context.WithoutCancel(ctx), gateway.CloseRequest{SessionID: session.SessionID, Reason: "unmetered policy"})
		return SessionView{}, fmt.Errorf("%w (backend %s reports metering=none)", ErrUnmeteredDenied, backend)
	}
	marks := kernel.ExternalMarks{
		Backend:  backend,
		Ref:      session.SessionID,
		Status:   kernel.ExternalStatusOpening,
		Metering: string(session.Metering),
		Recovery: session.Recovery,
	}
	if _, err := s.opts.Sessions.OpenExternalSession(ctx, scope, marks); err != nil {
		_ = client.Close(context.WithoutCancel(ctx), gateway.CloseRequest{SessionID: session.SessionID, Reason: "open failed"})
		return SessionView{}, err
	}
	link := runtime.addLink(scope, session)
	if _, err := client.EventStream(ctx, gateway.EventStreamRequest{SessionID: session.SessionID, FromSeq: 0}); err != nil {
		runtime.removeLink(session.SessionID)
		_ = s.opts.Sessions.SetExternalStatus(context.WithoutCancel(ctx), scope, kernel.ExternalStatusClosed)
		_ = client.Close(context.WithoutCancel(ctx), gateway.CloseRequest{SessionID: session.SessionID, Reason: "subscribe failed"})
		return SessionView{}, runtime.mapError("event.stream", err)
	}
	if err := s.opts.Sessions.SetExternalStatus(ctx, scope, kernel.ExternalStatusActive); err != nil {
		runtime.removeLink(session.SessionID)
		_ = client.Close(context.WithoutCancel(ctx), gateway.CloseRequest{SessionID: session.SessionID, Reason: "activate failed"})
		return SessionView{}, err
	}
	link.setStatus(kernel.ExternalStatusActive)
	return s.view(ctx, scope, link, runtime), nil
}

func (s *AgentService) checkSessionLimit(ctx context.Context, tenantID uuid.UUID, runtime *backendRuntime) error {
	// The effective cap is the tighter of the tenant-wide and provider-specific
	// limits (0 means unset).
	limit := 0
	for _, candidate := range []int{runtime.cfg.MaxSessions, s.config.MaxSessionsPerBackend} {
		if candidate <= 0 {
			continue
		}
		if limit == 0 || candidate < limit {
			limit = candidate
		}
	}
	if limit <= 0 {
		return nil
	}
	sessions, err := s.opts.Sessions.ListExternalSessions(ctx, tenantID)
	if err != nil {
		return err
	}
	open := 0
	for _, session := range sessions {
		if session.Backend == runtime.name && session.Status != kernel.ExternalStatusClosed {
			open++
		}
	}
	if open >= limit {
		return fmt.Errorf("%w: backend %s has %d open sessions (limit %d)", ErrSessionLimit, runtime.name, open, limit)
	}
	return nil
}

// Get answers one external session's marks plus the live connection state. A
// native session answers ErrNotExternal; a backend that died upgrades the marks
// to backend-unavailable before answering, so the terminal state is queryable.
func (s *AgentService) Get(ctx context.Context, scope kernel.Scope) (SessionView, error) {
	if s == nil || len(s.backends) == 0 {
		return SessionView{}, fmt.Errorf("%w: no external backends are configured", ErrBackendUnavailable)
	}
	if scope.IsZero() {
		return SessionView{}, fmt.Errorf("externalgw: tenant and session are required")
	}
	session, err := s.opts.Sessions.ExternalSession(ctx, scope)
	if err != nil {
		return SessionView{}, err
	}
	if session.StateOwner != kernel.StateOwnerExternal {
		return SessionView{}, fmt.Errorf("%w: %s/%s", ErrNotExternal, scope.TenantID, scope.SessionID)
	}
	runtime, ok := s.backends[session.Backend]
	if !ok {
		return SessionView{}, fmt.Errorf("%w: %s", ErrBackendUnknown, session.Backend)
	}
	link := runtime.link(session.Ref)
	status := session.Status
	if status != kernel.ExternalStatusClosed && !runtime.ready() {
		status = kernel.ExternalStatusUnavailable
		if session.Status != kernel.ExternalStatusUnavailable {
			if err := s.opts.Sessions.SetExternalStatus(ctx, scope, kernel.ExternalStatusUnavailable); err != nil {
				s.log.Warn("mark external session unavailable", "backend", session.Backend, "error", err)
			}
		}
	}
	if link != nil {
		link.setStatus(status)
	}
	return s.view(ctx, scope, link, runtime), nil
}

// List answers a tenant's external sessions, newest first.
func (s *AgentService) List(ctx context.Context, tenantID uuid.UUID) ([]SessionView, error) {
	if s == nil || len(s.backends) == 0 {
		return nil, fmt.Errorf("%w: no external backends are configured", ErrBackendUnavailable)
	}
	sessions, err := s.opts.Sessions.ListExternalSessions(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]SessionView, 0, len(sessions))
	for _, session := range sessions {
		runtime := s.backends[session.Backend]
		var link *sessionLink
		if runtime != nil {
			link = runtime.link(session.Ref)
		}
		out = append(out, s.viewFor(session, link, runtime))
	}
	return out, nil
}

// Send hands one message to the backend. The backend's own events are projected
// separately; the send itself is recorded as an external.event once the backend
// has accepted it, so the Keel-side transcript shows what was asked.
func (s *AgentService) Send(ctx context.Context, scope kernel.Scope, message json.RawMessage, requestID string) (SendResult, error) {
	link, runtime, err := s.ensureLink(ctx, scope)
	if err != nil {
		return SendResult{}, err
	}
	status := link.sessionStatus()
	if status == kernel.ExternalStatusClosed {
		return SendResult{}, ErrSessionClosed
	}
	client, err := runtime.clientOrErr()
	if err != nil {
		return SendResult{}, err
	}
	result, err := client.Send(ctx, gateway.MessageSendRequest{SessionID: link.ref, Message: message})
	if err != nil {
		return SendResult{}, runtime.mapError("message.send", err)
	}
	if requestID == "" {
		requestID = uuid.NewString()
	}
	recorded := s.recordSend(ctx, runtime, link, message, requestID, result.TurnID)
	return SendResult{TurnID: result.TurnID, Recorded: recorded}, nil
}

// Cancel cancels the session's current turn on the backend. A backend that
// ignores cancellation still answers the protocol; the call does not hang.
func (s *AgentService) Cancel(ctx context.Context, scope kernel.Scope, request CancelRequest) error {
	link, runtime, err := s.ensureLink(ctx, scope)
	if err != nil {
		return err
	}
	if link.sessionStatus() == kernel.ExternalStatusClosed {
		return ErrSessionClosed
	}
	client, err := runtime.clientOrErr()
	if err != nil {
		return err
	}
	if err := client.Cancel(ctx, gateway.CancelRequest{SessionID: link.ref, TurnID: request.TurnID, Reason: request.Reason}); err != nil {
		return runtime.mapError("cancel", err)
	}
	return nil
}

// Close closes one external session. The local terminal state is closed even
// when the backend is already gone: the user asked to end it, and a query must
// keep answering closed.
func (s *AgentService) Close(ctx context.Context, scope kernel.Scope, reason string) error {
	if s == nil || len(s.backends) == 0 {
		return fmt.Errorf("%w: no external backends are configured", ErrBackendUnavailable)
	}
	session, err := s.opts.Sessions.ExternalSession(ctx, scope)
	if err != nil {
		return err
	}
	if session.StateOwner != kernel.StateOwnerExternal {
		return fmt.Errorf("%w: %s/%s", ErrNotExternal, scope.TenantID, scope.SessionID)
	}
	if session.Status == kernel.ExternalStatusClosed {
		return nil
	}
	runtime := s.backends[session.Backend]
	if runtime != nil {
		if client, err := runtime.clientOrErr(); err == nil && session.Ref != "" {
			_ = client.Close(ctx, gateway.CloseRequest{SessionID: session.Ref, Reason: reason})
		}
		runtime.removeLink(session.Ref)
	}
	if err := s.opts.Sessions.SetExternalStatus(ctx, scope, kernel.ExternalStatusClosed); err != nil {
		return err
	}
	return nil
}

// ensureLink resolves a Keel session to a live backend session link, resuming
// the backend session when this process has no in-memory record of it (a
// runtime restart, or a session opened by another process). Resumption is
// backend-defined: resumed=false leaves the session backend-unavailable, and
// Keel never claims a native recovery.
func (s *AgentService) ensureLink(ctx context.Context, scope kernel.Scope) (*sessionLink, *backendRuntime, error) {
	if s == nil || len(s.backends) == 0 {
		return nil, nil, fmt.Errorf("%w: no external backends are configured", ErrBackendUnavailable)
	}
	if scope.IsZero() {
		return nil, nil, fmt.Errorf("externalgw: tenant and session are required")
	}
	session, err := s.opts.Sessions.ExternalSession(ctx, scope)
	if err != nil {
		return nil, nil, err
	}
	if session.StateOwner != kernel.StateOwnerExternal {
		return nil, nil, fmt.Errorf("%w: %s/%s", ErrNotExternal, scope.TenantID, scope.SessionID)
	}
	if session.Status == kernel.ExternalStatusClosed {
		return nil, nil, ErrSessionClosed
	}
	runtime, ok := s.backends[session.Backend]
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s", ErrBackendUnknown, session.Backend)
	}
	if link := runtime.link(session.Ref); link != nil {
		// A live link is used as is. A backend-unavailable link is not a dead
		// end: fall through to the resume path below, which either revives it
		// or returns a definite unavailable error.
		if link.sessionStatus() != kernel.ExternalStatusUnavailable {
			return link, runtime, nil
		}
	}
	client, err := runtime.clientOrErr()
	if err != nil {
		return nil, nil, err
	}
	link := runtime.addLink(scope, gateway.ExternalSession{
		SessionID: session.Ref,
		Backend:   session.Backend,
		Status:    session.Status,
		Metering:  gateway.Metering(session.Metering),
		Recovery:  session.Recovery,
	})
	resumed, err := client.Resume(ctx, gateway.SessionResumeRequest{SessionID: session.Ref})
	if err != nil {
		runtime.removeLink(session.Ref)
		return nil, nil, runtime.mapError("session.resume", err)
	}
	if !resumed.Resumed {
		runtime.removeLink(session.Ref)
		link.setStatus(kernel.ExternalStatusUnavailable)
		_ = s.opts.Sessions.SetExternalStatus(context.WithoutCancel(ctx), scope, kernel.ExternalStatusUnavailable)
		return nil, nil, fmt.Errorf("%w: backend %s did not resume the session", ErrBackendUnavailable, session.Backend)
	}
	if _, err := client.EventStream(ctx, gateway.EventStreamRequest{SessionID: session.Ref, FromSeq: resumed.LastSeq}); err != nil {
		runtime.removeLink(session.Ref)
		return nil, nil, runtime.mapError("event.stream", err)
	}
	if err := s.opts.Sessions.SetExternalStatus(ctx, scope, kernel.ExternalStatusActive); err != nil {
		runtime.removeLink(session.Ref)
		return nil, nil, err
	}
	link.setStatus(kernel.ExternalStatusActive)
	return link, runtime, nil
}

// recordSend writes the outgoing message as an external.event. A failure only
// logs: the backend accepted the message, and the projection is not the call's
// acknowledgement.
func (s *AgentService) recordSend(ctx context.Context, runtime *backendRuntime, link *sessionLink, message json.RawMessage, requestID, turnID string) bool {
	payload, err := json.Marshal(map[string]any{
		"backend":       runtime.name,
		"externalRef":   link.ref,
		"stateOwner":    kernel.StateOwnerExternal,
		"compatibility": true,
		"direction":     "send",
		"requestId":     requestID,
		"turnId":        turnID,
		"message":       json.RawMessage(message),
	})
	if err != nil {
		s.log.Warn("encode external send record", "backend", runtime.name, "error", err)
		return false
	}
	_, err = s.opts.Committer.Commit(ctx, kernel.CommitRequest{
		Scope:     link.scope,
		Kind:      kernel.KindExternalEvent,
		RequestID: "send:" + requestID,
		Payload:   payload,
	})
	if err != nil {
		s.log.Warn("project external send", "backend", runtime.name, "session", link.scope.SessionID, "error", err)
		return false
	}
	return true
}

// view renders the current marks of one session.
func (s *AgentService) view(ctx context.Context, scope kernel.Scope, link *sessionLink, runtime *backendRuntime) SessionView {
	session, err := s.opts.Sessions.ExternalSession(ctx, scope)
	if err != nil {
		return SessionView{SessionID: scope.SessionID.String()}
	}
	return s.viewFor(session, link, runtime)
}

func (s *AgentService) viewFor(session kernel.ExternalSession, link *sessionLink, runtime *backendRuntime) SessionView {
	view := SessionView{
		SessionID:      session.Scope.SessionID.String(),
		Backend:        session.Backend,
		ExternalRef:    session.Ref,
		StateOwner:     session.StateOwner,
		Status:         session.Status,
		Metering:       session.Metering,
		Recovery:       session.Recovery,
		Compatible:     session.CompatMode(),
		NativeRecovery: false,
		CreatedAt:      session.CreatedAt,
		UpdatedAt:      session.UpdatedAt,
	}
	if runtime != nil {
		view.Connection = runtime.connectionState()
		if view.Status != kernel.ExternalStatusClosed && runtime != nil && !runtime.ready() {
			view.Status = kernel.ExternalStatusUnavailable
		}
	}
	if link != nil {
		if status := link.sessionStatus(); status != "" && view.Status != kernel.ExternalStatusClosed {
			view.Status = status
		}
		if usage := link.usageSummary(); usage != nil {
			view.Usage = usage
		}
	}
	return view
}

// --- backend runtime ------------------------------------------------------

type backendRuntime struct {
	name string
	cfg  ProviderConfig
	svc  *AgentService

	mu         sync.Mutex
	client     *gateway.AgentClient
	conn       *gateway.Conn
	transport  gateway.Transport
	state      string
	lastErr    string
	attempts   int
	generation int64
	sessions   map[string]*sessionLink

	cancel context.CancelFunc
}

type sessionLink struct {
	mu      sync.Mutex
	scope   kernel.Scope
	ref     string
	status  string
	lastSeq int64
	usage   UsageSummary
}

func (l *sessionLink) setStatus(status string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.status = status
	l.mu.Unlock()
}

func (l *sessionLink) sessionStatus() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.status
}

func (l *sessionLink) noteEvent(event gateway.AgentEvent) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if event.Sequence > l.lastSeq {
		l.lastSeq = event.Sequence
	}
	if event.Usage == nil {
		return
	}
	l.usage.Metering = string(event.Usage.Metering)
	if event.Usage.Model != "" {
		l.usage.Model = event.Usage.Model
	}
	l.usage.InputTokens += event.Usage.InputTokens
	l.usage.OutputTokens += event.Usage.OutputTokens
	l.usage.TotalTokens += event.Usage.TotalTokens
}

func (l *sessionLink) usageSummary() *UsageSummary {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.usage.Metering == "" || l.usage.Metering == string(gateway.MeteringNone) {
		return nil
	}
	usage := l.usage
	return &usage
}

func (b *backendRuntime) addLink(scope kernel.Scope, session gateway.ExternalSession) *sessionLink {
	link := &sessionLink{scope: scope, ref: session.SessionID, status: session.Status}
	if link.status == "" {
		link.status = kernel.ExternalStatusActive
	}
	b.mu.Lock()
	if b.sessions == nil {
		b.sessions = make(map[string]*sessionLink)
	}
	b.sessions[session.SessionID] = link
	b.mu.Unlock()
	return link
}

func (b *backendRuntime) link(ref string) *sessionLink {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[ref]
}

func (b *backendRuntime) removeLink(ref string) {
	b.mu.Lock()
	delete(b.sessions, ref)
	b.mu.Unlock()
}

func (b *backendRuntime) links() []*sessionLink {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*sessionLink, 0, len(b.sessions))
	for _, link := range b.sessions {
		out = append(out, link)
	}
	return out
}

func (b *backendRuntime) ready() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.client != nil && b.state == backendReady
}

func (b *backendRuntime) connectionState() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case backendReady:
		return "ok"
	case backendConnecting:
		return "connecting"
	case backendDegraded:
		return "degraded"
	default:
		return "unavailable"
	}
}

func (b *backendRuntime) lastError() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastErr
}

func (b *backendRuntime) clientOrErr() (*gateway.AgentClient, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client == nil || b.state != backendReady {
		detail := b.lastErr
		if detail == "" {
			detail = b.state
		}
		return nil, fmt.Errorf("%w: %s (%s)", ErrBackendUnavailable, b.name, detail)
	}
	return b.client, nil
}

func (b *backendRuntime) status() BackendStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	status := BackendStatus{
		Name:     b.name,
		State:    b.state,
		Health:   b.state,
		Error:    b.lastErr,
		Sessions: len(b.sessions),
	}
	if b.conn != nil {
		status.Health = string(b.conn.Health().State)
	}
	return status
}

// tryConnect establishes one connection and, on success, installs it and starts
// the bridge. It is safe to call from the supervisor after a disconnection.
func (b *backendRuntime) tryConnect(ctx context.Context) error {
	transport, conn, handshake, err := b.dial(ctx)
	if err != nil {
		b.noteFailure(err)
		return err
	}
	client := gateway.NewAgentBackend(conn)
	b.mu.Lock()
	b.transport = transport
	b.conn = conn
	b.client = client
	b.state = backendReady
	b.lastErr = ""
	b.attempts = 0
	b.generation++
	generation := b.generation
	b.mu.Unlock()
	b.svc.log.Info("external agent backend connected",
		"backend", b.name, "provider", handshake.ServerInfo.Name,
		"version", handshake.ServerInfo.Version, "state_owner", string(handshake.Provider.StateOwner),
		"metering", string(handshake.Provider.Metering))
	go b.bridge(b.svc.context(), client, generation)
	b.resumeLinks(b.svc.context(), client)
	return nil
}

// dial builds the transport and performs the handshake. The gateway rejects an
// incompatible revision, a wrong provider kind and missing capabilities; a
// failure here leaves the connection unactivated.
func (b *backendRuntime) dial(ctx context.Context) (gateway.Transport, *gateway.Conn, gateway.Handshake, error) {
	var (
		transport gateway.Transport
		err       error
	)
	switch b.cfg.Transport {
	case host.ExternalTransportStdio:
		stdio, spawnErr := gateway.SpawnStdio(gateway.SpawnOptions{
			Command: b.cfg.Command,
			Args:    b.cfg.Args,
			Dir:     b.cfg.Dir,
			Env:     b.cfg.Env,
			Stderr:  b.svc.opts.stderr(),
		})
		if spawnErr != nil {
			return nil, nil, gateway.Handshake{}, spawnErr
		}
		transport = stdio
	case host.ExternalTransportHTTP:
		transport = gateway.NewHTTPTransport(gateway.HTTPTransportOptions{
			URL:     b.cfg.URL,
			Headers: b.cfg.Headers,
			Client:  b.svc.opts.HTTPClient,
		})
	default:
		return nil, nil, gateway.Handshake{}, fmt.Errorf("externalgw: unsupported transport %q", b.cfg.Transport)
	}
	conn := gateway.NewConn(gateway.ConnOptions{
		ClientInfo:      gateway.Implementation{Name: "keel-runtime", Version: "1.0.0"},
		AcceptProviders: []gateway.ProviderType{gateway.ProviderTypeAgentBackend},
		RequestTimeout:  b.cfg.Timeout,
		EventsBuffer:    16,
		OnCall: func(record gateway.CallRecord) {
			// Identifiers and statuses only: CallRecord has no payload field by
			// construction, so an audit can never accidentally log a message.
			b.svc.log.Info("external call", "backend", b.name,
				"provider", record.Provider, "method", record.Method, "target", record.Target,
				"status", record.Status, "error_code", record.ErrorCode,
				"duration_ms", record.Duration.Milliseconds())
		},
		OnDiagnostic: func(diag gateway.Diagnostic) {
			b.svc.log.Warn("external diagnostic", "backend", b.name, "kind", diag.Kind, "detail", diag.Detail)
		},
		OnHealth: func(info gateway.HealthInfo) {
			b.svc.log.Info("external health", "backend", b.name, "state", string(info.State), "reason", info.Reason)
		},
	})
	handshake, err := conn.Connect(ctx, transport)
	if err != nil {
		_ = transport.Close()
		return nil, nil, gateway.Handshake{}, err
	}
	if handshake.Provider.Type != gateway.ProviderTypeAgentBackend {
		_ = conn.Close()
		return nil, nil, gateway.Handshake{}, fmt.Errorf("externalgw: provider %s is %q, want agent_backend", b.name, handshake.Provider.Type)
	}
	return transport, conn, handshake, nil
}

func (b *backendRuntime) noteFailure(err error) {
	b.mu.Lock()
	b.state = backendDegraded
	b.lastErr = err.Error()
	b.attempts++
	b.mu.Unlock()
	b.svc.log.Warn("external agent backend connect failed", "backend", b.name, "error", err)
}

// supervise keeps one backend alive: it waits for the connection to end, marks
// the sessions unavailable, and reconnects with the bounded backoff policy
// until the service stops or the budget is exhausted.
func (b *backendRuntime) supervise(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			b.shutdown(context.Background())
			return
		}
		b.mu.Lock()
		conn := b.conn
		ready := b.client != nil && b.state == backendReady
		attempts := b.attempts
		b.mu.Unlock()
		if ready && conn != nil {
			select {
			case <-ctx.Done():
				b.shutdown(context.Background())
				return
			case <-conn.Done():
			}
			b.handleDisconnect()
			continue
		}
		// Not connected: apply the restart policy.
		attempts++
		if !b.svc.opts.Restart.Allowed(attempts) {
			b.mu.Lock()
			b.state = backendDegraded
			b.mu.Unlock()
			b.svc.log.Error("external agent backend restart budget exhausted", "backend", b.name)
			<-ctx.Done()
			b.shutdown(context.Background())
			return
		}
		b.mu.Lock()
		b.attempts = attempts
		b.mu.Unlock()
		delay := b.svc.opts.Restart.Delay(attempts)
		b.svc.log.Info("external agent backend restarting", "backend", b.name, "attempt", attempts, "delay", delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			b.shutdown(context.Background())
			return
		case <-timer.C:
		}
		connectCtx, cancel := context.WithTimeout(ctx, b.svc.opts.connectTimeout())
		err := b.tryConnect(connectCtx)
		cancel()
		if err == nil {
			b.mu.Lock()
			b.attempts = 0
			b.mu.Unlock()
		}
	}
}

// handleDisconnect marks every non-closed session of this backend unavailable.
// No event is written and no native recovery is claimed: the marks are the
// queryable terminal state the spec asks for.
func (b *backendRuntime) handleDisconnect() {
	b.mu.Lock()
	conn := b.conn
	b.client = nil
	b.conn = nil
	b.transport = nil
	b.state = backendDegraded
	if conn != nil {
		terminal := conn.Terminal()
		if terminal != nil {
			b.lastErr = terminal.Error()
		}
	}
	b.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	links := b.links()
	if len(links) == 0 {
		return
	}
	s := b.svc
	byTenant := make(map[uuid.UUID][]*sessionLink)
	for _, link := range links {
		link.mu.Lock()
		status := link.status
		link.mu.Unlock()
		if status == kernel.ExternalStatusClosed {
			continue
		}
		link.setStatus(kernel.ExternalStatusUnavailable)
		byTenant[link.scope.TenantID] = append(byTenant[link.scope.TenantID], link)
	}
	for tenant, tenantLinks := range byTenant {
		n, err := s.opts.Sessions.MarkBackendUnavailable(context.WithoutCancel(context.Background()), tenant, b.name)
		if err != nil {
			s.log.Warn("mark external backend unavailable", "backend", b.name, "tenant", tenant, "error", err)
			continue
		}
		s.log.Warn("external backend unavailable", "backend", b.name, "tenant", tenant,
			"sessions", len(tenantLinks), "rows", n)
	}
}

// resumeLinks asks the backend to resume each known session after a reconnect.
// resumed=false is terminal (backend-unavailable), not a claim of recovery.
func (b *backendRuntime) resumeLinks(ctx context.Context, client *gateway.AgentClient) {
	for _, link := range b.links() {
		link.mu.Lock()
		status := link.status
		fromSeq := link.lastSeq
		ref := link.ref
		link.mu.Unlock()
		if status == kernel.ExternalStatusClosed {
			continue
		}
		resumed, err := client.Resume(ctx, gateway.SessionResumeRequest{SessionID: ref, FromSeq: fromSeq})
		if err != nil || !resumed.Resumed {
			link.setStatus(kernel.ExternalStatusUnavailable)
			_ = b.svc.opts.Sessions.SetExternalStatus(context.WithoutCancel(ctx), link.scope, kernel.ExternalStatusUnavailable)
			continue
		}
		if _, err := client.EventStream(ctx, gateway.EventStreamRequest{SessionID: ref, FromSeq: fromSeq}); err != nil {
			link.setStatus(kernel.ExternalStatusUnavailable)
			continue
		}
		if err := b.svc.opts.Sessions.SetExternalStatus(ctx, link.scope, kernel.ExternalStatusActive); err == nil {
			link.setStatus(kernel.ExternalStatusActive)
		}
	}
}

// bridge projects the backend's event stream into the kernel's external lane.
// The commit is synchronous, so a slow database stops this goroutine, stops
// draining the gateway's bounded event channel, and the backpressure reaches
// the provider's write - no unbounded buffering in Keel.
func (b *backendRuntime) bridge(ctx context.Context, client *gateway.AgentClient, generation int64) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-client.Events():
			if !ok {
				return
			}
			b.mu.Lock()
			current := b.generation
			b.mu.Unlock()
			if current != generation {
				return
			}
			b.project(ctx, event)
		}
	}
}

func (b *backendRuntime) project(ctx context.Context, event gateway.AgentEvent) {
	link := b.link(event.SessionID)
	if link == nil {
		b.svc.log.Debug("drop backend event for an unknown session", "backend", b.name, "ref", event.SessionID)
		return
	}
	payload, err := json.Marshal(externalEventPayload(b.name, link.ref, event))
	if err != nil {
		b.svc.log.Warn("encode external event", "backend", b.name, "ref", link.ref, "error", err)
		return
	}
	requestID := b.name + ":" + link.ref + ":" + eventIdentity(event)
	req := kernel.CommitRequest{
		Scope:     link.scope,
		Kind:      kernel.KindExternalEvent,
		RequestID: requestID,
		Payload:   payload,
	}
	var lastErr error
	for attempt := 0; attempt < DefaultProjectionBudget; attempt++ {
		_, err := b.svc.opts.Committer.Commit(ctx, req)
		if err == nil {
			lastErr = nil
			link.noteEvent(event)
			return
		}
		lastErr = err
		if !retryableProjectionError(err) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(50*(attempt+1)) * time.Millisecond):
		}
	}
	b.svc.log.Warn("drop external event projection", "backend", b.name, "ref", link.ref,
		"seq", event.Sequence, "error", lastErr)
}

// retryableProjectionError reports a projection failure worth retrying: a
// transient store failure or a quota refusal (which may clear on the next
// attempt window). A shape/provenance rejection is permanent and dropped.
func retryableProjectionError(err error) bool {
	return errors.Is(err, kernel.ErrStoreFailure) || errors.Is(err, kernel.ErrQuota)
}

func eventIdentity(event gateway.AgentEvent) string {
	if event.Sequence > 0 {
		return fmt.Sprintf("%d", event.Sequence)
	}
	// A backend that never assigns sequence numbers gets a unique local
	// identity; replay dedupe is then backend-defined (documented).
	return uuid.NewString()
}

// externalEventPayload is the durable shape of one projected backend fact: the
// provenance marks are mandatory (the kernel validates them) and direction
// distinguishes what Keel sent from what the backend produced.
func externalEventPayload(backend, ref string, event gateway.AgentEvent) map[string]any {
	payload := map[string]any{
		"backend":       backend,
		"externalRef":   ref,
		"stateOwner":    kernel.StateOwnerExternal,
		"compatibility": true,
		"direction":     "receive",
		"seq":           event.Sequence,
		"type":          event.Type,
	}
	if event.TurnID != "" {
		payload["turnId"] = event.TurnID
	}
	if len(event.Data) > 0 {
		payload["data"] = json.RawMessage(event.Data)
	}
	if event.Usage != nil {
		payload["usage"] = event.Usage
	}
	return payload
}

// shutdown closes the backend connection and takes the provider's process tree
// down (stdio transport: close stdin, grace, SIGTERM/SIGKILL to the group). It
// also stops the supervisor, so a release that runs without Stop cannot leave a
// reconnect loop behind.
func (b *backendRuntime) shutdown(ctx context.Context) {
	b.mu.Lock()
	cancel := b.cancel
	conn := b.conn
	client := b.client
	b.client = nil
	b.conn = nil
	b.transport = nil
	b.state = backendStopped
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if client != nil {
		closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		_ = client.Close(closeCtx, gateway.CloseRequest{Reason: "runtime shutdown"})
		cancelClose()
	}
	if conn != nil {
		_ = conn.Close()
	}
}

// mapError adds backend context while keeping the gateway error types visible
// to the HTTP edge (errors.As still finds *gateway.TimeoutError etc.).
func (b *backendRuntime) mapError(call string, err error) error {
	if err == nil {
		return nil
	}
	var timeout *gateway.TimeoutError
	if errors.As(err, &timeout) {
		return fmt.Errorf("%w: backend %s %s timed out after %s", err, b.name, call, timeout.Timeout)
	}
	var unhealthy *gateway.UnhealthyError
	if errors.As(err, &unhealthy) {
		return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	return fmt.Errorf("externalgw: backend %s %s: %w", b.name, call, err)
}

// sortStrings is a tiny ordering helper kept local so the package has no
// dependency beyond the standard library and the runtime seams.
func sortStrings(values []string) { sort.Strings(values) }

// context returns the service's base context (nil before Start).
func (s *AgentService) context() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}
