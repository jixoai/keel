package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func lookupFrom(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

type dialCapture struct {
	conn   Conn
	url    string
	header http.Header
	err    error
	calls  int
}

func (d *dialCapture) dial(_ context.Context, endpoint string, header http.Header) (Conn, error) {
	d.calls++
	d.url = endpoint
	d.header = header.Clone()
	return d.conn, d.err
}

func TestFromLookupNotConfigured(t *testing.T) {
	if _, err := FromLookup(lookupFrom(nil)); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if _, err := NewProvider(Config{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("NewProvider(empty) err = %v, want ErrNotConfigured", err)
	}
}

func TestFromLookupQwenUsesDashScopeKey(t *testing.T) {
	provider, err := FromLookup(lookupFrom(map[string]string{
		envWSURL:        "wss://workspace.cn-beijing.maas.aliyuncs.com/api-ws/v1/realtime?model=qwen-omni-test",
		envDashScopeKey: "dash-key",
	}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	if got := ProviderName(provider); got != string(KindQwen) {
		t.Fatalf("provider name = %q, want qwen", got)
	}
	ws := provider.(*WSProvider)
	capture := &dialCapture{conn: newFakeConn()}
	ws.cfg.Dial = capture.dial
	conn, err := provider.Connect(context.Background(), Session{}, "")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn == nil {
		t.Fatal("Connect returned a nil conn")
	}
	if got := capture.header.Get("Authorization"); got != "Bearer dash-key" {
		t.Fatalf("Authorization = %q, want the DashScope key", got)
	}
	if !strings.Contains(capture.url, "model=qwen-omni-test") {
		t.Fatalf("provider URL = %q, want the URL model preserved", capture.url)
	}
	if _, ok := capture.header["OpenAI-Beta"]; ok {
		t.Fatalf("qwen must not send the OpenAI beta header: %v", capture.header)
	}
}

func TestFromLookupAPIKeyTakesPrecedence(t *testing.T) {
	provider, err := FromLookup(lookupFrom(map[string]string{
		envWSURL:        "wss://example.test/api-ws/v1/realtime",
		envAPIKey:       "explicit-key",
		envDashScopeKey: "dash-key",
	}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	ws := provider.(*WSProvider)
	capture := &dialCapture{conn: newFakeConn()}
	ws.cfg.Dial = capture.dial
	if _, err := provider.Connect(context.Background(), Session{}, ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := capture.header.Get("Authorization"); got != "Bearer explicit-key" {
		t.Fatalf("Authorization = %q, want the explicit key", got)
	}
}

func TestOpenAIProviderHeadersAndModelOverride(t *testing.T) {
	ws, err := NewProvider(Config{
		Kind:         KindOpenAI,
		URL:          "wss://api.openai.com/v1/realtime?model=default-model",
		APIKey:       "openai-key",
		DefaultModel: "default-model",
		AllowModels:  []string{"default-model", "allowed-model"},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	capture := &dialCapture{conn: newFakeConn()}
	ws.cfg.Dial = capture.dial
	if _, err := ws.Connect(context.Background(), Session{}, "allowed-model"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := capture.header.Get("Authorization"); got != "Bearer openai-key" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := capture.header.Get("OpenAI-Beta"); got != "realtime=v1" {
		t.Fatalf("OpenAI-Beta = %q, want realtime=v1", got)
	}
	u, err := url.Parse(capture.url)
	if err != nil {
		t.Fatalf("parse provider URL: %v", err)
	}
	if got := u.Query().Get("model"); got != "allowed-model" {
		t.Fatalf("model query = %q, want allowed-model", got)
	}
}

func TestModelAllowlist(t *testing.T) {
	ws, err := NewProvider(Config{
		URL:          "wss://example.test/api-ws/v1/realtime",
		DefaultModel: "a",
		AllowModels:  []string{"a", "b"},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	capture := &dialCapture{conn: newFakeConn()}
	ws.cfg.Dial = capture.dial

	if err := ws.ValidateModel("c"); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("ValidateModel(c) = %v, want ErrModelNotAllowed", err)
	}
	if err := ws.ValidateModel("b"); err != nil {
		t.Fatalf("ValidateModel(b) = %v, want nil", err)
	}
	if _, err := ws.Connect(context.Background(), Session{}, "c"); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("Connect(c) = %v, want ErrModelNotAllowed", err)
	}
	if capture.calls != 0 {
		t.Fatalf("dialer called %d times for a rejected model, want 0", capture.calls)
	}

	if _, err := ws.Connect(context.Background(), Session{}, ""); err != nil {
		t.Fatalf("Connect(default): %v", err)
	}
	if got := capture.url; !strings.Contains(got, "model=a") {
		t.Fatalf("URL = %q, want the default model applied", got)
	}
}

func TestModelDefaultComesFromURLWhenUnset(t *testing.T) {
	ws, err := NewProvider(Config{URL: "wss://example.test/api-ws/v1/realtime?model=url-model&region=cn"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	capture := &dialCapture{conn: newFakeConn()}
	ws.cfg.Dial = capture.dial
	if _, err := ws.Connect(context.Background(), Session{}, ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	u, err := url.Parse(capture.url)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	if got := u.Query().Get("model"); got != "url-model" {
		t.Fatalf("model = %q, want url-model", got)
	}
	if got := u.Query().Get("region"); got != "cn" {
		t.Fatalf("existing query parameter dropped: %q", capture.url)
	}
	if err := ws.ValidateModel("url-model"); err != nil {
		t.Fatalf("ValidateModel(url-model) = %v, want nil (URL model becomes the allowlist)", err)
	}
}

func TestProviderConfigValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"unsupported kind", Config{Kind: "bogus", URL: "wss://example.test/realtime"}, "unsupported"},
		{"bad scheme", Config{URL: "https://example.test/realtime"}, "ws:// or wss://"},
		{"no host", Config{URL: "wss:///realtime"}, "no host"},
		{"default not allowlisted", Config{URL: "wss://example.test/realtime", DefaultModel: "x", AllowModels: []string{"y"}}, "not listed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewProvider(tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestQwenDefaultSessionUpdateShape(t *testing.T) {
	ws, err := NewProvider(Config{Kind: KindQwen, URL: "wss://example.test/api-ws/v1/realtime", DefaultModel: "qwen-omni"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	var body struct {
		Type    string `json:"type"`
		Session struct {
			Modalities    []string `json:"modalities"`
			TurnDetection struct {
				Type string `json:"type"`
			} `json:"turn_detection"`
			Audio struct {
				Input struct {
					Format struct {
						Type       string `json:"type"`
						SampleRate int    `json:"sample_rate"`
						Channels   int    `json:"channels"`
					} `json:"format"`
				} `json:"input"`
				Output struct {
					Format struct {
						Type       string `json:"type"`
						SampleRate int    `json:"sample_rate"`
					} `json:"format"`
				} `json:"output"`
			} `json:"audio"`
			Transcription struct {
				Model string `json:"model"`
			} `json:"input_audio_transcription"`
		} `json:"session"`
	}
	if err := json.Unmarshal(ws.DefaultSessionUpdate(), &body); err != nil {
		t.Fatalf("default session.update is not valid JSON: %v", err)
	}
	if body.Type != "session.update" {
		t.Fatalf("type = %q", body.Type)
	}
	if len(body.Session.Modalities) != 2 || body.Session.Modalities[0] != "text" || body.Session.Modalities[1] != "audio" {
		t.Fatalf("modalities = %v, want [text audio]", body.Session.Modalities)
	}
	if body.Session.TurnDetection.Type != "server_vad" {
		t.Fatalf("turn_detection = %q, want server_vad", body.Session.TurnDetection.Type)
	}
	if got := body.Session.Audio.Input.Format; got.Type != "pcm" || got.SampleRate != 16000 || got.Channels != 1 {
		t.Fatalf("input format = %+v, want pcm/16000/1", got)
	}
	if got := body.Session.Audio.Output.Format; got.Type != "pcm" || got.SampleRate != 24000 {
		t.Fatalf("output format = %+v, want pcm/24000", got)
	}
	if got := body.Session.Transcription.Model; got != "qwen3-asr-flash-realtime" {
		t.Fatalf("input_audio_transcription.model = %q", got)
	}
}

func TestOpenAIDefaultSessionUpdateShape(t *testing.T) {
	ws, err := NewProvider(Config{Kind: KindOpenAI, URL: "wss://example.test/realtime"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	var body struct {
		Session struct {
			InputAudioFormat  string `json:"input_audio_format"`
			OutputAudioFormat string `json:"output_audio_format"`
			TurnDetection     struct {
				Type string `json:"type"`
			} `json:"turn_detection"`
		} `json:"session"`
	}
	if err := json.Unmarshal(ws.DefaultSessionUpdate(), &body); err != nil {
		t.Fatalf("default session.update is not valid JSON: %v", err)
	}
	if body.Session.InputAudioFormat != "pcm16" || body.Session.OutputAudioFormat != "pcm16" {
		t.Fatalf("formats = %q/%q, want pcm16/pcm16", body.Session.InputAudioFormat, body.Session.OutputAudioFormat)
	}
	if body.Session.TurnDetection.Type != "server_vad" {
		t.Fatalf("turn_detection = %q", body.Session.TurnDetection.Type)
	}
}

type bareProvider struct{}

func (bareProvider) Connect(context.Context, Session, string) (Conn, error) {
	return newFakeConn(), nil
}

func TestSessionUpdateDefaultsFallbackIsACopy(t *testing.T) {
	first := SessionUpdateDefaults(bareProvider{})
	if len(first) == 0 {
		t.Fatal("generic default is empty")
	}
	first[0] = 'X'
	second := SessionUpdateDefaults(bareProvider{})
	if second[0] == 'X' {
		t.Fatal("SessionUpdateDefaults returned a shared mutable buffer")
	}
}

func TestMissingAPIKeyOmitsAuthorization(t *testing.T) {
	ws, err := NewProvider(Config{URL: "ws://127.0.0.1:9/realtime"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	capture := &dialCapture{conn: newFakeConn()}
	ws.cfg.Dial = capture.dial
	if _, err := ws.Connect(context.Background(), Session{}, ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := capture.header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want none", got)
	}
}
