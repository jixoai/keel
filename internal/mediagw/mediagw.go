// Package mediagw is the server-side media gateway. It owns the real provider
// credential and exposes capability-specific operations (image generation,
// image editing, structured decisions, and a small read-only chat proxy) to
// clients.
//
// Invariants:
//   - the API key never leaves this package: it is only written into the
//     upstream Authorization header, and response bodies are scrubbed of it;
//   - the provider base URL is configuration, never client input;
//   - an unconfigured gateway answers ErrNotConfigured (HTTP 503 at the edge)
//     instead of inventing a result;
//   - no request or response body is logged here; the HTTP layer logs route,
//     status, duration and byte count only.
package mediagw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jixoai/keel/internal/demo"
)

// Environment variables read by ConfigFromEnv. KEEL_MEDIA_* wins; the flat
// KEEL_BASE_URL / KEEL_API_KEY pair is the fallback used by the rest of the
// runtime.
const (
	EnvBaseURL       = "KEEL_MEDIA_BASE_URL"
	EnvAPIKey        = "KEEL_MEDIA_API_KEY"
	EnvModelLLM      = "KEEL_MEDIA_MODEL_LLM"
	EnvModelImage    = "KEEL_MEDIA_MODEL_IMAGE"
	EnvModelDecision = "KEEL_MEDIA_MODEL_DECISION"

	// Fallbacks for the generic provider configuration.
	EnvFallbackBaseURL = "KEEL_BASE_URL"
	EnvFallbackAPIKey  = "KEEL_API_KEY"
)

// Default models for the token plan this deployment is expected to run.
const (
	DefaultModelLLM      = "bailian-token-plan/qwen3.8-flash"
	DefaultModelImage    = "bailian-token-plan/wan2.7-image"
	DefaultModelDecision = "bailian-token-plan/decision-model-preview"
)

// DefaultTimeout bounds one provider call. Image generation and decisions are
// interactive; the same bound is applied to every capability so a stuck
// upstream can never pin a handler indefinitely.
const DefaultTimeout = 30 * time.Second

// maxProviderResponse caps how much of an upstream body is buffered. It is a
// safety bound, not a content contract: the asset URLs we forward are short.
const maxProviderResponse = 8 << 20

// ImageSizes is the request-size allowlist for image generation.
var ImageSizes = []string{"1024x1024", "1024x1536", "1536x1024"}

// SizeAllowed reports whether size is in the generation allowlist.
func SizeAllowed(size string) bool {
	for _, allowed := range ImageSizes {
		if size == allowed {
			return true
		}
	}
	return false
}

// ErrNotConfigured is returned (wrapped by nothing) when the media provider
// has no base URL or no credential. The edge maps it to 503 with the exact
// body {"error":"media provider is not configured"}.
var ErrNotConfigured = errors.New("media provider is not configured")

// Error is a gateway failure that already carries the HTTP status and a
// sanitized, client-safe message. Provider headers and the API key are never
// part of it.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// Config is the provider configuration. Zero values are filled by New.
type Config struct {
	BaseURL string
	APIKey  string

	ModelLLM      string
	ModelImage    string
	ModelDecision string

	// Timeout overrides DefaultTimeout. Tests use it; production leaves it 0.
	Timeout time.Duration
	// Client overrides the default HTTP client.
	Client *http.Client
	// DemoDisabled suppresses the in-process demo provider even when
	// KEEL_DEMO_PROVIDER is on. It exists for one purpose: a test that wants to
	// assert the unconfigured (503) behavior must not be flipped by the
	// developer's own shell setting the switch. Production never sets it.
	DemoDisabled bool
	// Getenv reads the demo switch. Nil means os.Getenv; tests pass a lookup so
	// they do not depend on the process environment.
	Getenv func(string) string
}

// Configured reports whether the gateway can reach a provider. Both halves are
// required: a URL without a credential would make the server act anonymously,
// and a credential without a URL has nowhere to go.
func (c Config) Configured() bool {
	return strings.TrimSpace(c.BaseURL) != "" && strings.TrimSpace(c.APIKey) != ""
}

