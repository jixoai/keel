// Tests for the audio media endpoints (media_audio.go). Everything goes
// through httptest: no vendor, no network, no environment from the developer
// shell (each test sets KEEL_DEMO_PROVIDER explicitly).

package httpapi

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	demopkg "github.com/jixoai/keel/internal/demo"
)

const demoEnv = "KEEL_DEMO_PROVIDER"

// setDemoAudioEnv turns the demo provider on (raw "1") or off (empty) for one
// test. Off is set explicitly rather than relying on the shell.
func setDemoAudioEnv(t *testing.T, on bool) {
	t.Helper()
	if on {
		t.Setenv(demoEnv, "1")
		return
	}
	t.Setenv(demoEnv, "")
}

// newAudioServer starts a bare server. The media gateway is left unconfigured
// (no KEEL_MEDIA_*/KEEL_BASE_URL), which is the "no vendor" deployment the
// audio routes must answer 503 in.
func newAudioServer(t *testing.T) *httptest.Server {
	t.Helper()
	clearMediaEnv(t)
	srv := New(Config{Logger: discardLogger()})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	return hs
}

// doSpeech posts a JSON speech request and returns the raw response.
func doSpeech(t *testing.T, hs *httptest.Server, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, hs.URL+"/v1/media/audio/speech", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", uuid.NewString())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// doTranscribe posts one multipart transcription request.
func doTranscribe(t *testing.T, hs *httptest.Server, filename string, audio []byte, fields map[string]string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if filename != "" {
		fw, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(audio); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, hs.URL+"/v1/media/audio/transcriptions", bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Tenant-ID", uuid.NewString())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// readBody drains a response body.
func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// wavInfo is the parsed header of a PCM WAV.
type wavInfo struct {
	riff        string
	wave        string
	format      uint16
	channels    uint16
	sampleRate  uint32
	bitsPer     uint16
	dataLen     uint32
	durationMS  float64
	headerBytes int
}

// parseWAV checks the canonical 44-byte PCM header and reports its fields.
func parseWAV(t *testing.T, raw []byte) wavInfo {
	t.Helper()
	if len(raw) < wavHeaderLen {
		t.Fatalf("wav too short: %d bytes", len(raw))
	}
	info := wavInfo{
		riff:        string(raw[0:4]),
		wave:        string(raw[8:12]),
		format:      binary.LittleEndian.Uint16(raw[20:22]),
		channels:    binary.LittleEndian.Uint16(raw[22:24]),
		sampleRate:  binary.LittleEndian.Uint32(raw[24:28]),
		bitsPer:     binary.LittleEndian.Uint16(raw[34:36]),
		dataLen:     binary.LittleEndian.Uint32(raw[40:44]),
		headerBytes: wavHeaderLen,
	}
	if info.riff != "RIFF" || info.wave != "WAVE" || string(raw[36:40]) != "data" {
		t.Fatalf("not a RIFF/WAVE file with a data chunk: riff=%q wave=%q chunk=%q",
			info.riff, info.wave, string(raw[36:40]))
	}
	if int(info.dataLen) != len(raw)-wavHeaderLen {
		t.Fatalf("data chunk = %d, actual payload = %d", info.dataLen, len(raw)-wavHeaderLen)
	}
	byteRate := float64(info.sampleRate) * float64(info.channels) * float64(info.bitsPer) / 8
	info.durationMS = float64(info.dataLen) / byteRate * 1000
	return info
}

func TestAudioSpeechDemoReturnsPlayableWAV(t *testing.T) {
	setDemoAudioEnv(t, true)
	hs := newAudioServer(t)

	resp := doSpeech(t, hs, map[string]any{"text": "你好，这是一段语音合成测试。"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, readBody(t, resp))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/wav" {
		t.Fatalf("content-type = %q, want audio/wav", ct)
	}
	if model := resp.Header.Get("x-keel-model"); model != "demo-tts" {
		t.Fatalf("x-keel-model = %q, want demo-tts", model)
	}
	raw := readBody(t, resp)
	info := parseWAV(t, raw)
	if info.format != 1 {
		t.Fatalf("format = %d, want 1 (PCM)", info.format)
	}
	if info.channels != 1 {
		t.Fatalf("channels = %d, want mono", info.channels)
	}
	if info.sampleRate != 16000 {
		t.Fatalf("sample rate = %d, want 16000", info.sampleRate)
	}
	if info.bitsPer != 16 {
		t.Fatalf("bits per sample = %d, want 16", info.bitsPer)
	}
	if info.durationMS <= 0 {
		t.Fatalf("duration = %.1f ms, want > 0", info.durationMS)
	}
	if info.durationMS > 1000 {
		t.Fatalf("duration = %.1f ms, want <= 1000", info.durationMS)
	}
	// A silent clip would satisfy the header but not "real audio": at least one
	// sample must be non-zero.
	nonZero := false
	for i := wavHeaderLen; i+1 < len(raw); i += 2 {
		if binary.LittleEndian.Uint16(raw[i:]) != 0 {
			nonZero = true
			break
		}
	}
	if !nonZero {
		t.Fatal("clip is all zero samples")
	}
}

func TestAudioSpeechDemoIsDeterministicAndLengthSensitive(t *testing.T) {
	setDemoAudioEnv(t, true)
	hs := newAudioServer(t)

	first := readBody(t, doSpeech(t, hs, map[string]any{"text": "short"}))
	again := readBody(t, doSpeech(t, hs, map[string]any{"text": "short"}))
	if !bytes.Equal(first, again) {
		t.Fatal("identical text produced different bytes")
	}
	shortMS := parseWAV(t, first).durationMS

	long := readBody(t, doSpeech(t, hs, map[string]any{"text": strings.Repeat("长", 200)}))
	longMS := parseWAV(t, long).durationMS
	if longMS < shortMS {
		t.Fatalf("longer text produced a shorter clip: %0.1f ms < %0.1f ms", longMS, shortMS)
	}
	if longMS > 1000 {
		t.Fatalf("duration = %.1f ms, want bounded at 1000", longMS)
	}
	if longMS != 1000 {
		t.Fatalf("max-length text duration = %.1f ms, want the 1000 ms bound", longMS)
	}
}

func TestAudioSpeechRejectsBadRequests(t *testing.T) {
	setDemoAudioEnv(t, true)
	hs := newAudioServer(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing text", map[string]any{}},
		{"blank text", map[string]any{"text": "   "}},
		{"unknown model", map[string]any{"text": "hi", "model": "vendor-tts-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doSpeech(t, hs, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, readBody(t, resp))
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("content-type = %q, want JSON error", ct)
			}
		})
	}
}

func TestAudioSpeechAcceptsExplicitDemoModel(t *testing.T) {
	setDemoAudioEnv(t, true)
	hs := newAudioServer(t)

	resp := doSpeech(t, hs, map[string]any{"text": "hello", "model": "demo-tts", "voice": "demo"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, readBody(t, resp))
	}
	if model := resp.Header.Get("x-keel-model"); model != "demo-tts" {
		t.Fatalf("x-keel-model = %q", model)
	}
}

func TestAudioTranscriptionsDemo(t *testing.T) {
	setDemoAudioEnv(t, true)
	hs := newAudioServer(t)

	audio := bytes.Repeat([]byte{0x01, 0x02}, 500) // 1000 bytes
	resp := doTranscribe(t, hs, "speech.webm", audio, map[string]string{"language": "zh"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, readBody(t, resp))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want JSON", ct)
	}
	var out struct {
		Text  string `json:"text"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(readBody(t, resp), &out); err != nil {
		t.Fatalf("bad response JSON: %v", err)
	}
	if !strings.HasPrefix(out.Text, demopkg.TranscriptionText) {
		t.Fatalf("text = %q, want the fixed demo transcript %q", out.Text, demopkg.TranscriptionText)
	}
	if !strings.Contains(out.Text, "1000 字节") {
		t.Fatalf("text = %q, want the uploaded byte count", out.Text)
	}
	if out.Model != "demo-stt" {
		t.Fatalf("model = %q", out.Model)
	}

	// The transcript is a function of the byte count only: same size, same
	// answer; different size, different answer.
	otherBody := readBody(t, doTranscribe(t, hs, "speech.wav", bytes.Repeat([]byte{0x00}, 1000), nil))
	var other struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(otherBody, &other); err != nil {
		t.Fatalf("bad response JSON: %v (%s)", err, otherBody)
	}
	if other.Text != out.Text {
		t.Fatalf("same-size upload changed the transcript: %q != %q", other.Text, out.Text)
	}
	smaller := readBody(t, doTranscribe(t, hs, "speech.wav", bytes.Repeat([]byte{0x00}, 10), nil))
	if !strings.Contains(string(smaller), "10 字节") {
		t.Fatalf("smaller upload transcript = %s", smaller)
	}
}

func TestAudioTranscriptionsRejectsBadRequests(t *testing.T) {
	setDemoAudioEnv(t, true)
	hs := newAudioServer(t)

	t.Run("missing file field", func(t *testing.T) {
		resp := doTranscribe(t, hs, "", nil, map[string]string{"model": "demo-stt"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, readBody(t, resp))
		}
	})
	t.Run("unknown model", func(t *testing.T) {
		resp := doTranscribe(t, hs, "a.wav", []byte("x"), map[string]string{"model": "vendor-whisper"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, readBody(t, resp))
		}
	})
	t.Run("non-multipart body", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, hs.URL+"/v1/media/audio/transcriptions",
			strings.NewReader(`{"file":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tenant-ID", uuid.NewString())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, readBody(t, resp))
		}
	})
}

func TestAudioTranscriptionsUploadCap(t *testing.T) {
	if testing.Short() {
		t.Skip("8 MiB upload")
	}
	setDemoAudioEnv(t, true)
	hs := newAudioServer(t)

	// 9 MiB of payload plus the multipart envelope: over the 8 MiB cap.
	resp := doTranscribe(t, hs, "big.wav", make([]byte, 9<<20), nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, readBody(t, resp))
	}

	// Just under the cap still works (the relaxation is real, not nominal).
	ok := doTranscribe(t, hs, "ok.wav", make([]byte, 7<<20), nil)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("7 MiB status = %d, body = %s", ok.StatusCode, readBody(t, ok))
	}
}

