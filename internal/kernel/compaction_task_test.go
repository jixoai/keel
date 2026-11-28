package kernel

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// TestCompactionTaskCheckpoints pins the in-flight recovery contract on both
// implementations: an entry checkpoint starts at select, a pinned summarize
// checkpoint round-trips its range, a retry checkpoint carries its deadline,
// and deleting a finished task is idempotent.
func TestCompactionTaskCheckpoints(t *testing.T) {
	for _, impl := range implementations(t) {
		t.Run(impl.name, func(t *testing.T) {
			h := impl.build(t)
			ctx := context.Background()
			scope := h.newSession()

			if _, found, err := h.k.LoadCompactionTask(ctx, scope, "manual-1"); err != nil || found {
				t.Fatalf("empty task = (found=%v, err=%v), want (false, nil)", found, err)
			}

			entry := CompactionTaskCheckpoint{
				RequestID:     "manual-1",
				Phase:         CompactionPhaseSelect,
				TokensBefore:  120,
				PolicyVersion: "v1",
			}
			if err := h.k.SaveCompactionTask(ctx, scope, entry); err != nil {
				t.Fatalf("save select checkpoint: %v", err)
			}
			got, found, err := h.k.LoadCompactionTask(ctx, scope, "manual-1")
			if err != nil || !found {
				t.Fatalf("load select checkpoint = (found=%v, err=%v)", found, err)
			}
			if got.Phase != CompactionPhaseSelect || got.TokensBefore != 120 ||
				got.PolicyVersion != "v1" || got.FirstKeptSeq != 0 {
				t.Fatalf("select checkpoint = %+v", got)
			}

			// Selecting a cut pins the range; the same request id upserts.
			pinned := CompactionTaskCheckpoint{
				RequestID:     "manual-1",
				Phase:         CompactionPhaseSummarize,
				FirstKeptSeq:  7,
				TailSeq:       12,
				TokensBefore:  340,
				PolicyVersion: "v1",
			}
			if err := h.k.SaveCompactionTask(ctx, scope, pinned); err != nil {
				t.Fatalf("save summarize checkpoint: %v", err)
			}
			got, found, err = h.k.LoadCompactionTask(ctx, scope, "manual-1")
			if err != nil || !found {
				t.Fatalf("load summarize checkpoint = (found=%v, err=%v)", found, err)
			}
			if got.Phase != CompactionPhaseSummarize || got.FirstKeptSeq != 7 ||
				got.TailSeq != 12 || got.TokensBefore != 340 {
				t.Fatalf("summarize checkpoint = %+v", got)
			}

			// Listing sees every in-flight task of the session.
			if err := h.k.SaveCompactionTask(ctx, scope, CompactionTaskCheckpoint{
				RequestID: "manual-2", Phase: CompactionPhaseSelect,
			}); err != nil {
				t.Fatalf("save second task: %v", err)
			}
			tasks, err := h.k.ListCompactionTasks(ctx, scope)
			if err != nil {
				t.Fatalf("list tasks: %v", err)
			}
			if len(tasks) != 2 {
				t.Fatalf("tasks = %+v, want 2", tasks)
			}

			// Completion is idempotent.
			if err := h.k.DeleteCompactionTask(ctx, scope, "manual-1"); err != nil {
				t.Fatalf("delete task: %v", err)
			}
			if err := h.k.DeleteCompactionTask(ctx, scope, "manual-1"); err != nil {
				t.Fatalf("delete missing task: %v", err)
			}
			if _, found, err := h.k.LoadCompactionTask(ctx, scope, "manual-1"); err != nil || found {
				t.Fatalf("deleted task = (found=%v, err=%v), want (false, nil)", found, err)
			}
		})
	}
}

