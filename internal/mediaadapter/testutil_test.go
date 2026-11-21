package mediaadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

// recordingStore is the in-memory ArtifactStore used by the adapter tests. It
// records every write so a test can assert the exact bytes and the resulting
// reference, without touching the filesystem.
type recordingStore struct {
	mu    sync.Mutex
	calls []ArtifactInput
	fail  error
}

func (s *recordingStore) Put(_ context.Context, in ArtifactInput) (Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return Artifact{}, s.fail
	}
	copied := in
	copied.Data = append([]byte(nil), in.Data...)
	s.calls = append(s.calls, copied)
	sum := sha256.Sum256(in.Data)
	return Artifact{
		Kind:           in.Kind,
		BlobID:         "sha256:" + hex.EncodeToString(sum[:]),
		Mime:           artifactMime(in.Kind, in.Mime),
		Size:           int64(len(in.Data)),
		Width:          in.Width,
		Height:         in.Height,
		DurationMillis: in.DurationMillis,
		Name:           in.Name,
	}, nil
}

func (s *recordingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *recordingStore) call(index int) ArtifactInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[index]
}

func (s *recordingStore) all() []ArtifactInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ArtifactInput(nil), s.calls...)
}

// fakeImageProvider is a deterministic image provider. When panicValue is set
// the call panics, exercising the adapter's crash boundary.
type fakeImageProvider struct {
	mu         sync.Mutex
	requests   []ImageRequest
	batch      ImageBatch
	err        error
	panicValue any
}

func (p *fakeImageProvider) GenerateImage(_ context.Context, req ImageRequest) (ImageBatch, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	if p.panicValue != nil {
		panic(p.panicValue)
	}
	if p.err != nil {
		return ImageBatch{}, p.err
	}
	return p.batch, nil
}

type fakeVideoProvider struct {
	mu         sync.Mutex
	requests   []VideoRequest
	batch      VideoBatch
	err        error
	panicValue any
}

func (p *fakeVideoProvider) GenerateVideo(_ context.Context, req VideoRequest) (VideoBatch, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	if p.panicValue != nil {
		panic(p.panicValue)
	}
	if p.err != nil {
		return VideoBatch{}, p.err
	}
	return p.batch, nil
}

func mustFailer(t *testing.T, err error, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want errors.Is(%v)", err, want)
	}
}

// contentText returns the single text block of a tool result.
func contentText(t *testing.T, list agentcore.ContentList) string {
	t.Helper()
	if len(list) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(list))
	}
	text, ok := list[0].(agentcore.TextContent)
	if !ok {
		t.Fatalf("content block = %T, want TextContent", list[0])
	}
	return text.Text
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