func TestAudioUnconfigured503(t *testing.T) {
	setDemoAudioEnv(t, false)
	hs := newAudioServer(t)

	t.Run("speech", func(t *testing.T) {
		resp := doSpeech(t, hs, map[string]any{"text": "hello"})
		body := string(readBody(t, resp))
		t.Logf("POST /v1/media/audio/speech, KEEL_DEMO_PROVIDER unset -> %d %s (content-type %q)",
			resp.StatusCode, body, resp.Header.Get("Content-Type"))
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		if body != `{"error":"audio provider is not configured"}` {
			t.Fatalf("body = %s", body)
		}
		if ct := resp.Header.Get("x-keel-model"); ct != "" {
			t.Fatalf("unconfigured speech must not name a model, got %q", ct)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Fatalf("unconfigured speech must answer a JSON error, content-type = %q", ct)
		}
	})

	t.Run("transcriptions", func(t *testing.T) {
		resp := doTranscribe(t, hs, "a.wav", []byte("data"), nil)
		body := string(readBody(t, resp))
		t.Logf("POST /v1/media/audio/transcriptions, KEEL_DEMO_PROVIDER unset -> %d %s", resp.StatusCode, body)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		if body != `{"error":"audio provider is not configured"}` {
			t.Fatalf("body = %s", body)
		}
	})
}

