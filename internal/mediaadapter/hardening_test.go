package mediaadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestImageAdapterRefusesLoopbackAssetURL pins the SSRF boundary at the adapter
// edge: a provider that answers with a loopback URL is a typed failure, not a
// fetched asset, and the local server is never reached.
func TestImageAdapterRefusesLoopbackAssetURL(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("internal-secret"))
	}))
	defer server.Close()

	adapter := &ImageAdapter{
		Provider: &fakeImageProvider{batch: ImageBatch{Assets: []ProviderAsset{{
			URL: server.URL + "/asset.png", Mime: "image/png",
		}}}},
		Store:   &recordingStore{},
		Fetcher: NewHTTPFetcher(0),
	}
	_, err := adapter.Generate(context.Background(), "session", ImageRequest{Prompt: "x"})
	if !errors.Is(err, ErrAssetHostNotPublic) {
		t.Fatalf("error = %v, want ErrAssetHostNotPublic", err)
	}
	if hits.Load() != 0 {
		t.Fatal("the adapter reached the loopback asset host")
	}
}

// TestImageAspectRatioMapsToWireSize pins the Hermes static parameter: the
// adapter turns aspect_ratio into the OpenAI-images size, while an explicit
// keel-extension size wins.
func TestImageAspectRatioMapsToWireSize(t *testing.T) {
	cases := []struct {
		aspectRatio string
		size        string
		want        string
	}{
		{aspectRatio: "landscape", want: "1536x1024"},
		{aspectRatio: "portrait", want: "1024x1536"},
		{aspectRatio: "square", want: "1024x1024"},
		{aspectRatio: "Portrait", want: "1024x1536"},
		{aspectRatio: "portrait", size: "512x512", want: "512x512"},
	}
	for _, tc := range cases {
		t.Run(tc.aspectRatio+"/"+tc.size, func(t *testing.T) {
			provider := &fakeImageProvider{batch: ImageBatch{
				Assets: []ProviderAsset{{Data: []byte("png"), Mime: "image/png"}},
			}}
			adapter := &ImageAdapter{Provider: provider, Store: &recordingStore{}}
			if _, err := adapter.Generate(context.Background(), "session", ImageRequest{
				Prompt: "a kite", AspectRatio: tc.aspectRatio, Size: tc.size,
			}); err != nil {
				t.Fatal(err)
			}
			if got := provider.requests[0].Size; got != tc.want {
				t.Fatalf("wire size = %q, want %q", got, tc.want)
			}
		})
	}

	provider := &fakeImageProvider{panicValue: "provider must not be called"}
	_, err := (&ImageAdapter{Provider: provider, Store: &recordingStore{}}).
		Generate(context.Background(), "session", ImageRequest{Prompt: "x", AspectRatio: "wide"})
	var typed *Error
	if !asAdapterError(err, &typed) || typed.Kind != KindRequest || typed.Fields[0].Field != "aspect_ratio" {
		t.Fatalf("invalid aspect ratio error = %v", err)
	}
	if len(provider.requests) != 0 {
		t.Fatal("provider was called with an invalid aspect ratio")
	}
}

// TestPromptLimitIsEnforcedAtTheAdapter pins the 32 KiB bound in both media
// adapters: the tool schema advertises maxLength, and a direct/older caller is
// still refused before the provider is touched.
func TestPromptLimitIsEnforcedAtTheAdapter(t *testing.T) {
	tooLong := strings.Repeat("x", MaxPromptBytes+1)

	imageProvider := &fakeImageProvider{panicValue: "provider must not be called"}
	_, err := (&ImageAdapter{Provider: imageProvider, Store: &recordingStore{}}).
		Generate(context.Background(), "session", ImageRequest{Prompt: tooLong})
	var imageErr *Error
	if !asAdapterError(err, &imageErr) || imageErr.Kind != KindRequest {
		t.Fatalf("image error = %v", err)
	}
	if len(imageErr.Fields) == 0 || imageErr.Fields[0].Field != "prompt" {
		t.Fatalf("image fields = %+v", imageErr.Fields)
	}
	if len(imageProvider.requests) != 0 {
		t.Fatal("image provider was called with an oversized prompt")
	}

	videoProvider := &fakeVideoProvider{panicValue: "provider must not be called"}
	_, err = (&VideoAdapter{Provider: videoProvider, Store: &recordingStore{}}).
		Generate(context.Background(), "session", VideoRequest{Prompt: tooLong})
	var videoErr *Error
	if !asAdapterError(err, &videoErr) || videoErr.Kind != KindRequest {
		t.Fatalf("video error = %v", err)
	}
	if len(videoErr.Fields) == 0 || videoErr.Fields[0].Field != "prompt" {
		t.Fatalf("video fields = %+v", videoErr.Fields)
	}
	if len(videoProvider.requests) != 0 {
		t.Fatal("video provider was called with an oversized prompt")
	}
}

