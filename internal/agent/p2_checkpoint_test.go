package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

// TestCorruptCheckpointSummaryHash pins the recovery integrity checks: a row
// whose summary no longer matches its recorded hash, or a compaction event whose
// hash disagrees with the row, must fall back to the full event log.
func TestCorruptCheckpointSummaryHash(t *testing.T) {
	const summary = "checkpoint summary"
	events := []store.Event{
		{Seq: 8, Kind: store.EventUserMessage},
		{Seq: 9, Kind: store.EventCompaction, Payload: []byte(`{"summary":"checkpoint summary","summaryHash":"` + kernel.CheckpointSummaryHash(summary) + `"}`)},
	}
	base := kernel.Checkpoint{
		UptoSeq: 9, KeptFromSeq: 8, Summary: summary,
		SummaryHash: kernel.CheckpointSummaryHash(summary),
	}
	if reason := corruptCheckpointReason(base, events); reason != "" {
		t.Fatalf("matching hash = %q, want valid", reason)
	}
	badRow := base
	badRow.SummaryHash = "deadbeef"
	if reason := corruptCheckpointReason(badRow, events); !strings.Contains(reason, "summary hash") {
		t.Fatalf("row hash mismatch = %q, want a summary-hash refusal", reason)
	}
	badEvent := append([]store.Event(nil), events...)
	badEvent[1].Payload = []byte(`{"summaryHash":"deadbeef"}`)
	if reason := corruptCheckpointReason(base, badEvent); !strings.Contains(reason, "compaction event") {
		t.Fatalf("event hash mismatch = %q, want a compaction-event refusal", reason)
	}
	// A pre-hash row/event carries no integrity fact; it is structurally
	// validated but not pretended to be verified.
	legacy := base
	legacy.SummaryHash = ""
	legacyEvents := append([]store.Event(nil), events...)
	legacyEvents[1].Payload = []byte(`{"summary":"checkpoint summary"}`)
	if reason := corruptCheckpointReason(legacy, legacyEvents); reason != "" {
		t.Fatalf("legacy hash-less checkpoint = %q, want valid", reason)
	}
}

// TestCompactionCarriesSummaryHash pins the two independent records of the
// same fact: the automatic compaction event and its checkpoint projection agree
// on sha256(summary).
func TestCompactionCarriesSummaryHash(t *testing.T) {
	st := testStore(t)
	tenant, session, _, cp, _ := seedCompaction(t, st)
	if cp.SummaryHash == "" || cp.SummaryHash != kernel.CheckpointSummaryHash(cp.Summary) {
		t.Fatalf("checkpoint summary hash = %q, want sha256(summary)", cp.SummaryHash)
	}
	eventHash := compactionEventSummaryHash(t, eventsOf(t, st, tenant, session), cp.UptoSeq)
	if eventHash != cp.SummaryHash {
		t.Fatalf("event summary hash = %q, checkpoint hash = %q", eventHash, cp.SummaryHash)
	}
}

// TestManualCompactionCarriesSummaryHash is the manual half of the same
// contract.
func TestManualCompactionCarriesSummaryHash(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session, _, scope := seedManualHistory(t, st)
	runner := manualRunner(t, st)
	lease := claimScope(t, testKernel(t, st), tenant, session)
	res, err := runner.CompactNow(ctx, lease, "hash-manual")
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	cp, found, err := runner.Checkpoints.(kernel.CheckpointRequestLookup).CheckpointByRequest(ctx, scope, "hash-manual")
	if err != nil || !found {
		t.Fatalf("checkpoint by request = (found=%v, err=%v)", found, err)
	}
	eventHash := compactionEventSummaryHash(t, eventsOf(t, st, tenant, session), res.CheckpointID)
	if eventHash == "" || eventHash != cp.SummaryHash {
		t.Fatalf("manual event hash = %q, checkpoint hash = %q", eventHash, cp.SummaryHash)
	}
}

// compactionEventSummaryHash reads the event's summaryHash for one seq.
func compactionEventSummaryHash(t *testing.T, events []store.Event, seq int64) string {
	t.Helper()
	for _, ev := range events {
		if ev.Seq != seq || ev.Kind != store.EventCompaction {
			continue
		}
		var payload struct {
			SummaryHash string `json:"summaryHash"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("decode compaction event: %v", err)
		}
		return payload.SummaryHash
	}
	t.Fatalf("no compaction event at seq %d", seq)
	return ""
}

// TestRunLazilyPrunesExpiredCheckpoints pins TTL cleanup on the read path: a
// session that never produced a new checkpoint still loses its expired older
// projections at the next run, while the newest remains the recovery anchor.
func TestRunLazilyPrunesExpiredCheckpoints(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session, k, newest, _ := seedCompaction(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}
	if newest.UptoSeq <= 2 {
		t.Skip("fixture has no room for an older anchored checkpoint")
	}
	if err := k.SaveCheckpoint(ctx, scope, kernel.CheckpointRecord{
		UptoSeq: newest.UptoSeq - 1, KeptFromSeq: 1, Summary: "older projection",
		TokensBefore: 500, TokensAfter: 50, PolicyVersion: "cp1-old",
	}); err != nil {
		t.Fatalf("save older checkpoint: %v", err)
	}
	if n := checkpointRows(t, st, tenant, session); n != 2 {
		t.Fatalf("checkpoint rows = %d, want 2", n)
	}
	// Age both rows past the policy TTL. The newest stays protected regardless.
	if _, err := st.Pool().Exec(ctx,
		`UPDATE checkpoints SET created_at = now() - interval '48 hours'
		  WHERE tenant_id = $1 AND session_id = $2`, tenant, session); err != nil {
		t.Fatalf("age checkpoints: %v", err)
	}
	runner, _ := newTestRunner(t, st)
	policy := compaction.CheckpointPolicy{Enabled: false, ManualCompact: true, TTL: time.Hour}
	runner.Compaction = &CompactionOptions{
		ContextWindow: 100000,
		Settings:      compaction.CompactionSettings{Enabled: false},
		Policy:        &policy,
	}
	commitInput(t, k, scope, "lazy-prune", "small follow-up", "", nil)
	requeue(t, st, tenant, session)
	if _, err := execRun(t, k, runner, tenant, session); err != nil {
		t.Fatalf("lazy-prune run: %v", err)
	}
	if n := checkpointRows(t, st, tenant, session); n != 1 {
		t.Fatalf("checkpoint rows after lazy TTL prune = %d, want 1 (newest kept)", n)
	}
	cp, found, err := k.LoadCheckpoint(ctx, scope)
	if err != nil || !found || cp.UptoSeq != newest.UptoSeq {
		t.Fatalf("newest after lazy prune = (%+v, found=%v, err=%v), want seq %d", cp, found, err, newest.UptoSeq)
	}
	// Pruning is projection-only: the compaction event is still readable.
	if got := countKind(eventsOf(t, st, tenant, session), store.EventCompaction); got == 0 {
		t.Fatal("lazy prune deleted a compaction event")
	}
}
