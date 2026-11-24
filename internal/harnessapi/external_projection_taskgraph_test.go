//go:build !windows

package harnessapi

// The task-graph non-pollution proof for the external-run projection (change
// keel-oar-renderer, task 3.1): an external session that already owns a task
// card runs one fake OAR turn (the real sidecar + oar.Bridge journal, the same
// path the host uses), and the task graph must not move:
//
//   - task_cards rows, task_events rows and task.created counts stay identical;
//   - no ledger row is written by the record round;
//   - the projection's taskId is the original card (external calls are
//     attributed to the card that started them, never a second card);
//   - the projection read itself writes nothing.
//
// Attribution is out-of-band metadata, so the open can name the deterministic
// initial card id (tasklog.InitialTaskID) before the row exists; the row is then
// created on the same external session and the record round echoes that id.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/oar"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/tasklog"
)

type taskGraphCounts struct {
	Cards             int64
	TaskEvents        int64
	TaskCreated       int64
	LedgerTaskCreated int64
	LedgerEvents      int64
}

func readTaskGraphCounts(t *testing.T, st *store.Store, tenant, session uuid.UUID) taskGraphCounts {
	t.Helper()
	var counts taskGraphCounts
	err := st.Pool().QueryRow(context.Background(), `
		SELECT
		  (SELECT count(*) FROM task_cards  WHERE tenant_id = $1 AND session_id = $2),
		  (SELECT count(*) FROM task_events WHERE tenant_id = $1 AND session_id = $2),
		  (SELECT count(*) FROM task_events WHERE tenant_id = $1 AND session_id = $2 AND kind = 'task.created'),
		  (SELECT count(*) FROM events      WHERE tenant_id = $1 AND session_id = $2 AND kind = 'task.created'),
		  (SELECT count(*) FROM events      WHERE tenant_id = $1 AND session_id = $2)`,
		tenant, session).Scan(&counts.Cards, &counts.TaskEvents, &counts.TaskCreated, &counts.LedgerTaskCreated, &counts.LedgerEvents)
	if err != nil {
		t.Fatalf("task graph counts: %v", err)
	}
	return counts
}

func (c taskGraphCounts) String() string {
	return fmt.Sprintf("task_cards=%d task_events=%d task_events.task.created=%d events.task.created=%d events.total=%d",
		c.Cards, c.TaskEvents, c.TaskCreated, c.LedgerTaskCreated, c.LedgerEvents)
}

// projectionFinished reports that the fake turn's closing status record has
// been folded (its state is the last completed status), so the assertion reads
// a whole turn rather than a prefix of one.
func projectionFinished(projection oar.Projection) bool {
	if len(projection.Items) == 0 {
		return false
	}
	last := projection.Items[len(projection.Items)-1]
	return last.Kind == oar.KindStatus && last.State == oar.StatusCompleted
}

// buildOARSidecar builds the real keel-oar executable for this test run.
func buildOARSidecar(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	bin := filepath.Join(t.TempDir(), "keel-oar")
	build := exec.Command("go", "build", "-o", bin, "./internal/oar/cmd/keel-oar")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build OAR sidecar: %v\n%s", err, output)
	}
	return bin
}

