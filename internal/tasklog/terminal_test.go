package tasklog_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/tasklog"
)

// TestTerminalCardIsReadOnly is C1's transition matrix: the legal transitions
// (open -> in_progress -> blocked -> closed) pass, every mutation of the closed
// card is refused with ErrCardClosed, and the ledger stays unchanged. Close
// stays idempotent because the history already carries the fact.
func TestTerminalCardIsReadOnly(t *testing.T) {
	st, _, service, scope := taskLogHarness(t)
	ctx := context.Background()

	card := mustCard(t)(service.Create(ctx, scope, tasklog.CreateInput{Title: "发布清单"}, "create"))
	if _, err := service.Update(ctx, scope, card.TaskID,
		tasklog.UpdateInput{Status: stringPtr(tasklog.StatusInProgress)}, "u1"); err != nil {
		t.Fatalf("open -> in_progress: %v", err)
	}
	if _, err := service.Update(ctx, scope, card.TaskID,
		tasklog.UpdateInput{Status: stringPtr(tasklog.StatusBlocked)}, "u2"); err != nil {
		t.Fatalf("in_progress -> blocked: %v", err)
	}
	closed := mustCard(t)(service.Close(ctx, scope, card.TaskID, "done", "c1"))
	if closed.Status != tasklog.StatusClosed {
		t.Fatalf("closed card status = %q, want %q", closed.Status, tasklog.StatusClosed)
	}

	before := countSessionEvents(t, st, scope)
	for _, probe := range []struct {
		name string
		run  func() error
	}{
		{"update", func() error {
			_, err := service.Update(ctx, scope, card.TaskID,
				tasklog.UpdateInput{Title: stringPtr("after close")}, "u3")
			return err
		}},
		{"attach", func() error {
			_, err := service.Attach(ctx, scope, card.TaskID, "a.txt", "sha256:x", "a1")
			return err
		}},
		{"detach", func() error {
			_, err := service.Detach(ctx, scope, card.TaskID, "a.txt", "sha256:x", "d1")
			return err
		}},
		{"note", func() error {
			_, err := service.Note(ctx, scope, card.TaskID, "late note", "n1")
			return err
		}},
	} {
		if err := probe.run(); !errors.Is(err, tasklog.ErrCardClosed) {
			t.Fatalf("%s on a closed card = %v, want ErrCardClosed", probe.name, err)
		}
	}
	if after := countSessionEvents(t, st, scope); after != before {
		t.Fatalf("a refused mutation appended events: %d -> %d", before, after)
	}

	// Close is idempotent: the history already carries the fact.
	again := mustCard(t)(service.Close(ctx, scope, card.TaskID, "done", "c1"))
	if again.Status != tasklog.StatusClosed {
		t.Fatalf("second close status = %q", again.Status)
	}

	// A card of another session is not reachable through this scope.
	otherSession := uuid.New()
	if err := st.CreateSession(ctx, scope.TenantID, otherSession); err != nil {
		t.Fatalf("create second session: %v", err)
	}
	other := kernel.Scope{TenantID: scope.TenantID, SessionID: otherSession}
	if _, err := service.Update(ctx, other, card.TaskID,
		tasklog.UpdateInput{Title: stringPtr("foreign")}, "u4"); err == nil {
		t.Fatal("a foreign session updated another session's card")
	}
}

func countSessionEvents(t *testing.T, st *store.Store, scope kernel.Scope) int {
	t.Helper()
	events, err := st.LoadEvents(context.Background(), scope.TenantID, scope.SessionID, 0, 2000)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	return len(events)
}
