package mediaadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeSpeechProvider struct {
	mu            sync.Mutex
	synthRequests []SynthesizeRequest
	transRequests []TranscribeRequest
	audio         SpeechAudio
	transcript    SpeechTranscript
	synthErr      error
	transcribeErr error
	panicValue    any
}

func (p *fakeSpeechProvider) Synthesize(_ context.Context, req SynthesizeRequest) (SpeechAudio, error) {
	p.mu.Lock()
	p.synthRequests = append(p.synthRequests, req)
	p.mu.Unlock()
	if p.panicValue != nil {
		panic(p.panicValue)
	}
	if p.synthErr != nil {
		return SpeechAudio{}, p.synthErr
	}
	return p.audio, nil
}

func (p *fakeSpeechProvider) Transcribe(_ context.Context, req TranscribeRequest) (SpeechTranscript, error) {
	p.mu.Lock()
	p.transRequests = append(p.transRequests, req)
	p.mu.Unlock()
	if p.panicValue != nil {
		panic(p.panicValue)
	}
	if p.transcribeErr != nil {
		return SpeechTranscript{}, p.transcribeErr
	}
	return p.transcript, nil
}

func TestSpeechSynthesizeWireAndArtifact(t *testing.T) {
	var posted map[string]any
	audio := []byte("RIFF....WAVEfmt ")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/speech" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer speech-key" {
			t.Errorf("authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("X-Audio-Duration-Ms", "1234")
		_, _ = w.Write(audio)
	}))
	defer server.Close()

	provider, err := NewHTTPSpeechProvider(HTTPSpeechConfig{BaseURL: server.URL, APIKey: "speech-key", Model: "tts-1"})
	if err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	adapter := &SpeechAdapter{Provider: provider, Store: store}
	result, err := adapter.Synthesize(context.Background(), "session-s", SynthesizeRequest{
		Text:     "你好，世界",
		Voice:    "alloy",
		Language: "zh-CN",
		Format:   "wav",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Model != "tts-1" {
		t.Fatalf("model = %q", result.Model)
	}
	if result.Audio.Kind != ArtifactAudio || result.Audio.Mime != "audio/wav" {
		t.Fatalf("artifact = %+v", result.Audio)
	}
	if result.Audio.DurationMillis != 1234 {
		t.Fatalf("duration = %d", result.Audio.DurationMillis)
	}
	if result.Usage.InputCharacters != 5 || result.Usage.AudioBytes != int64(len(audio)) || result.Usage.AudioMillis != 1234 {
		t.Fatalf("usage = %+v", result.Usage)
	}
	if got := string(store.call(0).Data); got != string(audio) {
		t.Fatalf("stored audio = %q", got)
	}
	body, _ := json.Marshal(posted)
	for _, want := range []string{`"input":"你好，世界"`, `"voice":"alloy"`, `"language":"zh-CN"`, `"response_format":"wav"`, `"model":"tts-1"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("wire body missing %s: %s", want, body)
		}
	}
	t.Logf("wire POST /audio/speech body=%s", body)
	t.Logf("audio reference=%+v usage=%+v", result.Audio, result.Usage)
	raw, _ := json.Marshal(result)
	for _, forbidden := range []string{"RIFF", server.URL, "speech-key", "cost"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("result leaked %q: %s", forbidden, raw)
		}
	}
}

func TestSpeechTranscribeWireTimestampsAndAudioReference(t *testing.T) {
	audio := []byte("fake-webm-bytes")
	var (
		fileBytes []byte
		model     string
		language  string
		format    string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("form file: %v", err)
			return
		}
		fileBytes, _ = io.ReadAll(file)
		model = r.FormValue("model")
		language = r.FormValue("language")
		format = r.FormValue("response_format")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "text": "hello world",
		  "language": "en",
		  "duration": 1.5,
		  "segments": [
		    {"start": 0, "end": 0.7, "text": "hello"},
		    {"start": 0.7, "end": 1.5, "text": "world"}
		  ],
		  "usage": {"input_tokens": 3, "output_tokens": 2, "cost": 0.11}
		}`))
	}))
	defer server.Close()

	provider, err := NewHTTPSpeechProvider(HTTPSpeechConfig{BaseURL: server.URL, APIKey: "k", Model: "stt-1"})
	if err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	adapter := &SpeechAdapter{Provider: provider, Store: store}
	result, err := adapter.Transcribe(context.Background(), "session-s", TranscribeRequest{
		Filename: "clip.webm",
		Mime:     "audio/webm",
		Language: "en",
		Audio:    audio,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Transcript.Text != "hello world" || result.Transcript.Language != "en" {
		t.Fatalf("transcript = %+v", result.Transcript)
	}
	if len(result.Transcript.Segments) != 2 ||
		result.Transcript.Segments[1].StartMillis != 700 ||
		result.Transcript.Segments[1].EndMillis != 1500 {
		t.Fatalf("segments = %+v", result.Transcript.Segments)
	}
	if result.Transcript.DurationMillis != 1500 {
		t.Fatalf("duration = %d", result.Transcript.DurationMillis)
	}
	if result.Transcript.Usage.InputTokens != 3 || result.Transcript.Usage.OutputTokens != 2 ||
		result.Transcript.Usage.TranscriptCharacters != 11 ||
		result.Transcript.Usage.AudioBytes != int64(len(audio)) {
		t.Fatalf("usage = %+v", result.Transcript.Usage)
	}
	if result.Audio == nil || result.Audio.Kind != ArtifactAudio {
		t.Fatalf("audio reference = %+v", result.Audio)
	}
	if got := string(store.call(0).Data); got != string(audio) {
		t.Fatalf("stored audio = %q", got)
	}
	if string(fileBytes) != string(audio) || model != "stt-1" || language != "en" || format != "verbose_json" {
		t.Fatalf("multipart = file=%q model=%q language=%q format=%q", fileBytes, model, language, format)
	}
	t.Logf("wire POST /audio/transcriptions file=%q model=%q language=%q format=%q", fileBytes, model, language, format)
	t.Logf("transcript=%+v audio reference=%+v", result.Transcript, result.Audio)
	raw, _ := json.Marshal(result)
	for _, forbidden := range []string{"cost", "0.11", "application/octet-stream"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("result leaked %q: %s", forbidden, raw)
		}
	}
}

