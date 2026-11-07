package handoff

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
)

// TestNormalHandoff covers the spec's 新 runtime 接班协议: new runtime ready,
// old runtime drains, releases the lease, the new runtime claims with epoch+1
// and rebuilds from the log.
func TestNormalHandoff(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()
			a := h.readyRuntime(t, "runtime-a", 1)
			leaseA := h.claim(a)
			h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindUserMessage,
				RequestID: "r1", Payload: userPayload("hello")})
			// A real attempt records its load point before answering.
			h.recordRead(leaseA, 1)
			h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
				Token: leaseA.Token, GenerationID: "g1", Payload: assistantText("hi")})
			cursorA := h.lastSeq()

			// The old runtime enters a handoff drain.
			status, err := a.BeginDrain(ctx, ModeHandoff, drainWindowTime())
			if err != nil {
				t.Fatalf("begin drain: %v", err)
			}
			if !status.Draining || status.Mode != ModeHandoff || status.Claiming {
				t.Fatalf("drain status = %+v, want draining handoff with claim gate closed", status)
			}
			if !a.AdmitsEdgeWrites() {
				t.Fatal("handoff drain must keep accepting edge-scoped input")
			}
			// ... and releases the lease after draining.
			if err := a.Release(ctx, leaseA); err != nil {
				t.Fatalf("release: %v", err)
			}

			// The new runtime takes over.
			b := h.readyRuntime(t, "runtime-b", 2)
			adoption := h.adopt(b, 2*time.Second)
			if adoption.Lease.Token.Owner != "runtime-b" {
				t.Fatalf("adopted owner = %q", adoption.Lease.Token.Owner)
			}
			if adoption.Lease.Token.Epoch != leaseA.Token.Epoch+1 {
				t.Fatalf("epoch = %d, want %d+1", adoption.Lease.Token.Epoch, leaseA.Token.Epoch)
			}
			if adoption.FirstClaim || adoption.Previous != leaseA.Token {
				t.Fatalf("previous token = %+v (first=%t), want %+v", adoption.Previous, adoption.FirstClaim, leaseA.Token)
			}
			if adoption.Rebuild.Cursor != cursorA || adoption.Rebuild.Events != 2 {
				t.Fatalf("rebuild = %s, want cursor %d and 2 events", adoption.Rebuild, cursorA)
			}
			if len(adoption.Rebuild.Unresolved) != 0 || adoption.Rebuild.PendingUserInput {
				t.Fatalf("rebuild = %s, want a clean continuation point", adoption.Rebuild)
			}
			if adoption.HandoffID == "" {
				t.Fatal("adoption recorded no handoff audit row")
			}

			// The adoption audit answers the spec's query.
			row := h.handoffRow(adoption.HandoffID)
			if row.FromRuntime != "runtime-a" || row.ToRuntime != "runtime-b" {
				t.Fatalf("audit runtimes = %s -> %s", row.FromRuntime, row.ToRuntime)
			}
			if row.FromEpoch != leaseA.Token.Epoch || row.ToEpoch != adoption.Lease.Token.Epoch {
				t.Fatalf("audit epochs = %d -> %d", row.FromEpoch, row.ToEpoch)
			}
			if row.Revision != 2 || row.LoadoutID != "keel-default" {
				t.Fatalf("audit loadout = %s@%d", row.LoadoutID, row.Revision)
			}
			if row.Status != HandoffCompleted || row.StartedAt.IsZero() || row.FinishedAt.IsZero() {
				t.Fatalf("audit row = %+v", row)
			}
			if row.SessionID != h.session || row.TenantID != h.tenant {
				t.Fatalf("audit scope = %s/%s", row.TenantID, row.SessionID)
			}

			// The run pin records who owns the run at which loadout revision.
			pin, err := h.k.RunPin(ctx, h.scope)
			if err != nil {
				t.Fatal(err)
			}
			if pin.Owner != "runtime-b" || pin.Epoch != adoption.Lease.Token.Epoch {
				t.Fatalf("run pin = %s", pin)
			}
			if pin.LoadoutRevision != 2 || pin.LoadoutID != "keel-default" {
				t.Fatalf("run pin loadout = %s@%d, want keel-default@2", pin.LoadoutID, pin.LoadoutRevision)
			}

			// The drain closes out: runtime row stopped, drain audit completed.
			if err := a.WaitDrain(ctx); err != nil {
				t.Fatalf("wait drain: %v", err)
			}
			aRow, err := h.reg.Runtime(ctx, "runtime-a")
			if err != nil {
				t.Fatal(err)
			}
			if aRow.Status != RuntimeStopped || aRow.Mode != ModeHandoff || aRow.DrainAt.IsZero() {
				t.Fatalf("old runtime row = %+v", aRow)
			}
			drains, err := h.reg.ListHandoffs(ctx, HandoffQuery{FromRuntime: "runtime-a"})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, d := range drains {
				if d.SessionID == uuid.Nil && d.Status == HandoffCompleted && d.Mode == ModeHandoff {
					found = true
				}
			}
			if !found {
				t.Fatalf("runtime-level drain audit row missing in %+v", drains)
			}

			// The successor finishes the run; the pin survives the settle, so
			// the audit answers "which revision did this run complete on".
			h.finish(adoption.Lease, cursorA, nil)
			if got := h.runStatus(); got != kernel.RunStatusDone {
				h.assertRunConverges(got)
				t.Fatalf("run status = %s, want done", got)
			}
			pin, err = h.k.RunPin(ctx, h.scope)
			if err != nil {
				t.Fatal(err)
			}
			if pin.LoadoutRevision != 2 {
				t.Fatalf("run pin after settle = %s, want the completing revision", pin)
			}
			h.assertContiguous()
		})
	}
}

