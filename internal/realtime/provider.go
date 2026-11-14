package realtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/jixoai/keel/internal/demo"
)

// Configuration errors. ErrNotConfigured is the exact condition the HTTP layer
// turns into 503 "realtime provider is not configured"; ErrModelNotAllowed is a
// request error (the client asked for a model the operator did not authorize).
var (
	ErrNotConfigured   = errors.New("realtime provider is not configured")
	ErrModelNotAllowed = errors.New("realtime model is not allowed")
)

// Kind is a realtime provider protocol family.
type Kind string

const (
	// KindQwen is DashScope's Qwen-Omni Realtime WebSocket API. Its session
	// update carries the nested session.audio.input/output shape and its event
	// names match the OpenAI realtime family.
	KindQwen Kind = "qwen"
	// KindOpenAI is the OpenAI Realtime WebSocket API.
	KindOpenAI Kind = "openai"
	// KindDemo is the in-process demo provider (demo.go): same event family,
	// no vendor, no socket. It is only reachable when demo.Env is
	// enabled, and FromLookup owns when it is selected.
	KindDemo Kind = "demo"
)

// Environment variables read by FromEnv. They are documented here rather than
// only in a README so `go doc` carries the contract.
const (
	envWSURL        = "KEEL_REALTIME_WS_URL"
	envProvider     = "KEEL_REALTIME_PROVIDER"
	envAPIKey       = "KEEL_REALTIME_API_KEY"
	envDashScopeKey = "DASHSCOPE_API_KEY"
	envModel        = "KEEL_REALTIME_MODEL"
	envModels       = "KEEL_REALTIME_MODELS"
)

// Config configures the built-in WebSocket provider.
type Config struct {
	// Kind selects the protocol family. Empty means KindQwen (the default when
	// KEEL_REALTIME_WS_URL is set).
	Kind Kind
	// URL is the full provider WebSocket URL. For Qwen it is the workspace
	// endpoint (wss://<workspace>.<region>.maas.aliyuncs.com/api-ws/v1/realtime)
	// and may already carry ?model=.
	URL string
	// APIKey is sent as `Authorization: Bearer <key>`. Empty omits the header
	// (a local provider may not require one; DashScope and OpenAI do).
	APIKey string
	// DefaultModel is used when the client does not name a model.
	DefaultModel string
	// AllowModels, when non-empty, is the only set of models a client may
	// request. When empty the allowlist is the default model, then the model
	// carried by URL; if neither exists the operator configured no model policy
	// and the request passes through.
	AllowModels []string
	// Dial overrides the WebSocket dialer (tests and future transports).
	Dial func(ctx context.Context, endpoint string, header http.Header) (Conn, error)
}

// FromEnv builds the configured provider from the process environment.
func FromEnv() (RealtimeProvider, error) { return FromLookup(os.Getenv) }

// FromLookup builds the configured provider from a getenv-shaped function. It
// exists so configuration parsing is testable without mutating the process
// environment.
//
// # Selection order
//
// The demo provider (KindDemo) and a real WebSocket provider can be configured
// at the same time, so the order is explicit and tested
// (TestFromLookupSelectionOrder):
//
//  1. KEEL_REALTIME_PROVIDER=demo: the operator named the demo provider, so it
//     wins even when KEEL_REALTIME_WS_URL is set. With the demo provider
//     disabled this is a configuration error, never a silent fallback: pinning
//     a provider that is not enabled must not quietly serve a different one.
//  2. KEEL_REALTIME_PROVIDER names another family (qwen/openai): that provider
//     is built from KEEL_REALTIME_WS_URL; a missing URL is ErrNotConfigured.
//     An explicit family never falls through to the demo.
//  3. KEEL_REALTIME_WS_URL is set (no explicit family): the WebSocket provider.
//     A configured vendor endpoint outranks an enabled demo, so turning the
//     demo on cannot silently replace a working voice service.
//  4. KEEL_DEMO_PROVIDER is enabled and no vendor endpoint exists: the demo.
//  5. Otherwise ErrNotConfigured.
func FromLookup(get func(string) string) (RealtimeProvider, error) {
	if get == nil {
		get = os.Getenv
	}
	kind := Kind(strings.ToLower(strings.TrimSpace(get(envProvider))))
	rawURL := strings.TrimSpace(get(envWSURL))
	demoEnabled := demo.EnabledFromGetenv(get)

	switch {
	case kind == KindDemo:
		if !demoEnabled {
			return nil, fmt.Errorf("realtime: %s=demo requires %s to be enabled",
				envProvider, demo.Env)
		}
		return NewDemoProvider(), nil
	case kind != "":
		if rawURL == "" {
			return nil, ErrNotConfigured
		}
		return newWSProviderFromLookup(get, kind, rawURL)
	case rawURL != "":
		return newWSProviderFromLookup(get, "", rawURL)
	case demoEnabled:
		return NewDemoProvider(), nil
	default:
		return nil, ErrNotConfigured
	}
}