// ConfigFromEnv reads the media provider configuration. lookup may be nil, in
// which case os.Getenv is used.
func ConfigFromEnv(lookup func(string) string) Config {
	if lookup == nil {
		lookup = os.Getenv
	}
	base := strings.TrimSpace(lookup(EnvBaseURL))
	if base == "" {
		base = strings.TrimSpace(lookup(EnvFallbackBaseURL))
	}
	key := strings.TrimSpace(lookup(EnvAPIKey))
	if key == "" {
		key = strings.TrimSpace(lookup(EnvFallbackAPIKey))
	}
	return Config{
		BaseURL:       strings.TrimRight(base, "/"),
		APIKey:        key,
		ModelLLM:      envOr(lookup, EnvModelLLM, DefaultModelLLM),
		ModelImage:    envOr(lookup, EnvModelImage, DefaultModelImage),
		ModelDecision: envOr(lookup, EnvModelDecision, DefaultModelDecision),
	}
}

func envOr(lookup func(string) string, name, fallback string) string {
	if v := strings.TrimSpace(lookup(name)); v != "" {
		return v
	}
	return fallback
}

// Asset is one generated image. Bytes are never proxied: URL is the
// provider's own (typically short-lived) asset URL.
type Asset struct {
	URL    string `json:"url"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Model  string `json:"model,omitempty"`
}

// ImageResult is the normalized image response for both generation and edit.
type ImageResult struct {
	Assets []Asset `json:"assets"`
	Model  string  `json:"model"`
}

// ImageRequest is one image generation request.
type ImageRequest struct {
	Prompt         string
	NegativePrompt string
	Size           string
	N              int
	Model          string
}

// ImageEditRequest is one image edit request. Image is a data URL or an
// http(s) URL; the gateway never fetches a client URL itself.
type ImageEditRequest struct {
	Prompt string
	Image  string
	Model  string
}

// Choice is one option of a decision question.
type Choice struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

// Question is one structured decision question. Only choice, predicate and
// score are accepted (the real gateway rejected anything else).
type Question struct {
	Type      string          `json:"type"`
	Predicate string          `json:"predicate"`
	Choices   []Choice        `json:"choices,omitempty"`
	Criteria  json.RawMessage `json:"criteria,omitempty"`
}

// DecisionRequest forwards the client's input and questions verbatim.
type DecisionRequest struct {
	Input     json.RawMessage
	Questions []Question
	Model     string
}

// DecisionResult is the normalized decision response.
type DecisionResult struct {
	Answers json.RawMessage `json:"answers"`
	Model   string          `json:"model"`
	Usage   json.RawMessage `json:"usage,omitempty"`
}

// ChatRequest is the narrow chat-completions proxy. It exists for read-only
// diagnostics, not as a general model gateway.
type ChatRequest struct {
	Messages  json.RawMessage
	Model     string
	MaxTokens int
}

// ModelNames lists the configured model per capability.
type ModelNames struct {
	LLM      string `json:"llm"`
	Image    string `json:"image"`
	Decision string `json:"decision"`
}

// Capabilities is the credential-free description of what this deployment can
// do. The current token plan has no video, TTS, STT or realtime upstream, so
// those flags stay false even when the provider is configured.
type Capabilities struct {
	Configured bool       `json:"configured"`
	LLM        bool       `json:"llm"`
	Image      bool       `json:"image"`
	Decision   bool       `json:"decision"`
	Video      bool       `json:"video"`
	TTS        bool       `json:"tts"`
	STT        bool       `json:"stt"`
	Realtime   bool       `json:"realtime"`
	Models     ModelNames `json:"models"`
}

// Gateway forwards capability calls to the configured provider.
type Gateway struct {
	cfg    Config
	client *http.Client
}

// New builds a gateway. Empty model names and timeout fall back to defaults;
// the default client does not follow redirects (a redirect must not carry the
// Authorization header to another host).
func New(cfg Config) *Gateway {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	if cfg.ModelLLM == "" {
		cfg.ModelLLM = DefaultModelLLM
	}
	if cfg.ModelImage == "" {
		cfg.ModelImage = DefaultModelImage
	}
	if cfg.ModelDecision == "" {
		cfg.ModelDecision = DefaultModelDecision
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Getenv == nil {
		cfg.Getenv = os.Getenv
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{
			Timeout: cfg.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &Gateway{cfg: cfg, client: client}
}

// Config returns the effective configuration.
func (g *Gateway) Config() Config { return g.cfg }

// Capabilities describes the deployment without exposing URL or key.
//
// A demo-enabled deployment reports the capabilities the in-process provider can
// answer (llm/image/decision plus speech) with the demo model names, so a client
// renders real controls instead of an unconfigured state. The report never says
// how the capability is served beyond the model name: `demo-image` is a model id
// like any other.
func (g *Gateway) Capabilities() Capabilities {
	if g.cfg.Configured() {
		return Capabilities{
			Configured: true,
			LLM:        true,
			Image:      true,
			Decision:   true,
			Video:      false,
			TTS:        false,
			STT:        false,
			Realtime:   false,
			Models: ModelNames{
				LLM:      g.cfg.ModelLLM,
				Image:    g.cfg.ModelImage,
				Decision: g.cfg.ModelDecision,
			},
		}
	}
	if g.demoActive() {
		return Capabilities{
			Configured: true,
			LLM:        true,
			Image:      true,
			Decision:   true,
			Video:      false,
			// The demo audio endpoints answer too; the flags are set by the HTTP
			// layer's own projection (it owns the speech routes), so the gateway
			// reports only the capabilities it serves here.
			TTS:      false,
			STT:      false,
			Realtime: false,
			Models: ModelNames{
				LLM:      demo.ModelLLM,
				Image:    demo.ModelImage,
				Decision: demo.ModelDecision,
			},
		}
	}
	return Capabilities{
		Models: ModelNames{
			LLM:      g.cfg.ModelLLM,
			Image:    g.cfg.ModelImage,
			Decision: g.cfg.ModelDecision,
		},
	}
}

// Generate creates images from a prompt.
//
// Provider routing: a request that names demo-image is answered by the in-process
// demo provider whenever KEEL_DEMO_PROVIDER is on, even in a deployment that also
// holds vendor credentials — that model is listed in the catalog, so it must be
// servable. Every other request keeps vendor-first behavior (an unnamed model uses
// the vendor's default; no vendor credentials at all leaves the historical 503).
func (g *Gateway) Generate(ctx context.Context, req ImageRequest) (*ImageResult, error) {
	if g.demoRequested(req.Model, demo.FamilyImage) || (!g.cfg.Configured() && g.demoActive()) {
		return g.demoGenerate(req)
	}
	if !g.cfg.Configured() {
		return nil, ErrNotConfigured
	}
	model, err := g.resolveModel(req.Model, g.cfg.ModelImage)
	if err != nil {
		return nil, err
	}
	n, err := validateImageRequest(req)
	if err != nil {
		return nil, err
	}
	payload := imagePayload{
		Model:          model,
		Prompt:         req.Prompt,
		N:              n,
		Size:           req.Size,
		NegativePrompt: req.NegativePrompt,
	}
	raw, err := g.postJSON(ctx, "/images/generations", payload)
	if err != nil {
		return nil, err
	}
	return parseImageResult(raw, model)
}

// Edit edits an image with a prompt. The reference must be an http(s) URL —
// normally one returned by Generate. A data URL is rejected: materializing it
// server-side produced a reference the real provider answers 403/400 for, so
// the client is told to use a provider URL (or upload first). The gateway
// itself never fetches a client URL (that would be client-controlled SSRF);
// the URL is forwarded as an OpenAI-images-style edit payload.
func (g *Gateway) Edit(ctx context.Context, req ImageEditRequest) (*ImageResult, error) {
	if g.demoRequested(req.Model, demo.FamilyImage) || (!g.cfg.Configured() && g.demoActive()) {
		return g.demoEdit(req)
	}
	if !g.cfg.Configured() {
		return nil, ErrNotConfigured
	}
	model, err := g.resolveModel(req.Model, g.cfg.ModelImage)
	if err != nil {
		return nil, err
	}
	if err := validateEditRequest(req); err != nil {
		return nil, err
	}
	payload := imageEditPayload{
		Model:  model,
		Prompt: strings.TrimSpace(req.Prompt),
		Image:  strings.TrimSpace(req.Image),
	}
	raw, err := g.postJSON(ctx, "/images/edits", payload)
	if err != nil {
		return nil, err
	}
	return parseImageResult(raw, model)
}

// Decision asks the provider's structured decision endpoint.
//
// Provider routing matches Generate: an explicit demo-decision is answered by the
// in-process demo provider when the switch is on (the catalog lists it, so it must
// be servable), and every other request keeps vendor-first behavior.
func (g *Gateway) Decision(ctx context.Context, req DecisionRequest) (*DecisionResult, error) {
	if g.demoRequested(req.Model, demo.FamilyDecision) || (!g.cfg.Configured() && g.demoActive()) {
		return g.demoDecision(req)
	}
	if !g.cfg.Configured() {
		return nil, ErrNotConfigured
	}
	model, err := g.resolveModel(req.Model, g.cfg.ModelDecision)
	if err != nil {
		return nil, err
	}
	if err := validateDecisionRequest(req); err != nil {
		return nil, err
	}
	payload := decisionPayload{
		Input:     json.RawMessage(bytes.TrimSpace(req.Input)),
		Questions: req.Questions,
		Model:     model,
	}
	raw, err := g.postJSON(ctx, "/decisions", payload)
	if err != nil {
		return nil, err
	}
	var prov decisionProviderResponse
	if err := json.Unmarshal(raw, &prov); err != nil {
		return nil, &Error{Status: http.StatusBadGateway, Message: "media provider returned invalid JSON"}
	}
	answers := prov.Answers
	if isEmptyJSON(answers) && !isEmptyJSON(prov.Results) {
		answers = prov.Results
	}
	if isEmptyJSON(answers) {
		answers = json.RawMessage(`[]`)
	}
	result := &DecisionResult{Answers: answers, Model: model}
	if prov.Model != "" {
		result.Model = prov.Model
	}
	if !isEmptyJSON(prov.Usage) {
		result.Usage = prov.Usage
	}
	return result, nil
}

// Chat forwards a narrow chat-completions request and returns the provider's
// JSON unchanged (after key scrubbing). It is intentionally not a general
// model proxy: clients cannot reach arbitrary provider paths or URLs.
func (g *Gateway) Chat(ctx context.Context, req ChatRequest) (json.RawMessage, error) {
	// Same routing rule as the other capabilities: an explicit demo-llm is served by
	// the in-process provider when the switch is on, so a listed model is never
	// rejected by this diagnostics proxy.
	if g.demoRequested(req.Model, demo.FamilyLLM) || (!g.cfg.Configured() && g.demoActive()) {
		return g.demoChat(req)
	}
	if !g.cfg.Configured() {
		return nil, ErrNotConfigured
	}
	model, err := g.resolveModel(req.Model, g.cfg.ModelLLM)
	if err != nil {
		return nil, err
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(req.Messages, &messages); err != nil || len(messages) == 0 {
		return nil, &Error{Status: http.StatusBadRequest, Message: "messages must be a non-empty array"}
	}
	if req.MaxTokens < 0 {
		return nil, &Error{Status: http.StatusBadRequest, Message: "max_tokens must not be negative"}
	}
	payload := chatPayload{Messages: req.Messages, Model: model, MaxTokens: req.MaxTokens}
	raw, err := g.postJSON(ctx, "/chat/completions", payload)
	if err != nil {
		return nil, err
	}
	if !json.Valid(raw) {
		return nil, &Error{Status: http.StatusBadGateway, Message: "media provider returned invalid JSON"}
	}
	return json.RawMessage(raw), nil
}

func (g *Gateway) resolveModel(requested, allowed string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return allowed, nil
	}
	if requested != allowed {
		return "", &Error{
			Status:  http.StatusBadRequest,
			Message: fmt.Sprintf("model %q is not allowed; use %q", requested, allowed),
		}
	}
	return requested, nil
}

func sizeMessage() string {
	return "size must be one of " + strings.Join(ImageSizes, ", ")
}

func isHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

type imagePayload struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size,omitempty"`
	NegativePrompt string `json:"negative_prompt,omitempty"`
}

