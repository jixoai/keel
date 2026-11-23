package spill

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

type memoryStore struct {
	mu   sync.Mutex
	data map[string][]byte
	refs map[string]Ref
}

func newMemoryStore() *memoryStore {
	return &memoryStore{data: map[string][]byte{}, refs: map[string]Ref{}}
}

func (m *memoryStore) Spill(_ context.Context, _ Scope, mime string, data []byte) (Ref, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	digest := Digest(data)
	id := "blob-" + digest[:12]
	ref, ok := m.refs[id]
	if ok {
		return ref, nil
	}
	ref = Ref{ID: id, MIME: mime, SHA256: digest, Size: int64(len(data))}
	m.refs[id] = ref
	m.data[id] = append([]byte(nil), data...)
	return ref, nil
}

func (m *memoryStore) Load(_ context.Context, _ Scope, ref Ref) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.data[ref.ID]
	if !ok {
		return nil, errors.New("not found")
	}
	return append([]byte(nil), data...), nil
}

func TestRenderReferenceNamesDurableFactsAndBoundsPreview(t *testing.T) {
	content := strings.Repeat("0123456789", 500)
	ref := Ref{ID: "blob-abc", MIME: "text/plain; charset=utf-8", SHA256: Digest([]byte(content)), Size: int64(len(content))}
	text := RenderReference(ref, `tool result "bash"`, content, 100)

	if !IsReference(text) {
		t.Fatalf("rendered text does not carry the reference prefix: %q", text)
	}
	for _, want := range []string{ref.ID, ref.SHA256, "5000 bytes", "recoverable by ref"} {
		if !strings.Contains(text, want) {
			t.Fatalf("reference %q does not contain %q", text, want)
		}
	}
	if strings.Contains(text, content) {
		t.Fatal("the full payload leaked into the reference")
	}
	if len(text) > 700 {
		t.Fatalf("reference = %d bytes, want bounded", len(text))
	}
}

func TestRenderReferenceTruncatesOnRuneBoundary(t *testing.T) {
	// "世" is 3 bytes; a 5-byte budget must yield one rune, not a broken one.
	content := strings.Repeat("世", 100)
	ref := Ref{ID: "b", MIME: "text/plain", SHA256: Digest([]byte(content)), Size: int64(len(content))}
	text := RenderReference(ref, "", content, 5)
	if !utf8.ValidString(text) {
		t.Fatalf("reference is not valid UTF-8: %q", text)
	}
	if strings.Count(text, "世") != 1 {
		t.Fatalf("preview = %q, want exactly one rune", text)
	}
}

func TestSpillTextRoundTripsExactBytes(t *testing.T) {
	store := newMemoryStore()
	content := "the full tool output\nwith two lines\n"
	ref, replacement, err := SpillText(context.Background(), store, Scope{TenantID: "t"}, "tool result", content, DefaultPolicy())
	if err != nil {
		t.Fatalf("SpillText: %v", err)
	}
	if replacement == content || !IsReference(replacement) {
		t.Fatalf("replacement = %q, want a bounded reference", replacement)
	}
	recovered, err := store.Load(context.Background(), Scope{TenantID: "t"}, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(recovered) != content {
		t.Fatalf("recovered = %q, want the exact payload", recovered)
	}
	if ref.SHA256 != Digest([]byte(content)) || ref.Size != int64(len(content)) {
		t.Fatalf("ref = %+v, want the payload identity", ref)
	}
}

func TestSpillTextWithoutStoreIsAnErrorNotADrop(t *testing.T) {
	if _, _, err := SpillText(context.Background(), nil, Scope{}, "x", "payload", DefaultPolicy()); err == nil {
		t.Fatal("a nil store must be an error")
	}
}

func TestPolicyShouldSpill(t *testing.T) {
	policy := Policy{MinBytes: 100}
	if policy.ShouldSpill(99) {
		t.Fatal("99 bytes must not spill at a 100-byte threshold")
	}
	if !policy.ShouldSpill(100) {
		t.Fatal("100 bytes must spill at a 100-byte threshold")
	}
	if (Policy{}).ShouldSpill(1 << 20) {
		t.Fatal("a zero MinBytes disables spilling")
	}
}

func TestTruncateUTF8(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 3, "hel"},
		{"hello", 0, ""},
		{"hello", 10, "hello"},
		{"世世", 4, "世"},
	}
	for _, tc := range cases {
		if got := TruncateUTF8(tc.in, tc.max); got != tc.want {
			t.Fatalf("TruncateUTF8(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}
