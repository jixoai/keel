package provider

// Outbound image resolution (keel-ga §3 provider-files): wire-form selection, the
// provider file cache, and the vendor uploaders.
//
// Every test here talks to an httptest server. The servers assert the *wire*
// shape (a data URI, an absolute URL, a file id, a multipart upload with the real
// image mime) and the byte ceiling behavior, because those are the facts a real
// vendor would reject differently — a resolver that gets them wrong fails in
// production, not in a struct comparison.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

// memBlobs is an in-memory provider.BlobSource. It records how many bytes each
// read path touched so the memory-cap tests can assert the inline path never
// materialised more than the encoded buffer.
type memBlobs struct {
	mu      sync.Mutex
	files   map[string][]byte
	mimes   map[string]string
	opens   int32
	encoded int32
}

func newMemBlobs() *memBlobs {
	return &memBlobs{files: map[string][]byte{}, mimes: map[string]string{}}
}

func (m *memBlobs) put(blobID, mime string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[blobID] = data
	m.mimes[blobID] = mime
}

func (m *memBlobs) Info(_ context.Context, _ string, blobID string) (BlobInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.files[blobID]
	if !ok {
		return BlobInfo{}, fmt.Errorf("no such blob %q", blobID)
	}
	sum := sha256.Sum256(data)
	return BlobInfo{
		BlobID: blobID,
		SHA256: fmt.Sprintf("%x", sum[:]),
		Size:   int64(len(data)),
		Mime:   m.mimes[blobID],
	}, nil
}

func (m *memBlobs) Open(_ context.Context, _ string, blobID string) (io.ReadCloser, error) {
	m.mu.Lock()
	data, ok := m.files[blobID]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no such blob %q", blobID)
	}
	atomic.AddInt32(&m.opens, 1)
	return io.NopCloser(strings.NewReader(string(data))), nil
}

func (m *memBlobs) EncodeBase64(_ context.Context, _ string, blobID string, dst []byte) error {
	m.mu.Lock()
	data, ok := m.files[blobID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such blob %q", blobID)
	}
	atomic.AddInt32(&m.encoded, 1)
	want := base64.StdEncoding.EncodedLen(len(data))
	if len(dst) != want {
		return fmt.Errorf("buffer %d bytes, want %d", len(dst), want)
	}
	base64.StdEncoding.Encode(dst, data)
	return nil
}

func (m *memBlobs) openCount() int   { return int(atomic.LoadInt32(&m.opens)) }
func (m *memBlobs) encodeCount() int { return int(atomic.LoadInt32(&m.encoded)) }

// fakeUploader is a FileUploader that records its calls and answers a handle with
// a declared expiry (so the TTL refresh test can drive it).
type fakeUploader struct {
	name   string
	id     string
	ttl    time.Duration
	mu     sync.Mutex
	calls  int
	last   []byte
	client *http.Client
}

func (f *fakeUploader) Provider() string { return f.name }

func (f *fakeUploader) Upload(_ context.Context, req FileUploadRequest) (FileRef, error) {
	src, err := req.Open(context.Background())
	if err != nil {
		return FileRef{}, err
	}
	defer src.Close()
	data, err := io.ReadAll(src)
	if err != nil {
		return FileRef{}, err
	}
	f.mu.Lock()
	f.calls++
	f.last = data
	id := f.id
	ttl := f.ttl
	f.mu.Unlock()
	// A distinct handle per call makes "was it re-uploaded" observable.
	if id == "" {
		id = fmt.Sprintf("file-%d", f.calls)
	} else if ttl > 0 {
		id = fmt.Sprintf("%s-%d", id, f.calls)
	}
	ref := FileRef{FileID: id}
	if ttl > 0 {
		ref.ExpiresAt = time.Now().Add(ttl)
	}
	return ref, nil
}

func (f *fakeUploader) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

const pngMagic = "\x89PNG\r\n\x1a\n"

