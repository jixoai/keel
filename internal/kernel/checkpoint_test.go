package kernel

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestCheckpointProjection pins the compaction-projection contract on both
// implementations: the row is keyed by the compaction event's seq, a replay of
// the same compaction upserts instead of duplicating, a later refinement fills
// kept_from_seq without clearing a known anchor, and the newest row wins.
func TestCheckpointProjection(t *testing.T) {
	for _, impl := range implementations(t) {
		t.Run(impl.name, func(t *testing.T) {
			h := impl.build(t)
			ctx := context.Background()
			scope := h.newSession()

			if _, found, err := h.k.LoadCheckpoint(ctx, scope); err != nil || found {
				t.Fatalf("empty session checkpoint = (found=%v, err=%v), want (false, nil)", found, err)
			}

			// First compaction: the summary row names the event seq and the
			// retained tail's first event.
			if err := h.k.SaveCheckpoint(ctx, scope, 5, 3, "first"); err != nil {
				t.Fatalf("save checkpoint: %v", err)
			}
			cp, found, err := h.k.LoadCheckpoint(ctx, scope)
			if err != nil || !found {
				t.Fatalf("load checkpoint = (found=%v, err=%v), want found", found, err)
			}
			if cp.UptoSeq != 5 || cp.KeptFromSeq != 3 || cp.Summary != "first" {
				t.Fatalf("checkpoint = %+v, want 5/3/first", cp)
			}

			// A replay of the same compaction (same upto_seq) updates the summary
			// and does not clear the anchor.
			if err := h.k.SaveCheckpoint(ctx, scope, 5, 0, "first-updated"); err != nil {
				t.Fatalf("upsert checkpoint: %v", err)
			}
			cp, found, err = h.k.LoadCheckpoint(ctx, scope)
			if err != nil || !found {
				t.Fatalf("reload checkpoint = (found=%v, err=%v), want found", found, err)
			}
			if cp.UptoSeq != 5 || cp.KeptFromSeq != 3 || cp.Summary != "first-updated" {
				t.Fatalf("upserted checkpoint = %+v, want 5/3/first-updated", cp)
			}

			// A newer compaction becomes the newest row even when an older row is
			// written afterwards.
			if err := h.k.SaveCheckpoint(ctx, scope, 9, 7, "second"); err != nil {
				t.Fatalf("save newer checkpoint: %v", err)
			}
			if err := h.k.SaveCheckpoint(ctx, scope, 4, 2, "older"); err != nil {
				t.Fatalf("save older checkpoint: %v", err)
			}
			cp, found, err = h.k.LoadCheckpoint(ctx, scope)
			if err != nil || !found {
				t.Fatalf("load newest checkpoint = (found=%v, err=%v), want found", found, err)
			}
			if cp.UptoSeq != 9 || cp.KeptFromSeq != 7 || cp.Summary != "second" {
				t.Fatalf("newest checkpoint = %+v, want 9/7/second", cp)
			}
			// An identical replay is still an upsert of an existing row, not a
			// missing session: the guarded PG insert must report one row.
			if err := h.k.SaveCheckpoint(ctx, scope, 9, 7, "second"); err != nil {
				t.Fatalf("identical replay: %v", err)
			}

			// Validation: the checkpoint must name a real compaction seq and a
			// tail inside it.
			for name, req := range map[string]struct {
				scope      Scope
				upto, kept int64
				summary    string
			}{
				"zero scope":       {Scope{}, 5, 3, "s"},
				"no upto seq":      {scope, 0, 0, "s"},
				"negative kept":    {scope, 5, -1, "s"},
				"kept beyond upto": {scope, 5, 6, "s"},
			} {
				if err := h.k.SaveCheckpoint(ctx, req.scope, req.upto, req.kept, req.summary); !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("%s: err = %v, want ErrInvalidRequest", name, err)
				}
			}

			// Tenant boundary: a session that does not exist answers the same
			// ErrNotFound a foreign session does.
			missing := Scope{TenantID: h.tenant, SessionID: uuid.New()}
			if _, _, err := h.k.LoadCheckpoint(ctx, missing); !errors.Is(err, ErrNotFound) {
				t.Fatalf("load missing session checkpoint err = %v, want ErrNotFound", err)
			}
			if err := h.k.SaveCheckpoint(ctx, missing, 1, 0, "foreign"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("save missing session checkpoint err = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestCheckpointTenantIsolation proves the projection carries the same tenant
// boundary as every other kernel read and write: a checkpoint written under one
// session/tenant is invisible to another.
func TestCheckpointTenantIsolation(t *testing.T) {
	for _, impl := range implementations(t) {
		t.Run(impl.name, func(t *testing.T) {
			h := impl.build(t)
			ctx := context.Background()
			owner := h.newSession()
			foreign := h.foreignSession()

			if err := h.k.SaveCheckpoint(ctx, owner, 8, 6, "owner-summary"); err != nil {
				t.Fatalf("save owner checkpoint: %v", err)
			}
			if cp, found, err := h.k.LoadCheckpoint(ctx, foreign); err != nil || found {
				t.Fatalf("foreign checkpoint = (%+v, found=%v, err=%v), want none", cp, found, err)
			}
			cp, found, err := h.k.LoadCheckpoint(ctx, owner)
			if err != nil || !found || cp.Summary != "owner-summary" {
				t.Fatalf("owner checkpoint = (%+v, found=%v, err=%v)", cp, found, err)
			}
		})
	}
}
