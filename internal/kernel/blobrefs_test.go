package kernel

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/store"
)

// completeBlob creates a readable blob row for a tenant. CompleteBlob also
// records usage, which is irrelevant here but is the production completion path.
func completeBlob(t *testing.T, h *harness, tenant uuid.UUID, mime string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	sum := sha256.Sum256(id[:])
	if _, _, err := h.st.CreateBlob(ctx, tenant, id, sum[:], 32, mime, store.StoragePGChunked); err != nil {
		t.Fatalf("create blob: %v", err)
	}
	if err := h.st.CompleteBlob(ctx, tenant, id, 0); err != nil {
		t.Fatalf("complete blob: %v", err)
	}
	return id
}

func blobRefCount(t *testing.T, h *harness, tenant, blobID uuid.UUID) int32 {
	t.Helper()
	meta, err := h.st.BlobMetaOf(context.Background(), tenant, blobID)
	if err != nil {
		t.Fatalf("blob meta: %v", err)
	}
	return meta.RefCount
}

func blobRefRows(t *testing.T, h *harness, tenant, blobID uuid.UUID) int {
	t.Helper()
	var n int
	if err := h.st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM blob_refs WHERE tenant_id = $1 AND blob_id = $2`,
		tenant, blobID).Scan(&n); err != nil {
		t.Fatalf("count refs: %v", err)
	}
	return n
}

// TestBlobRefsFollowCommittedEvents pins the production projection: refs are
// recorded in the append transaction, replaying the same event does not double
// count, and only complete same-tenant blobs are counted.
func TestBlobRefsFollowCommittedEvents(t *testing.T) {
	if os.Getenv("KEEL_TEST_DSN") == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	h := newPGHarness(t)
	ctx := context.Background()
	tenant := h.tenant

	userBlob := completeBlob(t, h, tenant, "image/png")
	userPayload := fmt.Sprintf(`{"text":"see attached","attachments":[{"fileId":%q,"mime":"image/png"}]}`, userBlob.String())

	// One user.message: +1, and the refs row names the event's seq.
	first := h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "blob-ref-1", Payload: []byte(userPayload)})
	if got := blobRefCount(t, h, tenant, userBlob); got != 1 {
		t.Fatalf("ref_count after first user.message = %d, want 1", got)
	}
	if got := blobRefRows(t, h, tenant, userBlob); got != 1 {
		t.Fatalf("blob_refs rows = %d, want 1", got)
	}
	var refSeq int64
	if err := h.st.Pool().QueryRow(ctx,
		`SELECT event_seq FROM blob_refs WHERE tenant_id = $1 AND blob_id = $2`,
		tenant, userBlob).Scan(&refSeq); err != nil {
		t.Fatalf("read ref seq: %v", err)
	}
	if refSeq != first.Seq {
		t.Fatalf("ref seq = %d, want committed event seq %d", refSeq, first.Seq)
	}

	// Replaying the same request is a dedupe: no second ref, no second event.
	h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "blob-ref-1", Payload: []byte(userPayload)})
	if got := blobRefCount(t, h, tenant, userBlob); got != 1 {
		t.Fatalf("replayed user.message ref_count = %d, want 1", got)
	}
	if got := blobRefRows(t, h, tenant, userBlob); got != 1 {
		t.Fatalf("replayed blob_refs rows = %d, want 1", got)
	}

	// A second event referencing the same blob: +1.
	h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "blob-ref-2", Payload: []byte(userPayload)})
	if got := blobRefCount(t, h, tenant, userBlob); got != 2 {
		t.Fatalf("ref_count after second event = %d, want 2", got)
	}

	// assistant.message imageRef (the transcript's durable image shape).
	lease := h.claimScope(h.scope, "blob-refs")
	imageBlob := completeBlob(t, h, tenant, "image/jpeg")
	assistantPayload := fmt.Sprintf(`{"content":[{"type":"text","text":"look"},{"type":"imageRef","blobId":%q,"mimeType":"image/jpeg"}]}`, imageBlob.String())
	h.mustCommit(CommitRequest{Kind: KindAssistantMessage, Token: lease.Token, GenerationID: "blob-ref-g1", Payload: []byte(assistantPayload)})
	if got := blobRefCount(t, h, tenant, imageBlob); got != 1 {
		t.Fatalf("assistant imageRef ref_count = %d, want 1", got)
	}

	// tool.result structuredContent (bash output blob) is a reference too.
	toolBlob := completeBlob(t, h, tenant, "text/plain")
	h.mustCommit(CommitRequest{Kind: KindAssistantMessage, Token: lease.Token, GenerationID: "blob-ref-g2",
		Payload: []byte(`{"content":[{"type":"toolCall","id":"blob-ref-call","name":"bash","arguments":{}}]}`)})
	toolPayload := fmt.Sprintf(`{"toolCallId":"blob-ref-call","toolName":"bash","content":[{"type":"text","text":"done"}],"structuredContent":{"stdout":{"bytes":4,"blobId":%q}}}`, toolBlob.String())
	h.mustCommit(CommitRequest{Kind: KindToolResult, Token: lease.Token, ToolCallID: "blob-ref-call", Payload: []byte(toolPayload)})
	if got := blobRefCount(t, h, tenant, toolBlob); got != 1 {
		t.Fatalf("tool.result ref_count = %d, want 1", got)
	}

	// Cross-tenant blob id: present in another tenant, must not gain a ref.
	foreignTenant := uuid.New()
	if err := h.st.CreateTenant(ctx, foreignTenant, "blob-refs-foreign"); err != nil {
		t.Fatalf("create foreign tenant: %v", err)
	}
	foreignBlob := completeBlob(t, h, foreignTenant, "image/png")
	foreignPayload := fmt.Sprintf(`{"text":"cross","attachments":[{"fileId":%q}]}`, foreignBlob.String())
	h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "blob-ref-foreign", Payload: []byte(foreignPayload)})
	if got := blobRefCount(t, h, foreignTenant, foreignBlob); got != 0 {
		t.Fatalf("cross-tenant ref_count = %d, want 0", got)
	}

	// Incomplete upload: metadata exists but no ref may be created.
	incomplete := uuid.New()
	sum := sha256.Sum256(incomplete[:])
	if _, _, err := h.st.CreateBlob(ctx, tenant, incomplete, sum[:], 32, "image/png", store.StoragePGChunked); err != nil {
		t.Fatalf("create incomplete blob: %v", err)
	}
	incompletePayload := fmt.Sprintf(`{"text":"partial","attachments":[{"fileId":%q}]}`, incomplete.String())
	h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "blob-ref-incomplete", Payload: []byte(incompletePayload)})
	if got := blobRefCount(t, h, tenant, incomplete); got != 0 {
		t.Fatalf("incomplete ref_count = %d, want 0", got)
	}

	// Unknown id: the event still commits (client data is tolerated), no ref.
	unknown := uuid.New()
	unknownPayload := fmt.Sprintf(`{"text":"missing","attachments":[{"fileId":%q}]}`, unknown.String())
	h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "blob-ref-unknown", Payload: []byte(unknownPayload)})
	var unknownRefs int
	if err := h.st.Pool().QueryRow(ctx,
		`SELECT count(*) FROM blob_refs WHERE tenant_id = $1 AND blob_id = $2`,
		tenant, unknown).Scan(&unknownRefs); err != nil {
		t.Fatalf("count unknown refs: %v", err)
	}
	if unknownRefs != 0 {
		t.Fatalf("unknown blob refs = %d, want 0", unknownRefs)
	}
}
