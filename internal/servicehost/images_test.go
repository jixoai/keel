package servicehost

// Production wiring of the outbound image pipeline (keel-ga §3 provider-files):
// the assembled runtime must hand the runner a resolver whose blob source is the
// hosting process's, and an unresolved reference must degrade instead of failing
// the run. These tests drive the real Boot assembly (not a hand-built runner), so
// a wiring regression — the resolver silently nil, the blob source dropped — fails
// here rather than in production.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
)

// stubBlobs is a provider.BlobSource over one in-memory image.
type stubBlobs struct {
	data   []byte
	mime   string
	info   int
	encode int
	open   int
}

func (s *stubBlobs) Info(context.Context, string, string) (provider.BlobInfo, error) {
	s.info++
	return provider.BlobInfo{BlobID: "blob-1", SHA256: "digest", Size: int64(len(s.data)), Mime: s.mime}, nil
}

func (s *stubBlobs) Open(context.Context, string, string) (io.ReadCloser, error) {
	s.open++
	return io.NopCloser(strings.NewReader(string(s.data))), nil
}

func (s *stubBlobs) EncodeBase64(_ context.Context, _ string, _ string, dst []byte) error {
	s.encode++
	base64.StdEncoding.Encode(dst, s.data)
	return nil
}

// TestBootWiresTheImageResolver pins that the assembled generation's runner has a
// resolver whenever the hosting process supplies a blob source.
func TestBootWiresTheImageResolver(t *testing.T) {
	blobs := &stubBlobs{data: []byte("img"), mime: "image/png"}
	llm := LLMOptions{
		LookupEnv: func(string) (string, bool) { return "", false },
		Images:    blobs,
	}
	rt := bootWithLLM(t, host.TenantConfig{}, llm)
	defer rt.Close(context.Background())

	bound, err := rt.Binding().Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer bound.Release(context.Background())
	runner := bound.Runner()
	if runner.Images == nil {
		t.Fatal("a hosting process with a blob source must get an image resolver")
	}
	if runner.Images == nil {
		t.Fatal("resolver missing")
	}
	// The resolver must actually reach the supplied blob source: resolving a
	// reference through it proves the seam is connected, not merely non-nil.
	resolved, err := runner.Images.Resolve(context.Background(), "tenant-1",
		provider.ImageTarget{Provider: "custom"},
		agentcore.ImageRefContent{Type: agentcore.ContentTypeImageRef, BlobID: "blob-1", MimeType: "image/png"},
		provider.ImageCaps{})
	if err != nil {
		t.Fatalf("resolve through the assembled resolver: %v", err)
	}
	if resolved.Form != provider.ImageFormBase64 || resolved.Content.Data == "" {
		t.Fatalf("resolved = %+v, want the base64 form from the supplied blob source", resolved)
	}
	if blobs.info == 0 || blobs.encode == 0 {
		t.Fatalf("blob source calls: info=%d encode=%d, want both", blobs.info, blobs.encode)
	}
}

// TestBootWithoutABlobSourceLeavesTheResolverNil pins the honest unwired state:
// no blob source means no resolver, and the runner's degradation path (a text
// notice) is what a referenced image becomes.
func TestBootWithoutABlobSourceLeavesTheResolverNil(t *testing.T) {
	rt := bootWithLLM(t, host.TenantConfig{}, LLMOptions{
		LookupEnv: func(string) (string, bool) { return "", false },
	})
	defer rt.Close(context.Background())

	bound, err := rt.Binding().Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer bound.Release(context.Background())
	if bound.Runner().Images != nil {
		t.Fatal("without a blob source the resolver must stay nil so the degradation path is visible")
	}
}

