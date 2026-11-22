package oar

import (
	"encoding/json"
	"sort"
)

// ProjectionSchema is the schema id of the renderer projection. The W12
// tool-renderer slot consumes exactly this document; the web UI owns the slot
// and must not grow a second record model. A checked-in fixture lives at
// internal/oar/testdata/tool-renderer.fixture.json (also copied into the
// plugin package).
const ProjectionSchema = "keel.oar.render-projection/1"

// Projection is the stable, host-side view of an OAR record stream: calls and
// their results folded together, status/message/usage records preserved in
// order, and per-agent rollups. It contains no wall-clock generation timestamp
// so the same records always produce the same JSON (the fixture is exact).
type Projection struct {
	Schema string `json:"schema"`
	// BackendSessionID is the runtime's opaque session handle.
	BackendSessionID string `json:"backendSessionId"`
	// HostSessionID is the Keel session from the out-of-band attribution.
	HostSessionID string `json:"hostSessionId,omitempty"`
	// TaskID is the task card the external call is attributed to. OAR never
	// creates a child card.
	TaskID string `json:"taskId,omitempty"`
	// Runtime is the adapter id ("codex", "fake").
	Runtime string `json:"runtime,omitempty"`
	// Simulated reports a fake runtime. A renderer must show this; a simulated
	// stream is never presented as a live model run.
	Simulated bool `json:"simulated,omitempty"`
	// Agents are the agent paths observed in the stream, in first-seen order.
	Agents []ProjectionAgent `json:"agents"`
	// Items are the folded records in seq order.
	Items []ProjectionItem `json:"items"`
}

// ProjectionAgent is one agent path's rollup.
type ProjectionAgent struct {
	AgentPath string `json:"agentPath"`
	Runtime   string `json:"runtime,omitempty"`
	// Status is the last status/result state seen for this agent.
	Status   string `json:"status,omitempty"`
	Calls    int    `json:"calls"`
	Failures int    `json:"failures"`
}

