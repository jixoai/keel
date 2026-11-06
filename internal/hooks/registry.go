package hooks

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// Handler is an in-process hook. Returning an error applies the registration's
// error policy; returning a Decision applies the event's decision semantics.
type Handler func(ctx context.Context, event Event) (Decision, error)

// Registration describes one hook subscription.
type Registration struct {
	// Name identifies the hook in audit records and error reports. It defaults
	// to hook-<n>.
	Name string
	// Event is one of the PI event names (see KnownEvent).
	Event string
	// Handler is invoked for every dispatch of Event, in registration order.
	Handler Handler
	// Observer marks a non-blocking hook: it is invoked in order but its
	// decision is ignored and its error never affects the run. Observer
	// failures are still audited and reported.
	Observer bool
	// Async runs the hook in a new goroutine and does not wait for it. It is
	// allowed only for notification events; gate/transform events must be
	// synchronous because their result is needed by the caller. An async hook
	// cannot gate and its decisions are ignored.
	Async bool
	// ErrorPolicy overrides DefaultErrorPolicy for this registration.
	ErrorPolicy ErrorPolicy
}

// Option mutates a Registration in On.
type Option func(*Registration)

// WithName sets the audit name of a hook.
func WithName(name string) Option {
	return func(r *Registration) { r.Name = name }
}

// AsObserver marks a hook as non-blocking (decision ignored, errors do not
// affect the run).
func AsObserver() Option {
	return func(r *Registration) { r.Observer = true }
}

// WithAsync runs the hook in its own goroutine and does not wait for it. Async
// hooks are only valid for notification events.
func WithAsync() Option {
	return func(r *Registration) { r.Async = true }
}

// WithErrorPolicy overrides the event's default error policy.
func WithErrorPolicy(policy ErrorPolicy) Option {
	return func(r *Registration) { r.ErrorPolicy = policy }
}

// RegistryOption configures a Registry.
type RegistryOption func(*Registry)

// WithAuditSink installs the audit sink at construction time.
func WithAuditSink(sink AuditSink) RegistryOption {
	return func(r *Registry) { r.audit = sink }
}

// WithStderrWriter installs the writer that receives command-hook stderr.
func WithStderrWriter(w io.Writer) RegistryOption {
	return func(r *Registry) { r.stderr = w }
}

// ErrorListener receives hook failures. Listeners run synchronously in the
// dispatching goroutine.
type ErrorListener func(HookError)

type registrationEntry struct {
	reg    Registration
	id     uint64
	active bool
}

type errorListenerEntry struct {
	fn     ErrorListener
	active bool
}

// Registry holds hook registrations and dispatches events. It is safe for
// concurrent use: registrations may be added or removed while another
// goroutine dispatches, and a dispatch always works on a snapshot.
type Registry struct {
	mu      sync.Mutex
	nextID  uint64
	byEvent map[string][]*registrationEntry

	auditMu sync.Mutex
	audit   AuditSink

	stderrMu sync.Mutex
	stderr   io.Writer

	errMu        sync.Mutex
	errListeners []*errorListenerEntry
}

// NewRegistry returns an empty registry.
func NewRegistry(opts ...RegistryOption) *Registry {
	r := &Registry{byEvent: make(map[string][]*registrationEntry)}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// SetAuditSink installs (or clears, with nil) the audit sink.
func (r *Registry) SetAuditSink(sink AuditSink) {
	r.auditMu.Lock()
	r.audit = sink
	r.auditMu.Unlock()
}

// SetStderrWriter installs (or clears, with nil) the writer that receives
// command-hook stderr.
func (r *Registry) SetStderrWriter(w io.Writer) {
	r.stderrMu.Lock()
	r.stderr = w
	r.stderrMu.Unlock()
}

func (r *Registry) stderrWriter() io.Writer {
	r.stderrMu.Lock()
	defer r.stderrMu.Unlock()
	return r.stderr
}

// OnError registers an error listener and returns an unsubscribe function.
func (r *Registry) OnError(listener ErrorListener) func() {
	entry := &errorListenerEntry{fn: listener, active: true}
	r.errMu.Lock()
	r.errListeners = append(r.errListeners, entry)
	r.errMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.errMu.Lock()
			entry.active = false
			r.errMu.Unlock()
		})
	}
}

