// Package sessionfork is the durable fork-origin fact of one session.
//
// A forked session inherits its parent's ledger prefix up to OriginSeq and its
// file view from TreeHash. The fact is stored on the child session row (the
// only place the kernel read path, the file plugin and the agent runner all
// already resolve a session from), so the three readers agree by construction
// and no component copies ledger events, manifest rows or bytes.
//
// The package is dependency-free on purpose: store, fileplugin and agent each
// consume the same struct without importing one another.
package sessionfork

import (
	"context"

	"github.com/google/uuid"
)

// Fact is the fork origin of one child session.
type Fact struct {
	// ParentSessionID is the session the fork started from. Same tenant as the
	// child: a fork never crosses tenants.
	ParentSessionID uuid.UUID
	// OriginSeq is the parent ledger sequence the fork started at. The child
	// inherits the parent's committed events with seq <= OriginSeq.
	OriginSeq int64
	// TreeHash is the parent's file-view root at OriginSeq (a layerfs tree
	// object). Empty means the parent had no captured files at the origin.
	TreeHash string
	// ConfigHash and ToolManifestHash are the parent attempt's run snapshot
	// values at the origin (empty when the parent never ran). They let the
	// child answer "which configuration did I inherit" without replaying the
	// parent log.
	ConfigHash       string
	ToolManifestHash string
	// AnchorSeq is the parent run snapshot's anchor eventSeq (0 when absent).
	AnchorSeq int64
}

// Valid reports whether the fact names a parent and a non-negative origin.
func (f Fact) Valid() bool {
	return f.ParentSessionID != uuid.Nil && f.OriginSeq >= 0
}

// Reader resolves the fork origin of a session. found=false means the session
// is not a fork.
type Reader interface {
	SessionFork(ctx context.Context, tenantID, sessionID uuid.UUID) (Fact, bool, error)
}

// Writer records the fork origin of a session. It is called once, before the
// child's first input, and is idempotent for the same fact.
type Writer interface {
	RecordSessionFork(ctx context.Context, tenantID, sessionID uuid.UUID, fact Fact) error
}