// ProjectionItem is one renderer row. A call and its linked result share one
// item: Seq is the call's seq, ResultSeq is the result's seq.
type ProjectionItem struct {
	Seq       int64      `json:"seq"`
	ResultSeq int64      `json:"resultSeq,omitempty"`
	AgentPath string     `json:"agentPath"`
	Kind      RecordKind `json:"kind"`
	CallID    string     `json:"callId,omitempty"`
	Name      string     `json:"name,omitempty"`
	// State is running|completed|failed|cancelled for calls, and the status
	// value for status records.
	State  string          `json:"state,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output json.RawMessage `json:"output,omitempty"`
	Error  *RecordError    `json:"error,omitempty"`
	Text   string          `json:"text,omitempty"`
	Usage  *RecordUsage    `json:"usage,omitempty"`
}

// BuildProjection folds records into the renderer projection. It sorts by Seq
// (stable), so a caller may pass records from a file, a live channel or a
// replay without pre-ordering them. Result records are linked to calls by
// callId; a result without a call remains a standalone item (lossless, never
// dropped).
func BuildProjection(records []Record) Projection {
	sorted := make([]Record, len(records))
	copy(sorted, records)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })

	projection := Projection{
		Schema: ProjectionSchema,
		Agents: []ProjectionAgent{},
		Items:  []ProjectionItem{},
	}
	agentIndex := make(map[string]int)
	callIndex := make(map[string]int)

	agentFor := func(record Record) *ProjectionAgent {
		if index, ok := agentIndex[record.AgentPath]; ok {
			return &projection.Agents[index]
		}
		projection.Agents = append(projection.Agents, ProjectionAgent{
			AgentPath: record.AgentPath,
			Runtime:   record.Runtime,
		})
		agentIndex[record.AgentPath] = len(projection.Agents) - 1
		return &projection.Agents[len(projection.Agents)-1]
	}
	noteFailure := func(record Record) {
		agent := agentFor(record)
		agent.Failures++
		if record.Status != "" {
			agent.Status = record.Status
		}
	}
	noteState := func(record Record, state string) {
		agent := agentFor(record)
		if state != "" {
			agent.Status = state
		}
	}

	for _, record := range sorted {
		if projection.BackendSessionID == "" {
			projection.BackendSessionID = record.SessionID
		}
		if projection.HostSessionID == "" {
			projection.HostSessionID = record.Attribution.SessionID
		}
		if projection.TaskID == "" {
			projection.TaskID = record.Attribution.TaskID
		}
		if projection.Runtime == "" {
			projection.Runtime = record.Runtime
		}
		if record.Runtime == RuntimeFake {
			projection.Simulated = true
		}
		switch record.Kind {
		case KindCall:
			item := ProjectionItem{
				Seq:       record.Seq,
				AgentPath: record.AgentPath,
				Kind:      KindCall,
				CallID:    record.CallID,
				Name:      record.Name,
				State:     record.Status,
				Input:     record.Input,
			}
			if item.State == "" {
				item.State = StatusRunning
			}
			projection.Items = append(projection.Items, item)
			callIndex[record.CallID] = len(projection.Items) - 1
			agent := agentFor(record)
			agent.Calls++
			noteState(record, item.State)
		case KindResult:
			state := record.Status
			if state == "" || state == StatusStarted || state == StatusRunning {
				state = StatusCompleted
			}
			if record.Error != nil {
				state = StatusFailed
			}
			if index, ok := callIndex[record.CallID]; ok {
				item := &projection.Items[index]
				item.ResultSeq = record.Seq
				item.State = state
				item.Output = record.Output
				item.Error = record.Error
				if item.Name == "" {
					item.Name = record.Name
				}
			} else {
				projection.Items = append(projection.Items, ProjectionItem{
					Seq:       record.Seq,
					AgentPath: record.AgentPath,
					Kind:      KindResult,
					CallID:    record.CallID,
					Name:      record.Name,
					State:     state,
					Output:    record.Output,
					Error:     record.Error,
				})
			}
			noteState(record, state)
			if record.Error != nil {
				noteFailure(record)
			}
		case KindStatus:
			projection.Items = append(projection.Items, ProjectionItem{
				Seq:       record.Seq,
				AgentPath: record.AgentPath,
				Kind:      KindStatus,
				Name:      record.Name,
				State:     record.Status,
				Output:    record.Output,
				Text:      record.Message,
			})
			noteState(record, record.Status)
		case KindMessage:
			projection.Items = append(projection.Items, ProjectionItem{
				Seq:       record.Seq,
				AgentPath: record.AgentPath,
				Kind:      KindMessage,
				Name:      record.Name,
				Text:      record.Message,
				Output:    record.Output,
			})
			agentFor(record)
		case KindUsage:
			projection.Items = append(projection.Items, ProjectionItem{
				Seq:       record.Seq,
				AgentPath: record.AgentPath,
				Kind:      KindUsage,
				Name:      record.Name,
				Usage:     record.Usage,
			})
			agentFor(record)
		case KindError:
			projection.Items = append(projection.Items, ProjectionItem{
				Seq:       record.Seq,
				AgentPath: record.AgentPath,
				Kind:      KindError,
				Name:      record.Name,
				Error:     record.Error,
				Text:      record.Message,
			})
			noteFailure(record)
		}
	}
	return projection
}

// ProjectionFromJournal reads a journal file and folds it into a projection.
func ProjectionFromJournal(path string) (Projection, error) {
	records, err := ReadJournal(path)
	if err != nil {
		return Projection{}, err
	}
	return BuildProjection(records), nil
}

// ProjectionJSON encodes the projection in its stable wire form.
func ProjectionJSON(projection Projection) ([]byte, error) {
	return json.MarshalIndent(projection, "", "  ")
}