func TestSpeechTranscribeRejectsEmptyTranscript(t *testing.T) {
	provider := &fakeSpeechProvider{transcript: SpeechTranscript{Text: " "}}
	adapter := &SpeechAdapter{Provider: provider}
	_, err := adapter.Transcribe(context.Background(), "s", TranscribeRequest{Audio: []byte("x")})
	var typed *Error
	if !asAdapterError(err, &typed) {
		t.Fatalf("error = %v", err)
	}
	if typed.Kind != KindUpstream {
		t.Fatalf("kind = %q", typed.Kind)
	}
}

func TestSpeechHTTPStatusIsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limited; retry-after 2"}}`)
	}))
	defer server.Close()
	provider, err := NewHTTPSpeechProvider(HTTPSpeechConfig{BaseURL: server.URL, APIKey: "k", Model: "tts"})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &SpeechAdapter{Provider: provider, Store: &recordingStore{}}
	_, err = adapter.Synthesize(context.Background(), "s", SynthesizeRequest{Text: "hi"})
	var typed *Error
	if !asAdapterError(err, &typed) {
		t.Fatalf("error = %v", err)
	}
	if typed.Status != http.StatusTooManyRequests || !typed.Retryable {
		t.Fatalf("error = %+v", typed)
	}
}

func TestSpeechSynthesizeRejectsOversizedAudio(t *testing.T) {
	provider := &fakeSpeechProvider{audio: SpeechAudio{Data: []byte("too-large"), Mime: "audio/wav"}}
	adapter := &SpeechAdapter{Provider: provider, Store: &recordingStore{}, MaxAudioSize: 4}
	_, err := adapter.Synthesize(context.Background(), "s", SynthesizeRequest{Text: "hi"})
	var typed *Error
	if !asAdapterError(err, &typed) {
		t.Fatalf("error = %v", err)
	}
	if typed.Kind != KindUpstream {
		t.Fatalf("kind = %q", typed.Kind)
	}
}