// TestCompactionTaskValidation pins the structural rules the store and the
// runner rely on: a summarize/retry checkpoint must name a non-empty range, and
// a select checkpoint must not pretend to have one.
func TestCompactionTaskValidation(t *testing.T) {
	for _, impl := range implementations(t) {
		t.Run(impl.name, func(t *testing.T) {
			h := impl.build(t)
			ctx := context.Background()
			scope := h.newSession()

			cases := []struct {
				name       string
				checkpoint CompactionTaskCheckpoint
			}{
				{"missing request id", CompactionTaskCheckpoint{Phase: CompactionPhaseSelect}},
				{"unknown phase", CompactionTaskCheckpoint{RequestID: "x", Phase: "done"}},
				{"select with range", CompactionTaskCheckpoint{RequestID: "x", Phase: CompactionPhaseSelect, FirstKeptSeq: 1, TailSeq: 2}},
				{"summarize without range", CompactionTaskCheckpoint{RequestID: "x", Phase: CompactionPhaseSummarize}},
				{"summarize tail before head", CompactionTaskCheckpoint{RequestID: "x", Phase: CompactionPhaseSummarize, FirstKeptSeq: 5, TailSeq: 4}},
				{"retry without deadline", CompactionTaskCheckpoint{RequestID: "x", Phase: CompactionPhaseRetry, FirstKeptSeq: 2, TailSeq: 3}},
			}
			for _, tc := range cases {
				if err := h.k.SaveCompactionTask(ctx, scope, tc.checkpoint); err == nil {
					t.Fatalf("%s: checkpoint was accepted: %+v", tc.name, tc.checkpoint)
				}
			}

			// A foreign session never sees another tenant's task.
			other := h.foreignSession()
			if err := h.k.SaveCompactionTask(ctx, other, CompactionTaskCheckpoint{
				RequestID: "foreign", Phase: CompactionPhaseSelect,
			}); err != nil {
				t.Fatalf("save foreign task: %v", err)
			}
			if _, found, err := h.k.LoadCompactionTask(ctx, scope, "foreign"); err != nil || found {
				t.Fatalf("foreign task = (found=%v, err=%v), want (false,nil)", found, err)
			}
		})
	}
}

// TestPruneCompactionTasksCapsRows pins the bounded task ledger: the newest N
// rows survive, older abandoned rows are deleted, projections/events are not
// touched, and the missing-session/negative-bound contracts hold.
func TestPruneCompactionTasksCapsRows(t *testing.T) {
	for _, impl := range implementations(t) {
		t.Run(impl.name, func(t *testing.T) {
			h := impl.build(t)
			ctx := context.Background()
			scope := h.newSession()
			for index := 1; index <= 5; index++ {
				if err := h.k.SaveCompactionTask(ctx, scope, CompactionTaskCheckpoint{
					RequestID: fmt.Sprintf("task-%d", index), Phase: CompactionPhaseSelect,
				}); err != nil {
					t.Fatalf("save task %d: %v", index, err)
				}
			}
			pruned, err := h.k.PruneCompactionTasks(ctx, scope, CompactionTaskPruneRequest{MaxTasks: 2})
			if err != nil || pruned != 3 {
				t.Fatalf("prune = (%d, %v), want 3", pruned, err)
			}
			tasks, err := h.k.ListCompactionTasks(ctx, scope)
			if err != nil || len(tasks) != 2 {
				t.Fatalf("tasks after prune = (%+v, %v), want 2", tasks, err)
			}
			kept := map[string]bool{}
			for _, task := range tasks {
				kept[task.RequestID] = true
			}
			if !kept["task-4"] || !kept["task-5"] {
				t.Fatalf("kept = %v, want the newest task-4/task-5", kept)
			}
			missing := Scope{TenantID: h.tenant, SessionID: uuid.New()}
			if _, err := h.k.PruneCompactionTasks(ctx, missing, CompactionTaskPruneRequest{MaxTasks: 1}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing session prune = %v, want ErrNotFound", err)
			}
			if _, err := h.k.PruneCompactionTasks(ctx, scope, CompactionTaskPruneRequest{MaxTasks: -1}); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("negative bound = %v, want ErrInvalidRequest", err)
			}
		})
	}
}
