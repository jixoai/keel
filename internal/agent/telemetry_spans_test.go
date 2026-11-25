package agent

import (
	"context"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/telemetry"
)

func TestRunSpanTreeIncludesTurnAndTool(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	r, k := newTestRunner(t, st)
	mem := telemetry.NewMemory()
	r.Telemetry = &RunTelemetry{Context: mem}
	if _, err := st.AppendEvent(ctx, tenant, session, "user.message", map[string]string{"text": "echo hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := execRun(t, k, r, tenant, session); err != nil {
		t.Fatal(err)
	}
	spans := mem.Spans()
	var run *telemetry.RecordedSpan
	turns := map[int64]bool{}
	var tool *telemetry.RecordedSpan
	for i := range spans {
		span := &spans[i]
		switch span.Name {
		case "keel.agent.run":
			run = span
		case "keel.agent.turn":
			turns[span.ID] = true
			if span.ParentID == nil || run == nil || *span.ParentID != run.ID {
				t.Fatalf("turn parent=%v run=%v", span.ParentID, run)
			}
			if !span.Settled {
				t.Fatalf("turn not settled: %+v", span)
			}
		case "keel.agent.tool":
			tool = span
		}
	}
	if run == nil || len(turns) == 0 || tool == nil {
		t.Fatalf("missing spans: %+v", spans)
	}
	if tool.ParentID == nil || !turns[*tool.ParentID] {
		t.Fatalf("tool parent=%v not a turn %v", tool.ParentID, turns)
	}
	if !tool.Settled {
		t.Fatalf("tool not settled: %+v", tool)
	}
}

func TestTurnSpanCarriesMessageEvents(t *testing.T) {
	mem := telemetry.NewMemory()
	root, endRoot := mem.StartManualSpan(telemetry.SpanOptions{Name: "keel.agent.run"})
	tracker := newRunSpanTracker(root)
	tracker.observe(agentcore.TurnStartEvent{})
	tracker.observe(agentcore.MessageStartEvent{Message: agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent("hi")},
	}})
	tracker.observe(agentcore.MessageEndEvent{Message: agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent("hi")},
	}})
	tracker.observe(agentcore.TurnEndEvent{})
	tracker.close()
	endRoot()
	for _, span := range mem.Spans() {
		if span.Name != "keel.agent.turn" {
			continue
		}
		if len(span.Events) < 2 || span.Events[0].Name != "message_start" || span.Events[1].Name != "message_end" {
			t.Fatalf("turn events=%+v", span.Events)
		}
		return
	}
	t.Fatal("missing turn span")
}

type fakeRuntime struct {
	turns, tools int
	context      float64
}

func (f *fakeRuntime) AgentTurn()                 { f.turns++ }
func (f *fakeRuntime) AgentToolCall()             { f.tools++ }
func (f *fakeRuntime) AgentContext(ratio float64) { f.context = ratio }

func TestSpanTrackerFeedsRuntimeMetrics(t *testing.T) {
	mem := telemetry.NewMemory()
	root, end := mem.StartManualSpan(telemetry.SpanOptions{Name: "keel.agent.run"})
	tracker := newRunSpanTracker(root)
	runtime := &fakeRuntime{}
	tracker.setRuntime(runtime)
	tracker.observe(agentcore.TurnStartEvent{})
	tracker.observe(agentcore.ToolExecutionStartEvent{ToolCallID: "c1", ToolName: "echo"})
	tracker.observe(agentcore.ToolExecutionEndEvent{ToolCallID: "c1", ToolName: "echo"})
	tracker.observe(agentcore.TelemetryEvent{ContextUtilization: 0.75})
	tracker.close()
	end()
	if runtime.turns != 1 || runtime.tools != 1 || runtime.context != 0.75 {
		t.Fatalf("runtime metrics = %+v", runtime)
	}
}
