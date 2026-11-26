package httpapi

// GET /v1/sessions/{id}/attachments/{fileId}/part (change keel-plugin-attachment,
// task 3.1): the message-scoped read-only projection a plugin preview consumes.
// Display facts come from the committed user.message `seq`; MIME/size/thumbnail
// lifecycle come from the blob record. Delivery is attempt-scoped and explicitly
// reported as not stored here.

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/attachment"
	"github.com/jixoai/keel/internal/store"
)

type attachmentPartInput struct {
	Seq     *int64 `json:"seq"`
	Created bool   `json:"created"`
}

func partPath(session, file uuid.UUID, seq int64) string {
	return "/v1/sessions/" + session.String() + "/attachments/" + file.String() +
		"/part?seq=" + strconv.FormatInt(seq, 10)
}

func TestAttachmentPartProjection(t *testing.T) {
	hs, st := newTestServer(t)
	tenant := uuid.NewString()
	session := inputSetup(t, hs, tenant)
	tenantID := uuid.MustParse(tenant)
	meta := completeTestBlob(t, st, tenantID, "text/plain", []byte("hello part"))

	var input attachmentPartInput
	if code := doJSON(t, hs.Client(), http.MethodPost, hs.URL+"/v1/sessions/"+session.String()+"/input", tenant,
		map[string]any{
			"text": "look", "requestId": "part-1",
			"attachments": []map[string]any{{
				"fileId": meta.BlobID.String(), "name": "notes.txt", "mime": "text/plain",
				"width": 12, "height": 7,
			}},
		}, &input); code != http.StatusCreated || input.Seq == nil {
		t.Fatalf("input = %d (%+v)", code, input)
	}
	var part attachment.Part
	if code := doJSON(t, hs.Client(), http.MethodGet, hs.URL+partPath(session, meta.BlobID, *input.Seq), tenant, nil, &part); code != http.StatusOK {
		t.Fatalf("part = %d", code)
	}
	if part.FileID != meta.BlobID.String() || part.Name != "notes.txt" ||
		part.Mime != "text/plain" || part.Size != int64(len("hello part")) || part.Kind != "file" ||
		part.Width != 12 || part.Height != 7 || part.ThumbStatus != "none" ||
		part.ThumbnailURL != "" || part.DownloadURL != "/v1/files/"+meta.BlobID.String() ||
		part.Delivery.Native || part.Delivery.Reason == "" {
		t.Fatalf("part = %+v", part)
	}

	// A query-supplied name cannot override the committed message's display name.
	var ignored attachment.Part
	if code := doJSON(t, hs.Client(), http.MethodGet,
		hs.URL+partPath(session, meta.BlobID, *input.Seq)+"&name=hacker.png&native=true", tenant, nil, &ignored); code != http.StatusOK {
		t.Fatalf("part with extra query = %d", code)
	}
	if ignored.Name != "notes.txt" || ignored.Delivery.Native {
		t.Fatalf("query became authority: %+v", ignored)
	}
}

