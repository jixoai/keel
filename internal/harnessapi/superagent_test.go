package harnessapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
)

type fakeSuperagent struct {
	tenant   uuid.UUID
	board    superagent.BoardView
	inbox    superagent.AttentionInbox
	boardErr error
	inboxErr error
}

func (f *fakeSuperagent) Board(_ context.Context, tenantID uuid.UUID, _ superagent.BoardFilter) (superagent.BoardView, error) {
	f.tenant = tenantID
	return f.board, f.boardErr
}

func (f *fakeSuperagent) Inbox(_ context.Context, tenantID uuid.UUID) (superagent.AttentionInbox, error) {
	f.tenant = tenantID
	return f.inbox, f.inboxErr
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