// newWSProviderFromLookup builds the built-in WebSocket provider from the
// remaining KEEL_REALTIME_* variables. kind is the explicit family (empty means
// the provider default, qwen).
func newWSProviderFromLookup(get func(string) string, kind Kind, rawURL string) (RealtimeProvider, error) {
	if kind == "" {
		kind = KindQwen
	}
	apiKey := strings.TrimSpace(get(envAPIKey))
	if apiKey == "" && kind == KindQwen {
		apiKey = strings.TrimSpace(get(envDashScopeKey))
	}
	return NewProvider(Config{
		Kind:         kind,
		URL:          rawURL,
		APIKey:       apiKey,
		DefaultModel: strings.TrimSpace(get(envModel)),
		AllowModels:  splitList(get(envModels)),
	})
}

// NewProvider validates the configuration and builds the built-in WebSocket
// provider. The returned value is safe for concurrent use.
func NewProvider(cfg Config) (*WSProvider, error) {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, ErrNotConfigured
	}
	if cfg.Kind == "" {
		cfg.Kind = KindQwen
	}
	switch cfg.Kind {
	case KindQwen, KindOpenAI:
	default:
		// The demo provider is not a WebSocket family: it has no URL and is
		// built by NewDemoProvider, so a Config that names it is a mistake.
		return nil, fmt.Errorf("realtime: unsupported %s %q (want %q or %q)", envProvider, cfg.Kind, KindQwen, KindOpenAI)
	}
	base, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("realtime: invalid %s: %w", envWSURL, err)
	}
	if base.Scheme != "ws" && base.Scheme != "wss" {
		return nil, fmt.Errorf("realtime: %s must be a ws:// or wss:// URL", envWSURL)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("realtime: %s has no host", envWSURL)
	}
	p := &WSProvider{
		cfg:          cfg,
		base:         *base,
		allow:        make(map[string]struct{}, len(cfg.AllowModels)),
		defaultModel: strings.TrimSpace(cfg.DefaultModel),
		urlModel:     base.Query().Get("model"),
	}
	for _, model := range cfg.AllowModels {
		if model = strings.TrimSpace(model); model != "" {
			p.allow[model] = struct{}{}
		}
	}
	if len(p.allow) == 0 {
		switch {
		case p.defaultModel != "":
			p.allow[p.defaultModel] = struct{}{}
		case p.urlModel != "":
			p.allow[p.urlModel] = struct{}{}
		}
	} else if p.defaultModel != "" {
		if _, ok := p.allow[p.defaultModel]; !ok {
			return nil, fmt.Errorf("realtime: %s %q is not listed in %s", envModel, p.defaultModel, envModels)
		}
	}
	return p, nil
}

// WSProvider is the built-in generic WebSocket realtime provider. It speaks the
// OpenAI Realtime event family, which Qwen-Omni Realtime follows, so one
// implementation covers both; only the session defaults and headers differ.
type WSProvider struct {
	cfg          Config
	base         url.URL
	allow        map[string]struct{}
	defaultModel string
	urlModel     string
}

