// Package externalgw wires the external-provider gateway into the runtime: it
// turns the tenant document's external section into live AgentBackend and
// ToolProvider services, opens/queries/sends/cancels/closes external sessions
// through the contract kernel's external-session seam, and projects backend
// events into Keel's event log so the ordinary SSE endpoint can replay them.
//
// # State ownership
//
// The package owns no state of its own beyond bookkeeping: sessions live in
// PostgreSQL (kernel.ExternalStore), events are committed through
// kernel.Committer under the external commit lane, and the backend owns the
// conversation itself. The package never receives a *kernel.Kernel, a
// *store.Store or a table handle; a boundary test pins that.
//
// # Failure isolation
//
// A broken provider is a degraded provider, never a broken runtime: every
// backend connection is supervised, a crash marks its sessions
// backend-unavailable (a queryable terminal state), and reconnect is bounded by
// gateway.RestartPolicy. The HTTP surface and the native run/lease flow keep
// working while any of that happens. A slow backend applies backpressure
// through the gateway's bounded queues instead of accumulating memory, and a
// commit failure only drops that projection - it never tears down the
// connection.
//
// # DSH
//
// The reference sidecar (internal/dshref) implements the same protocol for
// tests and the smoke script. docs/DSH-REFERENCE.md documents the real Node
// sidecar and its manual test.
package externalgw

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jixoai/keel/internal/gateway"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
)

// Errors the HTTP edge maps onto statuses.
var (
	// ErrBackendUnknown: the tenant document declares no such provider.
	ErrBackendUnknown = errors.New("externalgw: unknown external backend")
	// ErrBackendUnavailable: the provider is not connected right now.
	ErrBackendUnavailable = errors.New("externalgw: external backend unavailable")
	// ErrNotExternal: the addressed session is not an external session.
	ErrNotExternal = errors.New("externalgw: session is not an external session")
	// ErrAlreadyExternal: the session already belongs to an external backend.
	ErrAlreadyExternal = errors.New("externalgw: session already belongs to an external backend")
	// ErrSessionClosed: the external session is closed (terminal).
	ErrSessionClosed = errors.New("externalgw: external session is closed")
	// ErrSessionNotActive: the session exists but cannot take this operation.
	ErrSessionNotActive = errors.New("externalgw: external session is not active")
	// ErrUnmeteredDenied: the tenant policy refuses sessions whose usage Keel
	// cannot see (metering=none).
	ErrUnmeteredDenied = errors.New("externalgw: the tenant policy denies unmetered external sessions")
	// ErrSessionLimit: the backend's concurrent-session limit is reached.
	ErrSessionLimit = errors.New("externalgw: external session limit reached")
)

// Options are the hosting-process inputs. Kernel seams are narrow interfaces;
// the gateway package itself never sees them.
type Options struct {
	// Committer commits external.event records (the kernel's write seam).
	Committer kernel.Committer
	// Sessions reads and writes the external-session marks (the kernel's
	// external-session seam).
	Sessions kernel.ExternalSessions
	// LookupEnv resolves the secret references in the tenant document.
	LookupEnv func(string) (string, bool)
	// Environ is the host environment a spawned provider may derive its
	// minimal environment from. Nil means os.Environ().
	Environ []string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Stderr receives a spawned provider's stderr (default os.Stderr).
	Stderr io.Writer
	// Restart bounds backend reconnect attempts.
	Restart gateway.RestartPolicy
	// ConnectTimeout bounds one connect attempt (default 10s).
	ConnectTimeout time.Duration
	// HTTPClient is used for http providers (default http.DefaultClient).
	HTTPClient *http.Client
}

// ProviderConfig is one resolved provider endpoint: secret references are
// already resolved, the environment is the exact child environment, and the
// transport is fixed.
type ProviderConfig struct {
	Name      string
	Transport string
	Command   string
	Args      []string
	Dir       string
	URL       string
	Env       []string
	Headers   map[string]string
	Timeout   time.Duration
	// MaxSessions is this provider's own concurrent-session cap (0 = no cap).
	MaxSessions int
}