// TestResolvePrefersFileIDThenURLThenBase64 pins the degradation order and the
// wire block each form produces.
func TestResolvePrefersFileIDThenURLThenBase64(t *testing.T) {
	blobs := newMemBlobs()
	blobs.put("blob-1", "image/png", []byte(pngMagic+"payload"))

	urlSigner := &PathURLSigner{Base: "https://keel.example"}
	resolver := &ImageResolver{
		Blobs: blobs,
		Files: NewFileCache(16),
		URLs:  urlSigner,
	}

	ref := agentcore.ImageRefContent{Type: agentcore.ContentTypeImageRef, BlobID: "blob-1", MimeType: "image/png"}

	t.Run("file_id when declared", func(t *testing.T) {
		resolver.uploaderOverride = &fakeUploader{name: "google", id: "gemini-file"}
		defer func() { resolver.uploaderOverride = nil }()
		got, err := resolver.Resolve(context.Background(), "ws", ImageTarget{Provider: "google", BaseURL: "https://gemini.example", APIKey: "k"},
			ref, ImageCaps{SupportsFileID: true, SupportsImageURL: true, MaxImageBytes: 1 << 20})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Form != ImageFormFileID {
			t.Fatalf("form = %q, want file_id", got.Form)
		}
		if got.Content.FileID != "gemini-file" || got.Content.URL != "" || got.Content.Data != "" {
			t.Fatalf("content = %+v, want only a file id", got.Content)
		}
	})

	t.Run("url when file ids are not declared", func(t *testing.T) {
		got, err := resolver.Resolve(context.Background(), "ws", ImageTarget{Provider: "openrouter", BaseURL: "https://gw.example", APIKey: "k"},
			ref, ImageCaps{SupportsImageURL: true, MaxImageBytes: 1 << 20})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Form != ImageFormURL {
			t.Fatalf("form = %q, want url", got.Form)
		}
		if got.Content.URL != "https://keel.example/v1/files/blob-1" {
			t.Fatalf("url = %q, want the platform file endpoint", got.Content.URL)
		}
	})

	t.Run("base64 when nothing else is declared", func(t *testing.T) {
		got, err := resolver.Resolve(context.Background(), "ws", ImageTarget{Provider: "custom", BaseURL: "https://c.example"},
			ref, ImageCaps{})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Form != ImageFormBase64 {
			t.Fatalf("form = %q, want base64", got.Form)
		}
		want := base64.StdEncoding.EncodeToString([]byte(pngMagic + "payload"))
		if got.Content.Data != want {
			t.Fatalf("data = %q, want the encoded bytes", got.Content.Data)
		}
		if blobs.encodeCount() != 1 {
			t.Fatalf("encode calls = %d, want 1", blobs.encodeCount())
		}
	})

	t.Run("a failed upload degrades to url with the reason recorded", func(t *testing.T) {
		resolver.Files = NewFileCache(16)
		resolver.uploaderOverride = &failingUploader{}
		defer func() { resolver.uploaderOverride = nil }()
		got, err := resolver.Resolve(context.Background(), "ws", ImageTarget{Provider: "google", BaseURL: "https://g.example", APIKey: "k"},
			ref, ImageCaps{SupportsFileID: true, SupportsImageURL: true, MaxImageBytes: 1 << 20})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Form != ImageFormURL {
			t.Fatalf("form = %q, want url after the upload failed", got.Form)
		}
		if !strings.Contains(got.Note, "upload failed") {
			t.Fatalf("note = %q, want the degradation reason", got.Note)
		}
	})
}

// failingUploader always errors, so a degradation path is exercised.
type failingUploader struct{}

func (failingUploader) Provider() string { return "google" }
func (failingUploader) Upload(context.Context, FileUploadRequest) (FileRef, error) {
	return FileRef{}, fmt.Errorf("upstream said no")
}

