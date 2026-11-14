// Media gateway endpoints. The provider credential lives in the server
// process (KEEL_MEDIA_* env); the client only reaches capability endpoints.
// Client bodies never contain a provider URL, and no request or response body
// is logged: the ordinary request logger records route, status, duration and
// bytes only.

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jixoai/keel/internal/mediagw"
)

// mediaMaxBody caps every media request at 1 MiB (data URLs included).
const mediaMaxBody = 1 << 20

// registerMediaRoutes wires the media capability surface. The provider config
// is read once at server construction; an absent URL or key keeps every
// capability endpoint at 503 while /capabilities still answers.
func (s *Server) registerMediaRoutes() {
	api := &mediaAPI{gw: mediagw.New(mediagw.ConfigFromEnv(nil))}
	s.mux.HandleFunc("POST /v1/media/images/generate", api.generate)
	s.mux.HandleFunc("POST /v1/media/images/edit", api.edit)
	s.mux.HandleFunc("POST /v1/media/decision", api.decision)
	s.mux.HandleFunc("POST /v1/media/llm/chat", api.chat)
	s.mux.HandleFunc("GET /v1/media/capabilities", api.capabilities)
}

type mediaAPI struct {
	gw *mediagw.Gateway
}

// mediaTenant applies the same tenant header contract as the rest of /v1.
func mediaTenant(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := tenantFrom(r); !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("X-Tenant-ID is required"))
		return false
	}
	return true
}

// writeMediaError maps gateway failures. The configured-503 body is the
// sentinel's exact text; provider errors already carry a status and a
// sanitized message; anything else becomes a generic 502 (no URL, no key).
func writeMediaError(w http.ResponseWriter, err error) {
	if errors.Is(err, mediagw.ErrNotConfigured) {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	var gwErr *mediagw.Error
	if errors.As(err, &gwErr) {
		status := gwErr.Status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		writeErr(w, status, errors.New(gwErr.Message))
		return
	}
	writeErr(w, http.StatusBadGateway, errors.New("media provider request failed"))
}

// generate handles POST /v1/media/images/generate.
func (a *mediaAPI) generate(w http.ResponseWriter, r *http.Request) {
	if !mediaTenant(w, r) {
		return
	}
	var body struct {
		Prompt         string `json:"prompt"`
		NegativePrompt string `json:"negativePrompt"`
		Size           string `json:"size"`
		N              *int   `json:"n"`
		Model          string `json:"model"`
	}
	if err := decodeBody(w, r, mediaMaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("prompt is required"))
		return
	}
	n := 1
	if body.N != nil {
		n = *body.N
	}
	if n < 1 || n > 4 {
		writeErr(w, http.StatusBadRequest, errors.New("n must be between 1 and 4"))
		return
	}
	if body.Size != "" && !mediagw.SizeAllowed(body.Size) {
		writeErr(w, http.StatusBadRequest, errors.New("size must be one of "+strings.Join(mediagw.ImageSizes, ", ")))
		return
	}
	result, err := a.gw.Generate(r.Context(), mediagw.ImageRequest{
		Prompt:         body.Prompt,
		NegativePrompt: body.NegativePrompt,
		Size:           body.Size,
		N:              n,
		Model:          body.Model,
	})
	if err != nil {
		writeMediaError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// edit handles POST /v1/media/images/edit.
func (a *mediaAPI) edit(w http.ResponseWriter, r *http.Request) {
	if !mediaTenant(w, r) {
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
		Image  string `json:"image"`
		Model  string `json:"model"`
	}
	if err := decodeBody(w, r, mediaMaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("prompt is required"))
		return
	}
	if strings.TrimSpace(body.Image) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("image is required"))
		return
	}
	result, err := a.gw.Edit(r.Context(), mediagw.ImageEditRequest{
		Prompt: body.Prompt,
		Image:  body.Image,
		Model:  body.Model,
	})
	if err != nil {
		writeMediaError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// decision handles POST /v1/media/decision.
func (a *mediaAPI) decision(w http.ResponseWriter, r *http.Request) {
	if !mediaTenant(w, r) {
		return
	}
	var body struct {
		Input     json.RawMessage    `json:"input"`
		Questions []mediagw.Question `json:"questions"`
		Model     string             `json:"model"`
	}
	if err := decodeBody(w, r, mediaMaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	result, err := a.gw.Decision(r.Context(), mediagw.DecisionRequest{
		Input:     body.Input,
		Questions: body.Questions,
		Model:     body.Model,
	})
	if err != nil {
		writeMediaError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// chat handles POST /v1/media/llm/chat. It is a narrow, read-only proxy for
// diagnostics; the response is the provider's JSON with the key scrubbed.
func (a *mediaAPI) chat(w http.ResponseWriter, r *http.Request) {
	if !mediaTenant(w, r) {
		return
	}
	var body struct {
		Messages  json.RawMessage `json:"messages"`
		Model     string          `json:"model"`
		MaxTokens int             `json:"max_tokens"`
	}
	if err := decodeBody(w, r, mediaMaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	raw, err := a.gw.Chat(r.Context(), mediagw.ChatRequest{
		Messages:  body.Messages,
		Model:     body.Model,
		MaxTokens: body.MaxTokens,
	})
	if err != nil {
		writeMediaError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, raw)
}

// capabilities handles GET /v1/media/capabilities.
func (a *mediaAPI) capabilities(w http.ResponseWriter, r *http.Request) {
	if !mediaTenant(w, r) {
		return
	}
	// withAudioCapabilities only turns tts/stt on when the demo audio provider
	// is enabled; every other flag is exactly the gateway's (see media_audio.go).
	writeJSON(w, http.StatusOK, withAudioCapabilities(a.gw.Capabilities()))
}
