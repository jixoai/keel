package sidecar

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/oar"
)

func TestMapCodexLine(t *testing.T) {
	cases := []struct {
		name       string
		line       string
		wantKind   oar.RecordKind
		wantName   string
		wantStatus string
	}{
		{"thread", `{"type":"thread.started","thread_id":"thread-1"}`, oar.KindStatus, "thread.started", oar.StatusStarted},
		{"turn started", `{"type":"turn.started"}`, oar.KindStatus, "codex.turn.started", oar.StatusStarted},
		{"item call", `{"type":"item.started","item":{"id":"item-1","type":"command_execution","command":"echo hi"}}`, oar.KindCall, "command_execution", oar.StatusStarted},
		{"item result", `{"type":"item.completed","item":{"id":"item-1","type":"command_execution","aggregated_output":"hi","exit_code":0,"status":"completed"}}`, oar.KindResult, "command_execution", oar.StatusCompleted},
		{"message", `{"type":"item.completed","item":{"id":"item-2","type":"agent_message","text":"hello"}}`, oar.KindMessage, "agent_message", ""},
		{"usage", `{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14}}`, oar.KindUsage, "turn.usage", ""},
		{"turn failed", `{"type":"turn.failed","error":{"message":"boom"}}`, oar.KindError, "codex.turn.failed", ""},
		{"reconnect", `{"type":"error","message":"Reconnecting..."}`, oar.KindStatus, "codex.error", oar.StatusDegraded},
		{"item error", `{"type":"item.completed","item":{"id":"item-3","type":"error","message":"warning"}}`, oar.KindStatus, "codex.item.error", oar.StatusDegraded},
		{"unknown", `{"type":"new.thing","value":1}`, oar.KindStatus, "codex.new.thing", oar.StatusRunning},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			records, err := MapCodexLine([]byte(test.line), "turn-1", 64<<10)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 {
				t.Fatalf("records = %+v", records)
			}
			record := records[0]
			if record.Kind != test.wantKind || record.Name != test.wantName {
				t.Fatalf("record = %+v, want kind=%s name=%s", record, test.wantKind, test.wantName)
			}
			if test.wantStatus != "" && record.Status != test.wantStatus {
				t.Fatalf("status = %q, want %q", record.Status, test.wantStatus)
			}
			if record.TurnID != "turn-1" {
				t.Fatalf("turn = %q", record.TurnID)
			}
		})
	}
}

func TestMapCodexLineLinksItemIDs(t *testing.T) {
	call, err := MapCodexLine([]byte(`{"type":"item.started","item":{"id":"item-7","type":"mcp_tool_call"}}`), "turn-1", 64<<10)
	if err != nil || len(call) != 1 || call[0].CallID != "item-7" {
		t.Fatalf("call = %+v, err = %v", call, err)
	}
	result, err := MapCodexLine([]byte(`{"type":"item.completed","item":{"id":"item-7","type":"mcp_tool_call","status":"completed"}}`), "turn-1", 64<<10)
	if err != nil || len(result) != 1 || result[0].CallID != "item-7" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestMapCodexLineBoundsOutput(t *testing.T) {
	big := strings.Repeat("x", 2048)
	line := `{"type":"item.completed","item":{"id":"item-1","type":"command_execution","aggregated_output":"` + big + `","exit_code":0}}`
	records, err := MapCodexLine([]byte(line), "turn-1", 256)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %+v", records)
	}
	var envelope struct {
		Truncated bool `json:"truncated"`
		Bytes     int  `json:"bytes"`
	}
	if err := json.Unmarshal(records[0].Output, &envelope); err != nil {
		t.Fatalf("bounded output is not an envelope: %s", records[0].Output)
	}
	if !envelope.Truncated || envelope.Bytes <= 256 {
		t.Fatalf("envelope = %+v", envelope)
	}
}

func TestMapCodexLineRejectsNonJSON(t *testing.T) {
	if _, err := MapCodexLine([]byte("not json"), "turn-1", 1024); err == nil {
		t.Fatal("non-JSON line was accepted")
	}
}

func TestDeclarationMatchesRuntimeMatrix(t *testing.T) {
	fake := Config{Runtime: RuntimeFake}.Declaration()
	if !fake.Simulated || fake.Has(oar.CapabilityLiveModel) {
		t.Fatalf("fake declaration = %+v", fake)
	}
	if fake.Has(oar.CapabilityUsage) {
		t.Fatal("fake without -usage must not declare usage")
	}
	withUsage := Config{Runtime: RuntimeFake, Usage: true}.Declaration()
	if !withUsage.Has(oar.CapabilityUsage) {
		t.Fatal("fake with usage must declare usage")
	}
	codex := Config{Runtime: RuntimeCodex}.Declaration()
	if err := codex.Validate(); err != nil {
		t.Fatalf("codex declaration invalid: %v", err)
	}
	if !codex.Has(oar.CapabilityLiveModel) {
		t.Fatal("codex must declare live_model")
	}
}
