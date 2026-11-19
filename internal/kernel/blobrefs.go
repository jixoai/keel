package kernel

// Blob reference projection (change keel-ga §5 blob-lifecycle).
//
// A blob's ref_count is derived state: it counts the committed events that
// still mention the blob. The derivation runs on the *same* transaction that
// appends the event, so an event and its references commit or roll back
// together; a crash can never leave a committed event whose references were
// never recorded (which would let the TTL sweeper collect a live image).
//
// The payload shapes are the durable ones, and they are deliberately walked
// generically instead of per-kind structs:
//
//	user.message      attachments: [{"fileId": "<blob uuid>", ...}]
//	assistant.message content:     [{"type": "imageRef", "blobId": "<blob uuid>"}]
//	tool.result       structuredContent: {"stdout": {"blobId": "<blob uuid>"}, ...}
//
// Scanning any nested "blobId"/"fileId" string keeps the projection working
// when a tool or client adds a nested attachment the transcript decoder does
// not need to know about, while the store still refuses to count anything
// except a complete blob in the same tenant (see Store.AddRefTx).

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BlobRefSink records one blob reference inside the caller's transaction. The
// store implements it (Store.AddRefTx); a MemStore has no durable refs and the
// kernel runner simply does not attach a sink.
type BlobRefSink interface {
	AddRefTx(ctx context.Context, tx pgx.Tx, tenantID, blobID, sessionID uuid.UUID, eventSeq int64) error
}

// referencedBlobIDs returns the distinct, parseable blob ids a committed event
// payload mentions. Order is deterministic (upper/lower UUID string order) so
// concurrent commits touching the same two blobs always take their row locks in
// the same order.
//
// Only the three message kinds can carry blob references; bookkeeping kinds are
// skipped without decoding the payload.
func referencedBlobIDs(kind EventKind, payload json.RawMessage) []uuid.UUID {
	switch kind {
	case KindUserMessage, KindAssistantMessage, KindToolResult:
	default:
		return nil
	}
	if len(payload) == 0 {
		return nil
	}
	var doc any
	if err := json.Unmarshal(payload, &doc); err != nil {
		// The payload was validated for the paths the contract needs. A shape
		// this walk cannot decode simply has no references to project; the event
		// must not fail to commit because a client included junk in a field no
		// consumer reads.
		return nil
	}
	seen := make(map[string]struct{})
	collectBlobIDs(doc, seen)
	if len(seen) == 0 {
		return nil
	}
	out := make([]uuid.UUID, 0, len(seen))
	for raw := range seen {
		id, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil || id == uuid.Nil {
			// A provider-side handle or an arbitrary file id is not a platform
			// blob. Ignoring it is the existing payload semantics: unknown fields
			// are tolerated, and only a store blob can be referenced.
			continue
		}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// collectBlobIDs walks a decoded JSON value. A key hit does not stop the walk:
// the same document may carry references in sibling objects.
func collectBlobIDs(value any, out map[string]struct{}) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if key == "blobId" || key == "fileId" {
				if s, ok := child.(string); ok && strings.TrimSpace(s) != "" {
					out[s] = struct{}{}
					continue
				}
			}
			collectBlobIDs(child, out)
		}
	case []any:
		for _, child := range v {
			collectBlobIDs(child, out)
		}
	}
}
