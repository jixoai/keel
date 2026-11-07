package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

// inputSetup provisions a tenant and a session and returns their ids.
func inputSetup(t *testing.T, hs *httptest.Server, tenant string) uuid.UUID {
	t.Helper()
	hc := hs.Client()
	if code := createTestTenant(t, hc, hs.URL, tenant, "input-test"); code != 201 {
		t.Fatalf("create tenant: %d", code)
	}
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions", tenant, map[string]string{}, &sess); code != 201 {
		t.Fatalf("create session: %d", code)
	}
	return uuid.MustParse(sess.SessionID)
}

// TestInputResponseReportsCommittedStatus: the response reports the state the
// bookkeeping holds after the commit, not the transient "pending" the row was
// inserted with.
func TestInputResponseReportsCommittedStatus(t *testing.T) {
	hs, _ := newTestServer(t)
	hc := hs.Client()
	tenant := uuid.NewString()
	session := inputSetup(t, hs, tenant)

	var first struct {
		Status  string `json:"status"`
		Seq     *int64 `json:"seq"`
		Created bool   `json:"created"`
		Error   string `json:"error"`
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+session.String()+"/input", tenant,
		map[string]string{"text": "hello", "requestId": "status-1"}, &first); code != 201 {
		t.Fatalf("input: %d (body %+v)", code, first)
	}
	if first.Status != "committed" {
		t.Fatalf("response status = %q, want committed", first.Status)
	}
	if first.Seq == nil || *first.Seq < 1 {
		t.Fatalf("response seq = %v, want the committed sequence", first.Seq)
	}

	var replay struct {
		Status  string `json:"status"`
		Seq     *int64 `json:"seq"`
		Created bool   `json:"created"`
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+session.String()+"/input", tenant,
		map[string]string{"text": "hello", "requestId": "status-1"}, &replay); code != 200 {
		t.Fatalf("replay: %d", code)
	}
	if replay.Status != "committed" || replay.Created || replay.Seq == nil || *replay.Seq != *first.Seq {
		t.Fatalf("replay = %+v, want the same committed receipt at %d", replay, *first.Seq)
	}
}

// TestInputRepairsBookkeepingAfterCrash covers the M1 shape: a commit that
// reached the log while the HTTP-layer bookkeeping did not (a crash between the
// two). Two things must hold:
//
//   - the commit itself queued the run, so the input cannot be stranded by a
//     bookkeeping failure;
//   - the client's retry repairs the submissions row without appending a second
//     event, and a conflicting retry (same key, different text) is refused with
//     409 and appends nothing.
func TestInputRepairsBookkeepingAfterCrash(t *testing.T) {
	hs, st := newTestServer(t)
	hc := hs.Client()
	tenant := uuid.NewString()
	session := inputSetup(t, hs, tenant)
	tenantID := uuid.MustParse(tenant)
	ctx := context.Background()

	// Simulate the crash: the edge commit ran (and queued the run), the
	// submissions row was never written.
	kernelLog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	kern := kernel.New(kernelLog, kernel.Options{})
	requestID := "repair-1"
	payload, _ := json.Marshal(map[string]any{"text": "hello", "requestId": requestID})
	receipt, err := kern.Commit(ctx, kernel.CommitRequest{
		Scope:     kernel.Scope{TenantID: tenantID, SessionID: session},
		Kind:      kernel.KindUserMessage,
		RequestID: requestID,
		Payload:   payload,
	})
	if err != nil {
		t.Fatalf("edge commit: %v", err)
	}

	// The commit alone queued the run: no HTTP bookkeeping was involved.
	run, err := st.GetRun(ctx, tenantID, session)
	if err != nil {
		t.Fatalf("run after the commit: %v", err)
	}
	if run.Status != store.RunPending {
		t.Fatalf("run status = %s, want pending (the commit queues it)", run.Status)
	}

	// The client retries: the bookkeeping is repaired, no second event.
	var sub struct {
		Status  string `json:"status"`
		Seq     *int64 `json:"seq"`
		Created bool   `json:"created"`
		Error   string `json:"error"`
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+session.String()+"/input", tenant,
		map[string]string{"text": "hello", "requestId": requestID}, &sub); code != 201 {
		t.Fatalf("repairing retry: %d (body %+v)", code, sub)
	}
	if sub.Status != "committed" || sub.Seq == nil || *sub.Seq != receipt.Seq {
		t.Fatalf("repair response = %+v, want committed at %d", sub, receipt.Seq)
	}
	var (
		eventCount int
		status     string
		seq        *int64
	)
	if err := st.Pool().QueryRow(ctx,
		`SELECT count(*) FROM events WHERE tenant_id = $1 AND session_id = $2`,
		tenantID, session).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("events = %d, want exactly 1 (the retry must not append)", eventCount)
	}
	if err := st.Pool().QueryRow(ctx,
		`SELECT status, seq FROM submissions WHERE tenant_id = $1 AND request_id = $2`,
		tenantID, requestID).Scan(&status, &seq); err != nil {
		t.Fatalf("submissions row: %v", err)
	}
	if status != "committed" || seq == nil || *seq != receipt.Seq {
		t.Fatalf("submission row = %s/%v, want committed at %d", status, seq, receipt.Seq)
	}

	// A conflicting retry is a client bug: 409 and nothing appended.
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+session.String()+"/input", tenant,
		map[string]string{"text": "different text", "requestId": requestID}, nil); code != http.StatusConflict {
		t.Fatalf("conflicting retry: %d, want 409", code)
	}
	if err := st.Pool().QueryRow(ctx,
		`SELECT count(*) FROM events WHERE tenant_id = $1 AND session_id = $2`,
		tenantID, session).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("events after the conflict = %d, want 1", eventCount)
	}
}
