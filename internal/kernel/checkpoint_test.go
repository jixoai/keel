package kernel

import (
	"context"
	"errors"
	"fmt"
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
			// Two different requests may select the same cut: both requestIds
			// resolve to it, and the projection keeps one row. Memory and PG
			// must agree on this (mem parity).
			if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{
				UptoSeq: 5, KeptFromSeq: 3, Summary: "s2", RequestID: "req-2",
			}); err != nil {
				t.Fatalf("a second requestId for the same seq: %v", err)
			}
			for _, requestID := range []string{"req-1", "req-2"} {
				byRequest, found, err := h.k.CheckpointByRequest(ctx, scope, requestID)
				if err != nil || !found || byRequest.UptoSeq != 5 {
					t.Fatalf("request %q = (%+v, found=%v, err=%v), want seq 5", requestID, byRequest, found, err)
				}
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

// TestPruneCheckpointRequestsBoundsLedger pins the independent retention of the
// manual-compaction idempotency ledger: a row cap keeps the newest requestIds,
// a TTL expires older ones, projections are never touched, and a missing
// session answers ErrNotFound like every other kernel call.
func TestPruneCheckpointRequestsBoundsLedger(t *testing.T) {
	for _, impl := range implementations(t) {
		t.Run(impl.name, func(t *testing.T) {
			h := impl.build(t)
			ctx := context.Background()
			scope := h.newSession()
			for seq := int64(1); seq <= 5; seq++ {
				if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{
					UptoSeq: seq, KeptFromSeq: seq - 1, Summary: "summary",
					RequestID: fmt.Sprintf("req-%d", seq),
				}); err != nil {
					t.Fatalf("save request %d: %v", seq, err)
				}
			}
			pruned, err := h.k.PruneCheckpointRequests(ctx, scope, CheckpointRequestPruneRequest{MaxRequests: 2})
			if err != nil || pruned != 3 {
				t.Fatalf("row-cap prune = (%d, %v), want 3", pruned, err)
			}
			for seq := int64(1); seq <= 3; seq++ {
				if _, found, err := h.k.CheckpointByRequest(ctx, scope, fmt.Sprintf("req-%d", seq)); err != nil || found {
					t.Fatalf("req-%d after cap = (found=%v, err=%v), want pruned", seq, found, err)
				}
			}
			for seq := int64(4); seq <= 5; seq++ {
				if cp, found, err := h.k.CheckpointByRequest(ctx, scope, fmt.Sprintf("req-%d", seq)); err != nil || !found || cp.UptoSeq != seq {
					t.Fatalf("req-%d after cap = (%+v, found=%v, err=%v)", seq, cp, found, err)
				}
			}
			// The projection is a separate fact and was not pruned.
			if cp, found, err := h.k.LoadCheckpoint(ctx, scope); err != nil || !found || cp.UptoSeq != 5 {
				t.Fatalf("projection after request prune = (%+v, found=%v, err=%v)", cp, found, err)
			}

			// Add two more requests, then age all rows past the TTL.
			for seq := int64(6); seq <= 7; seq++ {
				if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{
					UptoSeq: seq, KeptFromSeq: seq - 1, Summary: "summary",
					RequestID: fmt.Sprintf("req-%d", seq),
				}); err != nil {
					t.Fatalf("save request %d: %v", seq, err)
				}
			}
			if h.mem != nil {
				h.clock.advanceTo(h.clock.Now().Add(48 * time.Hour))
			} else if _, err := h.st.Pool().Exec(ctx,
				`UPDATE checkpoint_requests SET created_at = now() - interval '10 days'
				  WHERE tenant_id = $1 AND session_id = $2`, scope.TenantID, scope.SessionID); err != nil {
				t.Fatalf("age request rows: %v", err)
			}
			pruned, err = h.k.PruneCheckpointRequests(ctx, scope, CheckpointRequestPruneRequest{TTL: 24 * time.Hour})
			if err != nil || pruned != 4 {
				t.Fatalf("TTL prune = (%d, %v), want the 4 remaining rows", pruned, err)
			}
			if _, found, err := h.k.CheckpointByRequest(ctx, scope, "req-7"); err != nil || found {
				t.Fatalf("expired request = (found=%v, err=%v), want pruned", found, err)
			}
			if cp, found, err := h.k.LoadCheckpoint(ctx, scope); err != nil || !found || cp.UptoSeq != 7 {
				t.Fatalf("projection after TTL = (%+v, found=%v, err=%v), want seq 7 intact", cp, found, err)
			}

			missing := Scope{TenantID: h.tenant, SessionID: uuid.New()}
			if _, err := h.k.PruneCheckpointRequests(ctx, missing, CheckpointRequestPruneRequest{MaxRequests: 1}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing session prune = %v, want ErrNotFound", err)
			}
			if _, err := h.k.PruneCheckpointRequests(ctx, scope, CheckpointRequestPruneRequest{MaxRequests: -1}); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("negative bound = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

// TestCheckpointRequestCreatedAtIsFirstSeen pins the unified retention-age
// semantics (P2-8): re-saving the same requestId/seq must keep the first-seen
// created_at in both stores, so mem and PG age the row identically.
func TestCheckpointRequestCreatedAtIsFirstSeen(t *testing.T) {
	for _, impl := range implementations(t) {
		t.Run(impl.name, func(t *testing.T) {
			h := impl.build(t)
			ctx := context.Background()
			scope := h.newSession()
			record := CheckpointRecord{UptoSeq: 5, KeptFromSeq: 3, Summary: "s", RequestID: "req-age"}
			if err := h.k.SaveCheckpoint(ctx, scope, record); err != nil {
				t.Fatalf("save: %v", err)
			}
			first, found, err := h.k.CheckpointByRequest(ctx, scope, "req-age")
			if err != nil || !found {
				t.Fatalf("first read = (found=%v, err=%v)", found, err)
			}
			if h.mem != nil {
				h.clock.advanceTo(h.clock.Now().Add(48 * time.Hour))
			}
			record.Summary = "s-refined"
			if err := h.k.SaveCheckpoint(ctx, scope, record); err != nil {
				t.Fatalf("re-save: %v", err)
			}
			again, found, err := h.k.CheckpointByRequest(ctx, scope, "req-age")
			if err != nil || !found {
				t.Fatalf("second read = (found=%v, err=%v)", found, err)
			}
			if !again.CreatedAt.Equal(first.CreatedAt) {
				t.Fatalf("created_at changed on re-save: %s -> %s (want first-seen)", first.CreatedAt, again.CreatedAt)
			}
		})
	}
}

// TestMissingSessionParity pins the shared missing-session contract across the
// memory and PostgreSQL stores: every checkpoint/task read and write answers
// ErrNotFound instead of a silent empty result (which a caller could mistake
// for "no row yet").
func TestMissingSessionParity(t *testing.T) {
	for _, impl := range implementations(t) {
		t.Run(impl.name, func(t *testing.T) {
			h := impl.build(t)
			ctx := context.Background()
			missing := Scope{TenantID: h.tenant, SessionID: uuid.New()}

			if err := h.k.SaveCheckpoint(ctx, missing, CheckpointRecord{
				UptoSeq: 2, KeptFromSeq: 1, Summary: "s",
			}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("SaveCheckpoint missing session = %v, want ErrNotFound", err)
			}
			if _, _, err := h.k.LoadCheckpoint(ctx, missing); !errors.Is(err, ErrNotFound) {
				t.Fatalf("LoadCheckpoint missing session = %v, want ErrNotFound", err)
			}
			if _, _, err := h.k.CheckpointByRequest(ctx, missing, "req"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("CheckpointByRequest missing session = %v, want ErrNotFound", err)
			}
			if _, err := h.k.PruneCheckpoints(ctx, missing, PruneRequest{MaxCheckpoints: 1}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("PruneCheckpoints missing session = %v, want ErrNotFound", err)
			}
			if err := h.k.SaveCompactionTask(ctx, missing, CompactionTaskCheckpoint{
				RequestID: "task", Phase: CompactionPhaseSelect,
			}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("SaveCompactionTask missing session = %v, want ErrNotFound", err)
			}
			if _, _, err := h.k.LoadCompactionTask(ctx, missing, "task"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("LoadCompactionTask missing session = %v, want ErrNotFound", err)
			}
			if _, err := h.k.ListCompactionTasks(ctx, missing); !errors.Is(err, ErrNotFound) {
				t.Fatalf("ListCompactionTasks missing session = %v, want ErrNotFound", err)
			}
			if err := h.k.DeleteCompactionTask(ctx, missing, "task"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("DeleteCompactionTask missing session = %v, want ErrNotFound", err)
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

// TestCheckpointSkipsUnanchoredRows pins the recovery rule: an unanchored
// projection cannot stand in for a prefix, so LoadCheckpoint serves the newest
// anchored row; pruning deletes unanchored rows and keeps the newest anchored.
func TestCheckpointSkipsUnanchoredRows(t *testing.T) {
	for _, impl := range implementations(t) {
		t.Run(impl.name, func(t *testing.T) {
			h := impl.build(t)
			ctx := context.Background()
			scope := h.newSession()
			if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{UptoSeq: 5, KeptFromSeq: 3, Summary: "anchored"}); err != nil {
				t.Fatal(err)
			}
			if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{UptoSeq: 9, Summary: "unanchored"}); err != nil {
				t.Fatal(err)
			}
			cp, found, err := h.k.LoadCheckpoint(ctx, scope)
			if err != nil || !found || cp.UptoSeq != 5 {
				t.Fatalf("load = (%+v, found=%v, err=%v), want anchored seq 5", cp, found, err)
			}
			if pruned, err := h.k.PruneCheckpoints(ctx, scope, PruneRequest{MaxCheckpoints: 1}); err != nil || pruned != 1 {
				t.Fatalf("prune = (%d, %v), want the unanchored row removed", pruned, err)
			}
			cp, found, err = h.k.LoadCheckpoint(ctx, scope)
			if err != nil || !found || cp.UptoSeq != 5 {
				t.Fatalf("load after prune = (%+v, found=%v, err=%v)", cp, found, err)
			}
		})
	}
}

// TestCheckpointSummaryHash pins the integrity fact: every save records
// sha256(summary), a mismatched caller-supplied hash is refused, and the load
// path returns the recorded hash.
func TestCheckpointSummaryHash(t *testing.T) {
	for _, impl := range implementations(t) {
		t.Run(impl.name, func(t *testing.T) {
			h := impl.build(t)
			ctx := context.Background()
			scope := h.newSession()

			summary := "the durable summary"
			if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{
				UptoSeq: 5, KeptFromSeq: 3, Summary: summary,
			}); err != nil {
				t.Fatalf("save checkpoint: %v", err)
			}
			cp, found, err := h.k.LoadCheckpoint(ctx, scope)
			if err != nil || !found {
				t.Fatalf("load checkpoint = (found=%v, err=%v)", found, err)
			}
			want := CheckpointSummaryHash(summary)
			if cp.SummaryHash != want {
				t.Fatalf("summary hash = %q, want %q", cp.SummaryHash, want)
			}
			// A caller-supplied hash that does not describe the summary is
			// refused before anything is written.
			if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{
				UptoSeq: 6, KeptFromSeq: 4, Summary: "different", SummaryHash: want,
			}); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("mismatched hash err = %v, want ErrInvalidRequest", err)
			}
			if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{
				UptoSeq: 6, KeptFromSeq: 4, Summary: summary, SummaryHash: want,
			}); err != nil {
				t.Fatalf("matching supplied hash: %v", err)
			}
		})
	}
}

// TestCheckpointRequestSurvivesPrune pins the manual idempotency contract:
// pruning the projection must not forget the requestId or its result.
func TestCheckpointRequestSurvivesPrune(t *testing.T) {
	for _, impl := range implementations(t) {
		t.Run(impl.name, func(t *testing.T) {
			h := impl.build(t)
			ctx := context.Background()
			scope := h.newSession()

			manual := CheckpointRecord{
				UptoSeq: 5, KeptFromSeq: 3, Summary: "manual summary",
				TokensBefore: 9000, TokensAfter: 1500, PolicyVersion: "cp1-manual", RequestID: "req-pruned",
			}
			if err := h.k.SaveCheckpoint(ctx, scope, manual); err != nil {
				t.Fatalf("save manual checkpoint: %v", err)
			}
			if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{
				UptoSeq: 9, KeptFromSeq: 7, Summary: "newest",
				TokensBefore: 8000, TokensAfter: 1200, PolicyVersion: "cp1-newest",
			}); err != nil {
				t.Fatalf("save newest checkpoint: %v", err)
			}
			pruned, err := h.k.PruneCheckpoints(ctx, scope, PruneRequest{MaxCheckpoints: 1})
			if err != nil || pruned != 1 {
				t.Fatalf("prune = (%d, %v), want the manual projection removed", pruned, err)
			}
			cp, found, err := h.k.LoadCheckpoint(ctx, scope)
			if err != nil || !found || cp.UptoSeq != 9 {
				t.Fatalf("newest after prune = (%+v, found=%v, err=%v), want 9", cp, found, err)
			}
			replay, found, err := h.k.CheckpointByRequest(ctx, scope, "req-pruned")
			if err != nil || !found {
				t.Fatalf("request after prune = (found=%v, err=%v), want the ledger row", found, err)
			}
			if replay.UptoSeq != 5 || replay.TokensBefore != 9000 || replay.TokensAfter != 1500 ||
				replay.PolicyVersion != "cp1-manual" || replay.SummaryHash != CheckpointSummaryHash("manual summary") {
				t.Fatalf("replayed checkpoint = %+v, want the original manual result", replay)
			}
			// The same requestId still cannot point at a second seq.
			if err := h.k.SaveCheckpoint(ctx, scope, CheckpointRecord{
				UptoSeq: 11, KeptFromSeq: 10, Summary: "third", RequestID: "req-pruned",
			}); err == nil {
				t.Fatal("a reused requestId for a different seq must fail after pruning")
			}
		})
	}
}