type imageEditPayload struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Image  string `json:"image"`
}

type decisionPayload struct {
	Input     json.RawMessage `json:"input"`
	Questions []Question      `json:"questions"`
	Model     string          `json:"model"`
}

type chatPayload struct {
	Messages  json.RawMessage `json:"messages"`
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens,omitempty"`
}

type providerImage struct {
	URL     string `json:"url"`
	B64JSON string `json:"b64_json"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Size    string `json:"size"`
}

type providerImageResponse struct {
	Model  string          `json:"model"`
	Data   []providerImage `json:"data"`
	Images []providerImage `json:"images"`
}

type decisionProviderResponse struct {
	Answers json.RawMessage `json:"answers"`
	Results json.RawMessage `json:"results"`
	Model   string          `json:"model"`
	Usage   json.RawMessage `json:"usage"`
}

// postJSON marshals payload, calls path and turns a non-2xx provider answer
// into a sanitized *Error.
func (g *Gateway) postJSON(ctx context.Context, path string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, &Error{Status: http.StatusInternalServerError, Message: "failed to encode provider request"}
	}
	status, body, err := g.do(ctx, path, "application/json", raw)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, g.providerError(status, body)
	}
	return body, nil
}

// do performs one provider call with the gateway timeout and credential. The
// response body is capped and scrubbed of the API key before it is returned.
func (g *Gateway) do(ctx context.Context, path, contentType string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, &Error{Status: http.StatusBadGateway, Message: "media provider request failed"}
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.APIKey)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, &Error{Status: http.StatusGatewayTimeout, Message: "media provider request timed out"}
		}
		// Never surface transport errors: http.Client errors embed the URL and
		// may embed request details.
		return 0, nil, &Error{Status: http.StatusBadGateway, Message: "media provider request failed"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderResponse+1))
	if err != nil {
		return 0, nil, &Error{Status: http.StatusBadGateway, Message: "media provider response failed"}
	}
	if len(raw) > maxProviderResponse {
		return 0, nil, &Error{Status: http.StatusBadGateway, Message: "media provider response is too large"}
	}
	if key := g.cfg.APIKey; key != "" {
		raw = redact(raw, key)
	}
	return resp.StatusCode, raw, nil
}

// minRedactLen keeps scrubbing from corrupting unrelated JSON (a short key
// such as "k" is a substring of "total_tokens"). Real provider credentials are
// far longer than this bound.
const minRedactLen = 8

// redact removes every occurrence of the credential from raw.
func redact(raw []byte, key string) []byte {
	if len(key) < minRedactLen {
		return raw
	}
	return bytes.ReplaceAll(raw, []byte(key), []byte("[redacted]"))
}

// providerError maps an upstream failure. A well-formed 4xx/5xx status is
// forwarded as-is with the sanitized provider message; anything else is a
// gateway error.
func (g *Gateway) providerError(status int, raw []byte) *Error {
	if status < 400 || status > 599 {
		return &Error{Status: http.StatusBadGateway, Message: "media provider returned an unexpected status"}
	}
	msg := providerMessage(raw)
	if msg == "" {
		msg = fmt.Sprintf("media provider returned status %d", status)
	}
	return &Error{Status: status, Message: sanitizeMessage(msg, g.cfg.APIKey)}
}

// providerMessage extracts the human message from the common error shapes:
// {"error":{"message":"..."}}, {"error":"..."} and {"message":"..."}.
func providerMessage(raw []byte) string {
	var obj struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	if len(obj.Error) > 0 {
		var text string
		if err := json.Unmarshal(obj.Error, &text); err == nil {
			return text
		}
		var nested struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(obj.Error, &nested); err == nil && nested.Message != "" {
			return nested.Message
		}
	}
	return obj.Message
}

// sanitizeMessage makes an upstream message safe for a client body: control
// characters collapse to spaces, the credential is redacted, and the result is
// bounded.
func sanitizeMessage(msg, key string) string {
	msg = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, msg)
	msg = strings.TrimSpace(msg)
	if key != "" && len(key) >= minRedactLen {
		msg = strings.ReplaceAll(msg, key, "[redacted]")
	}
	const limit = 512
	if utf8.RuneCountInString(msg) > limit {
		runes := []rune(msg)
		msg = string(runes[:limit]) + "..."
	}
	if msg == "" {
		msg = "media provider request failed"
	}
	return msg
}

// parseImageResult normalizes an OpenAI-images response. Only URL assets are
// surfaced: base64 payloads would turn this endpoint into a byte proxy.
func parseImageResult(raw []byte, effectiveModel string) (*ImageResult, error) {
	var prov providerImageResponse
	if err := json.Unmarshal(raw, &prov); err != nil {
		return nil, &Error{Status: http.StatusBadGateway, Message: "media provider returned invalid JSON"}
	}
	model := effectiveModel
	if prov.Model != "" {
		model = prov.Model
	}
	assets := make([]Asset, 0, len(prov.Data)+len(prov.Images))
	appendItems := func(items []providerImage) {
		for _, item := range items {
			assetURL := strings.TrimSpace(item.URL)
			if assetURL == "" {
				continue
			}
			width, height := item.Width, item.Height
			if (width == 0 || height == 0) && item.Size != "" {
				if w, h, ok := parseSize(item.Size); ok {
					if width == 0 {
						width = w
					}
					if height == 0 {
						height = h
					}
				}
			}
			assets = append(assets, Asset{URL: assetURL, Width: width, Height: height, Model: model})
		}
	}
	appendItems(prov.Data)
	appendItems(prov.Images)
	if len(assets) == 0 {
		return nil, &Error{Status: http.StatusBadGateway, Message: "media provider returned no image URL"}
	}
	return &ImageResult{Assets: assets, Model: model}, nil
}

func parseSize(size string) (int, int, bool) {
	left, right, ok := strings.Cut(strings.ToLower(size), "x")
	if !ok {
		return 0, 0, false
	}
	w, errW := strconv.Atoi(strings.TrimSpace(left))
	h, errH := strconv.Atoi(strings.TrimSpace(right))
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

func isEmptyJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}