// TestResolveRefusesAnOversizedImage pins the byte ceiling: an image over
// MaxImageBytes is refused in every form, and one over the inline cap but under
// the provider cap degrades to a URL instead of allocating.
func TestResolveRefusesAnOversizedImage(t *testing.T) {
	blobs := newMemBlobs()
	big := make([]byte, 4096)
	blobs.put("big", "image/png", big)
	resolver := &ImageResolver{Blobs: blobs, URLs: &PathURLSigner{Base: "https://keel.example"}}
	ref := agentcore.ImageRefContent{Type: agentcore.ContentTypeImageRef, BlobID: "big", MimeType: "image/png"}

	t.Run("over the provider cap is refused", func(t *testing.T) {
		_, err := resolver.Resolve(context.Background(), "ws", ImageTarget{Provider: "p"},
			ref, ImageCaps{MaxImageBytes: 1024})
		if err == nil {
			t.Fatal("an image over MaxImageBytes must be refused")
		}
		if !strings.Contains(err.Error(), "4096") || !strings.Contains(err.Error(), "1024") {
			t.Fatalf("error must name the size and the limit: %v", err)
		}
	})

	t.Run("over the inline cap degrades to a url", func(t *testing.T) {
		got, err := resolver.Resolve(context.Background(), "ws", ImageTarget{Provider: "p"},
			ref, ImageCaps{SupportsImageURL: true, MaxImageBytes: 1 << 20, MaxInlineBytes: 64})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Form != ImageFormURL {
			t.Fatalf("form = %q, want url", got.Form)
		}
		if blobs.encodeCount() != 0 {
			t.Fatalf("encode calls = %d, want 0 (the image must never be inlined)", blobs.encodeCount())
		}
	})

	t.Run("over the inline cap with no cheaper form fails closed", func(t *testing.T) {
		_, err := resolver.Resolve(context.Background(), "ws", ImageTarget{Provider: "p"},
			ref, ImageCaps{MaxImageBytes: 1 << 20, MaxInlineBytes: 64})
		if err == nil {
			t.Fatal("an image over the inline cap with no url form must fail closed")
		}
		if !strings.Contains(err.Error(), "inline limit") {
			t.Fatalf("error must name the inline limit: %v", err)
		}
	})
}