func TestAttachmentPartThumbnailLifecycle(t *testing.T) {
	hs, st := newTestServer(t)
	ctx := context.Background()
	tenant := uuid.NewString()
	session := inputSetup(t, hs, tenant)
	tenantID := uuid.MustParse(tenant)
	parent := completeTestBlob(t, st, tenantID, "image/png", testPNG(t, 3, 4, 5))
	if _, err := st.MarkThumbPending(ctx, tenantID, parent.BlobID); err != nil {
		t.Fatal(err)
	}
	var input attachmentPartInput
	if code := doJSON(t, hs.Client(), http.MethodPost, hs.URL+"/v1/sessions/"+session.String()+"/input", tenant,
		map[string]any{
			"text": "look", "requestId": "part-thumb",
			"attachments": []map[string]any{{
				"fileId": parent.BlobID.String(), "name": "shot.png", "mime": "image/png",
			}},
		}, &input); code != http.StatusCreated || input.Seq == nil {
		t.Fatalf("input = %d (%+v)", code, input)
	}
	read := func() attachment.Part {
		t.Helper()
		var part attachment.Part
		if code := doJSON(t, hs.Client(), http.MethodGet, hs.URL+partPath(session, parent.BlobID, *input.Seq), tenant, nil, &part); code != http.StatusOK {
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
	var failedInput attachmentPartInput
	if code := doJSON(t, hs.Client(), http.MethodPost, hs.URL+"/v1/sessions/"+session.String()+"/input", tenant,
		map[string]any{
			"text": "look", "requestId": "part-thumb-failed",
			"attachments": []map[string]any{{
				"fileId": failedParent.BlobID.String(), "name": "broken.png", "mime": "image/png",
			}},
		}, &failedInput); code != http.StatusCreated || failedInput.Seq == nil {
		t.Fatalf("failed input = %d (%+v)", code, failedInput)
	}
	var failed attachment.Part
	if code := doJSON(t, hs.Client(), http.MethodGet, hs.URL+partPath(session, failedParent.BlobID, *failedInput.Seq), tenant, nil, &failed); code != http.StatusOK {
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
	session := inputSetup(t, hs, tenant)
	tenantID := uuid.MustParse(tenant)
	other := uuid.New()
	if err := st.CreateTenant(ctx, other, "attachment-part-other"); err != nil {
		t.Fatal(err)
	}
	own := completeTestBlob(t, st, tenantID, "application/pdf", []byte("%PDF-1.4"))
	foreign := completeTestBlob(t, st, other, "text/plain", []byte("foreign"))
	incomplete := uuid.New()
	if _, _, err := st.CreateBlob(ctx, tenantID, incomplete, []byte("01234567890123456789012345678901"), 1, "application/pdf", store.StoragePGChunked); err != nil {
		t.Fatal(err)
	}
	var input attachmentPartInput
	if code := doJSON(t, hs.Client(), http.MethodPost, hs.URL+"/v1/sessions/"+session.String()+"/input", tenant,
		map[string]any{
			"text": "look", "requestId": "part-boundary",
			"attachments": []map[string]any{{"fileId": own.BlobID.String(), "name": "a.pdf", "mime": "application/pdf"}},
		}, &input); code != http.StatusCreated || input.Seq == nil {
		t.Fatalf("input = %d (%+v)", code, input)
	}
	// Raw append bypasses input validation to plant foreign/incomplete refs.
	appendRaw := func(fileID uuid.UUID, name string, requestID string) int64 {
		t.Helper()
		var appended struct {
			Seq int64 `json:"seq"`
		}
		if code := doJSON(t, hs.Client(), http.MethodPost, hs.URL+"/v1/sessions/"+session.String()+"/events", tenant,
			map[string]any{"kind": "user.message", "payload": map[string]any{
				"text":        "raw",
				"attachments": []map[string]any{{"fileId": fileID.String(), "name": name, "mime": "text/plain"}},
			}, "requestId": requestID}, &appended); code != http.StatusCreated {
			t.Fatalf("append %s: %d", requestID, code)
		}
		return appended.Seq
	}
	foreignSeq := appendRaw(foreign.BlobID, "foreign.txt", "part-foreign")
	incompleteSeq := appendRaw(incomplete, "partial.pdf", "part-incomplete")

	cases := []struct {
		name string
		path string
		want int
	}{
		{"missing seq", "/v1/sessions/" + session.String() + "/attachments/" + own.BlobID.String() + "/part", http.StatusBadRequest},
		{"bad seq", "/v1/sessions/" + session.String() + "/attachments/" + own.BlobID.String() + "/part?seq=nope", http.StatusBadRequest},
		{"unknown message", "/v1/sessions/" + session.String() + "/attachments/" + own.BlobID.String() + "/part?seq=999", http.StatusNotFound},
		{"attachment not in message", partPath(session, uuid.New(), *input.Seq), http.StatusNotFound},
		{"foreign blob", partPath(session, foreign.BlobID, foreignSeq), http.StatusNotFound},
		{"incomplete blob", partPath(session, incomplete, incompleteSeq), http.StatusConflict},
		{"bad session", "/v1/sessions/not-a-uuid/attachments/" + own.BlobID.String() + "/part?seq=1", http.StatusBadRequest},
		{"bad file", "/v1/sessions/" + session.String() + "/attachments/not-a-uuid/part?seq=1", http.StatusBadRequest},
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
