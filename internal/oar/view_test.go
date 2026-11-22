package oar

import (
	"bytes"
	"strings"
	"testing"
)

func TestFilterRecords(t *testing.T) {
	records := projectionFixtureRecords()
	filtered := FilterRecords(records, ViewOptions{AfterSeq: 4, AgentPath: "/main/worker-1"})
	if len(filtered) != 1 || filtered[0].Seq != 5 {
		t.Fatalf("filtered = %+v", filtered)
	}
}

func TestWriteTable(t *testing.T) {
	var buffer bytes.Buffer
	if err := WriteTable(&buffer, projectionFixtureRecords()); err != nil {
		t.Fatal(err)
	}
	output := buffer.String()
	for _, want := range []string{"SEQ", "AGENT", "call-1", "/main/worker-1", "OAR-OK", "fake answer"} {
		if !strings.Contains(output, want) {
			t.Fatalf("table output missing %q:\n%s", want, output)
		}
	}
}