// Add registers a hook and returns an idempotent unsubscribe function.
func (r *Registry) Add(reg Registration) (func(), error) {
	if reg.Event == "" || !KnownEvent(reg.Event) {
		return nil, fmt.Errorf("hooks: unknown event %q", reg.Event)
	}
	if reg.Handler == nil {
		return nil, fmt.Errorf("hooks: %s registration has no handler", reg.Event)
	}
	if reg.ErrorPolicy != "" &&
		reg.ErrorPolicy != ErrorPolicyFailOpen &&
		reg.ErrorPolicy != ErrorPolicyFailClosed &&
		reg.ErrorPolicy != ErrorPolicyIgnore {
		return nil, fmt.Errorf("hooks: unknown error policy %q", reg.ErrorPolicy)
	}
	if reg.Async && !IsNotificationEvent(reg.Event) {
		return nil, fmt.Errorf("hooks: async is only valid for notification events, got %q", reg.Event)
	}

	r.mu.Lock()
	r.nextID++
	entry := &registrationEntry{reg: reg, id: r.nextID, active: true}
	if entry.reg.Name == "" {
		entry.reg.Name = fmt.Sprintf("hook-%d", entry.id)
	}
	r.byEvent[reg.Event] = append(r.byEvent[reg.Event], entry)
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			entry.active = false
			r.mu.Unlock()
		})
	}, nil
}

// On registers an in-process handler with options. It panics on an invalid
// event or nil handler, which is a programmer error at startup.
func (r *Registry) On(event string, handler Handler, opts ...Option) func() {
	reg := Registration{Event: event, Handler: handler}
	for _, opt := range opts {
		opt(&reg)
	}
	unsubscribe, err := r.Add(reg)
	if err != nil {
		panic(err)
	}
	return unsubscribe
}

// HasHandlers reports whether any active handler is registered for event.
func (r *Registry) HasHandlers(event string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, entry := range r.byEvent[event] {
		if entry.active {
			return true
		}
	}
	return false
}

// snapshot returns active registrations for event in registration order. The
// returned slice is independent of the registry: unsubscribing or registering
// during a dispatch does not affect it, matching PI's snapshot semantics.
func (r *Registry) snapshot(event string) []Registration {
	r.mu.Lock()
	entries := r.byEvent[event]
	out := make([]Registration, 0, len(entries))
	for _, entry := range entries {
		if entry.active {
			out = append(out, entry.reg)
		}
	}
	r.mu.Unlock()
	return out
}

// invocation is one handler call before auditing.
type invocation struct {
	decision Decision
	err      error
	duration time.Duration
}

// invoke calls one handler, timing it. It does not audit; dispatchers decide
// how the result is interpreted and then call record.
func (r *Registry) invoke(ctx context.Context, reg Registration, event Event) invocation {
	start := time.Now()
	decision, err := reg.Handler(ctx, event)
	return invocation{decision: decision, err: err, duration: time.Since(start)}
}

// record audits one invocation. decision must be the action actually applied.
func (r *Registry) record(reg Registration, event Event, inv invocation, action Action, reason string) {
	record := AuditRecord{
		Hook:      reg.Name,
		Event:     event.EventType(),
		Duration:  inv.duration,
		Decision:  action.Normalized(),
		Reason:    reason,
		Observer:  reg.Observer,
		Timestamp: time.Now(),
	}
	if inv.err != nil {
		record.Error = inv.err.Error()
	}
	r.auditMu.Lock()
	defer r.auditMu.Unlock()
	if r.audit == nil {
		return
	}
	r.audit.Record(record)
}

// reportError notifies every active error listener in registration order.
func (r *Registry) reportError(hookErr HookError) {
	r.errMu.Lock()
	listeners := make([]ErrorListener, 0, len(r.errListeners))
	for _, entry := range r.errListeners {
		if entry.active {
			listeners = append(listeners, entry.fn)
		}
	}
	r.errMu.Unlock()
	for _, listener := range listeners {
		listener(hookErr)
	}
}

// handleFailure applies a registration's error policy and reports the error.
// It returns the effective policy (observer hooks always fail open).
func (r *Registry) handleFailure(reg Registration, event Event, err error) ErrorPolicy {
	policy := reg.ErrorPolicy
	if policy == "" {
		policy = DefaultErrorPolicy(event.EventType())
	}
	if reg.Observer {
		policy = ErrorPolicyFailOpen
	}
	if policy != ErrorPolicyIgnore {
		r.reportError(HookError{Hook: reg.Name, Event: event.EventType(), Err: err})
	}
	return policy
}
