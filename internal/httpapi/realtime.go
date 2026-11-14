// Realtime provider bridge: the browser-side WebSocket of a voice session.
//
// GET /v1/realtime upgrades the request (the usual cookie/passphrase
// middleware has already injected X-Tenant-ID), checks that the session belongs
// to the tenant, dials the configured provider with server-side credentials and
// proxies provider JSON events in both directions. The browser never sees the
// provider URL or key; the request may only name a session and an allowlisted
// model.

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/realtime"
)

// errRealtimeNotConfigured is the exact 503 body when no provider is wired:
// the UI shows its "未配置 Provider" state instead of a fake conversation.
var errRealtimeNotConfigured = errors.New("realtime provider is not configured")

// realtimeConnectTimeout bounds the outbound provider handshake so a hung
// provider cannot pin a browser socket forever.
const realtimeConnectTimeout = 15 * time.Second

// realtimeStatusResponse is the GET /v1/realtime/status body: the preflight
// facts a client needs to render a true configuration state before it opens the
// WebSocket (instead of surfacing an opaque WS 1006). It never carries the
// provider URL or key — only the protocol family, the allowlisted model names
// and the session default's sample rates.
type realtimeStatusResponse struct {
	Configured bool `json:"configured"`
	// Provider is the provider's readonly display name (the built-in WebSocket
	// provider names its protocol family).
	Provider string `json:"provider"`
	// Kind is the protocol family ("qwen", "openai" or "demo"); it decides the
	// session.update dialect and the browser's sample-rate defaults.
	Kind string `json:"kind"`
	// Models is the effective allowlist: the models a client may request.
	// Empty means the operator expressed no model policy.
	Models           []string `json:"models"`
	InputSampleRate  int      `json:"inputSampleRate"`
	OutputSampleRate int      `json:"outputSampleRate"`
}

// The KEEL_REALTIME_* variables the built-in provider reads. internal/realtime
// owns the parsing; they are mirrored here because the provider interface has no
// allowlist accessor, and the status endpoint must report the same policy the
// bridge enforces without exposing the URL or key.
const (
	realtimeStatusEnvWSURL  = "KEEL_REALTIME_WS_URL"
	realtimeStatusEnvModel  = "KEEL_REALTIME_MODEL"
	realtimeStatusEnvModels = "KEEL_REALTIME_MODELS"
)

// realtimeProvider builds the provider from the process environment. It is a
// variable so tests can substitute a provider without touching the environment;
// production always goes through realtime.FromEnv (KEEL_REALTIME_*).
var realtimeProvider = func() (realtime.RealtimeProvider, error) { return realtime.FromEnv() }

