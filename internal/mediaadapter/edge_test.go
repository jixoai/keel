package mediaadapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImageURLAssetWithoutFetcherIsAConfigError(t *testing.T) {
	provider := &fakeImageProvider{batch: ImageBatch{Assets: []ProviderAsset{{URL: "https://asset.example/a.png"}}}}
	adapter := &ImageAdapter{Provider: provider, Store: &recordingStore{}}
	_, err := adapter.Generate(context.Background(), "s", ImageRequest{Prompt: "x"})
	var typed *Error
	if !asAdapterError(err, &typed) || typed.Kind != KindConfig {
		t.Fatalf("error = %v", err)
	}
}

func TestImageURLErrorAndFetcherLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing.png":
			http.NotFound(w, r)
		case "/large.png":
			_, _ = w.Write([]byte("0123456789"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter := &ImageAdapter{
		Provider: &fakeImageProvider{batch: ImageBatch{Assets: []ProviderAsset{{URL: server.URL + "/missing.png"}}}},
		Store:    &recordingStore{},
		Fetcher:  NewHTTPFetcher(0),
	}
	if _, err := adapter.Generate(context.Background(), "s", ImageRequest{Prompt: "x"}); err == nil {
		t.Fatal("404 asset did not fail")
	}
	adapter.Provider = &fakeImageProvider{batch: ImageBatch{Assets: []ProviderAsset{{URL: server.URL + "/large.png"}}}}
	adapter.Fetcher = NewHTTPFetcher(4)
	_, err := adapter.Generate(context.Background(), "s", ImageRequest{Prompt: "x"})
	var typed *Error
	if !asAdapterError(err, &typed) || !strings.Contains(typed.Message, "exceeds") {
		t.Fatalf("error = %v", err)
	}
}

func TestVideoInlineBase64Asset(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("inline-mp4"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"model":"vid","data":[{"b64_json":%q,"mime_type":"video/mp4","duration_ms":900}]}`, payload)
	}))
	defer server.Close()
	provider, err := NewHTTPVideoProvider(HTTPVideoConfig{BaseURL: server.URL, Model: "vid"})
	if err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	result, err := (&VideoAdapter{Provider: provider, Store: store}).Generate(context.Background(), "s", VideoRequest{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if string(store.call(0).Data) != "inline-mp4" || result.Videos[0].DurationMillis != 900 {
		t.Fatalf("artifact=%+v data=%q", result.Videos[0], store.call(0).Data)
	}
}

func TestTranscriptPlainTextFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("plain transcript"))
	}))
	defer server.Close()
	provider, err := NewHTTPSpeechProvider(HTTPSpeechConfig{BaseURL: server.URL, Model: "stt"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&SpeechAdapter{Provider: provider}).Transcribe(context.Background(), "s", TranscribeRequest{
		Audio: []byte("wav"), Mime: "audio/wav",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Transcript.Text != "plain transcript" || len(result.Transcript.Segments) != 0 {
		t.Fatalf("transcript = %+v", result.Transcript)
	}
}

func TestArtifactStoreFailureIsTyped(t *testing.T) {
	adapter := &ImageAdapter{
		Provider: &fakeImageProvider{batch: ImageBatch{Assets: []ProviderAsset{{Data: []byte("x")}}}},
		Store:    &recordingStore{fail: fmt.Errorf("disk full")},
	}
	_, err := adapter.Generate(context.Background(), "s", ImageRequest{Prompt: "x"})
	var typed *Error
	if !asAdapterError(err, &typed) || typed.Kind != KindArtifact || !typed.Retryable {
		t.Fatalf("error = %v", err)
	}
}

func TestDecisionHelpersAndDescriptorCopy(t *testing.T) {
	policy := DefaultDecisionPolicy()
	if policy.Mode != DecisionModeEnforce || policy.FailPolicy != DecisionFailClosed {
		t.Fatalf("default policy = %+v", policy)
	}
	client, err := NewDecisionClient(DecisionConfig{
		Adapter: DecisionAdapterTypeSafe, BaseURL: "https://decision.example", Model: "jev-1.14", AllowAnonymous: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := client.Descriptor()
	if descriptor.Adapter != DecisionAdapterTypeSafe || descriptor.Model != "jev-1.14" {
		t.Fatalf("descriptor = %+v", descriptor)
	}
	descriptor.QuestionTypes[0] = "mutated"
	if client.Descriptor().QuestionTypes[0] == "mutated" {
		t.Fatal("Descriptor did not return a copy")
	}
	if fields := client.CheckPolicy(DecisionPolicy{Mode: DecisionModeEnforce, FailPolicy: DecisionFailOpen}); len(fields) == 0 {
		t.Fatal("fail_open outside shadow passed policy validation")
	}
	if fields := client.CheckPolicy(DecisionPolicy{Mode: DecisionModeShadow, Threshold: floatPtr(1.5)}); len(fields) == 0 {
		t.Fatal("out-of-range threshold passed policy validation")
	}
}

func floatPtr(value float64) *float64 { return &value }

func TestDecisionWireRejectsInvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer server.Close()
	client, err := NewDecisionClient(DecisionConfig{
		Adapter: DecisionAdapterTypeSafe, BaseURL: server.URL, APIKey: "k", Model: "jev-1.14",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Decide(context.Background(), DecisionRequest{
		State: json.RawMessage(`{"text":"x"}`), Questions: decisionQuestions(),
	})
	if err == nil {
		t.Fatal("invalid provider JSON passed")
	}
}
