package metrics

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestAgentMetricsFamilies(t *testing.T) {
	reg := NewRegistry()
	set := NewSet(reg)
	set.AgentTurn()
	set.AgentToolCall()
	set.AgentContext(0.9)
	var buf bytes.Buffer
	if err := reg.WriteText(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	text := buf.String()
	for _, name := range []string{
		"keel_agent_turns_total",
		"keel_agent_tool_calls_total",
		"keel_agent_context_utilization",
	} {
		if !strings.Contains(text, name) {
			t.Errorf("metric %s missing:\n%s", name, text)
		}
	}
}