// TestAudioUnconfiguredEvenWithImageVendor pins the second half of the 503 rule:
// a deployment that *does* have a media vendor configured still has no audio
// upstream (the token plan has no TTS/STT vendor), so the audio routes answer
// 503 rather than forwarding text/audio to a vendor endpoint that does not
// serve audio.
func TestAudioUnconfiguredEvenWithImageVendor(t *testing.T) {
	setDemoAudioEnv(t, false)
	fake, provider := newFakeProvider(t)
	setMediaEnv(t, provider.URL)
	srv := New(Config{Logger: discardLogger()})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	if resp := doSpeech(t, hs, map[string]any{"text": "hello"}); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("speech status = %d, want 503", resp.StatusCode)
	}
	if resp := doTranscribe(t, hs, "a.wav", []byte("data"), nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("transcriptions status = %d, want 503", resp.StatusCode)
	}
	if got := fake.callCount("/audio/speech") + fake.callCount("/audio/transcriptions"); got != 0 {
		t.Fatalf("audio routes reached the media vendor %d times", got)
	}
}

func TestAudioCapabilitiesFollowDemoSwitch(t *testing.T) {
	type caps struct {
		Configured bool `json:"configured"`
		Image      bool `json:"image"`
		Decision   bool `json:"decision"`
		TTS        bool `json:"tts"`
		STT        bool `json:"stt"`
	}
	read := func(t *testing.T, hs *httptest.Server) (caps, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, hs.URL+"/v1/media/capabilities", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Tenant-ID", uuid.NewString())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body := string(readBody(t, resp))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		var out caps
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("bad capabilities JSON: %v (%s)", err, body)
		}
		return out, body
	}

	t.Run("demo off", func(t *testing.T) {
		setDemoAudioEnv(t, false)
		hs := newAudioServer(t)
		out, body := read(t, hs)
		t.Logf("GET /v1/media/capabilities, KEEL_DEMO_PROVIDER unset -> %s", body)
		if out.TTS || out.STT {
			t.Fatalf("tts/stt must be false without the demo provider: %s", body)
		}
		if out.Configured || out.Image || out.Decision {
			t.Fatalf("demo off must not change the vendor flags: %s", body)
		}
	})

	t.Run("demo on", func(t *testing.T) {
		setDemoAudioEnv(t, true)
		hs := newAudioServer(t)
		out, body := read(t, hs)
		t.Logf("GET /v1/media/capabilities, KEEL_DEMO_PROVIDER=1 -> %s", body)
		if !out.TTS || !out.STT {
			t.Fatalf("tts/stt must be true with the demo provider: %s", body)
		}
		// Only the audio flags are this file's concern. The image/decision/
		// configured flags belong to the media gateway's own demo provider
		// (internal/mediagw); this test must not pin their policy here.
	})
}

