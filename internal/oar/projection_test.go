package oar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func projectionFixtureRecords() []Record {
	timestamp := func(seq int64) time.Time {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Millisecond)
	}
	attribution := Attribution{SessionID: "session-host", TaskID: "task-card-1"}
	base := func(seq int64, kind RecordKind, path string) Record {
		return Record{
			Schema:      RecordSchema,
			Seq:         seq,
			AgentPath:   path,
			SessionID:   "oar-1",
			TurnID:      "turn-1",
			Kind:        kind,
			Runtime:     RuntimeFake,
			Attribution: attribution,
			Timestamp:   timestamp(seq),
		}
	}
	call := base(2, KindCall, RootAgentPath)
	call.CallID = "call-1"
	call.Name = "shell"
	call.Status = StatusStarted
	call.Input = json.RawMessage(`{"command":"printf OAR-OK"}`)
	result := base(3, KindResult, RootAgentPath)
	result.CallID = "call-1"
	result.Name = "shell"
	result.Status = StatusCompleted
	result.Output = json.RawMessage(`{"stdout":"OAR-OK","exitCode":0}`)
	childCall := base(4, KindCall, "/main/worker-1")
	childCall.CallID = "call-2"
	childCall.Name = "read_file"
	childCall.Status = StatusStarted
	childCall.Input = json.RawMessage(`{"path":"README.md"}`)
	childResult := base(5, KindResult, "/main/worker-1")
	childResult.CallID = "call-2"
	childResult.Name = "read_file"
	childResult.Status = StatusCompleted
	childResult.Output = json.RawMessage(`{"bytes":42}`)
	message := base(6, KindMessage, RootAgentPath)
	message.Name = "agent_message"
	message.Message = "fake answer to: inspect the repo"
	usage := base(7, KindUsage, RootAgentPath)
	usage.Name = "turn.usage"
	usage.Usage = &RecordUsage{Metering: "external", Model: "fake-model", InputTokens: 12, OutputTokens: 5, TotalTokens: 17}
	start := base(1, KindStatus, RootAgentPath)
	start.Name = "turn.started"
	start.Status = StatusStarted
	start.Message = "turn started"
	finish := base(8, KindStatus, RootAgentPath)
	finish.Name = "turn.finished"
	finish.Status = StatusCompleted
	finish.Message = "turn completed"
	return []Record{start, call, result, childCall, childResult, message, usage, finish}
}

func TestBuildProjectionFoldsCallsAndResults(t *testing.T) {
	projection := BuildProjection(projectionFixtureRecords())
	if projection.Schema != ProjectionSchema {
		t.Fatalf("schema = %q", projection.Schema)
	}
	if projection.BackendSessionID != "oar-1" || projection.HostSessionID != "session-host" || projection.TaskID != "task-card-1" {
		t.Fatalf("projection identity = %+v", projection)
	}
	if !projection.Simulated {
		t.Fatal("fake projection must be marked simulated")
	}
	if len(projection.Agents) != 2 {
		t.Fatalf("agents = %+v", projection.Agents)
	}
	if projection.Agents[1].AgentPath != "/main/worker-1" || projection.Agents[1].Calls != 1 {
		t.Fatalf("child agent rollup = %+v", projection.Agents[1])
	}
	var shell *ProjectionItem
	for index := range projection.Items {
		if projection.Items[index].CallID == "call-1" && projection.Items[index].Kind == KindCall {
			shell = &projection.Items[index]
		}
	}
	if shell == nil {
		t.Fatal("call-1 item missing")
	}
	if shell.State != StatusCompleted || shell.ResultSeq != 3 || string(shell.Output) != `{"stdout":"OAR-OK","exitCode":0}` {
		t.Fatalf("folded shell = %+v", shell)
	}
	if len(projection.Items) != 6 {
		t.Fatalf("items = %d, want 6 (status, call, call, message, usage, status): %+v", len(projection.Items), projection.Items)
	}
}

func TestResultWithoutCallStaysAStandaloneItem(t *testing.T) {
	record := validRecord(KindResult)
	record.Seq = 1
	record.CallID = "orphan"
	projection := BuildProjection([]Record{record})
	if len(projection.Items) != 1 || projection.Items[0].Kind != KindResult || projection.Items[0].CallID != "orphan" {
		t.Fatalf("orphan result = %+v", projection.Items)
	}
}

// TestProjectionFixture pins the renderer seam. Regenerate intentionally with:
//
//	UPDATE_OAR_FIXTURE=1 go test ./internal/oar -run TestProjectionFixture
func TestProjectionFixture(t *testing.T) {
	path := filepath.Join("testdata", "tool-renderer.fixture.json")
	projection := BuildProjection(projectionFixtureRecords())
	raw, err := ProjectionJSON(projection)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if os.Getenv("UPDATE_OAR_FIXTURE") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture (run with UPDATE_OAR_FIXTURE=1 first): %v", err)
	}
	var gotProjection, wantProjection Projection
	if err := json.Unmarshal(raw, &gotProjection); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &wantProjection); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotProjection, wantProjection) {
		t.Fatalf("projection drifted from the fixture:\n got: %s\nwant: %s", raw, want)
	}
}

func TestProjectionSortsBySeq(t *testing.T) {
	records := projectionFixtureRecords()
	reversed := make([]Record, len(records))
	for index := range records {
		reversed[index] = records[len(records)-1-index]
	}
	projection := BuildProjection(reversed)
	for index := 1; index < len(projection.Items); index++ {
		if projection.Items[index].Seq < projection.Items[index-1].Seq {
			t.Fatalf("items are not seq ordered: %+v", projection.Items)
		}
	}
}
