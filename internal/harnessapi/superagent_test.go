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
	answerErr     error
	retryErr      error
	dismissErr    error
	retriedID     string
	dismissedID   string
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
	attention := collab.Attention{Status: collab.AttentionAnswered, Answer: &collab.AttentionAnswer{OptionID: optionID, Text: text, Actor: actor}}
	return attention, f.answerErr
}

func (f *fakeSuperagent) RetryPending(_ context.Context, _, _ uuid.UUID, pendingID, _ string) (superagent.Ticket, error) {
	f.retriedID = pendingID
	return superagent.Ticket{}, f.retryErr
}

func (f *fakeSuperagent) DismissPending(_ context.Context, _, _ uuid.UUID, pendingID, _ string) (superagent.Ticket, error) {
	f.dismissedID = pendingID
	return superagent.Ticket{}, f.dismissErr
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

// TestSuperagentAnswerDeliveryStates pins the operator answer status codes: a
// recorded answer is 200; a recorded but undelivered answer is 202 with
// deliveryPending; a permanently failed delivery is an explicit 409 (never a
// 400); an unwired delivery seam is 503. The attention travels with the pending
// answer so a client can show it immediately.
func TestSuperagentAnswerDeliveryStates(t *testing.T) {
	tenant := uuid.New()
	attentionID := uuid.New()
	cases := []struct {
		name    string
		err     error
		status  int
		code    string
		pending bool
	}{
		{name: "delivered", status: http.StatusOK},
		{name: "pending", err: superagent.ErrAnswerDeliveryPending, status: http.StatusAccepted, pending: true},
		{name: "failed", err: superagent.ErrAnswerDeliveryFailed, status: http.StatusConflict, code: "answer_delivery_failed"},
		{name: "not delivered", err: superagent.ErrAnswerNotDelivered, status: http.StatusConflict, code: "answer_not_delivered"},
		{name: "unwired", err: superagent.ErrPendingNotDeliverable, status: http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := &fakeSuperagent{answerErr: tc.err}
			server := New(Config{Superagent: service, Access: &fakeAccess{grants: access.Grants{access.KeyTaskWrite: access.ScopeTenant}}})
			req := httptest.NewRequest(http.MethodPost, "/v1/superagent/attention/"+attentionID.String()+"/answer", strings.NewReader(`{"optionId":"approve"}`))
			req.Header.Set("X-Tenant-ID", tenant.String())
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			var payload map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if tc.pending {
				if payload["deliveryPending"] != true {
					t.Fatalf("deliveryPending = %v, want true", payload["deliveryPending"])
				}
				attention, ok := payload["attention"].(map[string]any)
				if !ok || attention["status"] != string(collab.AttentionAnswered) {
					t.Fatalf("attention = %v", payload["attention"])
				}
			}
			if tc.code != "" && payload["code"] != tc.code {
				t.Fatalf("code = %v, want %q", payload["code"], tc.code)
			}
			if tc.status == http.StatusConflict {
				if attention, ok := payload["attention"].(map[string]any); !ok || attention["status"] != string(collab.AttentionAnswered) {
					t.Fatalf("failed answer must carry the recorded attention: %v", payload["attention"])
				}
			}
		})
	}
}

// TestSuperagentPendingRecoveryRoutes pins the two operator decisions on a
// failed pending input: retry and dismiss reach the service with the exact
// pending id, under the same task.write gate.
func TestSuperagentPendingRecoveryRoutes(t *testing.T) {
	tenant := uuid.New()
	ticketID := uuid.New()
	service := &fakeSuperagent{}
	server := New(Config{Superagent: service, Access: &fakeAccess{grants: access.Grants{access.KeyTaskWrite: access.ScopeTenant}}})
	pendingID := "attention:b7c1d2e3-f4a5-4b6c-9d7e-8f90a1b2c301"
	for _, action := range []string{"retry", "dismiss"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/superagent/tickets/"+ticketID.String()+"/pending/"+pendingID+"/"+action, strings.NewReader(`{}`))
		req.Header.Set("X-Tenant-ID", tenant.String())
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", action, rec.Code, rec.Body.String())
		}
	}
	if service.retriedID != pendingID || service.dismissedID != pendingID {
		t.Fatalf("retried=%q dismissed=%q, want %q", service.retriedID, service.dismissedID, pendingID)
	}
	denied := New(Config{Superagent: service, Access: &fakeAccess{}})
	req := httptest.NewRequest(http.MethodPost, "/v1/superagent/tickets/"+ticketID.String()+"/pending/"+pendingID+"/retry", strings.NewReader(`{}`))
	req.Header.Set("X-Tenant-ID", tenant.String())
	rec := httptest.NewRecorder()
	denied.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("retry without task.write = %d, want 403", rec.Code)
	}
}

