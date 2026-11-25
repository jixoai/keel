package harnessapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

func TestCapabilitiesProjectCollabModes(t *testing.T) {
	server := httptest.NewServer(New(Config{
		CollabModes: func() []collab.ModeView {
			tenant := uuid.MustParse("11111111-1111-4111-8111-111111111111")
			return []collab.ModeView{
				{TenantID: tenant, ID: collab.ModeSubagent, Topology: collab.TopologyTree, WorkUnit: collab.WorkUnitDelegation, Enabled: true},
				{TenantID: tenant, ID: collab.ModeSuperagent, Topology: collab.TopologyWorkQueue, WorkUnit: collab.WorkUnitTicket, Enabled: false},
			}
		},
		Logger: testLogger(),
	}))
	defer server.Close()

	var body Capabilities
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/capabilities", uuid.Nil, nil, &body); code != 200 {
		t.Fatalf("capabilities: %d", code)
	}
	if !body.Features["collab.modes"] {
		t.Fatalf("collab.modes feature = false: %+v", body.Features)
	}
	if body.Collab == nil || len(body.Collab.Modes) != 2 {
		t.Fatalf("collab block = %+v", body.Collab)
	}
	if body.Collab.Modes[0].ID != collab.ModeSubagent || !body.Collab.Modes[0].Enabled {
		t.Fatalf("subagent mode = %+v", body.Collab.Modes[0])
	}
	if body.Collab.Modes[1].ID != collab.ModeSuperagent || body.Collab.Modes[1].Enabled {
		t.Fatalf("superagent mode = %+v", body.Collab.Modes[1])
	}
}

func TestCapabilitiesOmitCollabWhenUnwired(t *testing.T) {
	server := httptest.NewServer(New(Config{Logger: testLogger()}))
	defer server.Close()

	var body Capabilities
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/capabilities", uuid.Nil, nil, &body); code != 200 {
		t.Fatalf("capabilities: %d", code)
	}
	if body.Features["collab.modes"] {
		t.Fatal("collab.modes must be false when no registry is wired")
	}
	if body.Collab != nil {
		t.Fatalf("unwired collab block = %+v", body.Collab)
	}
}