func TestExternalProjectionDoesNotPolluteTaskGraph(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "oar-graph-"+tenant.String()[:8]); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	sessionID := uuid.New()
	cardID := tasklog.InitialTaskID(tenant, sessionID)

	journalDir := t.TempDir()
	service, err := oar.NewService(oar.Config{
		Runtime:     oar.RuntimeFake,
		SidecarPath: buildOARSidecar(t),
		JournalDir:  journalDir,
		Stderr:      io.Discard,
	})
	if err != nil {
		t.Fatalf("new OAR service: %v", err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatalf("start OAR service: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := service.Stop(stopCtx); err != nil {
			t.Errorf("stop OAR service: %v", err)
		}
	})

	// The external call is attributed to the session's initial card out of band.
	backendSession, err := service.Open(ctx, oar.OpenRequest{
		Reference:     "oar-taskgraph-1",
		HostSessionID: sessionID.String(),
		TaskID:        cardID.String(),
	})
	if err != nil {
		t.Fatalf("open OAR session: %v", err)
	}
	// The external session row binds the backend handle the route resolves the
	// journal from, then the session's initial card is created on it.
	if _, err := st.CreateExternalSession(ctx, tenant, sessionID, store.ExternalMarks{
		Backend: "oar",
		Ref:     backendSession.Ref,
	}); err != nil {
		t.Fatalf("create external session: %v", err)
	}
	kernelLog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatalf("kernel store: %v", err)
	}
	kern := kernel.New(kernelLog, kernel.Options{})
	tasks := tasklog.New(tasklog.Options{
		Committer:  kern,
		Reader:     kern,
		Projection: tasklog.NewPGProjection(st),
		Now:        func() time.Time { return time.Now().UTC() },
	})
	scope := kernel.Scope{TenantID: tenant, SessionID: sessionID}
	card, err := tasks.EnsureCard(ctx, scope)
	if err != nil {
		t.Fatalf("ensure session card: %v", err)
	}
	if card.TaskID != cardID {
		t.Fatalf("initial card %s != predicted %s", card.TaskID, cardID)
	}

	before := readTaskGraphCounts(t, st, tenant, sessionID)
	if before.Cards != 1 || before.TaskCreated != 1 || before.LedgerTaskCreated != 1 {
		t.Fatalf("fixture did not start with exactly one card: %s", before)
	}

	if _, err := service.Send(ctx, backendSession.Ref, json.RawMessage(`{"text":"外部工具检查任务图"}`)); err != nil {
		t.Fatalf("send OAR turn: %v", err)
	}
	journalPath := service.JournalPath(backendSession.Ref)
	deadline := time.Now().Add(15 * time.Second)
	var projection oar.Projection
	for {
		projection, err = oar.ProjectionFromJournal(journalPath)
		if err == nil && projection.TaskID == card.TaskID.String() && projectionFinished(projection) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("journal %s never carried the attributed records: projection=%+v err=%v", journalPath, projection, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	afterRun := readTaskGraphCounts(t, st, tenant, sessionID)
	if afterRun != before {
		t.Fatalf("the fake OAR record round moved the task graph:\n before: %s\n  after: %s", before, afterRun)
	}

	server := newExternalProjectionServer(t, Config{ExternalSessions: st, OARJournalDir: journalDir})
	response, body := getExternalProjection(t, server, tenant, sessionID.String())
	transcriptExternalProjection(t, "task-graph (200)", tenant, sessionID.String(), response, body)
	if response.StatusCode != 200 {
		t.Fatalf("projection status = %d, body = %s", response.StatusCode, body)
	}
	var wire oar.Projection
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("projection body: %v", err)
	}
	if wire.TaskID != card.TaskID.String() {
		t.Fatalf("projection taskId = %q, want the original card %q", wire.TaskID, card.TaskID)
	}
	if wire.HostSessionID != sessionID.String() || wire.BackendSessionID != backendSession.Ref || !wire.Simulated {
		t.Fatalf("projection identity = %+v", wire)
	}
	var cardStillThere bool
	if err := st.Pool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM task_cards WHERE tenant_id = $1 AND session_id = $2 AND task_id = $3)`,
		tenant, sessionID, card.TaskID).Scan(&cardStillThere); err != nil {
		t.Fatalf("card existence: %v", err)
	}
	if !cardStillThere {
		t.Fatalf("the original card %s disappeared", card.TaskID)
	}
	afterRead := readTaskGraphCounts(t, st, tenant, sessionID)
	if afterRead != before {
		t.Fatalf("the projection read moved the task graph:\n before: %s\n  after: %s", before, afterRead)
	}

	record := fmt.Sprintf("session=%s card=%s backendRef=%s\nbefore: %s\nafter fake OAR turn: %s\nafter projection read: %s\nprojection.taskId=%s projection.items=%d projection.simulated=%v\n",
		sessionID, card.TaskID, backendSession.Ref, before, afterRun, afterRead, wire.TaskID, len(wire.Items), wire.Simulated)
	t.Logf("\n%s", record)
	appendExternalProjectionEvidence(t, "task-graph-before-after.txt", record)
}

// appendExternalProjectionEvidence appends one block when KEEL_EVIDENCE_DIR is
// set (the evidence run points it at the change's evidence directory).
func appendExternalProjectionEvidence(t *testing.T, name, block string) {
	t.Helper()
	dir := os.Getenv("KEEL_EVIDENCE_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create evidence dir: %v", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open evidence file: %v", err)
	}
	defer file.Close()
	if _, err := file.WriteString(block + "\n"); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
}