// TestModelsDocumentResolvesItsImageCaps pins the product-layer half of the
// capability declaration: the resolved identity carries the endpoint and the
// wire's image capabilities, which is what the resolver selects a form from.
func TestModelsDocumentResolvesItsImageCaps(t *testing.T) {
	doc := json.RawMessage(`{
	  "providers": [
	    {"id": "resp", "api": {"type": "openai-responses", "baseUrl": "https://resp.example/v1"},
	     "access": {"type": "api-key", "env": "RESP_KEY"}, "modelIds": ["gpt-x"]},
	    {"id": "chat", "api": {"type": "openai-chat-completions", "baseUrl": "https://chat.example/v1"},
	     "access": {"type": "api-key", "env": "CHAT_KEY"}, "modelIds": ["gpt-y"]}
	  ]
	}`)
	service := ungatedModels(t, string(doc), secretLLM(map[string]string{
		"RESP_KEY": "k1", "CHAT_KEY": "k2",
	}, nil, ""))

	resp, err := service.Resolve(context.Background(), kernel.Scope{},
		json.RawMessage(`{"providerId":"resp","modelId":"gpt-x"}`))
	if err != nil {
		t.Fatalf("resolve responses model: %v", err)
	}
	if !resp.ImageCaps.SupportsFileID || !resp.ImageCaps.SupportsImageURL {
		t.Fatalf("responses caps = %+v, want file_id and url", resp.ImageCaps)
	}
	if resp.Endpoint != "https://resp.example/v1" {
		t.Fatalf("endpoint = %q, want the provider's base URL", resp.Endpoint)
	}

	chat, err := service.Resolve(context.Background(), kernel.Scope{},
		json.RawMessage(`{"providerId":"chat","modelId":"gpt-y"}`))
	if err != nil {
		t.Fatalf("resolve chat model: %v", err)
	}
	if chat.ImageCaps.SupportsFileID {
		t.Fatalf("chat caps = %+v, want no file_id (the chat wire cannot carry one)", chat.ImageCaps)
	}
	if !chat.ImageCaps.SupportsImageURL {
		t.Fatalf("chat caps = %+v, want url support", chat.ImageCaps)
	}
}

// TestAssembledRunnerResolvesAnImageOntoTheWire is the end-to-end acceptance for
// the change's first two items: an image reference committed as a user.message
// travels through the *assembled* runtime (Boot → generation → runner), through
// the real resolver and the real blob source, and arrives at a mock provider as a
// wire-form image block. The mock vendor asserts what it received.
func TestAssembledRunnerResolvesAnImageOntoTheWire(t *testing.T) {
	var sawImage, sawData bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Messages []struct {
				Content any `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		for _, message := range body.Messages {
			parts, ok := message.Content.([]any)
			if !ok {
				continue
			}
			for _, part := range parts {
				block, _ := part.(map[string]any)
				if block["type"] != "image_url" {
					continue
				}
				sawImage = true
				imageURL, _ := block["image_url"].(map[string]any)
				target, _ := imageURL["url"].(string)
				if strings.HasPrefix(target, "data:image/png;base64,") {
					sawData = true
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"seen\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n"+
			"data: [DONE]\n\n")
	}))
	defer server.Close()

	blobs := &stubBlobs{data: []byte("\x89PNG\r\n\x1a\npayload"), mime: "image/png"}
	doc := `{
	  "providers": [{"id": "mock", "api": {"type": "openai-chat-completions", "baseUrl": "` + server.URL + `"},
	                 "access": {"type": "api-key", "env": "MOCK_KEY"}, "modelIds": ["mock-vision"]}],
	  "modelRules": [{"match": "mock-vision", "config": {"properties": {"inputFormat": {"text": true, "image": true}}}}],
	  "selection": {"providerId": "mock", "modelId": "mock-vision"}
	}`
	llm := LLMOptions{
		LookupEnv: func(name string) (string, bool) {
			if name == "MOCK_KEY" {
				return "mock-key", true
			}
			return "", false
		},
		Images: blobs,
	}
	st := kernel.NewMemStore()
	k := kernel.New(st, kernel.Options{})
	rt, err := Boot(context.Background(), Config{
		Seams:     Seams{Committer: k, Reader: k, Leases: k, External: k, Runs: fakeMirror{}, Logger: discardLogger()},
		Tenant:    host.TenantConfig{Models: json.RawMessage(doc)},
		LLM:       llm,
		RuntimeID: "test-runtime",
	})
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	defer rt.Close(context.Background())

	bound, err := rt.Binding().Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer bound.Release(context.Background())
	runner := bound.Runner()

	tenant := uuid.New()
	session := uuid.New()
	st.CreateSession(tenant, session)
	payload, _ := json.Marshal(map[string]any{
		"text":        "what is this?",
		"requestId":   "req-1",
		"attachments": []map[string]any{{"fileId": "blob-1", "name": "shot.png", "mime": "image/png"}},
	})
	if _, err := k.Commit(context.Background(), kernel.CommitRequest{
		Scope:      kernel.Scope{TenantID: tenant, SessionID: session},
		Kind:       kernel.KindUserMessage,
		RequestID:  "req-1",
		Payload:    payload,
		NativeEdge: true,
	}); err != nil {
		t.Fatalf("commit input: %v", err)
	}

	lease, err := k.ClaimScope(context.Background(), kernel.Scope{TenantID: tenant, SessionID: session}, "test-owner", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := runner.RunAttempt(context.Background(), lease); err != nil {
		t.Fatalf("RunAttempt: %v", err)
	}
	if !sawImage {
		t.Fatal("the mock provider received no image part: the assembled pipeline did not resolve the reference")
	}
	if !sawData {
		t.Fatal("the mock provider received no base64 data URI: the chat wire's form was not used")
	}
}