// realtime serves GET /v1/realtime?session_id=<uuid>&model=<id>.
func (s *Server) realtime(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("X-Tenant-ID is required"))
		return
	}
	sessionID, err := uuid.Parse(strings.TrimSpace(r.URL.Query().Get("session_id")))
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("bad session_id: %w", err))
		return
	}

	// Provider selection is server configuration only. Query parameters that
	// look like a provider URL or key are ignored by construction: only the
	// session and the model are read from the request.
	provider, err := realtimeProvider()
	if err != nil {
		level := slog.LevelWarn
		if errors.Is(err, realtime.ErrNotConfigured) {
			// A missing provider is the normal "voice is not configured" state,
			// not an operational fault; an invalid configuration still warns.
			level = slog.LevelDebug
		}
		s.cfg.Logger.Log(r.Context(), level, "realtime provider is not configured",
			"err", err,
			"request_id", RequestIDFrom(r.Context()))
		writeErr(w, http.StatusServiceUnavailable, errRealtimeNotConfigured)
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if validator, ok := provider.(interface{ ValidateModel(string) error }); ok {
		if err := validator.ValidateModel(model); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}

	// Tenant boundary: the same check the SSE stream performs, so a foreign or
	// missing session is a 404 before the upgrade rather than a live bridge. An
	// unwired read seam is 503, never an open bridge: without it a foreign
	// session id would be upgraded and billed against this tenant.
	if s.cfg.Events == nil {
		writeErr(w, http.StatusServiceUnavailable, errKernelUnwired)
		return
	}
	if _, err := s.cfg.Events.LoadEvents(r.Context(),
		kernel.Scope{TenantID: tenantID, SessionID: sessionID}, 0, 1); err != nil {
		writeEdgeError(w, err)
		return
	}

	client, err := realtime.Accept(w, r)
	if err != nil {
		// Accept has already written the HTTP error.
		s.cfg.Logger.Warn("realtime upgrade rejected",
			"err", err,
			"request_id", RequestIDFrom(r.Context()),
			"tenant_id", tenantID,
			"session_id", sessionID)
		return
	}

	dialCtx, cancelDial := context.WithTimeout(r.Context(), realtimeConnectTimeout)
	providerConn, err := provider.Connect(dialCtx,
		realtime.Session{TenantID: tenantID, SessionID: sessionID}, model)
	cancelDial()
	if err != nil {
		s.cfg.Logger.Warn("realtime provider connect failed",
			"err", err,
			"provider", realtime.ProviderName(provider),
			"request_id", RequestIDFrom(r.Context()),
			"tenant_id", tenantID,
			"session_id", sessionID)
		writeRealtimeErrorEvent(client, "provider_unavailable", "realtime provider is unavailable")
		_ = client.Close(1011, "provider unavailable")
		return
	}

	s.cfg.Logger.Info("realtime bridge open",
		"provider", realtime.ProviderName(provider),
		"request_id", RequestIDFrom(r.Context()),
		"tenant_id", tenantID,
		"session_id", sessionID)
	result := realtime.Proxy(r.Context(), client, providerConn, realtime.ProxyOptions{
		DefaultSessionUpdate: realtime.SessionUpdateDefaults(provider),
		OnEvent: func(ev realtime.Event) {
			// Metadata only: event type and size. Payloads (base64 audio,
			// transcripts) are never a log field.
			s.cfg.Logger.Debug("realtime event",
				"direction", string(ev.Direction),
				"type", ev.Type,
				"bytes", ev.Bytes,
				"injected", ev.Injected,
				"tenant_id", tenantID,
				"session_id", sessionID)
		},
	})
	s.cfg.Logger.Info("realtime bridge closed",
		"provider", realtime.ProviderName(provider),
		"ended_by", result.EndedBy,
		"code", result.Code,
		"err", realtimeLogError(result.Err),
		"request_id", RequestIDFrom(r.Context()),
		"tenant_id", tenantID,
		"session_id", sessionID)
}

// realtimeStatus implements GET /v1/realtime/status. It answers the same
// tenant boundary as the bridge (a missing header is 401) and reports whether
// the built-in provider is configured, which models it allows and what its
// session defaults are. Unconfigured is a normal state, not an error: the UI
// shows "voice is not configured" instead of opening a socket that closes with
// 1006.
func (s *Server) realtimeStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantFrom(r); !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("X-Tenant-ID is required"))
		return
	}
	writeJSON(w, http.StatusOK, realtimeStatusFromEnv(os.Getenv))
}

// realtimeStatusFromEnv builds the preflight report from the same environment
// the built-in provider reads. get is getenv-shaped so the report is testable
// without the process environment.
func realtimeStatusFromEnv(get func(string) string) realtimeStatusResponse {
	status := realtimeStatusResponse{Models: []string{}}
	if get == nil {
		get = os.Getenv
	}
	provider, err := realtime.FromLookup(get)
	if err != nil {
		// A missing or unusable provider is the documented "not configured"
		// state; the body carries no reason because the reason can name the
		// provider URL. Operators read the detailed diagnostic from the logs.
		return status
	}
	status.Configured = true
	status.Provider = realtime.ProviderName(provider)
	status.Kind = status.Provider
	// An in-process provider (the demo) has no environment policy to mirror: it
	// serves its own fixed set, and reporting an env allowlist there would name
	// models the provider rejects. The built-in WebSocket provider has no model
	// accessor, so its policy is read back from the same variables it parses.
	if models, ok := realtime.RealtimeModels(provider); ok {
		status.Models = models
	} else {
		status.Models = realtimeAllowlistFromEnv(get)
	}
	status.InputSampleRate, status.OutputSampleRate = realtimeSampleRates(provider)
	return status
}

