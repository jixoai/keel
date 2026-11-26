package httpapi

// Input-time attachment validation (change keel-plugin-attachment). The shared
// commit path must refuse a reference before any event: foreign/incomplete ids,
// MIME forgery, size/quota breaches. The one tolerated degradation is a
// missing fileId, which becomes the explicit notice the transcript rebuild
// already implements.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/attachment"
	"github.com/jixoai/keel/internal/quota"
	"github.com/jixoai/keel/internal/store"
)

// completeTestBlob writes one readable blob row for the tenant. Tests that need
// bytes can use this without going through the HTTP upload path (the subject
// under test is the reference validation, not the uploader).
func completeTestBlob(t *testing.T, st *store.Store, tenant uuid.UUID, mime string, data []byte) store.BlobMeta {
	t.Helper()
	ctx := context.Background()
	blobID := uuid.New()
	sum := sha256.Sum256(data)
	if _, _, err := st.CreateBlob(ctx, tenant, blobID, sum[:], int64(len(data)), mime, store.StoragePGChunked); err != nil {
		t.Fatalf("create blob: %v", err)
	}
	if err := st.CompleteBlob(ctx, tenant, blobID, 0); err != nil {
		t.Fatalf("complete blob: %v", err)
	}
	meta, err := st.BlobMetaOf(ctx, tenant, blobID)
	if err != nil {
		t.Fatalf("blob meta: %v", err)
	}
	return meta
}

type inputErrorBody struct {
	Error string `json:"error"`
	Field string `json:"field"`
}

func postInputWithAttachments(t *testing.T, hs *httptest.Server, tenant string, session uuid.UUID, requestID string, attachments any) (int, inputErrorBody) {
	t.Helper()
	var body inputErrorBody
	code := doJSON(t, hs.Client(), http.MethodPost, hs.URL+"/v1/sessions/"+session.String()+"/input", tenant,
		map[string]any{"text": "look", "requestId": requestID, "attachments": attachments}, &body)
	return code, body
}

func storedAttachments(t *testing.T, st *store.Store, tenant uuid.UUID, session uuid.UUID, requestID string) ([]attachment.Ref, map[string]json.RawMessage) {
	t.Helper()
	payload, _ := committedInput(t, st, tenant, session, requestID)
	raw, ok := payload["attachments"]
	if !ok {
		t.Fatalf("committed input carries no attachments: %+v", payload)
	}
	var refs []attachment.Ref
	if err := json.Unmarshal(raw, &refs); err != nil {
		t.Fatalf("attachments = %s: %v", raw, err)
	}
	return refs, payload
}

// TestInputAttachmentAcceptsAndCanonicalizes pins the happy path: the blob
// record is the authority for MIME and size, the declared value is compared
// after normalization, and unknown client fields never enter the log.
func TestInputAttachmentAcceptsAndCanonicalizes(t *testing.T) {
	hs, st := newTestServer(t)
	tenant := uuid.NewString()
	session := inputSetup(t, hs, tenant)
	tenantID := uuid.MustParse(tenant)
	meta := completeTestBlob(t, st, tenantID, "text/plain", []byte("hello attachment"))

	code, body := postInputWithAttachments(t, hs, tenant, session, "attach-ok", []map[string]any{{
		"fileId": meta.BlobID.String(),
		"name":   "notes.txt",
		"mime":   " Text/Plain ; charset=utf-8",
		"size":   999,
		"width":  3,
		"height": 4,
		"url":    "data:text/plain,not-stored",
	}})
	if code != http.StatusCreated {
		t.Fatalf("input = %d (%+v)", code, body)
	}
	refs, _ := storedAttachments(t, st, tenantID, session, "attach-ok")
	if len(refs) != 1 {
		t.Fatalf("refs = %+v", refs)
	}
	ref := refs[0]
	if ref.FileID != meta.BlobID.String() || ref.Name != "notes.txt" || ref.Mime != "text/plain" ||
		ref.Size != int64(len("hello attachment")) || ref.Width != 3 || ref.Height != 4 {
		t.Fatalf("canonical ref = %+v", ref)
	}
}