func TestAudioRoutesRequireTenant(t *testing.T) {
	setDemoAudioEnv(t, true)
	hs := newAudioServer(t)

	cases := []struct {
		name string
		make func() *http.Request
	}{
		{"speech", func() *http.Request {
			req, _ := http.NewRequest(http.MethodPost, hs.URL+"/v1/media/audio/speech",
				strings.NewReader(`{"text":"hi"}`))
			req.Header.Set("Content-Type", "application/json")
			return req
		}},
		{"transcriptions", func() *http.Request {
			req, _ := http.NewRequest(http.MethodPost, hs.URL+"/v1/media/audio/transcriptions",
				strings.NewReader(""))
			req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
			return req
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.DefaultClient.Do(tc.make())
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, readBody(t, resp))
			}
		})
	}
}

// A TTS → STT round trip is the microphone-free way to prove the speech path:
// the endpoint recognizes the deployment's own synthesized clip and answers with
// the sentence it "says", instead of the generic fallback. This is the product
// behaviour (an upload is a first-class input), not a test hook.
func TestSpeechToTranscriptionRoundTrip(t *testing.T) {
	setDemoAudioEnv(t, true)
	hs := newAudioServer(t)

	// 1) Synthesize.
	speech := doSpeech(t, hs, map[string]any{"text": "这是服务端合成的确认音频，用于验证语音链路。"})
	if speech.StatusCode != http.StatusOK {
		t.Fatalf("speech = %d", speech.StatusCode)
	}
	clip, err := io.ReadAll(speech.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !demopkg.SpeechWAVLooksValid(clip) {
		t.Fatalf("speech body is not a WAV (%d bytes)", len(clip))
	}

	// 2) Feed the same bytes back as a recording.
	resp := doTranscribe(t, hs, "roundtrip.wav", clip, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transcriptions = %d", resp.StatusCode)
	}
	var body struct {
		Text  string `json:"text"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Model != demopkg.ModelSTT {
		t.Fatalf("model = %q, want %q", body.Model, demopkg.ModelSTT)
	}
	if !strings.Contains(body.Text, "这是服务端合成的确认音频，用于验证语音链路。") {
		t.Fatalf("round-trip transcript = %q, want it to carry the synthesized sentence", body.Text)
	}
	// The byte count still proves the upload was read.
	if !strings.Contains(body.Text, fmt.Sprintf("%d 字节", len(clip))) {
		t.Fatalf("transcript lost the received byte count: %q", body.Text)
	}
}

// Any other audio keeps the deterministic fallback: the endpoint must not claim
// to understand content it never read.
func TestTranscriptionOfForeignAudioUsesTheFallback(t *testing.T) {
	setDemoAudioEnv(t, true)
	hs := newAudioServer(t)
	resp := doTranscribe(t, hs, "other.wav", []byte("not the demo clip"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transcriptions = %d", resp.StatusCode)
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Text, demopkg.TranscriptionText) {
		t.Fatalf("fallback transcript = %q", body.Text)
	}
	if strings.Contains(body.Text, "这是服务端合成的确认音频，用于验证语音链路。") {
		t.Fatal("foreign audio was reported as the synthesized clip")
	}
}