// TestStaleWriterFencedAfterHandoff covers the fencing requirement: a drained
// runtime cannot commit owner-scoped facts with its old epoch, the rejection is
// audited, and the client-visible log keeps exactly one final reply.
func TestStaleWriterFencedAfterHandoff(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()
			a := h.readyRuntime(t, "runtime-a", 1)
			leaseA := h.claim(a)
			h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindUserMessage,
				RequestID: "r1", Payload: userPayload("hello")})
			h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
				Token: leaseA.Token, GenerationID: "g1", Payload: assistantToolCall("tc1", "echo")})

			// Handoff: A releases, B claims epoch+1.
			if _, err := a.BeginDrain(ctx, ModeHandoff, drainWindowTime()); err != nil {
				t.Fatal(err)
			}
			if err := a.Release(ctx, leaseA); err != nil {
				t.Fatal(err)
			}
			b := h.readyRuntime(t, "runtime-b", 2)
			adoption := h.adopt(b, 2*time.Second)

			// The stale writer tries to commit late facts.
			lateAssistant := h.commitErr(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
				Token: leaseA.Token, GenerationID: "g2", Payload: assistantText("late")})
			h.mustIso(lateAssistant, kernel.ErrStaleEpoch, "late assistant commit")
			lateResult := h.commitErr(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindToolResult,
				Token: leaseA.Token, ToolCallID: "tc1", Payload: toolResultPayload("tc1", "echo", "late")})
			h.mustIso(lateResult, kernel.ErrStaleEpoch, "late tool result commit")

			violations := h.staleViolations()
			if len(violations) == 0 {
				t.Fatal("stale writer rejection was not audited")
			}
			if violations[0].Token.Owner != "runtime-a" {
				t.Fatalf("audited stale token = %+v, want runtime-a's", violations[0].Token)
			}
			if h.countKind(kernel.KindAssistantMessage) != 1 {
				t.Fatalf("late assistant reached the log: %d assistant events", h.countKind(kernel.KindAssistantMessage))
			}
			if h.countKind(kernel.KindToolResult) != 0 {
				t.Fatal("late tool result reached the log")
			}

			// The new owner continues and produces the single final reply.
			h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindToolResult,
				Token: adoption.Lease.Token, ToolCallID: "tc1", Payload: toolResultPayload("tc1", "echo", "ok")})
			h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
				Token: adoption.Lease.Token, GenerationID: "g2", Payload: assistantText("final")})
			if n := h.countKind(kernel.KindAssistantMessage); n != 2 {
				t.Fatalf("assistant events = %d, want exactly 2 (tool call + final)", n)
			}
			if n := h.countKind(kernel.KindToolResult); n != 1 {
				t.Fatalf("tool results = %d, want exactly 1", n)
			}
			h.finish(adoption.Lease, h.lastSeq(), nil)
			if got := h.runStatus(); got != kernel.RunStatusDone {
				t.Fatalf("run status = %s, want done", got)
			}
			h.assertContiguous()
		})
	}
}

