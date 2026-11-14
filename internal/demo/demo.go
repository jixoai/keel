// Package demo is the server-side demo provider's shared vocabulary and its
// deterministic content.
//
// # Why this package exists
//
// The demo capability must live on the *server*: a client that knows about
// demo models cannot ship to production (it would have to render a mode, a
// badge or an exit action nobody wants). So the server registers one ordinary
// provider — providerId "demo" — behind an environment switch, and the client
// sees only provider/model identities like any other vendor's.
//
// Several packages need the same names (modelcfg registers them, modelprovider
// shapes the LLM wire, mediagw produces the assets and answers, realtime opens
// the channel, httpapi serves the endpoints). This package is the one place
// those names and the fixed content live, so they cannot drift apart.
//
// # Boundaries
//
//   - Default off. KEEL_DEMO_PROVIDER must be an affirmative token; anything
//     else (unset, "0", "false", a typo) leaves the demo provider absent, and a
//     production deployment is byte-for-byte the deployment it was before.
//   - No IO, no network, no credentials. Everything here is compiled-in data, so
//     the demo cannot leak a real user's data and cannot be used to reach a
//     third party.
//   - Deterministic. The same model id produces the same bytes on every call:
//     the tests can assert exact content, and a reviewer sees a stable result.
//   - Not a UI concept. Nothing in this package carries a label, badge or mode
//     name a client should render; the identity is the model id.
package demo

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Environment and identity constants.
const (
	// Env is the environment variable that enables the demo provider.
	Env = "KEEL_DEMO_PROVIDER"

	// ProviderID is the provider identity. It is deliberately a plain provider
	// id: the picker groups by it and a selection names it, exactly like a
	// vendor.
	ProviderID = "demo"

	// ProviderName is the provider's display name. It reads as a provider name,
	// not as a mode.
	ProviderName = "Demo"

	// Model ids, one per capability family the harness ships.
	ModelLLM      = "demo-llm"
	ModelImage    = "demo-image"
	ModelDecision = "demo-decision"
	ModelRealtime = "demo-realtime"
	ModelTTS      = "demo-tts"
	ModelSTT      = "demo-stt"
)

// Family tokens. They mirror the provider-family vocabulary of the packages
// that consume this one (modelcfg, modelprovider) without importing either, so
// this package stays dependency-free. A mismatch is a compile-time concern of
// the consumers, which map these tokens onto their own constants.
const (
	FamilyLLM      = "llm"
	FamilyImage    = "image"
	FamilyDecision = "decision"
	FamilyRealtime = "realtime"
	FamilyTTS      = "tts"
	FamilySTT      = "stt"
)

// ModelIDs returns the demo models in registration (and display) order.
func ModelIDs() []string {
	return []string{ModelLLM, ModelImage, ModelDecision, ModelRealtime, ModelTTS, ModelSTT}
}

// Family maps a demo model id onto its capability family. ok is false for any
// other id, including a real provider's: a driver must never accept a model it
// does not serve.
func Family(modelID string) (string, bool) {
	switch strings.TrimSpace(modelID) {
	case ModelLLM:
		return FamilyLLM, true
	case ModelImage:
		return FamilyImage, true
	case ModelDecision:
		return FamilyDecision, true
	case ModelRealtime:
		return FamilyRealtime, true
	case ModelTTS:
		return FamilyTTS, true
	case ModelSTT:
		return FamilySTT, true
	default:
		return "", false
	}
}

// Enabled reports whether a raw environment value turns the demo provider on.
// Only an affirmative token counts, so a typo disables the demo instead of
// silently enabling it in a production deployment.
func Enabled(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// EnabledFromGetenv applies Enabled to a getenv-shaped function. Nil is off.
func EnabledFromGetenv(get func(string) string) bool {
	if get == nil {
		return false
	}
	return Enabled(get(Env))
}

// EnabledFromLookup applies Enabled to a LookupEnv-shaped function. Nil is off.
func EnabledFromLookup(lookup func(string) (string, bool)) bool {
	if lookup == nil {
		return false
	}
	value, _ := lookup(Env)
	return Enabled(value)
}

// ReplyText is the assistant text the demo LLM answers with. It is a fixed
// sentence: the demo never echoes user input, so the reply cannot carry data
// out of a session. It reads like an ordinary assistant answer — the demo-ness
// lives in the model id, not in the copy.
const ReplyText = "已收到。这是 keel 服务端直接生成的回复：内容固定、可复现，不来自任何外部模型调用。你可以用它验证会话、流式增量与渲染链路。"

// ResponseID is the fixed completion id every demo response carries, so the
// wire payloads are byte-stable.
const ResponseID = "chatcmpl-demo-0001"

// createdSeconds is the fixed `created` timestamp (2025-01-01T00:00:00Z). A
// real gateway stamps the wall clock; a demo must be reproducible instead.
const createdSeconds = 1735689600

// replyChunkRunes is the delta size in runes. Chunk boundaries sit on rune
// boundaries (never inside a UTF-8 sequence) and the concatenation is exactly
// ReplyText.
const replyChunkRunes = 12

// ReplyChunks splits ReplyText into the deltas one streamed response carries.
func ReplyChunks() []string {
	runes := []rune(ReplyText)
	chunks := make([]string, 0, len(runes)/replyChunkRunes+1)
	for start := 0; start < len(runes); start += replyChunkRunes {
		end := start + replyChunkRunes
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[start:end]))
	}
	return chunks
}

