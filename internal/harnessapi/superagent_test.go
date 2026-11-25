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

type fakeSuperagent struct {
	tenant        uuid.UUID
	board         superagent.BoardView
	inbox         superagent.AttentionInbox
	boardErr      error
	inboxErr      error
	reported      superagent.ReportRequest
	answeredActor string
	resumedActor  string
	stoppedActor  string
}

func (f *fakeSuperagent) Board(_ context.Context, tenantID uuid.UUID, _ superagent.BoardFilter) (superagent.BoardView, error) {
	f.tenant = tenantID
	return f.board, f.boardErr
}

func (f *fakeSuperagent) Inbox(_ context.Context, tenantID uuid.UUID) (superagent.AttentionInbox, error) {
	f.tenant = tenantID
	return f.inbox, f.inboxErr
}

func (f *fakeSuperagent) Report(_ context.Context, request superagent.ReportRequest) error {
	f.reported = request
	return nil
}

func (f *fakeSuperagent) Ask(_ context.Context, _, _ uuid.UUID, request collab.AttentionRequest) (collab.Attention, error) {
	return collab.Attention{Prompt: request.Prompt, Status: collab.AttentionOpen}, nil
}

func (f *fakeSuperagent) AnswerAttention(_ context.Context, _, _ uuid.UUID, optionID, text, actor string) (collab.Attention, error) {
	f.answeredActor = actor
	return collab.Attention{Status: collab.AttentionAnswered, Answer: &collab.AttentionAnswer{OptionID: optionID, Text: text, Actor: actor}}, nil
}

func (f *fakeSuperagent) ResumeTicket(_ context.Context, _, _ uuid.UUID, actor, _ string) (superagent.Ticket, error) {
	f.resumedActor = actor
	return superagent.Ticket{}, nil
}

func (f *fakeSuperagent) StopTicket(_ context.Context, _, _ uuid.UUID, actor, _ string) error {
	f.stoppedActor = actor
	return nil
}

func TestSuperagentRoutesFailClosedWithoutSeam(t *testing.T) {
	server := New(Config{})
	req := httptest.NewRequest(http.MethodGet, "/v1/superagent/tickets", nil)
	req.Header.Set("X-Tenant-ID", uuid.New().String())
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestSuperagentRoutesProjectBoardAndInbox(t *testing.T) {
	tenant := uuid.New()
	reader := &fakeSuperagent{
		board: superagent.BoardView{
			TenantID: tenant,
			ModeID:   collab.ModeSuperagent,
			Tickets: []superagent.TicketView{{
				Ticket: superagent.Ticket{WorkItem: collab.WorkItem{ID: uuid.New(), TenantID: tenant, ModeID: collab.ModeSuperagent, Status: collab.WorkItemActive}},
			}},
			Counts: map[string]int{"active": 1},
		},
		inbox: superagent.AttentionInbox{TenantID: tenant, Items: []superagent.AttentionView{
			{Attention: collab.Attention{ID: uuid.New(), TenantID: tenant, Status: collab.AttentionOpen}},
		}},
	}
	server := New(Config{Superagent: reader, Access: &fakeAccess{grants: access.Grants{access.KeyTaskRead: access.ScopeTenant}}})
	for _, path := range []string{"/v1/superagent/tickets", "/v1/superagent/attention"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Tenant-ID", tenant.String())
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", path, rec.Code, rec.Body.String())
		}
		if reader.tenant != tenant {
			t.Fatalf("%s tenant = %s, want %s", path, reader.tenant, tenant)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/superagent/tickets", nil)
	req.Header.Set("X-Tenant-ID", tenant.String())
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	var body superagent.BoardView
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode board: %v", err)
	}
	if len(body.Tickets) != 1 || body.ModeID != collab.ModeSuperagent {
		t.Fatalf("board = %+v", body)
	}
}

func TestSuperagentRoutesRequireTaskRead(t *testing.T) {
	tenant := uuid.New()
	reader := &fakeSuperagent{}
	server := New(Config{
		Superagent: reader,
		Access:     &fakeAccess{},
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/superagent/tickets", nil)
	req.Header.Set("X-Tenant-ID", tenant.String())
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
}

func TestSuperagentWriteRoutesRequireTaskWrite(t *testing.T) {
	tenant := uuid.New()
	ticketID := uuid.New()
	service := &fakeSuperagent{}
	server := New(Config{
		Superagent: service,
		Access:     &fakeAccess{grants: access.Grants{access.KeyTaskWrite: access.ScopeTenant}},
	})
	body := `{"assignmentId":"` + uuid.New().String() + `","sessionId":"` + uuid.New().String() + `","state":"ready","summary":"done"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/superagent/tickets/"+ticketID.String()+"/report", strings.NewReader(body))
	req.Header.Set("X-Tenant-ID", tenant.String())
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("report status = %d: %s", rec.Code, rec.Body.String())
	}
	if service.reported.WorkItemID != ticketID {
		t.Fatalf("reported ticket = %s, want %s", service.reported.WorkItemID, ticketID)
	}

	denied := New(Config{Superagent: service, Access: &fakeAccess{}})
	req = httptest.NewRequest(http.MethodPost, "/v1/superagent/tickets/"+ticketID.String()+"/stop", strings.NewReader(`{}`))
	req.Header.Set("X-Tenant-ID", tenant.String())
	rec = httptest.NewRecorder()
	denied.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("stop without task.write = %d, want 403", rec.Code)
	}
}