// TestImageBatchIsTruncatedToTheHarnessCeiling: a provider that ignores n (or
// returns a larger batch) cannot make the adapter persist more than
// MaxImagesPerCall assets, the same rule the video adapter already applies.
func TestImageBatchIsTruncatedToTheHarnessCeiling(t *testing.T) {
	assets := make([]ProviderAsset, 0, MaxImagesPerCall+3)
	for index := 0; index < MaxImagesPerCall+3; index++ {
		assets = append(assets, ProviderAsset{Data: []byte{'p', byte(index)}, Mime: "image/png"})
	}
	store := &recordingStore{}
	adapter := &ImageAdapter{
		Provider: &fakeImageProvider{batch: ImageBatch{
			Model:  "img-1",
			Assets: assets,
			// A provider-reported count must not be added to the persisted count.
			Usage: Usage{InputTokens: 7, Images: 100, Bytes: 9999},
		}},
		Store: store,
	}
	result, err := adapter.Generate(context.Background(), "session", ImageRequest{Prompt: "a kite"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Images) != MaxImagesPerCall || len(store.all()) != MaxImagesPerCall {
		t.Fatalf("persisted images = %d (store %d), want %d", len(result.Images), len(store.all()), MaxImagesPerCall)
	}
	if result.Usage.Images != MaxImagesPerCall || result.Usage.Bytes != int64(MaxImagesPerCall*2) {
		t.Fatalf("usage = %+v", result.Usage)
	}
	if result.Usage.InputTokens != 7 {
		t.Fatalf("provider token usage was lost: %+v", result.Usage)
	}
}

// TestProviderPanicMessageIsBoundedAndFlattened: a panic value is untrusted
// text. It is collapsed to one line and capped like any upstream error before
// it reaches a tool result or a log.
func TestProviderPanicMessageIsBoundedAndFlattened(t *testing.T) {
	panicValue := "boom\n" + strings.Repeat("secret-ish payload ", 2000)
	_, err := (&ImageAdapter{
		Provider: &fakeImageProvider{panicValue: panicValue},
		Store:    &recordingStore{},
	}).Generate(context.Background(), "session", ImageRequest{Prompt: "x"})
	var typed *Error
	if !asAdapterError(err, &typed) || typed.Kind != KindProviderUnavailable {
		t.Fatalf("error = %v", err)
	}
	if !strings.HasPrefix(typed.Message, "provider panicked: ") {
		t.Fatalf("message = %q", typed.Message)
	}
	if strings.ContainsAny(typed.Message, "\r\n") {
		t.Fatalf("panic message kept line breaks: %q", typed.Message)
	}
	if len(typed.Message) > 520 {
		t.Fatalf("panic message length = %d, want <= 520", len(typed.Message))
	}
}

// TestDecisionStateRejectsRemoteURLAnywhere: the decision protocol never
// dereferences state, so an http(s) URL in any leaf — not just image-keyed
// fields — is refused at Preflight, before a provider call.
func TestDecisionStateRejectsRemoteURLAnywhere(t *testing.T) {
	client, err := NewDecisionClient(DecisionConfig{
		Adapter: DecisionAdapterTypeSafe, BaseURL: "https://decision.example",
		Model: "jev-1.14", AllowAnonymous: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	questions := map[string]DecisionQuestion{
		"ok": {Type: QuestionNoul, Instructions: json.RawMessage(`"is this ok?"`)},
	}
	for name, state := range map[string]string{
		"plain text URL":       `{"text":"see https://example.com/page for details"}`,
		"nested non-image URL": `{"meta":{"source":"http://10.0.0.1/secret"}}`,
		"array leaf URL":       `{"items":["ok","https://example.com/a"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			fields := client.Preflight(json.RawMessage(state), questions)
			found := false
			for _, field := range fields {
				if field.Field == "state" {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s was not refused: %+v", state, fields)
			}
		})
	}
}
