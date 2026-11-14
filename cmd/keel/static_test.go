package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A build ships `<asset>.br` / `<asset>.gz` beside every asset. Serving the
// sidecar is what keeps the 7.2 MB wasm from crossing the network uncompressed;
// these tests pin the negotiation and the two exceptions (index.html, Range).
func newStaticFixture(t *testing.T) *staticHandler {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("index.html", "<!doctype html><title>Keel</title>")
	write("app.abc123def.js", "console.log('plain')")
	write("app.abc123def.js.br", "BROTLI-BODY")
	write("nsh.abc123def.wasm", "WASM-PLAIN")
	write("nsh.abc123def.wasm.gz", "GZIP-BODY")
	return newStaticHandler(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func get(t *testing.T, h *staticHandler, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestStaticServesBrotliSidecar(t *testing.T) {
	h := newStaticFixture(t)
	rec := get(t, h, "/app.abc123def.js", map[string]string{"Accept-Encoding": "gzip, deflate, br"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("content-encoding = %q, want br", got)
	}
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("vary = %q", got)
	}
	if got := rec.Body.String(); got != "BROTLI-BODY" {
		t.Fatalf("body = %q, want the sidecar", got)
	}
	// The type comes from the original name, not the ".br" suffix.
	if got := rec.Header().Get("Content-Type"); got == "" || got == "application/octet-stream" {
		t.Fatalf("content-type = %q, want the original asset's type", got)
	}
}

func TestStaticFallsBackToGzipThenPlain(t *testing.T) {
	h := newStaticFixture(t)
	gz := get(t, h, "/nsh.abc123def.wasm", map[string]string{"Accept-Encoding": "gzip"})
	if gz.Header().Get("Content-Encoding") != "gz" || gz.Body.String() != "GZIP-BODY" {
		t.Fatalf("gzip path: encoding=%q body=%q", gz.Header().Get("Content-Encoding"), gz.Body.String())
	}
	plain := get(t, h, "/nsh.abc123def.wasm", nil)
	if plain.Header().Get("Content-Encoding") != "" || plain.Body.String() != "WASM-PLAIN" {
		t.Fatalf("plain path: encoding=%q body=%q", plain.Header().Get("Content-Encoding"), plain.Body.String())
	}
}

func TestStaticKeepsIndexAndRangeUncompressed(t *testing.T) {
	h := newStaticFixture(t)
	index := get(t, h, "/index.html", map[string]string{"Accept-Encoding": "br"})
	if index.Header().Get("Content-Encoding") != "" {
		t.Fatalf("index.html must stay uncompressed, got %q", index.Header().Get("Content-Encoding"))
	}
	ranged := get(t, h, "/app.abc123def.js", map[string]string{"Accept-Encoding": "br", "Range": "bytes=0-3"})
	if ranged.Header().Get("Content-Encoding") != "" {
		t.Fatalf("range request must not be re-encoded, got %q", ranged.Header().Get("Content-Encoding"))
	}
}
