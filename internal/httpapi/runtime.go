// Runtime control surface: the admin routes that answer "who is this runtime"
// and start a drain in an explicit mode (the operator trigger of the handoff
// protocol), plus the mode-aware HTTP drain gate.
//
// The gate is where the two drain modes differ observably: a handoff drain
// keeps admitting the client-input endpoints (教材: 排水不丢新输入), while a
// shutdown drain answers 503 + Retry-After for everything but probes and
// metrics. Handoff never reuses shutdown's reject-input branch.
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jixoai/keel/internal/handoff"
)

// RuntimeControl is the runtime-level control the admin routes drive.
// *handoff.Coordinator satisfies it.
type RuntimeControl interface {
	RuntimeID() string
	LoadoutID() string
	LoadoutRevision() int64
	Draining() bool
	Claiming() bool
	DrainMode() handoff.Mode
	DrainDeadline() time.Time
	InFlight() int
	BeginDrain(ctx context.Context, mode handoff.Mode, deadline time.Duration) (handoff.Status, error)
}

// drainState is the mode-aware drain state of a ModeDrainer.
type drainState struct {
	mode      handoff.Mode
	deadline  time.Time
	startedAt time.Time
}

// ModeDrainer is the mode-aware drainer: the graceful SIGTERM drainer plus the
// drain mode the HTTP gate and the SSE drain frames report. It implements
// Drainer, so it drops into Config.Drain unchanged.
type ModeDrainer struct {
	*GracefulDrainer
	state atomic.Value // drainState
}

var _ Drainer = (*ModeDrainer)(nil)

// NewModeDrainer builds a drainer with the given SSE reconnect hint and close
// rate.
func NewModeDrainer(retryAfterMS, closePerSecond int) *ModeDrainer {
	d := &ModeDrainer{GracefulDrainer: NewDrainer(retryAfterMS, closePerSecond)}
	d.state.Store(drainState{})
	return d
}

// BeginMode starts the drain (idempotent) and records the mode and deadline.
// The first mode wins: a runtime that is already draining does not change mode.
func (d *ModeDrainer) BeginMode(mode handoff.Mode, deadline time.Duration) {
	started := time.Now()
	next := drainState{mode: mode, deadline: started.Add(deadline), startedAt: started}
	cur := d.state.Load().(drainState)
	if cur.mode == "" {
		d.state.Store(next)
	}
	d.Begin()
}

// BeginShutdown starts a shutdown drain (the SIGTERM path).
func (d *ModeDrainer) BeginShutdown(deadline time.Duration) {
	d.BeginMode(handoff.ModeShutdown, deadline)
}

// BeginHandoff starts a handoff drain (the upgrade path).
func (d *ModeDrainer) BeginHandoff(deadline time.Duration) {
	d.BeginMode(handoff.ModeHandoff, deadline)
}

// DrainMode returns the mode of the active drain ("" when not draining).
func (d *ModeDrainer) DrainMode() handoff.Mode {
	return d.state.Load().(drainState).mode
}

// DrainDeadline returns the drain deadline (zero when not draining).
func (d *ModeDrainer) DrainDeadline() time.Time {
	return d.state.Load().(drainState).deadline
}

// DrainStartedAt returns when the drain began (zero when not draining).
func (d *ModeDrainer) DrainStartedAt() time.Time {
	return d.state.Load().(drainState).startedAt
}

// AdmitsEdgeWrites reports whether the edge-scoped input endpoints stay open.
// True while not draining, and during a handoff drain only.
func (d *ModeDrainer) AdmitsEdgeWrites() bool {
	if !d.Draining() {
		return true
	}
	return d.DrainMode().AdmitsEdgeWrites()
}

// runtimeControl returns the configured control, or nil.
func (s *Server) runtimeControl() RuntimeControl { return s.cfg.Control }

// modeDrainer returns the mode-aware drainer when the configured drainer is
// one; nil otherwise (a plain Drainer keeps the shutdown-only behavior).
func (s *Server) modeDrainer() *ModeDrainer {
	d, _ := s.cfg.Drain.(*ModeDrainer)
	return d
}

// edgeAdmission is the narrow interface the drain gate needs from a
// mode-aware drainer.
type edgeAdmission interface {
	DrainMode() handoff.Mode
	AdmitsEdgeWrites() bool
}