// TestSuperagentBotPrincipalCannotUseTheOperatorAPI pins the identity boundary:
// a Bot principal (a worker Bot key or any delegation) never reaches the
// product API, even when its narrowed grants include task.read/task.write. The
// model tools are the Bot's only write seam.
func TestSuperagentBotPrincipalCannotUseTheOperatorAPI(t *testing.T) {
	tenant := uuid.New()
	service := &fakeSuperagent{}
	server := New(Config{Superagent: service, Access: &fakeAccess{
		principal: access.Principal{TenantID: tenant, AccountID: uuid.New(), BotID: "worker"},
		grants: access.Grants{
			access.KeyTaskRead:  access.ScopeTenant,
			access.KeyTaskWrite: access.ScopeTenant,
		},
	}})
	ticketID := uuid.New()
	probes := []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/v1/superagent/tickets"},
		{method: http.MethodGet, path: "/v1/superagent/attention"},
		{method: http.MethodPost, path: "/v1/superagent/tickets/" + ticketID.String() + "/stop", body: "{}"},
		{method: http.MethodPost, path: "/v1/superagent/attention/" + uuid.New().String() + "/answer", body: `{"optionId":"approve"}`},
	}
	for _, probe := range probes {
		var reader *strings.Reader
		if probe.body == "" {
			reader = strings.NewReader("")
		} else {
			reader = strings.NewReader(probe.body)
		}
		req := httptest.NewRequest(probe.method, probe.path, reader)
		req.Header.Set("X-Tenant-ID", tenant.String())
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s = %d, want 403: %s", probe.method, probe.path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Bot principals") {
			t.Fatalf("%s %s body = %s", probe.method, probe.path, rec.Body.String())
		}
	}
}

// TestSuperagentClosedTicketRetryIs409: a retry on a stopped/terminal ticket is
// an explicit conflict, never a 400 and never a delivery. Dismiss remains 200
// because it is the settlement action for a terminal pending row.
func TestSuperagentClosedTicketRetryIs409(t *testing.T) {
	tenant := uuid.New()
	ticketID := uuid.New()
	service := &fakeSuperagent{retryErr: collab.ErrWorkItemClosed}
	server := New(Config{Superagent: service, Access: &fakeAccess{grants: access.Grants{access.KeyTaskWrite: access.ScopeTenant}}})
	req := httptest.NewRequest(http.MethodPost, "/v1/superagent/tickets/"+ticketID.String()+"/pending/fixture:e1/retry", strings.NewReader(`{}`))
	req.Header.Set("X-Tenant-ID", tenant.String())
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("retry on a closed ticket = %d, want 409: %s", rec.Code, rec.Body.String())
	}

	service.dismissErr = nil
	req = httptest.NewRequest(http.MethodPost, "/v1/superagent/tickets/"+ticketID.String()+"/pending/fixture:e1/dismiss", strings.NewReader(`{}`))
	req.Header.Set("X-Tenant-ID", tenant.String())
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss on a closed ticket = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestSuperagentClosedTicketAnswerIs409: the answer path maps the closed gate to
// 409 (not the generic 400 fallback).
func TestSuperagentClosedTicketAnswerIs409(t *testing.T) {
	tenant := uuid.New()
	service := &fakeSuperagent{answerErr: collab.ErrWorkItemClosed}
	server := New(Config{Superagent: service, Access: &fakeAccess{grants: access.Grants{access.KeyTaskWrite: access.ScopeTenant}}})
	req := httptest.NewRequest(http.MethodPost, "/v1/superagent/attention/"+uuid.New().String()+"/answer", strings.NewReader(`{"optionId":"approve"}`))
	req.Header.Set("X-Tenant-ID", tenant.String())
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("answer on a closed ticket = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}