// TestSpeechTranscribeRejectsMultipartHeaderInjection pins the STT boundary:
// filename and mime are model-controlled and both become multipart headers. A
// CR/LF or control character must be refused before a request is built, and
// mime must be a bare type/subtype token (no parameters, no separators).
func TestSpeechTranscribeRejectsMultipartHeaderInjection(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"should not be reached"}`)
	}))
	defer server.Close()
	provider, err := NewHTTPSpeechProvider(HTTPSpeechConfig{BaseURL: server.URL, APIKey: "k", Model: "stt-1"})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		filename string
		mime     string
		field    string
	}{
		{name: "filename CRLF", filename: "memo.wav\r\nX-Evil: 1", mime: "audio/wav", field: "filename"},
		{name: "filename LF", filename: "memo.wav\nX-Evil: 1", mime: "audio/wav", field: "filename"},
		{name: "filename NUL", filename: "memo\x00.wav", mime: "audio/wav", field: "filename"},
		{name: "filename tab", filename: "memo\t.wav", mime: "audio/wav", field: "filename"},
		{name: "mime CRLF", filename: "memo.wav", mime: "audio/wav\r\nX-Evil: 1", field: "mime"},
		{name: "mime control", filename: "memo.wav", mime: "audio/wav\x01", field: "mime"},
		{name: "mime parameters", filename: "memo.wav", mime: "audio/wav; charset=binary", field: "mime"},
		{name: "mime whitespace", filename: "memo.wav", mime: "audio /wav", field: "mime"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := provider.Transcribe(context.Background(), TranscribeRequest{
				Filename: tc.filename,
				Mime:     tc.mime,
				Audio:    []byte("clip"),
			})
			var typed *Error
			if !asAdapterError(err, &typed) {
				t.Fatalf("error = %v, want a typed adapter error", err)
			}
			if typed.Kind != KindRequest {
				t.Fatalf("kind = %q, want invalid_request", typed.Kind)
			}
			if !hasField(typed.Fields, tc.field, "") {
				t.Fatalf("fields = %+v, want %s", typed.Fields, tc.field)
			}
			if strings.ContainsAny(err.Error(), "\r\n\x00\t") {
				t.Fatalf("error carried a control character: %q", err.Error())
			}
		})
	}
	if calls != 0 {
		t.Fatalf("an invalid filename/mime reached the wire %d times", calls)
	}
}

// TestSpeechTranscribeEscapesQuotedFilename keeps quotes and backslashes legal
// while proving the resulting part has exactly one Content-Disposition and no
// injected header.
func TestSpeechTranscribeEscapesQuotedFilename(t *testing.T) {
	type observation struct {
		filename string
		partType string
		evil     string
		data     string
		err      error
	}
	observed := make(chan observation, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out observation
		reader, err := r.MultipartReader()
		if err != nil {
			out.err = err
			observed <- out
			return
		}
		part, err := reader.NextPart()
		if err != nil {
			out.err = err
			observed <- out
			return
		}
		out.filename = part.FileName()
		out.partType = part.Header.Get("Content-Type")
		out.evil = part.Header.Get("X-Evil")
		data, readErr := io.ReadAll(part)
		out.data = string(data)
		out.err = readErr
		observed <- out
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"ok"}`)
	}))
	defer server.Close()
	provider, err := NewHTTPSpeechProvider(HTTPSpeechConfig{BaseURL: server.URL, APIKey: "k", Model: "stt-1"})
	if err != nil {
		t.Fatal(err)
	}
	filename := `memo"quote\name.wav`
	result, err := provider.Transcribe(context.Background(), TranscribeRequest{
		Filename: filename,
		Mime:     "audio/wav",
		Audio:    []byte("clip"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "ok" {
		t.Fatalf("transcript = %q", result.Text)
	}
	out := <-observed
	if out.err != nil {
		t.Fatalf("multipart parse: %v", out.err)
	}
	if out.filename != filename {
		t.Fatalf("filename = %q, want %q", out.filename, filename)
	}
	if out.partType != "audio/wav" {
		t.Fatalf("part content-type = %q", out.partType)
	}
	if out.evil != "" {
		t.Fatalf("injected header reached the part: X-Evil=%q", out.evil)
	}
	if out.data != "clip" {
		t.Fatalf("part data = %q", out.data)
	}
}

func TestSpeechTranscribeDefaultsSafeMetadata(t *testing.T) {
	var filename, partType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := r.MultipartReader()
		if err != nil {
			t.Errorf("multipart reader: %v", err)
			return
		}
		part, err := reader.NextPart()
		if err != nil {
			t.Errorf("next part: %v", err)
			return
		}
		filename, partType = part.FileName(), part.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"ok"}`)
	}))
	defer server.Close()
	provider, err := NewHTTPSpeechProvider(HTTPSpeechConfig{BaseURL: server.URL, APIKey: "k", Model: "stt-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Transcribe(context.Background(), TranscribeRequest{Audio: []byte("clip")}); err != nil {
		t.Fatal(err)
	}
	if filename != "audio" || partType != "application/octet-stream" {
		t.Fatalf("defaults = filename %q type %q", filename, partType)
	}
}
