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

func TestImageWirePersistsReferencesAndObjectiveUsage(t *testing.T) {
	inline := []byte("inline-png-bytes")
	remote := []byte("remote-png-bytes")
	var posted map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/images/generations":
			if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
				t.Errorf("authorization = %q", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Errorf("decode body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
			  "model": "img-1",
			  "data": [
			    {"b64_json": %q, "mime_type": "image/png", "width": 640, "height": 480},
			    {"url": %q, "mime_type": "image/png", "width": 320, "height": 240}
			  ],
			  "usage": {"input_tokens": 7, "cost": 0.42}
			}`, base64.StdEncoding.EncodeToString(inline), serverURL(r)+"/asset.png")
		case "/asset.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(remote)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider, err := NewHTTPImageProvider(HTTPImageConfig{
		BaseURL: server.URL,
		APIKey:  "test-key",
		Model:   "img-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	adapter := &ImageAdapter{Provider: provider, Store: store, Fetcher: NewHTTPFetcher(0)}

	result, err := adapter.Generate(context.Background(), "session-1", ImageRequest{
		Prompt:             "a lighthouse at dusk",
		NegativePrompt:     "text, watermark",
		Size:               "1024x768",
		N:                  2,
		ReferenceImageURLs: []string{"data:image/png;base64,AAAA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Model != "img-1" {
		t.Fatalf("model = %q", result.Model)
	}
	if len(result.Images) != 2 {
		t.Fatalf("images = %d", len(result.Images))
	}
	if result.Usage.Images != 2 || result.Usage.Bytes != int64(len(inline)+len(remote)) {
		t.Fatalf("usage = %+v", result.Usage)
	}
	if result.Usage.InputTokens != 7 {
		t.Fatalf("input tokens = %d, want 7", result.Usage.InputTokens)
	}
	if store.count() != 2 {
		t.Fatalf("store calls = %d", store.count())
	}
	if got := string(store.call(0).Data); got != string(inline) {
		t.Fatalf("inline bytes = %q", got)
	}
	if got := string(store.call(1).Data); got != string(remote) {
		t.Fatalf("fetched bytes = %q", got)
	}
	if result.Images[0].BlobID == "" || !strings.HasPrefix(result.Images[0].BlobID, "sha256:") {
		t.Fatalf("blob id = %q", result.Images[0].BlobID)
	}
	if result.Images[0].Width != 640 || result.Images[0].Height != 480 {
		t.Fatalf("dimensions = %dx%d", result.Images[0].Width, result.Images[0].Height)
	}

	// The wire body keeps the Hermes-shaped optional fields.
	body, _ := json.Marshal(posted)
	for _, want := range []string{`"prompt":"a lighthouse at dusk"`, `"negative_prompt":"text, watermark"`, `"size":"1024x768"`, `"n":2`, `"reference_image_urls"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("wire body missing %s: %s", want, body)
		}
	}

	t.Logf("wire POST /images/generations body=%s", body)
	t.Logf("artifacts=%s usage=%+v", mustJSON(t, result.Images), result.Usage)

	// Results and events carry references only: no base64, URL or credential.
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		base64.StdEncoding.EncodeToString(inline),
		base64.StdEncoding.EncodeToString(remote),
		server.URL,
		"test-key",
		"cost",
		"0.42",
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("result leaked %q: %s", forbidden, raw)
		}
	}
}

func serverURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func TestImageToolSuccessAndRetryableFailureEcho(t *testing.T) {
	provider := &fakeImageProvider{
		batch: ImageBatch{
			Model:  "img-1",
			Assets: []ProviderAsset{{Data: []byte("png"), Mime: "image/png", Width: 8, Height: 8}},
		},
	}
	store := &recordingStore{}
	tool := NewImageTool(&ImageAdapter{Provider: provider, Store: store})
	ctx := WithArtifactSession(context.Background(), "session-1")

	result, err := tool.Execute(ctx, "call-1", json.RawMessage(`{
	  "prompt": "a red fox",
	  "negative_prompt": "blur",
	  "size": "512x512",
	  "n": 1,
	  "reference_image_urls": ["data:image/png;base64,AAAA"]
	}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError != nil && *result.IsError {
		t.Fatalf("unexpected error result: %+v", result)
	}
	text := contentText(t, result.Content)
	if strings.Contains(text, "base64") || strings.Contains(text, "data:image") {
		t.Fatalf("tool output inlined image data: %s", text)
	}
	var output ImageToolOutput
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		t.Fatal(err)
	}
	if !output.Success || len(output.Images) != 1 || output.Images[0].BlobID == "" {
		t.Fatalf("output = %+v", output)
	}
	if got := provider.requests[0]; got.Prompt != "a red fox" || got.NegativePrompt != "blur" || got.N != 1 {
		t.Fatalf("provider request = %+v", got)
	}

	// A provider failure is a typed error result that keeps the input.
	failing := NewImageTool(&ImageAdapter{
		Provider: &fakeImageProvider{err: &Error{
			Family: FamilyImage, Op: "generate", Kind: KindUpstream, Status: 503,
			Message: "upstream overloaded", Retryable: true,
		}},
		Store: store,
	})
	result, err = failing.Execute(ctx, "call-2", json.RawMessage(`{"prompt":"a red fox","size":"512x512","n":2}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError == nil || !*result.IsError {
		t.Fatal("failure result did not set isError")
	}
	var failure ToolFailure
	if err := json.Unmarshal([]byte(contentText(t, result.Content)), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.ErrorType != string(KindUpstream) || !failure.Retryable {
		t.Fatalf("failure = %+v", failure)
	}
	echo, ok := failure.Request.(map[string]any)
	if !ok {
		t.Fatalf("request echo = %#v", failure.Request)
	}
	if echo["prompt"] != "a red fox" || echo["size"] != "512x512" || echo["n"] != float64(2) {
		t.Fatalf("request echo lost input: %#v", echo)
	}
}

func TestImageToolRequiresSession(t *testing.T) {
	tool := NewImageTool(&ImageAdapter{Provider: &fakeImageProvider{}, Store: &recordingStore{}})
	result, err := tool.Execute(context.Background(), "call", json.RawMessage(`{"prompt":"x"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError == nil || !*result.IsError {
		t.Fatal("missing session must be an error result")
	}
	var failure ToolFailure
	if err := json.Unmarshal([]byte(contentText(t, result.Content)), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.ErrorType != ToolCodeSessionMissing {
		t.Fatalf("error type = %q", failure.ErrorType)
	}
}

func TestImageAdapterRejectsInvalidInputBeforeProvider(t *testing.T) {
	provider := &fakeImageProvider{panicValue: "provider must not be called"}
	adapter := &ImageAdapter{Provider: provider, Store: &recordingStore{}}

	cases := []struct {
		name  string
		req   ImageRequest
		field string
	}{
		{name: "empty prompt", req: ImageRequest{Prompt: " "}, field: "prompt"},
		{name: "too many images", req: ImageRequest{Prompt: "x", N: MaxImagesPerCall + 1}, field: "n"},
		{name: "bad size", req: ImageRequest{Prompt: "x", Size: "large"}, field: "size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := adapter.Generate(context.Background(), "session", tc.req)
			var typed *Error
			if !asAdapterError(err, &typed) {
				t.Fatalf("error = %v, want *Error", err)
			}
			if typed.Kind != KindRequest {
				t.Fatalf("kind = %q", typed.Kind)
			}
			found := false
			for _, field := range typed.Fields {
				if field.Field == tc.field {
					found = true
				}
			}
			if !found {
				t.Fatalf("fields = %+v, want %s", typed.Fields, tc.field)
			}
		})
	}
	if len(provider.requests) != 0 {
		t.Fatalf("provider was called %d times", len(provider.requests))
	}
}

func TestImageProviderHTTPErrorCarriesStatusAndRetryability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"overloaded"}}`))
	}))
	defer server.Close()
	provider, err := NewHTTPImageProvider(HTTPImageConfig{BaseURL: server.URL, APIKey: "s3cret", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &ImageAdapter{Provider: provider, Store: &recordingStore{}}
	_, err = adapter.Generate(context.Background(), "session", ImageRequest{Prompt: "x"})
	var typed *Error
	if !asAdapterError(err, &typed) {
		t.Fatalf("error = %v", err)
	}
	if typed.Status != http.StatusServiceUnavailable || !typed.Retryable {
		t.Fatalf("error = %+v", typed)
	}
	if strings.Contains(typed.Error(), "s3cret") {
		t.Fatalf("error leaked the credential: %v", typed)
	}
}