// realtimeAllowlistFromEnv mirrors the built-in provider's model policy: an
// explicit allowlist wins, otherwise the default model, otherwise the model
// carried by the URL. No policy at all is an empty (not nil) list, so the wire
// shape stays [].
func realtimeAllowlistFromEnv(get func(string) string) []string {
	if models := splitRealtimeList(get(realtimeStatusEnvModels)); len(models) > 0 {
		return models
	}
	if model := strings.TrimSpace(get(realtimeStatusEnvModel)); model != "" {
		return []string{model}
	}
	if rawURL := strings.TrimSpace(get(realtimeStatusEnvWSURL)); rawURL != "" {
		if parsed, err := url.Parse(rawURL); err == nil {
			if model := strings.TrimSpace(parsed.Query().Get("model")); model != "" {
				return []string{model}
			}
		}
	}
	return []string{}
}

// splitRealtimeList parses a comma/space separated environment list exactly
// like the provider's own parser.
func splitRealtimeList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if field = strings.TrimSpace(field); field != "" {
			out = append(out, field)
		}
	}
	return out
}

// realtimeSampleRates reads the rates from the session default the bridge would
// inject, so the status cannot drift from the actual defaults. The
// OpenAI-compatible default is pcm16 at 24 kHz in both directions; Qwen names
// numeric rates in its nested audio shape.
func realtimeSampleRates(provider realtime.RealtimeProvider) (input, output int) {
	input, output = 24000, 24000
	defaulter, ok := provider.(realtime.SessionDefaulter)
	if !ok {
		return input, output
	}
	var body struct {
		Session struct {
			Audio struct {
				Input struct {
					Format struct {
						SampleRate int `json:"sample_rate"`
					} `json:"format"`
				} `json:"input"`
				Output struct {
					Format struct {
						SampleRate int `json:"sample_rate"`
					} `json:"format"`
				} `json:"output"`
			} `json:"audio"`
		} `json:"session"`
	}
	if err := json.Unmarshal(defaulter.DefaultSessionUpdate(), &body); err != nil {
		return input, output
	}
	if rate := body.Session.Audio.Input.Format.SampleRate; rate > 0 {
		input = rate
	}
	if rate := body.Session.Audio.Output.Format.SampleRate; rate > 0 {
		output = rate
	}
	return input, output
}

// writeRealtimeErrorEvent tells the browser why the bridge is closing. It is a
// structured provider-style error event, never a fabricated assistant message.
func writeRealtimeErrorEvent(client realtime.Conn, kind, message string) {
	payload, err := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    kind,
			"message": message,
		},
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = client.Write(ctx, realtime.Message{Type: realtime.MessageText, Data: payload})
}

// realtimeLogError keeps peer-controlled text out of the logs. A close error is
// reduced to its code (the reason is peer-controlled and could carry transcript
// text); transport errors are bounded. The full error is never needed to
// operate the bridge.
func realtimeLogError(err error) string {
	if err == nil {
		return ""
	}
	var closeErr *realtime.CloseError
	if errors.As(err, &closeErr) {
		return fmt.Sprintf("peer closed (%d)", closeErr.Code)
	}
	msg := err.Error()
	if i := strings.Index(msg, "reason = "); i >= 0 {
		msg = msg[:i] + "reason = [redacted]"
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