// drainAdmits reports whether a request may proceed while draining. Probes and
// metrics always pass (the orchestrator observes the drain through them), and
// so does the runtime control route: an operator must be able to watch a drain
// in progress and to ask for one. During a handoff drain the client-input
// endpoints also pass; during shutdown nothing else does.
func (s *Server) drainAdmits(r *http.Request) bool {
	if drainExempt(r.URL.Path) || runtimeControlPath(r.URL.Path) {
		return true
	}
	if s.cfg.Drain == nil || !s.cfg.Drain.Draining() {
		return true
	}
	gate, ok := s.cfg.Drain.(edgeAdmission)
	if !ok || !gate.AdmitsEdgeWrites() {
		return false
	}
	return edgeWritePath(r.Method, r.URL.Path)
}

// runtimeControlPath reports the runtime admin routes, which stay reachable
// while draining (they are admin-authenticated).
func runtimeControlPath(path string) bool {
	switch path {
	case "/v1/runtime/status", "/v1/runtime/drain":
		return true
	}
	return false
}

// edgeWritePath reports the client-input endpoints that keep accepting writes
// during a handoff drain: the idempotent input path and the edge event append
// (which only accepts user.message). "Start executing" requests are not edge
// writes: a draining runtime no longer starts runs.
func edgeWritePath(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	if !strings.HasPrefix(path, "/v1/sessions/") {
		return false
	}
	rest := strings.TrimPrefix(path, "/v1/sessions/")
	id, tail, ok := strings.Cut(rest, "/")
	if !ok || id == "" {
		return false
	}
	switch tail {
	case "input", "events":
		return true
	}
	return false
}

func (s *Server) registerRuntimeRoutes() {
	s.mux.HandleFunc("GET /v1/runtime/status", s.runtimeStatus)
	s.mux.HandleFunc("POST /v1/runtime/drain", s.runtimeDrain)
}

// runtimeStatus answers the runtime's identity, loadout pin and drain state.
func (s *Server) runtimeStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	ctl := s.runtimeControl()
	if ctl == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("runtime control is not configured"))
		return
	}
	writeJSON(w, http.StatusOK, s.runtimeStatusBody(ctl))
}

// runtimeDrain starts a drain in an explicit mode. The body is
// {"mode":"handoff"|"shutdown","deadlineMs":30000}.
func (s *Server) runtimeDrain(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	ctl := s.runtimeControl()
	if ctl == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("runtime control is not configured"))
		return
	}
	var body struct {
		Mode       string `json:"mode"`
		DeadlineMS int64  `json:"deadlineMs"`
	}
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	mode := handoff.Mode(strings.TrimSpace(body.Mode))
	if !mode.Valid() {
		writeErr(w, http.StatusBadRequest, errors.New("mode must be shutdown or handoff"))
		return
	}
	// A missing deadline is the runtime's default (the process decides: see
	// cmd/runtime's -handoff-window), not the HTTP layer's.
	deadline := time.Duration(body.DeadlineMS) * time.Millisecond
	if _, err := ctl.BeginDrain(r.Context(), mode, deadline); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, handoff.ErrAlreadyDraining) {
			status = http.StatusConflict
		}
		writeErr(w, status, err)
		return
	}
	// Wake the SSE drainer with the mode so open streams flush, hint a
	// reconnect and close at the configured rate, and so the HTTP gate knows
	// which mode it is in.
	if md := s.modeDrainer(); md != nil {
		md.BeginMode(mode, deadline)
	}
	s.cfg.Logger.Info("runtime drain started", "runtime_id", ctl.RuntimeID(),
		"mode", string(mode), "deadline_ms", deadline.Milliseconds(),
		"in_flight_runs", ctl.InFlight())
	writeJSON(w, http.StatusOK, s.runtimeStatusBody(ctl))
}

// runtimeStatusBody renders the control's snapshot. The drain source of truth
// for the gate is the drainer; the coordinator answers the identity fields.
func (s *Server) runtimeStatusBody(ctl RuntimeControl) map[string]any {
	body := map[string]any{
		"runtimeId":       ctl.RuntimeID(),
		"loadoutId":       ctl.LoadoutID(),
		"loadoutRevision": ctl.LoadoutRevision(),
		"draining":        ctl.Draining(),
		"claiming":        ctl.Claiming(),
		"inFlightRuns":    ctl.InFlight(),
	}
	if md := s.modeDrainer(); md != nil && md.Draining() {
		body["drainMode"] = string(md.DrainMode())
		if deadline := md.DrainDeadline(); !deadline.IsZero() {
			body["deadline"] = deadline.UTC()
		}
		if started := md.DrainStartedAt(); !started.IsZero() {
			body["drainStartedAt"] = started.UTC()
		}
	} else if mode := ctl.DrainMode(); mode != "" {
		body["drainMode"] = string(mode)
		if deadline := ctl.DrainDeadline(); !deadline.IsZero() {
			body["deadline"] = deadline.UTC()
		}
	}
	return body
}
