package harnessapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
)

// fakeSuperagent implements the narrow service the HTTP surface still needs:
// worker report/ask, plus the operators the board bridge calls in-process.
type fakeSuperagent struct {
	reported  superagent.ReportRequest
	reportErr error
	askErr    error
}

func (f *fakeSuperagent) Report(_ context.Context, request superagent.ReportRequest) error {
	f.reported = request
	return f.reportErr
}

func (f *fakeSuperagent) Ask(_ context.Context, _, _ uuid.UUID, request collab.AttentionRequest) (collab.Attention, error) {
	if f.askErr != nil {
		return collab.Attention{}, f.askErr
	}
	return collab.Attention{Prompt: request.Prompt, Status: collab.AttentionOpen}, nil
}

func (f *fakeSuperagent) AnswerAttention(_ context.Context, _, _ uuid.UUID, _, _, _ string) (collab.Attention, error) {
	return collab.Attention{}, nil
}

func (f *fakeSuperagent) ResumeTicket(_ context.Context, _, _ uuid.UUID, _, _ string) (superagent.Ticket, error) {
	return superagent.Ticket{}, nil
}

func (f *fakeSuperagent) StopTicket(_ context.Context, _, _ uuid.UUID, _, _ string) error { return nil }

func superagentServer() *Server {
	return New(Config{
		Superagent: &fakeSuperagent{},
		Access:     &fakeAccess{grants: access.Grants{access.KeyTaskWrite: access.ScopeTenant}},
	})
}

// TestRetiredSuperagentBoardRoutesAreGone pins the destructive change: the old
// board's read projections and operator writes are not mounted. A request to
// any of them is a 404, not an empty projection and not a redirect.
func TestRetiredSuperagentBoardRoutesAreGone(t *testing.T) {
	server := superagentServer()
	tenant := uuid.New()
	ticket := uuid.New()
	attention := uuid.New()
	probes := []struct{ method, path string }{
		{http.MethodGet, "/v1/superagent/tickets"},
		{http.MethodGet, "/v1/superagent/attention"},
		{http.MethodPost, "/v1/superagent/tickets/" + ticket.String() + "/resume"},
		{http.MethodPost, "/v1/superagent/tickets/" + ticket.String() + "/stop"},
		{http.MethodPost, "/v1/superagent/attention/" + attention.String() + "/answer"},
		{http.MethodPost, "/v1/superagent/tickets/" + ticket.String() + "/pending/x/retry"},
		{http.MethodPost, "/v1/superagent/tickets/" + ticket.String() + "/pending/x/dismiss"},
	}
	for _, probe := range probes {
		req := httptest.NewRequest(probe.method, probe.path, strings.NewReader(`{}`))
		req.Header.Set("X-Tenant-ID", tenant.String())
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404: %s", probe.method, probe.path, rec.Code, rec.Body.String())
		}
	}
}

// TestWorkerReportAndAskStillWork keeps the worker seam the supervisor tools
// rely on; these two routes are the only superagent HTTP writes left.
func TestWorkerReportAndAskStillWork(t *testing.T) {
	tenant := uuid.New()
	ticketID := uuid.New()
	service := &fakeSuperagent{}
	server := New(Config{
		Superagent: service,
		Access:     &fakeAccess{grants: access.Grants{access.KeyTaskWrite: access.ScopeTenant}},
	})
	report := `{"assignmentId":"` + uuid.New().String() + `","sessionId":"` + uuid.New().String() + `","state":"ready","summary":"done"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/superagent/tickets/"+ticketID.String()+"/report", strings.NewReader(report))
	req.Header.Set("X-Tenant-ID", tenant.String())
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("report status = %d: %s", rec.Code, rec.Body.String())
	}
	if service.reported.WorkItemID != ticketID {
		t.Fatalf("reported ticket = %s, want %s", service.reported.WorkItemID, ticketID)
	}

	ask := `{"assignmentId":"` + uuid.New().String() + `","sessionId":"` + uuid.New().String() + `","prompt":"which?"}`
	req = httptest.NewRequest(http.MethodPost, "/v1/superagent/tickets/"+ticketID.String()+"/ask", strings.NewReader(ask))
	req.Header.Set("X-Tenant-ID", tenant.String())
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ask status = %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode ask: %v", err)
	}
	if body["attention"] == nil {
		t.Fatalf("ask body = %v", body)
	}
}

func TestWorkerRoutesRequireTaskWrite(t *testing.T) {
	tenant := uuid.New()
	ticketID := uuid.New()
	server := New(Config{Superagent: &fakeSuperagent{}, Access: &fakeAccess{}})
	req := httptest.NewRequest(http.MethodPost, "/v1/superagent/tickets/"+ticketID.String()+"/report", strings.NewReader(`{}`))
	req.Header.Set("X-Tenant-ID", tenant.String())
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("report without task.write = %d, want 403: %s", rec.Code, rec.Body.String())
	}
}