// TestInputAttachmentRefusals pins the four field-level refusals and the
// no-event guarantee. Cross-tenant and unknown ids share one message so the
// endpoint never confirms a foreign id.
func TestInputAttachmentRefusals(t *testing.T) {
	hs, st := newTestServer(t)
	ctx := context.Background()
	tenant := uuid.NewString()
	session := inputSetup(t, hs, tenant)
	tenantID := uuid.MustParse(tenant)

	otherTenant := uuid.New()
	if err := st.CreateTenant(ctx, otherTenant, "attachment-foreign"); err != nil {
		t.Fatal(err)
	}
	foreign := completeTestBlob(t, st, otherTenant, "text/plain", []byte("foreign"))
	incomplete := uuid.New()
	sum := sha256.Sum256([]byte("incomplete"))
	if _, _, err := st.CreateBlob(ctx, tenantID, incomplete, sum[:], 3, "application/pdf", store.StoragePGChunked); err != nil {
		t.Fatal(err)
	}
	textBlob := completeTestBlob(t, st, tenantID, "text/plain", []byte("plain"))
	imageBlob := completeTestBlob(t, st, tenantID, "image/png", []byte("\x89PNG"))

	cases := []struct {
		name        string
		requestID   string
		attachments []map[string]any
		field       string
		wantStatus  int
	}{
		{
			name:        "foreign tenant id",
			requestID:   "attach-foreign",
			attachments: []map[string]any{{"fileId": foreign.BlobID.String(), "name": "x.txt", "mime": "text/plain"}},
			field:       "attachments[0].fileId",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "unknown id",
			requestID:   "attach-unknown",
			attachments: []map[string]any{{"fileId": uuid.NewString(), "name": "x.txt", "mime": "text/plain"}},
			field:       "attachments[0].fileId",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "incomplete upload",
			requestID:   "attach-incomplete",
			attachments: []map[string]any{{"fileId": incomplete.String(), "name": "x.pdf", "mime": "application/pdf"}},
			field:       "attachments[0].fileId",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "declared image for text bytes",
			requestID:   "attach-forge-image",
			attachments: []map[string]any{{"fileId": textBlob.BlobID.String(), "name": "x.png", "mime": "image/png"}},
			field:       "attachments[0].mime",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "declared text for image bytes",
			requestID:   "attach-forge-text",
			attachments: []map[string]any{{"fileId": imageBlob.BlobID.String(), "name": "x.txt", "mime": "text/plain"}},
			field:       "attachments[0].mime",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:      "more than sixteen",
			requestID: "attach-count",
			attachments: func() []map[string]any {
				items := make([]map[string]any, attachment.MaxPerInput+1)
				for index := range items {
					items[index] = map[string]any{"name": "x.txt", "mime": "text/plain"}
				}
				return items
			}(),
			field:      "attachments",
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			code, body := postInputWithAttachments(t, hs, tenant, session, testCase.requestID, testCase.attachments)
			if code != testCase.wantStatus {
				t.Fatalf("status = %d (%+v), want %d", code, body, testCase.wantStatus)
			}
			if body.Field != testCase.field {
				t.Fatalf("field = %q (%+v), want %q", body.Field, body, testCase.field)
			}
			if events, err := st.LoadEvents(ctx, tenantID, session, 0, 100); err != nil || len(events) != 0 {
				t.Fatalf("a refused input wrote events: %d, %v", len(events), err)
			}
		})
	}
}

// TestInputAttachmentSizeAndQuotaAre413 pins the two resource refusals: the
// deployment's upload bound and the tenant's storage ceiling (measured, never
// charged twice).
func TestInputAttachmentSizeAndQuotaAre413(t *testing.T) {
	t.Run("max upload", func(t *testing.T) {
		hs, st := newTestServer(t, func(c *Config) { c.MaxUpload = 8 })
		tenant := uuid.NewString()
		session := inputSetup(t, hs, tenant)
		meta := completeTestBlob(t, st, uuid.MustParse(tenant), "text/plain", []byte("more than eight bytes"))
		code, body := postInputWithAttachments(t, hs, tenant, session, "attach-too-big", []map[string]any{{
			"fileId": meta.BlobID.String(), "name": "big.txt", "mime": "text/plain",
		}})
		if code != http.StatusRequestEntityTooLarge || body.Field != "attachments[0].size" {
			t.Fatalf("status = %d field=%q (%+v)", code, body.Field, body)
		}
		if !strings.Contains(body.Error, "8") {
			t.Fatalf("the refusal must name the limit: %q", body.Error)
		}
	})

	t.Run("tenant quota", func(t *testing.T) {
		src := quota.QuotaSourceFunc(func(context.Context, uuid.UUID) (quota.TenantQuota, error) {
			return quota.TenantQuota{StorageBytes: 5}, nil
		})
		edge := quota.NewEdge(src, func(context.Context, uuid.UUID) (int64, error) { return 9, nil })
		hs, st := newTestServer(t, func(c *Config) { c.Quota = edge })
		tenant := uuid.NewString()
		session := inputSetup(t, hs, tenant)
		meta := completeTestBlob(t, st, uuid.MustParse(tenant), "text/plain", []byte("1234"))
		code, body := postInputWithAttachments(t, hs, tenant, session, "attach-quota", []map[string]any{{
			"fileId": meta.BlobID.String(), "name": "q.txt", "mime": "text/plain",
		}})
		if code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d (%+v), want 413", code, body)
		}
		if !strings.Contains(body.Error, "limit is 5") {
			t.Fatalf("the refusal must name the ceiling: %q", body.Error)
		}
	})
}