// TestResolveBase64AllocatesExactlyTheEncodedLength pins the memory bound: the
// inline path allocates one buffer of the encoded size and the blob source fills
// it, so the resident cost is the wire bytes and nothing else.
func TestResolveBase64AllocatesExactlyTheEncodedLength(t *testing.T) {
	blobs := newMemBlobs()
	data := make([]byte, 3*(1<<20)+1) // 3MiB+1: crosses many chunk boundaries
	for i := range data {
		data[i] = byte(i)
	}
	blobs.put("hero", "image/png", data)
	resolver := &ImageResolver{Blobs: blobs}
	got, err := resolver.Resolve(context.Background(), "ws", ImageTarget{Provider: "p"},
		agentcore.ImageRefContent{Type: agentcore.ContentTypeImageRef, BlobID: "hero"}, ImageCaps{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := base64.StdEncoding.EncodedLen(len(data))
	if len(got.Content.Data) != want {
		t.Fatalf("encoded length = %d, want %d", len(got.Content.Data), want)
	}
	if blobs.openCount() != 0 {
		t.Fatalf("open calls = %d, want 0 (the inline path reads through EncodeBase64 only)", blobs.openCount())
	}
	if blobs.encodeCount() != 1 {
		t.Fatalf("encode calls = %d, want 1", blobs.encodeCount())
	}
}

// TestFileCacheDedupesAndRefreshesExpiredHandles pins the cache contract:
// (sha256, provider, workspace) dedupes a repeat resolution, and an expired
// handle (the Gemini 48h case) is re-uploaded lazily on the next read.
func TestFileCacheDedupesAndRefreshesExpiredHandles(t *testing.T) {
	blobs := newMemBlobs()
	blobs.put("blob-1", "image/png", []byte(pngMagic+"cache"))
	resolver := &ImageResolver{Blobs: blobs, Files: NewFileCache(16)}
	uploader := &fakeUploader{name: "google", id: "gemini", ttl: 48 * time.Hour}
	resolver.uploaderOverride = uploader
	defer func() { resolver.uploaderOverride = nil }()

	caps := ImageCaps{SupportsFileID: true, MaxImageBytes: 1 << 20}
	target := ImageTarget{Provider: "google", BaseURL: "https://g.example", APIKey: "k"}
	ref := agentcore.ImageRefContent{Type: agentcore.ContentTypeImageRef, BlobID: "blob-1"}

	first, err := resolver.Resolve(context.Background(), "ws-1", target, ref, caps)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	second, err := resolver.Resolve(context.Background(), "ws-1", target, ref, caps)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if first.Content.FileID != second.Content.FileID {
		t.Fatalf("handles differ (%q vs %q): an unexpired handle must be reused",
			first.Content.FileID, second.Content.FileID)
	}
	if uploader.callCount() != 1 {
		t.Fatalf("uploads = %d, want 1 (the second resolve must hit the cache)", uploader.callCount())
	}

	// A different workspace is a different account: it must upload again.
	if _, err := resolver.Resolve(context.Background(), "ws-2", target, ref, caps); err != nil {
		t.Fatalf("other workspace: %v", err)
	}
	if uploader.callCount() != 2 {
		t.Fatalf("uploads = %d, want 2 (workspace isolates the cache key)", uploader.callCount())
	}

	// Expire every handle and resolve again: the lazy refresh re-uploads.
	cache := resolver.Files
	cache.mu.Lock()
	for key, entry := range cache.entries {
		entry.ref.ExpiresAt = time.Now().Add(-time.Minute)
		cache.entries[key] = entry
	}
	cache.mu.Unlock()
	if _, err := resolver.Resolve(context.Background(), "ws-1", target, ref, caps); err != nil {
		t.Fatalf("resolve after expiry: %v", err)
	}
	if uploader.callCount() != 3 {
		t.Fatalf("uploads = %d, want 3 (an expired handle must be refreshed)", uploader.callCount())
	}
}

// TestFileCacheKeyUsesTheContentDigest pins the key's content dimension: two blob
// ids with identical bytes share one handle (the same file re-uploaded), while
// identical bytes in another workspace do not.
func TestFileCacheKeyUsesTheContentDigest(t *testing.T) {
	data := []byte(pngMagic + "same-bytes")
	blobs := newMemBlobs()
	blobs.put("a", "image/png", data)
	blobs.put("b", "image/png", data)
	resolver := &ImageResolver{Blobs: blobs, Files: NewFileCache(16)}
	uploader := &fakeUploader{name: "openai", id: "file"}
	resolver.uploaderOverride = uploader
	defer func() { resolver.uploaderOverride = nil }()
	caps := ImageCaps{SupportsFileID: true}
	target := ImageTarget{Provider: "openai", BaseURL: "https://o.example", APIKey: "k"}

	for _, blobID := range []string{"a", "b"} {
		if _, err := resolver.Resolve(context.Background(), "ws", target,
			agentcore.ImageRefContent{Type: agentcore.ContentTypeImageRef, BlobID: blobID}, caps); err != nil {
			t.Fatalf("resolve %s: %v", blobID, err)
		}
	}
	if uploader.callCount() != 1 {
		t.Fatalf("uploads = %d, want 1 (identical bytes share one provider handle)", uploader.callCount())
	}
}

// TestGeminiUploaderHandshake pins the Gemini Files wire: the resumable session
// start carries the size and mime headers, and the finalize call posts the raw
// bytes with the upload command.
func TestGeminiUploaderHandshake(t *testing.T) {
	blobs := newMemBlobs()
	blobs.put("blob-1", "image/png", []byte(pngMagic+"gemini"))
	var (
		startHeaders http.Header
		finalHeaders http.Header
		finalBody    []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/upload/v1beta/files":
			startHeaders = r.Header.Clone()
			w.Header().Set("X-Goog-Upload-URL", "http://"+r.Host+"/upload-session")
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/upload-session":
			finalHeaders = r.Header.Clone()
			finalBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"file":{"uri":"https://generativelanguage.googleapis.com/v1beta/files/abc","name":"files/abc"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	uploader := NewGeminiFileUploader(server.URL+"/v1beta", "test-key", server.Client())
	resolver := &ImageResolver{Blobs: blobs, Files: NewFileCache(8), HTTPClient: server.Client(), uploaderOverride: uploader}
	defer func() { resolver.uploaderOverride = nil }()

	got, err := resolver.Resolve(context.Background(), "ws",
		ImageTarget{Provider: "google", BaseURL: server.URL + "/v1beta", APIKey: "test-key"},
		agentcore.ImageRefContent{Type: agentcore.ContentTypeImageRef, BlobID: "blob-1", Name: "shot.png"},
		ImageCaps{SupportsFileID: true})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Form != ImageFormFileID || got.Content.FileID != "https://generativelanguage.googleapis.com/v1beta/files/abc" {
		t.Fatalf("resolved = %+v, want the uploaded file.uri", got)
	}
	if startHeaders.Get("X-Goog-Upload-Protocol") != "resumable" {
		t.Fatalf("session start protocol = %q, want resumable", startHeaders.Get("X-Goog-Upload-Protocol"))
	}
	if startHeaders.Get("X-Goog-Upload-Header-Content-Type") != "image/png" {
		t.Fatalf("declared mime = %q, want the blob's mime", startHeaders.Get("X-Goog-Upload-Header-Content-Type"))
	}
	if startHeaders.Get("x-goog-api-key") != "test-key" {
		t.Fatalf("session start auth header missing")
	}
	if finalHeaders.Get("X-Goog-Upload-Command") != "upload, finalize" {
		t.Fatalf("finalize command = %q", finalHeaders.Get("X-Goog-Upload-Command"))
	}
	if string(finalBody) != pngMagic+"gemini" {
		t.Fatalf("uploaded bytes = %q, want the raw image", finalBody)
	}
}

// TestOpenAIUploaderSendsMultipart pins the OpenAI Files wire: a multipart body
// with the real image content type on the part and Bearer auth.
func TestOpenAIUploaderSendsMultipart(t *testing.T) {
	blobs := newMemBlobs()
	blobs.put("blob-2", "image/webp", []byte("RIFFxxxxWEBP"))
	var (
		partType string
		partBody []byte
		auth     string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/files" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Errorf("form file: %v", err)
			return
		}
		defer file.Close()
		partType = header.Header.Get("Content-Type")
		partBody, _ = io.ReadAll(file)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"file-openai-1","purpose":"user_data"}`))
	}))
	defer server.Close()

	uploader := NewOpenAIFileUploader(server.URL+"/v1", "sk-test", server.Client())
	resolver := &ImageResolver{Blobs: blobs, Files: NewFileCache(8), HTTPClient: server.Client(), uploaderOverride: uploader}
	defer func() { resolver.uploaderOverride = nil }()

	got, err := resolver.Resolve(context.Background(), "ws",
		ImageTarget{Provider: "openai", BaseURL: server.URL + "/v1", APIKey: "sk-test"},
		agentcore.ImageRefContent{Type: agentcore.ContentTypeImageRef, BlobID: "blob-2"},
		ImageCaps{SupportsFileID: true})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Content.FileID != "file-openai-1" {
		t.Fatalf("file id = %q, want the uploaded id", got.Content.FileID)
	}
	if auth != "Bearer sk-test" {
		t.Fatalf("auth = %q, want a Bearer header", auth)
	}
	if partType != "image/webp" {
		t.Fatalf("part content type = %q, want the image mime (the Files API keys off it)", partType)
	}
	if string(partBody) != "RIFFxxxxWEBP" {
		t.Fatalf("uploaded bytes = %q", partBody)
	}
}

// TestAnthropicUploaderSendsBetaHeader pins the Anthropic Files wire: the beta
// header, the x-api-key auth, and the returned id.
func TestAnthropicUploaderSendsBetaHeader(t *testing.T) {
	blobs := newMemBlobs()
	blobs.put("blob-3", "image/jpeg", []byte("\xff\xd8\xff\xe0jpeg"))
	var beta, version, apiKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/files" {
			http.NotFound(w, r)
			return
		}
		beta = r.Header.Get("anthropic-beta")
		version = r.Header.Get("anthropic-version")
		apiKey = r.Header.Get("x-api-key")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"file_anthropic_1","type":"file"}`))
	}))
	defer server.Close()

	uploader := NewAnthropicFileUploader(server.URL+"/v1", "ant-key", server.Client())
	resolver := &ImageResolver{Blobs: blobs, Files: NewFileCache(8), HTTPClient: server.Client(), uploaderOverride: uploader}
	defer func() { resolver.uploaderOverride = nil }()

	got, err := resolver.Resolve(context.Background(), "ws",
		ImageTarget{Provider: "anthropic", BaseURL: server.URL + "/v1", APIKey: "ant-key"},
		agentcore.ImageRefContent{Type: agentcore.ContentTypeImageRef, BlobID: "blob-3"},
		ImageCaps{SupportsFileID: true})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Content.FileID != "file_anthropic_1" {
		t.Fatalf("file id = %q", got.Content.FileID)
	}
	if beta != anthropicFilesBeta {
		t.Fatalf("anthropic-beta = %q, want %q", beta, anthropicFilesBeta)
	}
	if version == "" {
		t.Fatal("anthropic-version header is required")
	}
	if apiKey != "ant-key" {
		t.Fatal("x-api-key header missing")
	}
}

