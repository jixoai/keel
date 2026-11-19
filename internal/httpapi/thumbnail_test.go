package httpapi

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/blob"
)

func testPNG(t *testing.T, r, g, b uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// TestFileThumbnailLifecycleAndTenantIsolation pins the route's explicit
// statuses and the tenant predicate: another tenant's id is a 404, pending is a
// 202, failed is a 409, and only a ready thumbnail serves bytes.
func TestFileThumbnailLifecycleAndTenantIsolation(t *testing.T) {
	hs, st := newTestServer(t)
	ctx := context.Background()
	hc := hs.Client()
	tenant := uuid.NewString()
	otherTenant := uuid.NewString()
	if code := createTestTenant(t, hc, hs.URL, tenant, "thumb"); code != 201 {
		t.Fatalf("create tenant: %d", code)
	}
	if code := createTestTenant(t, hc, hs.URL, otherTenant, "thumb-other"); code != 201 {
		t.Fatalf("create other tenant: %d", code)
	}

	up := &blob.Uploader{Store: st, CASDir: t.TempDir()}
	tenantID := uuid.MustParse(tenant)
	parent, err := up.Put(ctx, tenantID, "image/png", bytes.NewReader(testPNG(t, 10, 20, 30)))
	if err != nil {
		t.Fatalf("upload parent: %v", err)
	}
	thumb, err := up.Put(ctx, tenantID, "image/png", bytes.NewReader(testPNG(t, 200, 100, 50)))
	if err != nil {
		t.Fatalf("upload thumb: %v", err)
	}
	if ok, err := st.MarkThumbPending(ctx, tenantID, parent.BlobID); err != nil || !ok {
		t.Fatalf("mark pending: ok=%v err=%v", ok, err)
	}

	get := func(path, tenantHeader string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest("GET", hs.URL+path, nil)
		req.Header.Set("X-Tenant-ID", tenantHeader)
		res, err := hc.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res, body
	}

	// Pending: explicit 202, not 500 and not an empty 200.
	res, body := get("/v1/files/"+parent.BlobID.String()+"/thumbnail", tenant)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("pending status = %d body=%s, want 202", res.StatusCode, body)
	}

	if ok, err := st.SetThumbReady(ctx, tenantID, parent.BlobID, thumb.BlobID); err != nil || !ok {
		t.Fatalf("set ready: ok=%v err=%v", ok, err)
	}
	res, body = get("/v1/files/"+parent.BlobID.String()+"/thumbnail", tenant)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("ready status=%d content-type=%q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if !bytes.HasPrefix(body, []byte("\x89PNG")) {
		t.Fatalf("ready body is not a PNG: %x", body[:minInt(len(body), 8)])
	}

	// Tenant isolation: the same id under another tenant is indistinguishable
	// from a missing blob.
	res, _ = get("/v1/files/"+parent.BlobID.String()+"/thumbnail", otherTenant)
	if res.StatusCode != 404 {
		t.Fatalf("foreign tenant status = %d, want 404", res.StatusCode)
	}

	// Failed: terminal 409.
	failedParent, err := up.Put(ctx, tenantID, "image/png", bytes.NewReader(testPNG(t, 1, 2, 3)))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.MarkThumbPending(ctx, tenantID, failedParent.BlobID); err != nil || !ok {
		t.Fatalf("mark pending: ok=%v err=%v", ok, err)
	}
	if ok, err := st.SetThumbFailed(ctx, tenantID, failedParent.BlobID); err != nil || !ok {
		t.Fatalf("set failed: ok=%v err=%v", ok, err)
	}
	res, _ = get("/v1/files/"+failedParent.BlobID.String()+"/thumbnail", tenant)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("failed status = %d, want 409", res.StatusCode)
	}

	// Non-image: no thumbnail applies.
	text, err := up.Put(ctx, tenantID, "text/plain", bytes.NewReader([]byte("plain text")))
	if err != nil {
		t.Fatal(err)
	}
	res, _ = get("/v1/files/"+text.BlobID.String()+"/thumbnail", tenant)
	if res.StatusCode != 404 {
		t.Fatalf("non-image thumbnail status = %d, want 404", res.StatusCode)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