// TestInputAttachmentWithoutFileIDStaysANotice is the regression for the one
// tolerated degradation: the whole input is accepted, the reference survives
// with its display metadata, and the model sees an explicit notice.
func TestInputAttachmentWithoutFileIDStaysANotice(t *testing.T) {
	hs, st := newTestServer(t)
	tenant := uuid.NewString()
	session := inputSetup(t, hs, tenant)
	tenantID := uuid.MustParse(tenant)
	code, body := postInputWithAttachments(t, hs, tenant, session, "attach-notice", []map[string]any{{
		"name": "orphan.pdf", "mime": "application/pdf", "size": 10,
	}})
	if code != http.StatusCreated {
		t.Fatalf("input = %d (%+v)", code, body)
	}
	refs, _ := storedAttachments(t, st, tenantID, session, "attach-notice")
	if len(refs) != 1 || refs[0].FileID != "" || refs[0].Name != "orphan.pdf" {
		t.Fatalf("refs = %+v", refs)
	}
	events, err := st.LoadEvents(context.Background(), tenantID, session, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	msgs := agent.MessagesFromEvents(events, nil)
	notice := agent.ContentText(msgs[0].(agentcore.UserMessage).Content)
	if !strings.Contains(notice, "no fileId") || !strings.Contains(notice, "orphan.pdf") {
		t.Fatalf("model transcript = %q, want the explicit no-fileId notice", notice)
	}
}

// TestInputReplaySkipsAttachmentQuotaRecheck pins idempotency over a changed
// quota: the first commit accepted the reference; replaying the same requestId
// after the tenant goes over its ceiling returns the original receipt instead of
// a new 413. A new requestId is still refused.
func TestInputReplaySkipsAttachmentQuotaRecheck(t *testing.T) {
	var stored int64 = 1
	src := quota.QuotaSourceFunc(func(context.Context, uuid.UUID) (quota.TenantQuota, error) {
		return quota.TenantQuota{StorageBytes: 10}, nil
	})
	edge := quota.NewEdge(src, func(context.Context, uuid.UUID) (int64, error) { return stored, nil })
	hs, st := newTestServer(t, func(c *Config) { c.Quota = edge })
	tenant := uuid.NewString()
	session := inputSetup(t, hs, tenant)
	meta := completeTestBlob(t, st, uuid.MustParse(tenant), "text/plain", []byte("hello"))
	attachment := []map[string]any{{"fileId": meta.BlobID.String(), "name": "a.txt", "mime": "text/plain"}}

	var first struct {
		Seq     *int64 `json:"seq"`
		Created bool   `json:"created"`
		Status  string `json:"status"`
		Error   string `json:"error"`
	}
	code := doJSON(t, hs.Client(), http.MethodPost, hs.URL+"/v1/sessions/"+session.String()+"/input", tenant,
		map[string]any{"text": "first", "requestId": "replay-quota", "attachments": attachment}, &first)
	if code != http.StatusCreated || first.Seq == nil {
		t.Fatalf("first input = %d (%+v)", code, first)
	}
	stored = 99 // the tenant is now over its ceiling

	var replay struct {
		Seq     *int64 `json:"seq"`
		Created bool   `json:"created"`
		Error   string `json:"error"`
	}
	code = doJSON(t, hs.Client(), http.MethodPost, hs.URL+"/v1/sessions/"+session.String()+"/input", tenant,
		map[string]any{"text": "first", "requestId": "replay-quota", "attachments": attachment}, &replay)
	if code != http.StatusOK || replay.Seq == nil || *replay.Seq != *first.Seq || replay.Created {
		t.Fatalf("replay = %d (%+v), want the original receipt", code, replay)
	}

	var fresh inputErrorBody
	code = doJSON(t, hs.Client(), http.MethodPost, hs.URL+"/v1/sessions/"+session.String()+"/input", tenant,
		map[string]any{"text": "new", "requestId": "replay-quota-new", "attachments": attachment}, &fresh)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("new input over quota = %d (%+v), want 413", code, fresh)
	}
	if fresh.Field != "attachments" {
		t.Fatalf("quota refusal field = %q, want attachments", fresh.Field)
	}
}
