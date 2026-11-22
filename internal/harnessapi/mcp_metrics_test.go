package harnessapi

import (
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/mcphost"
)

// recordingMetrics captures bridge frame increments.
type recordingMetrics struct {
	frames [][2]string
}

func (m *recordingMetrics) BridgeFrame(method, direction string) {
	m.frames = append(m.frames, [2]string{method, direction})
}

func TestMCPAppGateCountsBridgeFrames(t *testing.T) {
	metrics := &recordingMetrics{}
	server := New(Config{MCPAppMetrics: metrics})
	app := mcphost.AppSpec{AppID: "mcpapp_0123456789abcdef", Server: "fixture", ResourceURI: "ui://fixture/panel"}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/v1/mcp/apps/x/tools/call", nil)
	release, ok := server.mcpAppGate(recorder, request, uuid.New(), uuid.Nil, app, "tools/call")
	if !ok {
		t.Fatalf("gate refused a first request: %d %s", recorder.Code, recorder.Body.String())
	}
	release()
	if len(metrics.frames) != 2 {
		t.Fatalf("frames = %v, want in+out", metrics.frames)
	}
	if metrics.frames[0] != [2]string{"tools/call", "in"} || metrics.frames[1] != [2]string{"tools/call", "out"} {
		t.Fatalf("frames = %v", metrics.frames)
	}
}