// TestDrainModesAreDistinct covers the two drain modes: shutdown rejects new
// writes, handoff keeps accepting edge-scoped input, both are distinguishable
// in the audit, and handoff does not reuse shutdown's reject-input branch.
func TestDrainModesAreDistinct(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()

			// Shutdown: the claim gate closes and edge writes are refused.
			shutdown := h.readyRuntime(t, "runtime-shutdown", 1)
			if _, err := shutdown.BeginDrain(ctx, ModeShutdown, drainWindowTime()); err != nil {
				t.Fatal(err)
			}
			if shutdown.AdmitsEdgeWrites() {
				t.Fatal("shutdown drain must not admit edge writes")
			}
			if shutdown.DrainMode().AdmitsEdgeWrites() {
				t.Fatal("shutdown mode must not admit edge writes")
			}

			// Handoff: the claim gate closes but the edge path stays open, and
			// the mode is carried in the audit.
			handoffRuntime := h.readyRuntime(t, "runtime-handoff", 2)
			if _, err := handoffRuntime.BeginDrain(ctx, ModeHandoff, drainWindowTime()); err != nil {
				t.Fatal(err)
			}
			if !handoffRuntime.AdmitsEdgeWrites() {
				t.Fatal("handoff drain must admit edge writes")
			}
			sRow, err := h.reg.Runtime(ctx, "runtime-shutdown")
			if err != nil {
				t.Fatal(err)
			}
			hRow, err := h.reg.Runtime(ctx, "runtime-handoff")
			if err != nil {
				t.Fatal(err)
			}
			if sRow.Mode != ModeShutdown || hRow.Mode != ModeHandoff {
				t.Fatalf("audit modes = %q / %q, want shutdown / handoff", sRow.Mode, hRow.Mode)
			}
			if sRow.Status != RuntimeDraining || hRow.Status != RuntimeDraining {
				t.Fatalf("audit statuses = %q / %q", sRow.Status, hRow.Status)
			}

			// Input keeps landing during the handoff drain: accepted, deduped,
			// and visible to the successor.
			edge := h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindUserMessage,
				RequestID: "drain-r1", Payload: userPayload("still here")})
			if edge.Deduped {
				t.Fatalf("first input during drain reported as replay: %+v", edge)
			}
			replay := h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindUserMessage,
				RequestID: "drain-r1", Payload: userPayload("still here")})
			if !replay.Deduped || replay.Seq != edge.Seq {
				t.Fatalf("replayed input = %+v, want deduped at seq %d", replay, edge.Seq)
			}
			if edge.Seq != h.lastSeq() {
				t.Fatalf("input did not land at the log tail: %d vs %d", edge.Seq, h.lastSeq())
			}

			// The successor picks the input up (nothing was dropped).
			b := h.readyRuntime(t, "runtime-b", 3)
			adoption := h.adopt(b, 2*time.Second)
			if !adoption.Rebuild.PendingUserInput {
				t.Fatalf("successor rebuild = %s, want pending user input", adoption.Rebuild)
			}
			last := h.events()
			if kernel.EventText(last[len(last)-1]) != "still here" {
				t.Fatalf("dropped input: last event = %s", last[len(last)-1].Payload)
			}
			h.finish(adoption.Lease, adoption.Rebuild.Cursor, nil)
			if got := h.runStatus(); got != kernel.RunStatusDone {
				t.Fatalf("run status = %s, want done", got)
			}
		})
	}
}

// TestDrainTimeoutForcesTakeover covers the drain deadline: an old runtime that
// does not drain is recorded as timed out, its lease expires, the successor
// force-takes-over with epoch+1 and the stale owner's commits stay rejected.
func TestDrainTimeoutForcedTakeover(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()
			a := h.readyRuntime(t, "runtime-a", 1)
			leaseA := h.claimShort(a)
			h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindUserMessage,
				RequestID: "r1", Payload: userPayload("hello")})

			// A run is in flight and never finishes: the drain times out.
			a.RunStarted()
			if _, err := a.BeginDrain(ctx, ModeHandoff, 100*time.Millisecond); err != nil {
				t.Fatal(err)
			}
			err := a.WaitDrain(ctx)
			if !errors.Is(err, ErrDrainTimeout) {
				t.Fatalf("wait drain = %v, want ErrDrainTimeout", err)
			}
			if !a.TimedOut() {
				t.Fatal("coordinator did not record the timeout")
			}
			rows, err := h.reg.ListHandoffs(ctx, HandoffQuery{FromRuntime: "runtime-a"})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) == 0 || rows[0].Status != HandoffTimedOut {
				t.Fatalf("drain audit = %+v, want a timed-out row", rows)
			}
			if !strings.Contains(rows[0].Detail, "in flight") {
				t.Fatalf("timeout detail = %q, want the in-flight reason", rows[0].Detail)
			}

			// The lease expires; the successor force-takes-over.
			h.expire(leaseA)
			b := h.readyRuntime(t, "runtime-b", 2)
			adoption := h.adopt(b, 3*time.Second)
			if adoption.Lease.Token.Epoch != leaseA.Token.Epoch+1 {
				t.Fatalf("takeover epoch = %d, want %d+1", adoption.Lease.Token.Epoch, leaseA.Token.Epoch)
			}
			if err := h.commitErr(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
				Token: leaseA.Token, GenerationID: "g-late", Payload: assistantText("late")}); !errors.Is(err, kernel.ErrStaleEpoch) && !errors.Is(err, kernel.ErrLeaseExpired) {
				t.Fatalf("stale commit after takeover = %v, want fencing rejection", err)
			}
			if h.countKind(kernel.KindAssistantMessage) != 0 {
				t.Fatal("stale commit reached the log")
			}
			h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
				Token: adoption.Lease.Token, GenerationID: "g-final", Payload: assistantText("final")})
			h.finish(adoption.Lease, h.lastSeq(), nil)
			if got := h.runStatus(); got != kernel.RunStatusDone {
				h.assertRunConverges(got)
				t.Fatalf("run status = %s, want done", got)
			}
			a.RunFinished()
			h.assertContiguous()
		})
	}
}
