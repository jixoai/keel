package httpapi

// GET /v1/attachments/{id}/part (change keel-plugin-attachment, task 3.1): the
// read-only projection a plugin preview consumes. The tests pin the shape, the
// tenant boundary, the incomplete-object refusal and the thumbnail lifecycle.

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/attachment"
	"github.com/jixoai/keel/internal/store"
)

func TestAttachmentPartProjection(t *testing.T) {
	hs, st := newTestServer(t)
	tenant := uuid.NewString()
	tenantID := uuid.MustParse(tenant)
	if code := createTestTenant(t, hs.Client(), hs.URL, tenant, "attachment-part"); code != http.StatusCreated {
		t.Fatalf("create tenant: %d", code)
	}
	blobMeta := completeTestBlob(t, st, tenantID, "text/plain", []byte("hello part"))

	var part attachment.Part
	code := doJSON(t, hs.Client(), http.MethodGet,
		hs.URL+"/v1/attachments/"+blobMeta.BlobID.String()+"/part?name=notes.txt&width=12&height=7&native=true&reason=the+wire+carries+it",
		tenant, nil, &part)
	if code != http.StatusOK {
		t.Fatalf("part = %d", code)
	}
	if part.FileID != blobMeta.BlobID.String() || part.Name != "notes.txt" ||
		part.Mime != "text/plain" || part.Size != int64(len("hello part")) || part.Kind != "file" ||
		part.Width != 12 || part.Height != 7 || part.ThumbStatus != "none" ||
		part.ThumbnailURL != "" || part.DownloadURL != "/v1/files/"+blobMeta.BlobID.String() ||
		!part.Delivery.Native || part.Delivery.Reason != "the wire carries it" {
		t.Fatalf("part = %+v", part)
	}
}

func TestAttachmentPartThumbnailLifecycle(t *testing.T) {
	hs, st := newTestServer(t)
	ctx := context.Background()
	tenant := uuid.NewString()
	tenantID := uuid.MustParse(tenant)
	if code := createTestTenant(t, hs.Client(), hs.URL, tenant, "attachment-part-thumb"); code != http.StatusCreated {
		t.Fatalf("create tenant: %d", code)
	}
	parent := completeTestBlob(t, st, tenantID, "image/png", testPNG(t, 3, 4, 5))
	if _, err := st.MarkThumbPending(ctx, tenantID, parent.BlobID); err != nil {
		t.Fatal(err)
	}
	read := func() attachment.Part {
		t.Helper()
		var part attachment.Part
		if code := doJSON(t, hs.Client(), http.MethodGet,
			hs.URL+"/v1/attachments/"+parent.BlobID.String()+"/part?name=shot.png", tenant, nil, &part); code != http.StatusOK {
			t.Fatalf("part = %d", code)
		}
		return part
	}
	pending := read()
	if pending.Kind != "image" || pending.ThumbStatus != "pending" || pending.ThumbnailURL != "" {
		t.Fatalf("pending part = %+v", pending)
	}
	thumb := completeTestBlob(t, st, tenantID, "image/png", testPNG(t, 9, 9, 9))
	if ok, err := st.SetThumbReady(ctx, tenantID, parent.BlobID, thumb.BlobID); err != nil || !ok {
		t.Fatalf("set ready: ok=%v err=%v", ok, err)
	}
	ready := read()
	if ready.ThumbStatus != "ready" || ready.ThumbnailURL != "/v1/files/"+parent.BlobID.String()+"/thumbnail" {
		t.Fatalf("ready part = %+v", ready)
	}
	failedParent := completeTestBlob(t, st, tenantID, "image/png", testPNG(t, 6, 6, 6))
	if _, err := st.MarkThumbPending(ctx, tenantID, failedParent.BlobID); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.SetThumbFailed(ctx, tenantID, failedParent.BlobID); err != nil || !ok {
		t.Fatalf("set failed: ok=%v err=%v", ok, err)
	}
	var failed attachment.Part
	if code := doJSON(t, hs.Client(), http.MethodGet,
		hs.URL+"/v1/attachments/"+failedParent.BlobID.String()+"/part?name=broken.png", tenant, nil, &failed); code != http.StatusOK {
		t.Fatalf("failed part = %d", code)
	}
	if failed.ThumbStatus != "failed" || failed.ThumbnailURL != "" {
		t.Fatalf("failed part = %+v", failed)
	}
}

func TestAttachmentPartBoundaries(t *testing.T) {
	hs, st := newTestServer(t)
	ctx := context.Background()
	tenant := uuid.NewString()
	tenantID := uuid.MustParse(tenant)
	if code := createTestTenant(t, hs.Client(), hs.URL, tenant, "attachment-part-boundary"); code != http.StatusCreated {
		t.Fatalf("create tenant: %d", code)
	}
	other := uuid.New()
	if err := st.CreateTenant(ctx, other, "attachment-part-other"); err != nil {
		t.Fatal(err)
	}
	foreign := completeTestBlob(t, st, other, "text/plain", []byte("foreign"))
	incomplete := uuid.New()
	if _, _, err := st.CreateBlob(ctx, tenantID, incomplete, []byte("01234567890123456789012345678901"), 1, "application/pdf", store.StoragePGChunked); err != nil {
		t.Fatal(err)
	}
	own := completeTestBlob(t, st, tenantID, "application/pdf", []byte("%PDF-1.4"))

	cases := []struct {
		name string
		path string
		want int
	}{
		{"foreign tenant", "/v1/attachments/" + foreign.BlobID.String() + "/part", http.StatusNotFound},
		{"unknown", "/v1/attachments/" + uuid.NewString() + "/part", http.StatusNotFound},
		{"incomplete", "/v1/attachments/" + incomplete.String() + "/part", http.StatusConflict},
		{"bad id", "/v1/attachments/not-a-uuid/part", http.StatusBadRequest},
		{"long name", "/v1/attachments/" + own.BlobID.String() + "/part?name=" + repeat("a", 257), http.StatusBadRequest},
		{"negative width", "/v1/attachments/" + own.BlobID.String() + "/part?width=-1", http.StatusBadRequest},
		{"bad native", "/v1/attachments/" + own.BlobID.String() + "/part?native=maybe", http.StatusBadRequest},
		{"long reason", "/v1/attachments/" + own.BlobID.String() + "/part?reason=" + repeat("r", 401), http.StatusBadRequest},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var body map[string]any
			if code := doJSON(t, hs.Client(), http.MethodGet, hs.URL+testCase.path, tenant, nil, &body); code != testCase.want {
				t.Fatalf("status = %d (%+v), want %d", code, body, testCase.want)
			}
		})
	}
}

func repeat(value string, count int) string {
	out := make([]byte, 0, len(value)*count)
	for index := 0; index < count; index++ {
		out = append(out, value...)
	}
	return string(out)
}