// Connect implements RealtimeProvider. It validates the requested model against
// the allowlist, builds the endpoint, and dials the provider with server-side
// credentials. The session identifiers are deliberately not sent to the
// third-party endpoint.
func (p *WSProvider) Connect(ctx context.Context, _ Session, model string) (Conn, error) {
	resolved, err := p.resolveModel(model)
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	if p.cfg.APIKey != "" {
		header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	if p.cfg.Kind == KindOpenAI {
		header.Set("OpenAI-Beta", "realtime=v1")
	}
	dial := p.cfg.Dial
	if dial == nil {
		dial = Dial
	}
	conn, err := dial(ctx, p.endpoint(resolved), header)
	if err != nil {
		return nil, fmt.Errorf("realtime: connect provider: %w", err)
	}
	return conn, nil
}

// ValidateModel reports whether a client-supplied model may be used with this
// provider. The HTTP layer calls it before upgrading so an unauthorized model
// is a plain 400 instead of a WebSocket error.
func (p *WSProvider) ValidateModel(model string) error {
	_, err := p.resolveModel(model)
	return err
}

// ProviderName implements ProviderNamer.
func (p *WSProvider) ProviderName() string { return string(p.cfg.Kind) }

// DefaultSessionUpdate implements SessionDefaulter with the provider-shaped
// session.update the bridge injects when the client never sends one.
func (p *WSProvider) DefaultSessionUpdate() []byte {
	if p.cfg.Kind == KindOpenAI {
		return append([]byte(nil), openAIDefaultSessionUpdate...)
	}
	return append([]byte(nil), qwenDefaultSessionUpdate...)
}

// resolveModel applies the allowlist: a client model overrides the default only
// when an allowlist exists and contains it. With no allowlist anywhere (no
// KEEL_REALTIME_MODELS, no KEEL_REALTIME_MODEL, no model in the URL) the
// operator expressed no policy and the request passes through.
func (p *WSProvider) resolveModel(requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		if len(p.allow) > 0 {
			if _, ok := p.allow[requested]; !ok {
				return "", fmt.Errorf("%w: %q", ErrModelNotAllowed, requested)
			}
		}
		return requested, nil
	}
	if p.defaultModel != "" {
		return p.defaultModel, nil
	}
	return p.urlModel, nil
}

// endpoint returns the configured URL with the effective model applied. Other
// query parameters (workspace or deployment selectors) are preserved.
func (p *WSProvider) endpoint(model string) string {
	u := p.base
	if model != "" {
		q := u.Query()
		q.Set("model", model)
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// qwenDefaultSessionUpdate is the safe default injected when the client never
// sends session.update. Qwen-Omni Realtime reads the nested session.audio shape:
// pcm/s16le/16 kHz/mono input, pcm/24 kHz output, server VAD, and its own ASR
// model for input transcription.
var qwenDefaultSessionUpdate = []byte(`{"type":"session.update","session":{"modalities":["text","audio"],"turn_detection":{"type":"server_vad"},"audio":{"input":{"format":{"type":"pcm","sample_rate":16000,"sample_format":"s16le","channels":1,"packing":"interleaved","channel_layout":"mono"}},"output":{"voice":"longanlingxin","format":{"type":"pcm","sample_rate":24000}}},"input_audio_transcription":{"model":"qwen3-asr-flash-realtime"}}}`)

// openAIDefaultSessionUpdate uses the GA OpenAI Realtime shape. In that protocol
// pcm16 is already 24 kHz, so both directions name the same format; the audio is
// 24 kHz in and out.
var openAIDefaultSessionUpdate = []byte(`{"type":"session.update","session":{"modalities":["text","audio"],"input_audio_format":"pcm16","output_audio_format":"pcm16","turn_detection":{"type":"server_vad"}}}`)

// genericDefaultSessionUpdate is used by a provider that does not declare its
// own defaults. It is the conservative OpenAI-compatible shape.
var genericDefaultSessionUpdate = openAIDefaultSessionUpdate

// splitList parses a comma/space separated environment list.
func splitList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