// TestUploaderHTTPFailureDegradesToBase64 pins the fail semantics of a broken
// vendor upload endpoint: the resolver still produces a usable block, and the
// note records why the handle form was skipped.
func TestUploaderHTTPFailureDegradesToBase64(t *testing.T) {
	blobs := newMemBlobs()
	blobs.put("blob-4", "image/png", []byte(pngMagic+"x"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	uploader := NewOpenAIFileUploader(server.URL, "key", server.Client())
	resolver := &ImageResolver{Blobs: blobs, Files: NewFileCache(8), HTTPClient: server.Client(), uploaderOverride: uploader}
	defer func() { resolver.uploaderOverride = nil }()

	got, err := resolver.Resolve(context.Background(), "ws",
		ImageTarget{Provider: "openai", BaseURL: server.URL, APIKey: "key"},
		agentcore.ImageRefContent{Type: agentcore.ContentTypeImageRef, BlobID: "blob-4"},
		ImageCaps{SupportsFileID: true})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Form != ImageFormBase64 {
		t.Fatalf("form = %q, want the base64 fallback", got.Form)
	}
	if !strings.Contains(got.Note, "503") {
		t.Fatalf("note = %q, want the vendor's status", got.Note)
	}
	if got.Content.Data == "" {
		t.Fatal("the fallback must carry the image bytes")
	}
}

// TestImageCapsForAPIDeclaresTheThreeWires pins the declaration table the product
// layer reads: Responses leads with file_id, Chat/Anthropic lead with url, an
// unknown protocol declares nothing (base64 only).
func TestImageCapsForAPIDeclaresTheThreeWires(t *testing.T) {
	if caps := ImageCapsForAPI("openai-responses"); !caps.SupportsFileID || !caps.SupportsImageURL {
		t.Fatalf("responses caps = %+v, want file_id and url", caps)
	}
	if caps := ImageCapsForAPI("anthropic-messages"); caps.SupportsFileID || !caps.SupportsImageURL {
		t.Fatalf("anthropic caps = %+v, want url only (the driver sends no Files beta header)", caps)
	}
	chat := ImageCapsForAPI("openai-chat-completions")
	if chat.SupportsFileID {
		t.Fatalf("chat caps = %+v, want no file_id (the chat wire has no such field)", chat)
	}
	if !chat.SupportsImageURL {
		t.Fatalf("chat caps = %+v, want url support", chat)
	}
	if caps := ImageCapsForAPI("something-else"); caps.BestForm() != ImageFormBase64 {
		t.Fatalf("unknown protocol caps = %+v, want the base64-only default", caps)
	}
}

// TestWireFormEncodingHelpers pins the small contract the encoders rely on:
// exactly one source, and the data URI shape.
func TestWireFormEncodingHelpers(t *testing.T) {
	if wire, err := splitImage(agentcore.ImageContent{FileID: "f"}); err != nil || wire.form() != ImageFormFileID {
		t.Fatalf("file id split = %+v err=%v", wire, err)
	}
	if _, err := splitImage(agentcore.ImageContent{FileID: "f", URL: "u"}); err == nil {
		t.Fatal("a block with two sources must be refused")
	}
	if _, err := splitImage(agentcore.ImageContent{}); err == nil {
		t.Fatal("a block with no source must be refused")
	}
	wire := imageWire{Mime: "image/png", Data: "AAAA"}
	if got := wire.dataURI(); got != "data:image/png;base64,AAAA" {
		t.Fatalf("data URI = %q", got)
	}
	if got := (imageWire{Data: "AAAA"}).dataURI(); !strings.HasPrefix(got, "data:application/octet-stream") {
		t.Fatalf("a missing mime must degrade to octet-stream, got %q", got)
	}
}
