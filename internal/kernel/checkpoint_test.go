package kernel

import (
	"context"
	"errors"
	"testing"
	"time"

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
			if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{UptoSeq: 5, KeptFromSeq: 3, Summary: "first"}); err != nil {
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
			if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{UptoSeq: 5, Summary: "first-updated"}); err != nil {
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
			if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{UptoSeq: 9, KeptFromSeq: 7, Summary: "second"}); err != nil {
				t.Fatalf("save newer checkpoint: %v", err)
			}
			if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{UptoSeq: 4, KeptFromSeq: 2, Summary: "older"}); err != nil {
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
			if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{UptoSeq: 9, KeptFromSeq: 7, Summary: "second"}); err != nil {
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
				"kept equals upto": {scope, 5, 5, "s"},
			} {
				if err := h.k.SaveCheckpoint(ctx, req.scope, CheckpointRecord{UptoSeq: req.upto, KeptFromSeq: req.kept, Summary: req.summary}); !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("%s: err = %v, want ErrInvalidRequest", name, err)
				}
			}

			// Tenant boundary: a session that does not exist answers the same
			// ErrNotFound a foreign session does.
			missing := Scope{TenantID: h.tenant, SessionID: uuid.New()}
			if _, _, err := h.k.LoadCheckpoint(ctx, missing); !errors.Is(err, ErrNotFound) {
				t.Fatalf("load missing session checkpoint err = %v, want ErrNotFound", err)
			}
			if err := h.k.SaveCheckpoint(ctx, missing, CheckpointRecord{UptoSeq: 1, Summary: "foreign"}); !errors.Is(err, ErrNotFound) {
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

			if err := h.k.SaveCheckpoint(ctx, owner, CheckpointRecord{UptoSeq: 8, KeptFromSeq: 6, Summary: "owner-summary"}); err != nil {
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

// TestCheckpointRecordRoundTrip pins the durable fields a checkpoint must carry
// (tokens, policy identity, manual requestId) and the requestId idempotency.
func TestCheckpointRecordRoundTrip(t *testing.T) {
	for _, impl := range implementations(t) {
		t.Run(impl.name, func(t *testing.T) {
			h := impl.build(t)
			ctx := context.Background()
			scope := h.newSession()

			rec := CheckpointRecord{
				UptoSeq: 5, KeptFromSeq: 3, Summary: "s",
				TokensBefore: 9000, TokensAfter: 1500,
				PolicyVersion: "cp1-abc", RequestID: "req-1",
			}
			if err := h.k.SaveCheckpoint(ctx, scope, rec); err != nil {
				t.Fatalf("save checkpoint: %v", err)
			}
			cp, found, err := h.k.LoadCheckpoint(ctx, scope)
			if err != nil || !found {
				t.Fatalf("load checkpoint = (found=%v, err=%v)", found, err)
			}
			if cp.TokensBefore != 9000 || cp.TokensAfter != 1500 || cp.PolicyVersion != "cp1-abc" || cp.RequestID != "req-1" {
				t.Fatalf("checkpoint = %+v, want the recorded accounting", cp)
			}
			byRequest, found, err := h.k.CheckpointByRequest(ctx, scope, "req-1")
			if err != nil || !found || byRequest.UptoSeq != 5 {
				t.Fatalf("by request = (%+v, found=%v, err=%v)", byRequest, found, err)
			}
			if _, found, err := h.k.CheckpointByRequest(ctx, scope, "missing"); err != nil || found {
				t.Fatalf("missing request = (found=%v, err=%v), want none", found, err)
			}
			// The same requestId may not produce a second checkpoint at another seq.
			if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{UptoSeq: 9, KeptFromSeq: 7, Summary: "other", RequestID: "req-1"}); err == nil {
				t.Fatal("a reused requestId for a different seq must fail")
			}
			// Empty summary/tokens validation.
			if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{UptoSeq: 10, Summary: "  "}); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("empty summary err = %v, want ErrInvalidRequest", err)
			}
			if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{UptoSeq: 10, Summary: "s", TokensBefore: -1}); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("negative tokens err = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

// TestPruneCheckpointsKeepsNewest pins the retention contract: count and TTL
// pruning delete projections only, and the newest projection always survives so
// the session keeps a recovery anchor.
func TestPruneCheckpointsKeepsNewest(t *testing.T) {
	for _, impl := range implementations(t) {
		t.Run(impl.name, func(t *testing.T) {
			h := impl.build(t)
			ctx := context.Background()
			scope := h.newSession()

			for seq := int64(3); seq <= 9; seq += 3 {
				if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{
					UptoSeq: seq, KeptFromSeq: seq - 1, Summary: "s", TokensBefore: 100, TokensAfter: 10,
				}); err != nil {
					t.Fatalf("save checkpoint %d: %v", seq, err)
				}
			}
			// Count pruning keeps the two newest.
			pruned, err := h.k.PruneCheckpoints(ctx, scope, PruneRequest{MaxCheckpoints: 2})
			if err != nil {
				t.Fatalf("prune by count: %v", err)
			}
			if pruned != 1 {
				t.Fatalf("pruned = %d, want 1", pruned)
			}
			cp, found, err := h.k.LoadCheckpoint(ctx, scope)
			if err != nil || !found || cp.UptoSeq != 9 {
				t.Fatalf("newest after count prune = (%+v, found=%v, err=%v), want seq 9", cp, found, err)
			}

			// TTL pruning never deletes the newest, even when it is older than the
			// TTL. Age every row past the TTL first.
			age := time.Now().Add(-48 * time.Hour)
			if h.mem != nil {
				h.clock.advanceTo(time.Now().Add(48 * time.Hour))
			} else {
				if _, err := h.st.Pool().Exec(ctx,
					`UPDATE checkpoints SET created_at = $3 WHERE tenant_id = $1 AND session_id = $2`,
					scope.TenantID, scope.SessionID, age); err != nil {
					t.Fatalf("age checkpoints: %v", err)
				}
			}
			pruned, err = h.k.PruneCheckpoints(ctx, scope, PruneRequest{TTL: time.Hour})
			if err != nil {
				t.Fatalf("prune by ttl: %v", err)
			}
			if pruned != 1 {
				t.Fatalf("pruned by ttl = %d, want 1 (only the older row)", pruned)
			}
			cp, found, err = h.k.LoadCheckpoint(ctx, scope)
			if err != nil || !found || cp.UptoSeq != 9 {
				t.Fatalf("newest after ttl prune = (%+v, found=%v, err=%v), want seq 9", cp, found, err)
			}

			// Validation: negative bounds are rejected, no-op is a no-op.
			if _, err := h.k.PruneCheckpoints(ctx, scope, PruneRequest{MaxCheckpoints: -1}); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("negative max err = %v, want ErrInvalidRequest", err)
			}
			if pruned, err := h.k.PruneCheckpoints(ctx, scope, PruneRequest{}); err != nil || pruned != 0 {
				t.Fatalf("no-op prune = (%d, %v), want (0, nil)", pruned, err)
			}
			// A foreign/missing session answers ErrNotFound.
			missing := Scope{TenantID: h.tenant, SessionID: uuid.New()}
			if _, err := h.k.PruneCheckpoints(ctx, missing, PruneRequest{MaxCheckpoints: 1}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing session prune err = %v, want ErrNotFound", err)
			}
		})
	}
}
