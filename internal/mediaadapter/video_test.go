package mediaadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVideoWirePersistsReferenceAndUsage(t *testing.T) {
	videoBytes := []byte("mp4-bytes")
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/videos/generations":
			if got := r.Header.Get("Authorization"); got != "Bearer video-key" {
				t.Errorf("authorization = %q", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Errorf("decode body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
			  "model": "vid-1",
			  "videos": [{
			    "url": %q,
			    "mime_type": "video/mp4",
			    "width": 1280,
			    "height": 720,
			    "duration_ms": 4200
			  }],
			  "usage": {"prompt_tokens": 3}
			}`, serverURL(r)+"/asset.mp4")
		case "/asset.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(videoBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider, err := NewHTTPVideoProvider(HTTPVideoConfig{BaseURL: server.URL, APIKey: "video-key", Model: "video-model"})
	if err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	adapter := &VideoAdapter{Provider: provider, Store: store, Fetcher: newTestFetcher(0)}
	seed := 42
	audio := true
	result, err := adapter.Generate(context.Background(), "session-v", VideoRequest{
		Prompt:             "a paper boat on a river",
		NegativePrompt:     "text",
		DurationSeconds:    5,
		AspectRatio:        "16:9",
		Resolution:         "1080p",
		ImageURL:           "data:image/png;base64,AAAA",
		ReferenceImageURLs: []string{"data:image/png;base64,BBBB"},
		Seed:               &seed,
		Audio:              &audio,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Model != "vid-1" || len(result.Videos) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if result.Videos[0].DurationMillis != 4200 || result.Videos[0].Width != 1280 {
		t.Fatalf("artifact = %+v", result.Videos[0])
	}
	if result.Usage.Videos != 1 || result.Usage.Bytes != int64(len(videoBytes)) || result.Usage.InputTokens != 3 {
		t.Fatalf("usage = %+v", result.Usage)
	}
	if got := string(store.call(0).Data); got != string(videoBytes) {
		t.Fatalf("stored bytes = %q", got)
	}
	body, _ := json.Marshal(posted)
	for _, want := range []string{`"prompt":"a paper boat on a river"`, `"duration":5`, `"aspect_ratio":"16:9"`, `"resolution":"1080p"`, `"seed":42`, `"audio":true`, `"image_url"`, `"reference_image_urls"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("wire body missing %s: %s", want, body)
		}
	}
	t.Logf("wire POST /videos/generations body=%s", body)
	t.Logf("artifacts=%s usage=%+v", mustJSON(t, result.Videos), result.Usage)
	raw, _ := json.Marshal(result)
	for _, forbidden := range []string{server.URL, "video-key", "cost", "price"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("result leaked %q: %s", forbidden, raw)
		}
	}
}

func TestVideoToolReturnsReferencesAndValidationEcho(t *testing.T) {
	provider := &fakeVideoProvider{
		batch: VideoBatch{
			Model:  "vid-1",
			Assets: []ProviderAsset{{Data: []byte("mp4"), Mime: "video/mp4", DurationMillis: 1000}},
		},
	}
	tool := NewVideoTool(&VideoAdapter{Provider: provider, Store: &recordingStore{}})
	ctx := WithArtifactSession(context.Background(), "session-v")
	result, err := tool.Execute(ctx, "call", json.RawMessage(`{"prompt":"waves","duration":3,"resolution":"720p"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError != nil && *result.IsError {
		t.Fatalf("error result: %+v", result)
	}
	var output VideoToolOutput
	if err := json.Unmarshal([]byte(contentText(t, result.Content)), &output); err != nil {
		t.Fatal(err)
	}
	if !output.Success || len(output.Videos) != 1 || output.Videos[0].BlobID == "" {
		t.Fatalf("output = %+v", output)
	}

	// A validation failure is a typed result that keeps the call input.
	result, err = tool.Execute(ctx, "call", json.RawMessage(`{"prompt":"waves","duration":301}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError == nil || !*result.IsError {
		t.Fatal("invalid duration must fail")
	}
	var failure ToolFailure
	if err := json.Unmarshal([]byte(contentText(t, result.Content)), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.ErrorType != string(KindRequest) {
		t.Fatalf("error type = %q", failure.ErrorType)
	}
	echo, _ := failure.Request.(map[string]any)
	if echo["prompt"] != "waves" || echo["duration"] != float64(301) {
		t.Fatalf("request echo = %#v", echo)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider calls = %d", len(provider.requests))
	}
}