// normalizeModelID maps an unknown id onto ModelLLM, so a caller that built a
// payload for a non-demo model still gets a valid (if unused) document instead
// of a malformed one. The driver refuses unknown models before this is reached.
func normalizeModelID(modelID string) string {
	if _, ok := Family(modelID); ok {
		return strings.TrimSpace(modelID)
	}
	return ModelLLM
}

// WireChunks returns the chat.completion.chunk payloads one demo response
// streams: a role chunk, one chunk per text delta, and a terminal chunk
// carrying finish_reason plus usage. The payloads are the exact OpenAI wire
// shape, so a client's real decoder (and the shared SSE transport) consumes
// them unchanged.
func WireChunks(modelID string) []json.RawMessage {
	modelID = normalizeModelID(modelID)
	deltas := ReplyChunks()
	chunks := make([]json.RawMessage, 0, len(deltas)+2)
	appendChunk := func(delta map[string]any) {
		chunks = append(chunks, mustMarshal(map[string]any{
			"id":      ResponseID,
			"object":  "chat.completion.chunk",
			"created": createdSeconds,
			"model":   modelID,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": nil,
			}},
		}))
	}
	appendChunk(map[string]any{"role": "assistant"})
	completionTokens := 0
	for _, delta := range deltas {
		appendChunk(map[string]any{"content": delta})
		completionTokens += len([]rune(delta))
	}
	terminal := map[string]any{
		"id":      ResponseID,
		"object":  "chat.completion.chunk",
		"created": createdSeconds,
		"model":   modelID,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": completionTokens,
			"total_tokens":      completionTokens,
		},
	}
	return append(chunks, mustMarshal(terminal))
}

// Completion returns the non-streaming chat.completion payload for one demo
// model: the same content the stream carries, in the single-object shape a
// caller that did not ask for streaming expects.
func Completion(modelID string) json.RawMessage {
	modelID = normalizeModelID(modelID)
	return mustMarshal(map[string]any{
		"id":      ResponseID,
		"object":  "chat.completion",
		"created": createdSeconds,
		"model":   modelID,
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": ReplyText,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": len([]rune(ReplyText)),
			"total_tokens":      len([]rune(ReplyText)),
		},
	})
}

// SSE returns the streaming wire body for one demo response in the exact SSE
// shape a gateway sends: one "data: <json>" line per chunk, a blank line after
// each, and the terminating "data: [DONE]". The in-process driver feeds the
// chunk payloads to the shared decoder instead of re-parsing this body; the
// body exists so the wire format itself is testable (and usable by a future
// out-of-process demo server) without an HTTP server.
func SSE(modelID string) []byte {
	var out strings.Builder
	for _, chunk := range WireChunks(modelID) {
		out.WriteString("data: ")
		out.Write(chunk)
		out.WriteString("\n\n")
	}
	out.WriteString("data: [DONE]\n\n")
	return []byte(out.String())
}

// TranscriptionText is the fixed transcript the demo STT returns. Like ReplyText
// it is content, not a description of the running mode: the client renders it as
// the user's speech, so it must read as a sentence someone might have said, never
// as a note about the provider being a demo.
const TranscriptionText = "这是一段由服务端生成的固定转写文本：它可复现、不读取录音内容，用于验证录音上传、转写响应与文本渲染链路。"

// TranscriptionTextFor returns the transcript for one uploaded clip.
//
// When the clip is one this deployment synthesized, it carries the sentence it
// "says" in its RIFF INFO comment, and that sentence is the transcript. Anything
// else gets the fixed fallback: the endpoint never claims to understand audio it
// did not produce, and the byte count keeps the answer a function of the upload.
//
// This is what makes a TTS → STT round trip verifiable without a microphone: the
// browser permission is the only thing blocking the mic path, and a reviewer can
// feed the synthesized clip straight back in.
func TranscriptionTextFor(audio []byte, received int) string {
	sentence, ok := InfoComment(audio)
	if !ok || strings.TrimSpace(sentence) == "" {
		sentence = TranscriptionText
	}
	return fmt.Sprintf("%s（已接收 %d 字节音频）", sentence, received)
}

// mustMarshal marshals a closed, marshallable document. A failure here is a
// programming defect: dropping the value would silently truncate a response,
// so it panics instead.
func mustMarshal(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic("demo: marshal fixed payload: " + err.Error())
	}
	return raw
}

// FamilyInModelIDs reports whether a model id names one of the demo models. It
// exists so a caller comparing a projection against the demo vocabulary does not
// have to inline the comparison (and cannot forget the trim).
func FamilyInModelIDs(modelID string) bool {
	_, ok := Family(modelID)
	return ok
}

// IsProvider reports whether a provider id names the demo provider. It is the
// provider-level counterpart of Family: a caller checking a resolved view does not
// have to repeat the case-insensitive trim.
func IsProvider(providerID string) bool {
	return strings.EqualFold(strings.TrimSpace(providerID), ProviderID)
}
