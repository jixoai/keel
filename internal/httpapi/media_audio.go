// Audio media endpoints: POST /v1/media/audio/speech (text to speech) and
// POST /v1/media/audio/transcriptions (speech to text).
//
// The web client already speaks this contract (web/src/data/media.ts):
//
//	speech          {"text","voice?","model?"} -> audio bytes (audio/wav) with
//	                                             an `x-keel-model` header;
//	transcriptions  multipart `file` (+ optional `model`,`language`)
//	                -> {"text","model?"}.
//
// The only provider behind these routes in this change is the demo provider
// (internal/demo). It is enabled by KEEL_DEMO_PROVIDER=1 and answers
// deterministic, server-side content: a short 16 kHz mono 16-bit sine WAV whose
// duration follows the input length (bounded to 1 s), and the fixed transcript
// the demo package ships. Nothing here performs IO, calls a vendor or reads
// user text back into the response.
//
// Without the demo provider there is no audio upstream at all (this deployment
// has no TTS/STT vendor), so both routes answer 503 with a neutral body. The
// body never names a credential, a URL or an upstream host.
//
// Body caps: the JSON speech body reuses the media gateway's 1 MiB bound. The
// transcription upload is capped at 8 MiB instead: a real recording (a few
// seconds of 16 kHz PCM or webm/opus) routinely exceeds 1 MiB, and rejecting it
// would make the endpoint unusable the moment a recorder is attached. The cap
// is still a hard request-body limit (multipart envelope included), never a
// content contract.

package httpapi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/jixoai/keel/internal/demo"
	"github.com/jixoai/keel/internal/mediagw"
)

const (
	// audioSpeechMaxBody caps the JSON speech request like the rest of the
	// media surface (1 MiB).
	audioSpeechMaxBody = 1 << 20

	// audioUploadMaxBytes caps one transcription upload (multipart body). See
	// the package comment for why it is larger than the media JSON bound.
	audioUploadMaxBytes = 8 << 20

	// audioUploadMemory caps how much of a multipart body stays in RAM; the
	// rest spills to a temp file that is removed below. The total size is
	// already bounded by audioUploadMaxBytes.
	audioUploadMemory = 1 << 20

	// demoSpeechSampleRate is the WAV sample rate. 16 kHz mono keeps the clip
	// small and matches the rate the realtime path uses for speech input.
	demoSpeechSampleRate = 16000

	// demoSpeechMinMillis / demoSpeechMaxMillis bound the generated clip. The
	// duration grows with the input so a long sentence is audible longer than
	// a short one, and never exceeds one second.
	demoSpeechMinMillis     = 200
	demoSpeechMaxMillis     = 1000
	demoSpeechMillisPerRune = 45

	// demoToneHz is the pitch of the synthetic tone. It is a constant (not
	// derived from the text) so identical requests produce identical bytes.
	demoToneHz = 440.0

	// demoAmplitude keeps the tone well below full scale.
	demoAmplitude = 0.25

	// wavHeaderLen is the canonical RIFF/WAVE PCM header length.
	wavHeaderLen = 44
)

// registerMediaAudioRoutes wires the two audio capability endpoints. They are
// registered next to the image/decision media routes but live in their own
// file so the audio path can be reviewed (and parallel work on the image path
// merged) independently.
func (s *Server) registerMediaAudioRoutes() {
	s.mux.HandleFunc("POST /v1/media/audio/speech", s.mediaAudioSpeech)
	s.mux.HandleFunc("POST /v1/media/audio/transcriptions", s.mediaAudioTranscriptions)
}

// audioDemoEnabled reports whether the demo audio provider is switched on. The
// environment is read per request, like the realtime bridge does: a process
// environment does not change at runtime, so this only keeps tests and
// single-binary demos honest without a restart.
func audioDemoEnabled() bool {
	return demo.EnabledFromLookup(os.LookupEnv)
}

// writeAudioUnconfigured is the single 503 shape of both routes.
func writeAudioUnconfigured(w http.ResponseWriter) {
	writeErr(w, http.StatusServiceUnavailable, errors.New("audio provider is not configured"))
}

// mediaAudioSpeech handles POST /v1/media/audio/speech.
func (s *Server) mediaAudioSpeech(w http.ResponseWriter, r *http.Request) {
	if !mediaTenant(w, r) {
		return
	}
	if !audioDemoEnabled() {
		writeAudioUnconfigured(w)
		return
	}
	var body struct {
		Text string `json:"text"`
		// Voice is accepted for contract compatibility and ignored: the demo
		// tone is a constant, so claiming to honor a voice would be a lie.
		Voice string `json:"voice"`
		Model string `json:"model"`
	}
	if err := decodeBody(w, r, audioSpeechMaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, http.StatusBadRequest, errors.New("text is required"))
		return
	}
	model, err := audioModel(body.Model, demo.ModelTTS)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// The clip carries the sentence it "says" (RIFF LIST/INFO "ICMT"), so the
	// transcription endpoint can read it back: that is what makes a TTS → STT
	// round trip real instead of two constants that happen to match.
	wav := demo.WithInfoComment(demoSpeechWAV(text), text)
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("x-keel-model", model)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(wav)
}