// Config is the resolved external section.
type Config struct {
	Backends []ProviderConfig
	Tools    []ProviderConfig
	// Unmetered is the explicit policy for metering=none sessions
	// (host.ExternalUnmeteredAllow or host.ExternalUnmeteredDeny).
	Unmetered string
	// MaxSessionsPerBackend is the tenant-wide cap per backend (0 = none).
	MaxSessionsPerBackend int
}

// Backend returns one resolved backend by name.
func (c Config) Backend(name string) (ProviderConfig, bool) {
	for _, backend := range c.Backends {
		if backend.Name == name {
			return backend, true
		}
	}
	return ProviderConfig{}, false
}

// ResolveConfig resolves and validates the tenant document's external section.
// It fails closed: a declared provider whose secret reference cannot be
// resolved, or whose endpoint shape is invalid, is a configuration error (the
// caller decides whether to boot without it).
func ResolveConfig(doc host.ExternalConfig, opts Options) (Config, error) {
	if opts.LookupEnv == nil {
		opts.LookupEnv = func(string) (string, bool) { return "", false }
	}
	environ := opts.Environ
	if environ == nil {
		environ = os.Environ()
	}
	out := Config{Unmetered: doc.Quota.Unmetered, MaxSessionsPerBackend: doc.Quota.MaxSessionsPerBackend}
	var err error
	out.Backends, err = resolveProviders(doc.Backends, opts, environ)
	if err != nil {
		return Config{}, fmt.Errorf("externalgw: backends: %w", err)
	}
	out.Tools, err = resolveProviders(doc.Tools, opts, environ)
	if err != nil {
		return Config{}, fmt.Errorf("externalgw: tools: %w", err)
	}
	return out, nil
}

func resolveProviders(providers map[string]host.ExternalProviderConfig, opts Options, environ []string) ([]ProviderConfig, error) {
	if len(providers) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ProviderConfig, 0, len(names))
	for _, name := range names {
		provider := providers[name]
		if !provider.ProviderEnabled() {
			continue
		}
		transport, err := provider.TransportKind()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		resolved := ProviderConfig{
			Name:        name,
			Transport:   transport,
			Command:     strings.TrimSpace(provider.Command),
			Args:        append([]string(nil), provider.Args...),
			Dir:         strings.TrimSpace(provider.Dir),
			URL:         strings.TrimSpace(provider.URL),
			Timeout:     time.Duration(provider.TimeoutMS) * time.Millisecond,
			MaxSessions: provider.MaxSessions,
		}
		if transport == host.ExternalTransportStdio {
			env, err := resolveEnv(provider.Env, opts.LookupEnv, environ)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			resolved.Env = env
		} else {
			headers, err := resolveHeaders(provider.Headers, opts.LookupEnv)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			resolved.Headers = headers
		}
		out = append(out, resolved)
	}
	return out, nil
}

// resolveEnv builds the exact child environment: the gateway's minimal
// allowlist from the host environment plus the provider's explicitly declared
// variables. Secrets are resolved here and never leave this slice; a reference
// that cannot be resolved is an error (fail closed).
func resolveEnv(declared map[string]string, lookup func(string) (string, bool), environ []string) ([]string, error) {
	env := gateway.MinimalEnv(environ)
	if len(declared) == 0 {
		return env, nil
	}
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value, err := resolveSecretRef(declared[name], lookup)
		if err != nil {
			return nil, fmt.Errorf("env[%q]: %w", name, err)
		}
		if strings.ContainsAny(value, "\x00\n\r") {
			return nil, fmt.Errorf("env[%q]: resolved value contains a control character", name)
		}
		env = append(env, name+"="+value)
	}
	return env, nil
}

