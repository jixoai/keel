package telemetry

import "testing"

func TestManualSpanTree(t *testing.T) {
	m := NewMemory()
	root, endRoot := m.StartManualSpan(SpanOptions{Name: "run"})
	turn, endTurn := root.(ManualStarter).StartManualSpan(SpanOptions{Name: "turn"})
	turn.AddEvent("message_start", nil)
	tool, endTool := turn.(ManualStarter).StartManualSpan(SpanOptions{Name: "tool"})
	tool.AddEvent("tool_end", nil)
	endTool()
	endTurn()
	endRoot()
	spans := m.Spans()
	if len(spans) != 3 {
		t.Fatalf("spans=%d", len(spans))
	}
	if spans[0].Name != "run" || spans[1].Name != "turn" || spans[2].Name != "tool" {
		t.Fatalf("order=%v", []string{spans[0].Name, spans[1].Name, spans[2].Name})
	}
	if spans[1].ParentID == nil || *spans[1].ParentID != spans[0].ID {
		t.Fatalf("turn parent=%v root=%d", spans[1].ParentID, spans[0].ID)
	}
	if spans[2].ParentID == nil || *spans[2].ParentID != spans[1].ID {
		t.Fatalf("tool parent=%v turn=%d", spans[2].ParentID, spans[1].ID)
	}
}