// mediaAudioTranscriptions handles POST /v1/media/audio/transcriptions.
func (s *Server) mediaAudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	if !mediaTenant(w, r) {
		return
	}
	if !audioDemoEnabled() {
		writeAudioUnconfigured(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, audioUploadMaxBytes)
	if err := r.ParseMultipartForm(audioUploadMemory); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, errors.New("audio upload exceeds 8 MiB"))
			return
		}
		writeErr(w, http.StatusBadRequest, errors.New("malformed multipart body"))
		return
	}
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New(`multipart field "file" is required`))
		return
	}
	defer func() { _ = file.Close() }()
	// The body cap already bounds this copy; the LimitReader keeps the handler
	// from trusting the multipart envelope alone. The bytes are kept (not
	// discarded) so the transcript can recognise this deployment's own
	// synthesized clip — that is what makes a TTS → STT round trip verifiable
	// without a microphone. Nothing here reads the audio for content.
	audio, err := io.ReadAll(io.LimitReader(file, audioUploadMaxBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("failed to read uploaded audio"))
		return
	}
	written := len(audio)
	if written > audioUploadMaxBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, errors.New("audio upload exceeds 8 MiB"))
		return
	}
	model, err := audioModel(r.FormValue("model"), demo.ModelSTT)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// `language` is accepted by the contract and ignored: the demo transcript is
	// fixed, and pretending to detect a language would be an invented fact. The
	// received byte count is appended so the transcript is still a function of
	// the upload (deterministic for a given request) without echoing content.
	writeJSON(w, http.StatusOK, struct {
		Text  string `json:"text"`
		Model string `json:"model"`
	}{
		Text:  demo.TranscriptionTextFor(audio, written),
		Model: model,
	})
}

// audioModel normalizes the optional `model` field against the single demo
// model this route serves. An empty value takes the default; any other value is
// refused instead of being silently re-routed to the demo model (a client that
// selected a real vendor model must learn that this deployment cannot serve
// it).
func audioModel(requested, allowed string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" || requested == allowed {
		return allowed, nil
	}
	return "", fmt.Errorf("model %q is not allowed; use %q", requested, allowed)
}

// withAudioCapabilities folds the demo audio provider into the media
// capability report. It only ever turns flags on: without the demo provider the
// mediagw values (currently always false for tts/stt) are reported unchanged,
// and no other capability is touched.
func withAudioCapabilities(base mediagw.Capabilities) mediagw.Capabilities {
	if audioDemoEnabled() {
		base.TTS = true
		base.STT = true
	}
	return base
}

// demoSpeechWAV renders the deterministic clip for text: a 16 kHz mono 16-bit
// PCM WAV whose duration grows with the input length (bounded).
func demoSpeechWAV(text string) []byte {
	return encodeWAVPCM16(demoSpeechPCM(text), demoSpeechSampleRate)
}

// demoSpeechDurationMillis maps text length onto a bounded clip duration.
func demoSpeechDurationMillis(text string) int {
	n := utf8.RuneCountInString(strings.TrimSpace(text))
	ms := demoSpeechMinMillis + n*demoSpeechMillisPerRune
	if ms > demoSpeechMaxMillis {
		ms = demoSpeechMaxMillis
	}
	return ms
}

// demoSpeechPCM synthesizes the samples: a steady tone with a short fade in and
// out so the clip does not start or end on a click.
func demoSpeechPCM(text string) []int16 {
	total := demoSpeechSampleRate * demoSpeechDurationMillis(text) / 1000
	if total < 1 {
		total = 1
	}
	fade := demoSpeechSampleRate / 200 // 5 ms
	if fade > total/2 {
		fade = total / 2
	}
	pcm := make([]int16, total)
	for i := range pcm {
		v := math.Sin(2*math.Pi*demoToneHz*float64(i)/float64(demoSpeechSampleRate)) * demoAmplitude
		if fade > 0 {
			if i < fade {
				v *= float64(i) / float64(fade)
			}
			if tail := total - 1 - i; tail < fade {
				v *= float64(tail) / float64(fade)
			}
		}
		pcm[i] = int16(math.Round(v * math.MaxInt16))
	}
	return pcm
}

// encodeWAVPCM16 writes the canonical 44-byte RIFF/WAVE header plus little
// endian PCM16 samples.
func encodeWAVPCM16(samples []int16, sampleRate int) []byte {
	dataLen := len(samples) * 2
	out := make([]byte, wavHeaderLen+dataLen)
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+dataLen))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16) // fmt chunk size
	binary.LittleEndian.PutUint16(out[20:22], 1)  // PCM
	binary.LittleEndian.PutUint16(out[22:24], 1)  // mono
	binary.LittleEndian.PutUint32(out[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(sampleRate*2)) // byte rate
	binary.LittleEndian.PutUint16(out[32:34], 2)                    // block align
	binary.LittleEndian.PutUint16(out[34:36], 16)                   // bits per sample
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(dataLen))
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(out[wavHeaderLen+i*2:], uint16(sample))
	}
	return out
}