func resolveHeaders(declared map[string]string, lookup func(string) (string, bool)) (map[string]string, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(declared))
	for name, raw := range declared {
		value, err := resolveSecretRef(raw, lookup)
		if err != nil {
			return nil, fmt.Errorf("headers[%q]: %w", name, err)
		}
		if strings.ContainsAny(value, "\x00\n\r") {
			return nil, fmt.Errorf("headers[%q]: resolved value contains a control character", name)
		}
		out[name] = value
	}
	return out, nil
}

// resolveSecretRef resolves ${NAME}, env:NAME or secret:NAME through lookup.
func resolveSecretRef(ref string, lookup func(string) (string, bool)) (string, error) {
	ref = strings.TrimSpace(ref)
	var name string
	switch {
	case strings.HasPrefix(ref, "${") && strings.HasSuffix(ref, "}"):
		name = ref[2 : len(ref)-1]
	case strings.HasPrefix(ref, "env:"):
		name = strings.TrimPrefix(ref, "env:")
	case strings.HasPrefix(ref, "secret:"):
		name = strings.TrimPrefix(ref, "secret:")
	default:
		return "", fmt.Errorf("not a secret reference (%q); literal secrets are not allowed", ref)
	}
	value, ok := lookup(name)
	if !ok || value == "" {
		return "", fmt.Errorf("secret reference %q cannot be resolved", ref)
	}
	return value, nil
}

func (o Options) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

func (o Options) connectTimeout() time.Duration {
	if o.ConnectTimeout > 0 {
		return o.ConnectTimeout
	}
	return 10 * time.Second
}

func (o Options) stderr() io.Writer {
	if o.Stderr != nil {
		return o.Stderr
	}
	return os.Stderr
}

func (o Options) requestTimeout(cfg ProviderConfig) time.Duration {
	if cfg.Timeout > 0 {
		return cfg.Timeout
	}
	return 0 // gateway default
}

// SessionView is the query/open representation of an external session. The
// JSON keys are the durable mark names the spec pins (state_owner,
// external_backend, external_ref, external_status, metering), and the view
// never claims native recovery.
type SessionView struct {
	SessionID   string `json:"sessionId"`
	Backend     string `json:"externalBackend"`
	ExternalRef string `json:"externalRef"`
	StateOwner  string `json:"stateOwner"`
	Status      string `json:"externalStatus"`
	Metering    string `json:"metering"`
	Recovery    string `json:"recovery,omitempty"`
	// Compatible marks this as a compatibility session; NativeRecovery is
	// always false because Keel cannot rebuild it from its own log.
	Compatible     bool      `json:"compatible"`
	NativeRecovery bool      `json:"nativeRecovery"`
	CreatedAt      time.Time `json:"createdAt,omitempty"`
	UpdatedAt      time.Time `json:"updatedAt,omitempty"`
	// Connection is the live backend connection state (ok|degraded|unavailable).
	Connection string        `json:"connection,omitempty"`
	Usage      *UsageSummary `json:"usage,omitempty"`
}

// UsageSummary is the backend-reported usage Keel has seen since this process
// opened the session. It is absent when the backend reports no usage: an
// unmeasurable session is never rendered as zero.
type UsageSummary struct {
	Metering     string `json:"metering"`
	Model        string `json:"model,omitempty"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	TotalTokens  int64  `json:"totalTokens"`
}

// BackendStatus is one provider's availability snapshot.
type BackendStatus struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Health   string `json:"health"`
	Error    string `json:"error,omitempty"`
	Sessions int    `json:"sessions"`
}

// SendResult acknowledges a message handed to a backend.
type SendResult struct {
	TurnID   string `json:"turnId,omitempty"`
	Recorded bool   `json:"recorded"`
}

// CancelRequest cancels one turn of an external session.
type CancelRequest struct {
	TurnID string `json:"turnId,omitempty"`
	Reason string `json:"reason,omitempty"`
}
