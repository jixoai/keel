package layerfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T, opts Options) *Store {
	t.Helper()
	store, err := Open(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestPutDedupesIdenticalBytes(t *testing.T) {
	store := newTestStore(t, Options{})
	first, size, err := store.Put(bytes.NewReader([]byte("same bytes")))
	if err != nil {
		t.Fatal(err)
	}
	if size != 10 {
		t.Fatalf("size = %d", size)
	}
	second, size2, err := store.Put(bytes.NewReader([]byte("same bytes")))
	if err != nil {
		t.Fatal(err)
	}
	if second != first || size2 != size {
		t.Fatalf("dedupe = %s/%d, want %s/%d", second, size2, first, size)
	}
	if store.UsageBytes() != 10 {
		t.Fatalf("usage = %d, want 10 (one copy)", store.UsageBytes())
	}
	object, err := store.Open(first)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(object)
	object.Close()
	if string(raw) != "same bytes" {
		t.Fatalf("object = %q", raw)
	}
}

func TestRefCountIsEventIdempotent(t *testing.T) {
	store := newTestStore(t, Options{})
	hash, _, err := store.Put(bytes.NewReader([]byte("shared")))
	if err != nil {
		t.Fatal(err)
	}
	refA := Ref{Session: "session-a", Seq: 7}
	refB := Ref{Session: "session-b", Seq: 9}
	if added, err := store.Retain(hash, refA); err != nil || !added {
		t.Fatalf("retain A = %v/%v", added, err)
	}
	if added, err := store.Retain(hash, refA); err != nil || added {
		t.Fatalf("replay retain A = %v/%v, want a no-op", added, err)
	}
	if added, err := store.Retain(hash, refB); err != nil || !added {
		t.Fatalf("retain B = %v/%v", added, err)
	}
	if got := store.RefCount(hash); got != 2 {
		t.Fatalf("refcount = %d, want 2", got)
	}
	// A fork shares the object: deleting the parent session leaves the child ref.
	if released, err := store.ReleaseSession("session-a"); err != nil || released != 1 {
		t.Fatalf("release session-a = %d/%v", released, err)
	}
	if got := store.RefCount(hash); got != 1 {
		t.Fatalf("refcount after parent delete = %d, want 1", got)
	}
	if removed, err := store.Collect(time.Nanosecond); err != nil || removed != 0 {
		t.Fatalf("collect with a live child ref = %d/%v", removed, err)
	}
	if !store.Has(hash) {
		t.Fatal("GC removed an object still referenced by the fork")
	}
	if _, err := store.Release(hash, refB); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if removed, err := store.Collect(time.Nanosecond); err != nil || removed != 1 {
		t.Fatalf("collect after last release = %d/%v", removed, err)
	}
	if _, err := store.Open(hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open after GC = %v, want not found", err)
	}
}

func TestRefJournalSurvivesReopen(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	hash, _, err := store.Put(bytes.NewReader([]byte("durable ref")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Retain(hash, Ref{Session: "s1", Seq: 3}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.RefCount(hash); got != 1 {
		t.Fatalf("refcount after reopen = %d, want 1", got)
	}
	if !reopened.Has(hash) {
		t.Fatal("object missing after reopen")
	}
}

func TestQuotaRejectsAtCommitPoint(t *testing.T) {
	store := newTestStore(t, Options{QuotaBytes: 8})
	_, _, err := store.Put(bytes.NewReader([]byte("0123456789")))
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over-quota put = %v, want quota_exceeded", err)
	}
	if store.UsageBytes() != 0 {
		t.Fatalf("usage after rejected put = %d", store.UsageBytes())
	}
	entries, err := os.ReadDir(filepath.Join(store.Root(), ".tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp objects left behind: %v", entries)
	}
	// A fitting object is accepted.
	if _, _, err := store.Put(bytes.NewReader([]byte("12345678"))); err != nil {
		t.Fatalf("fitting put = %v", err)
	}
}

// guardReader fails the test if the store ever asks for a buffer larger than
// the configured chunk. That is the deterministic form of "a large file never
// materialises in memory".
type guardReader struct {
	t    *testing.T
	max  int
	data []byte
	pos  int
}

func (g *guardReader) Read(p []byte) (int, error) {
	if len(p) > g.max {
		g.t.Fatalf("read buffer = %d bytes, want at most %d", len(p), g.max)
	}
	if g.pos >= len(g.data) {
		return 0, io.EOF
	}
	n := copy(p, g.data[g.pos:])
	g.pos += n
	return n, nil
}

func TestLargePutAndReadAreChunked(t *testing.T) {
	store := newTestStore(t, Options{})
	const size = 8 << 20
	data := bytes.Repeat([]byte("0123456789abcdef"), size/16)
	hash, stored, err := store.Put(&guardReader{t: t, max: DefaultChunkSize, data: data})
	if err != nil {
		t.Fatal(err)
	}
	if stored != size {
		t.Fatalf("stored = %d", stored)
	}
	sum := sha256.Sum256(data)
	if hash != Hash(hex.EncodeToString(sum[:])) {
		t.Fatal("hash mismatch")
	}
	var out bytes.Buffer
	if _, err := store.ReadRange(hash, 0, 0, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != size {
		t.Fatalf("read = %d bytes", out.Len())
	}
	// Paging reads exactly the requested window.
	out.Reset()
	if _, err := store.ReadRange(hash, 100, 50, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data[100:150]) {
		t.Fatalf("range = %q", out.Bytes())
	}
}

func TestCollectIsConcurrencySafe(t *testing.T) {
	store := newTestStore(t, Options{})
	kept, _, err := store.Put(bytes.NewReader([]byte("kept")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Retain(kept, Ref{Session: "keeper", Seq: 1}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				hash, _, err := store.Put(bytes.NewReader([]byte{byte(worker), byte(i)}))
				if err != nil {
					t.Errorf("put: %v", err)
					return
				}
				if _, err := store.Retain(hash, Ref{Session: "writer", Seq: int64(worker*1000 + i)}); err != nil {
					t.Errorf("retain: %v", err)
					return
				}
			}
		}(worker)
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 20; i++ {
			if _, err := store.Collect(time.Hour); err != nil {
				t.Errorf("collect: %v", err)
				break
			}
		}
		close(done)
	}()
	wg.Wait()
	<-done
	if !store.Has(kept) {
		t.Fatal("GC removed a referenced object during concurrent writes")
	}
}

func TestPutResultReportsCreated(t *testing.T) {
	store := newTestStore(t, Options{})
	first, err := store.PutStatus(bytes.NewReader([]byte("once")))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.PutStatus(bytes.NewReader([]byte("once")))
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || second.Created {
		t.Fatalf("created flags = %v/%v", first.Created, second.Created)
	}
}
