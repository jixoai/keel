package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Task and submission rows: the PostgreSQL projection of internal/durable's
// state machine. Skipped when KEEL_TEST_DSN is unset, like the other store
// integration tests.

func taskTestStore(t *testing.T) (*Store, uuid.UUID, uuid.UUID) {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	st, err := Open(context.Background(), Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	tenant := uuid.New()
	session := uuid.New()
	ctx := context.Background()
	if err := st.CreateTenant(ctx, tenant, "durable-store-test"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	return st, tenant, session
}

func TestTasksPersistStateMachineFields(t *testing.T) {
	st, tenant, session := taskTestStore(t)
	ctx := context.Background()
	taskID := uuid.New()
	started := time.Now().Add(-time.Second).UTC().Truncate(time.Millisecond)
	row, err := st.CreateTask(ctx, TaskRow{
		TenantID: tenant, SessionID: session, TaskID: taskID,
		Kind: "pi.generation", Version: 2,
		Input:      json.RawMessage(`{}`),
		Checkpoint: json.RawMessage(`{"phase":"request","attempt":1}`),
		Status:     "running",
		StartedAt:  &started,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.Kind != "pi.generation" || row.Version != 2 || row.Status != "running" || row.Restarts != 0 {
		t.Fatalf("created = %+v", row)
	}
	again, err := st.CreateTask(ctx, TaskRow{
		TenantID: tenant, SessionID: session, TaskID: taskID, Kind: "pi.generation", Status: "pending",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != "running" {
		t.Fatalf("idempotent create overwrote row: %+v", again)
	}
	owner := uuid.New()
	row.Status = "terminal"
	row.Restarts = 3
	row.Outcome = json.RawMessage(`{"status":"completed","result":{"entryId":7}}`)
	row.Owner = &owner
	row.EndedAt = &started
	if err := st.SaveTask(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTask(ctx, tenant, session, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var outcome struct {
		Status string `json:"status"`
		Result struct {
			EntryID int64 `json:"entryId"`
		} `json:"result"`
	}
	if err := json.Unmarshal(got.Outcome, &outcome); err != nil {
		t.Fatalf("outcome json: %v (%s)", err, got.Outcome)
	}
	if got.Status != "terminal" || got.Restarts != 3 || got.Owner == nil || *got.Owner != owner ||
		outcome.Status != "completed" || outcome.Result.EntryID != 7 {
		t.Fatalf("saved = %+v outcome=%s", got, got.Outcome)
	}
	live, err := st.ListTasks(ctx, tenant, session, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("terminal task listed live: %+v", live)
	}
	all, err := st.ListTasks(ctx, tenant, session, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].TaskID != taskID {
		t.Fatalf("all = %+v", all)
	}
	missing := uuid.New()
	if _, err := st.GetTask(ctx, tenant, session, missing); err != ErrTaskNotFound {
		t.Fatalf("missing error = %v", err)
	}
}

func TestSubmissionsDeduplicateByRequestAndPersistSettlement(t *testing.T) {
	st, tenant, session := taskTestStore(t)
	ctx := context.Background()
	subID := uuid.New()
	entry := int64(3)
	created, isNew, err := st.CreateSubmission(ctx, SubmissionRow{
		TenantID: tenant, SessionID: session, SubmissionID: subID,
		RequestID: "r1", Type: "input", Status: "queued", EntryID: &entry,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !isNew || created.SubmissionID != subID || created.Type != "input" || created.Status != "queued" {
		t.Fatalf("created = %+v new=%v", created, isNew)
	}
	dupID := uuid.New()
	dup, dupNew, err := st.CreateSubmission(ctx, SubmissionRow{
		TenantID: tenant, SessionID: session, SubmissionID: dupID,
		RequestID: "r1", Type: "input", Status: "queued",
	})
	if err != nil {
		t.Fatal(err)
	}
	if dupNew || dup.SubmissionID != subID {
		t.Fatalf("duplicate = %+v new=%v", dup, dupNew)
	}
	// The same requestId in another session is an independent submission: the
	// key is per session, exactly like the event log's dedupe scope (M3).
	otherSession := uuid.New()
	if err := st.CreateSession(ctx, tenant, otherSession); err != nil {
		t.Fatal(err)
	}
	otherID := uuid.New()
	other, otherNew, err := st.CreateSubmission(ctx, SubmissionRow{
		TenantID: tenant, SessionID: otherSession, SubmissionID: otherID,
		RequestID: "r1", Type: "input", Status: "queued",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !otherNew || other.SubmissionID != otherID {
		t.Fatalf("cross-session submission = %+v new=%v, want a new row", other, otherNew)
	}
	if stored, err := st.GetSubmissionByRequest(ctx, tenant, otherSession, "r1"); err != nil || stored.SubmissionID != otherID {
		t.Fatalf("other session lookup = %+v, %v", stored, err)
	}
	answer := int64(9)
	dup.Status = "done"
	dup.AnswerID = &answer
	if err := st.SaveSubmission(ctx, dup); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSubmissionByRequest(ctx, tenant, session, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" || got.AnswerID == nil || *got.AnswerID != answer {
		t.Fatalf("settled = %+v", got)
	}
	listed, err := st.ListSubmissions(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].SubmissionID != subID {
		t.Fatalf("listed = %+v", listed)
	}
	if _, err := st.GetSubmission(ctx, tenant, uuid.New()); err != ErrSubmissionNotFound {
		t.Fatalf("missing error = %v", err)
	}
}
