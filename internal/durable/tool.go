package durable

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Default output bounds, matching PI's truncate.ts.
const (
	DefaultMaxBytes = 50 * 1024
	DefaultMaxLines = 2000
)

// OutputLimits bounds a tool result's text.
type OutputLimits struct {
	MaxBytes int    `json:"maxBytes,omitempty"`
	MaxLines int    `json:"maxLines,omitempty"`
	Retain   string `json:"retain,omitempty"` // head | tail
}

func (l OutputLimits) resolved() OutputLimits {
	if l.MaxBytes == 0 {
		l.MaxBytes = DefaultMaxBytes
	}
	if l.MaxLines == 0 {
		l.MaxLines = DefaultMaxLines
	}
	if l.Retain == "" {
		l.Retain = "head"
	}
	return l
}

// ToolControl is what a result asks the round to do.
type ToolControl struct {
	AddTools  []string `json:"addTools,omitempty"`
	Terminate bool     `json:"terminate,omitempty"`
	Handoff   string   `json:"handoff,omitempty"`
}

// ToolResult is what a tool returns on three channels: output for the model,
// structured output for programs, and details for UIs.
type ToolResult struct {
	Output           agentcore.ContentList `json:"output,omitempty"`
	HasOutput        bool                  `json:"-"`
	StructuredOutput json.RawMessage       `json:"structuredOutput,omitempty"`
	IsError          bool                  `json:"isError,omitempty"`
	Details          any                   `json:"details,omitempty"`
	Diagnostics      []Diagnostic          `json:"diagnostics,omitempty"`
	Usage            *agentcore.Usage      `json:"usage,omitempty"`
	Control          *ToolControl          `json:"control,omitempty"`
}

// OutputText concatenates a content list's text items.
func OutputText(content agentcore.ContentList) string {
	var b strings.Builder
	for _, c := range content {
		if t, ok := c.(agentcore.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// Tool is a tool registration. Execute receives arguments already prepared and
// validated.
type Tool struct {
	Name                   string
	Description            string
	Parameters             json.RawMessage
	StructuredOutputSchema json.RawMessage
	Replay                 string // safe | unsafe (default unsafe)
	ExecutionMode          string
	PrepareArguments       func(args json.RawMessage) (json.RawMessage, error)
	OutputLimits           OutputLimits
	Execute                func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error)
}

func (t *Tool) replay() string {
	if t.Replay == ReplaySafe {
		return ReplaySafe
	}
	return ReplayUnsafe
}

// ToolHookCall is the call as the model made it, plus its parent for a nested
// call.
type ToolHookCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Parent    *ParentCall     `json:"parent,omitempty"`
}

// ParentCall names the calling tool task and call of a nested call.
type ParentCall struct {
	TaskID TaskID `json:"taskId"`
	CallID string `json:"callId"`
}

// BeforeToolDecision blocks a call or rewrites its arguments.
type BeforeToolDecision struct {
	Block     string
	Arguments json.RawMessage
}

// Hook registers beforeTool/afterTool handlers for a task kind.
type Hook struct {
	Task       string
	BeforeTool func(ctx context.Context, call ToolHookCall, api *ToolAPI) (*BeforeToolDecision, error)
	AfterTool  func(ctx context.Context, call ToolHookCall, result ToolResult, api *ToolAPI) (*ToolResult, error)
}

// ToolCall is one model tool call, or a nested call's call.
type ToolCall struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolTaskInput is the input of a pi.tool task.
type ToolTaskInput struct {
	Kind         string    `json:"kind"` // model | nested
	Assistant    EntryID   `json:"assistant,omitempty"`
	CallID       string    `json:"callId,omitempty"`
	Unoffered    bool      `json:"unoffered,omitempty"`
	Parent       TaskID    `json:"parent,omitempty"`
	ParentCallID string    `json:"parentCallId,omitempty"`
	Key          string    `json:"key,omitempty"`
	Call         *ToolCall `json:"call,omitempty"`
	// Progress false: commit no running output, details, or diagnostics to the
	// nested call's slot, only its status.
	Progress *bool `json:"progress,omitempty"`
}

// ToolTaskCheckpoint is the durable intent of a tool task.
type ToolTaskCheckpoint struct {
	Phase     string          `json:"phase"` // call | execute
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Replay    string          `json:"replay,omitempty"`
}

// ToolTaskResult is a tool task's outcome result.
type ToolTaskResult struct {
	Kind    string       `json:"kind"`
	EntryID EntryID      `json:"entryId,omitempty"`
	Control *ToolControl `json:"control,omitempty"`
}

func decodeToolCheckpoint(task TaskRecord) ToolTaskCheckpoint {
	var cp ToolTaskCheckpoint
	if len(task.State.Checkpoint) > 0 {
		_ = json.Unmarshal(task.State.Checkpoint, &cp)
	}
	if cp.Phase == "" {
		cp.Phase = "call"
	}
	return cp
}

func decodeToolInput(task TaskRecord) (ToolTaskInput, error) {
	var in ToolTaskInput
	if err := json.Unmarshal(task.Input, &in); err != nil {
		return ToolTaskInput{}, err
	}
	return in, nil
}

// ToolAPI is the invocation-bound API of one tool call.
type ToolAPI struct {
	TaskID         TaskID
	ConversationID ConversationID
	CallID         string
	Parent         *ParentCall

	h           *Harness
	tool        *Tool
	ctx         context.Context
	mu          sync.Mutex
	output      strings.Builder
	details     any
	diagnostics []Diagnostic
	ended       bool
	sequence    int
	progress    bool
}

func (api *ToolAPI) assertLive() error {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.ended {
		return fmt.Errorf("Tool call %s has settled", api.CallID)
	}
	return nil
}

// Output appends running output.
func (api *ToolAPI) Output(chunk string) {
	api.mu.Lock()
	if api.ended {
		api.mu.Unlock()
		return
	}
	api.output.WriteString(chunk)
	api.mu.Unlock()
	api.publishProgress()
}

// Diagnostic records a model-visible remark.
func (api *ToolAPI) Diagnostic(d Diagnostic) {
	api.mu.Lock()
	if api.ended {
		api.mu.Unlock()
		return
	}
	api.diagnostics = append(api.diagnostics, d)
	api.mu.Unlock()
	api.publishProgress()
}

// Details replaces the running details.
func (api *ToolAPI) Details(value any) {
	api.mu.Lock()
	if api.ended {
		api.mu.Unlock()
		return
	}
	api.details = value
	api.mu.Unlock()
	api.publishProgress()
}

// publishProgress stores the running output/details/diagnostics in the live
// slot, so an interrupted call's result can be rebuilt from it.
func (api *ToolAPI) publishProgress() {
	if !api.progress {
		return
	}
	text, details, diagnostics := api.snapshot()
	api.h.recordToolProgress(api.ConversationID, api.TaskID, text, details, diagnostics)
}

// RetainedOutput returns the bounded output retained so far.
func (api *ToolAPI) RetainedOutput() (string, bool) {
	api.mu.Lock()
	defer api.mu.Unlock()
	text := api.output.String()
	bounded, dropped, _ := boundText(text, api.tool.OutputLimits.resolved())
	return bounded, dropped > 0
}

// Snapshot returns what the tool reported so far.
func (api *ToolAPI) snapshot() (text string, details any, diagnostics []Diagnostic) {
	api.mu.Lock()
	defer api.mu.Unlock()
	return api.output.String(), api.details, append([]Diagnostic(nil), api.diagnostics...)
}

func (api *ToolAPI) end() {
	api.mu.Lock()
	api.ended = true
	api.mu.Unlock()
}

// ExecuteTool runs a nested tool call and waits for its result. Cancelling ctx
// stops only the wait: the call runs on.
func (api *ToolAPI) ExecuteTool(ctx context.Context, name string, args json.RawMessage, opts ...NestedOption) (NestedResult, error) {
	if err := api.assertLive(); err != nil {
		return NestedResult{}, err
	}
	options := NestedOptions{}
	for _, opt := range opts {
		opt(&options)
	}
	key := options.Key
	if !options.KeySet {
		api.mu.Lock()
		api.sequence++
		key = fmt.Sprintf("%d", api.sequence)
		api.mu.Unlock()
	} else if err := checkNestedKey(key); err != nil {
		return NestedResult{}, err
	}
	return api.h.executeNestedCall(ctx, api.TaskID, api.CallID, name, args, key, options)
}

// NestedOption configures one nested call.
type NestedOption func(*NestedOptions)

// NestedOptions are the per-call options of executeTool.
type NestedOptions struct {
	Key      string
	KeySet   bool
	Progress *bool
}

// WithKey names a nested call explicitly (an empty explicit key is rejected).
func WithKey(key string) NestedOption { return func(o *NestedOptions) { o.Key = key; o.KeySet = true } }

// WithoutProgress suppresses running progress commits for a nested call.
func WithoutProgress() NestedOption {
	return func(o *NestedOptions) { f := false; o.Progress = &f }
}

// ---- argument handling ----

var schemaCache sync.Map // string -> *jsonschema.Schema

func compileJSONSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	if cached, ok := schemaCache.Load(string(raw)); ok {
		if cached == nil {
			return nil, nil
		}
		return cached.(*jsonschema.Schema), nil
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	loc := fmt.Sprintf("mem:///durable-%d.json", len(string(raw)))
	if err := c.AddResource(loc, doc); err != nil {
		return nil, err
	}
	sch, err := c.Compile(loc)
	if err != nil {
		return nil, err
	}
	schemaCache.Store(string(raw), sch)
	return sch, nil
}

func schemaPrinterForMessage() func(*jsonschema.ValidationError) string {
	return func(e *jsonschema.ValidationError) string {
		path := ""
		segments := e.InstanceLocation
		if len(segments) > 1 {
			segments = segments[1:]
		}
		for _, segment := range segments {
			if path != "" {
				path += "."
			}
			path += strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
		}
		if path == "" {
			path = "root"
		}
		return path + ": " + e.Error()
	}
}

func validateAgainst(schema *jsonschema.Schema, args json.RawMessage) error {
	if schema == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	var inst any
	if err := dec.Decode(&inst); err != nil {
		return fmt.Errorf("arguments are not valid JSON: %v", err)
	}
	return schema.Validate(inst)
}

func validateToolArguments(tool *Tool, args json.RawMessage) (json.RawMessage, error) {
	// Shape checks models commonly get wrong: parse and reject non-objects.
	var probe any
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	if err := dec.Decode(&probe); err != nil {
		return nil, fmt.Errorf("arguments are not valid JSON: %v", err)
	}
	if _, ok := probe.(map[string]any); !ok {
		return nil, fmt.Errorf("arguments must be an object")
	}
	schema, err := compileJSONSchema(tool.Parameters)
	if err != nil {
		return nil, err
	}
	if err := validateAgainst(schema, args); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return nil, fmt.Errorf("%s", schemaPrinterForMessage()(ve))
		}
		return nil, err
	}
	return args, nil
}

func jsonEqual(a, b json.RawMessage) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	an, _ := json.Marshal(av)
	bn, _ := json.Marshal(bv)
	return bytes.Equal(an, bn)
}

// ---- tool execution ----

// executeToolTask runs one pi.tool task to terminal.
func (h *Harness) executeToolTask(ctx context.Context, conv ConversationID, taskID TaskID, recovering bool) {
	defer h.clearRunning(taskID)

	h.mu.Lock()
	if !h.aliveLocked() {
		h.mu.Unlock()
		return
	}
	task, ok := h.store.GetTask(taskID)
	if !ok || task.Terminal() {
		h.mu.Unlock()
		return
	}
	input, err := decodeToolInput(task)
	if err != nil {
		h.mu.Unlock()
		h.faultTool(taskID, fmt.Errorf("decode tool input: %w", err))
		return
	}
	cp := decodeToolCheckpoint(task)
	_, _ = h.store.UpdateTask(taskID, func(t *TaskRecord) {
		t.State.Status = TaskRunning
		if t.StartedAt == 0 {
			t.StartedAt = h.now()
		}
	})
	h.notifyLocked()
	h.mu.Unlock()

	call, err := h.readToolCall(conv, input)
	if err != nil {
		h.faultTool(taskID, err)
		return
	}
	api := &ToolAPI{
		TaskID: taskID, ConversationID: conv, CallID: call.ID, Parent: call.Parent,
		h: h, ctx: ctx, progress: input.Progress == nil || *input.Progress,
	}
	var tool *Tool
	if input.Kind == "model" && input.Unoffered {
		tool = nil
	} else if input.Kind == "model" {
		tool = h.resolveOfferedTool(conv, call.Name)
	} else {
		tool = h.tools[call.Name]
	}
	api.tool = tool
	if api.tool == nil {
		api.tool = &Tool{Name: call.Name}
	}

	// Recovery: a call interrupted after durable intent follows the replay rule.
	if recovering && cp.Phase == "execute" {
		if cp.Replay == ReplaySafe && tool != nil && tool.replay() == ReplaySafe {
			// Rerun from scratch, clearing what the interrupted attempt published.
			h.clearSlot(conv, taskID)
			h.runTool(ctx, conv, taskID, input, call, tool, cp.Arguments, api)
			return
		}
		h.settleFromSlot(conv, taskID, input, call, tool, "interrupted",
			fmt.Sprintf("Tool %s was interrupted and may have partially run", call.Name), recovering)
		return
	}
	if recovering && cp.Phase == "call" && input.Kind == "nested" && tool == nil {
		// A nested call whose tool no longer resolves after a restart.
		h.settleFromSlot(conv, taskID, input, call, nil, "tool_unavailable",
			fmt.Sprintf("Tool %s is not available", call.Name), recovering)
		return
	}

	var result ToolResult
	decided := false
	if tool == nil {
		result = harnessError("tool_unavailable", fmt.Sprintf("Tool %s is not available", call.Name))
		decided = true
	} else {
		prepared := call.Arguments
		if tool.PrepareArguments != nil {
			next, perr := tool.PrepareArguments(call.Arguments)
			if perr != nil {
				result = harnessError("invalid_arguments", perr.Error())
				decided = true
			} else {
				prepared = next
			}
		}
		if !decided {
			validated, verr := validateToolArguments(tool, prepared)
			if verr != nil {
				result = harnessError("invalid_arguments", verr.Error())
				decided = true
			} else {
				prepared = validated
			}
		}
		if !decided {
			var block string
			for _, hook := range h.toolHooks() {
				if hook.BeforeTool == nil {
					continue
				}
				if block != "" {
					break
				}
				decision, herr := hook.BeforeTool(ctx, ToolHookCall{
					ID: call.ID, Name: call.Name, Arguments: prepared, Parent: call.Parent,
				}, api)
				if herr != nil {
					if ctx.Err() != nil {
						result = harnessError("aborted", herr.Error())
						decided = true
						break
					}
					block = herr.Error()
					continue
				}
				if decision == nil {
					continue
				}
				if decision.Block != "" {
					block = decision.Block
					continue
				}
				if decision.Arguments != nil {
					prepared = decision.Arguments
				}
			}
			if block != "" {
				result = harnessError("blocked", "Tool call blocked: "+block)
				decided = true
			} else {
				validated, verr := validateToolArguments(tool, prepared)
				if verr != nil {
					result = harnessError("invalid_arguments", verr.Error())
					decided = true
				} else {
					prepared = validated
					h.recordIntent(conv, taskID, prepared, tool.replay())
					h.runTool(ctx, conv, taskID, input, call, tool, prepared, api)
					return
				}
			}
		}
	}

	// A Harness-decided result still passes afterTool.
	settled := h.afterHarnessResult(ctx, call, tool, result, api)
	h.finishToolTask(conv, taskID, input, call, Outcome{Status: OutcomeCompleted}, &settled, -1)
}

func (h *Harness) readToolCall(conv ConversationID, input ToolTaskInput) (ToolHookCall, error) {
	if input.Kind == "nested" {
		if input.Call == nil {
			return ToolHookCall{}, errors.New("nested tool input has no call")
		}
		return ToolHookCall{
			ID: input.Call.ID, Name: input.Call.Name, Arguments: input.Call.Arguments,
			Parent: &ParentCall{TaskID: input.Parent, CallID: input.ParentCallID},
		}, nil
	}
	entries := h.store.Entries(conv)
	for _, entry := range entries {
		if entry.ID != input.Assistant {
			continue
		}
		for _, m := range entry.Model {
			assistant, ok := m.(agentcore.AssistantMessage)
			if !ok {
				continue
			}
			for _, tc := range assistant.ToolCalls() {
				if tc.ID == input.CallID {
					return ToolHookCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}, nil
				}
			}
		}
	}
	return ToolHookCall{}, fmt.Errorf("Entry %d has no tool call %s", input.Assistant, input.CallID)
}

func (h *Harness) resolveOfferedTool(conv ConversationID, name string) *Tool {
	for _, tool := range h.offeredTools(conv) {
		if tool.Name == name {
			return tool
		}
	}
	return nil
}

func (h *Harness) toolHooks() []*Hook {
	var out []*Hook
	for _, hook := range h.hooks {
		if hook.Task == "" || hook.Task == ToolTaskKind {
			out = append(out, hook)
		}
	}
	return out
}

// recordToolProgress stores a running call's output in its live slot.
func (h *Harness) recordToolProgress(conv ConversationID, taskID TaskID, output string, details any, diagnostics []Diagnostic) {
	if h.Closed() {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	live := h.loadLive(conv)
	changed := false
	for i := range live.Tools {
		if live.Tools[i].TaskID == taskID {
			live.Tools[i].Status = "running"
			live.Tools[i].Output = output
			if details != nil {
				if raw, err := json.Marshal(details); err == nil {
					live.Tools[i].Details = raw
				} else {
					h.report(err)
				}
			}
			live.Tools[i].Diagnostics = append([]Diagnostic(nil), diagnostics...)
			changed = true
		}
	}
	for i := range live.NestedTools {
		if live.NestedTools[i].TaskID == taskID {
			live.NestedTools[i].Status = "running"
			live.NestedTools[i].Output = output
			if details != nil {
				if raw, err := json.Marshal(details); err == nil {
					live.NestedTools[i].Details = raw
				} else {
					h.report(err)
				}
			}
			live.NestedTools[i].Diagnostics = append([]Diagnostic(nil), diagnostics...)
			changed = true
		}
	}
	if changed {
		h.saveLive(conv, live)
		h.notifyLocked()
	}
}

func (h *Harness) recordIntent(conv ConversationID, taskID TaskID, args json.RawMessage, replay string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, _ = h.store.UpdateTask(taskID, func(t *TaskRecord) {
		t.State.Status = TaskRunning
		t.State.Checkpoint = rawJSON(ToolTaskCheckpoint{Phase: "execute", Arguments: cloneRaw(args), Replay: replay})
	})
	live := h.loadLive(conv)
	for i := range live.Tools {
		if live.Tools[i].TaskID == taskID {
			live.Tools[i].Status = "running"
		}
	}
	for i := range live.NestedTools {
		if live.NestedTools[i].TaskID == taskID {
			live.NestedTools[i].Status = "running"
			live.NestedTools[i].Arguments = cloneRaw(args)
		}
	}
	h.saveLive(conv, live)
	h.notifyLocked()
}

func (h *Harness) clearSlot(conv ConversationID, taskID TaskID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	live := h.loadLive(conv)
	for i := range live.Tools {
		if live.Tools[i].TaskID == taskID {
			live.Tools[i].Status = "running"
		}
	}
	for i := range live.NestedTools {
		if live.NestedTools[i].TaskID == taskID {
			live.NestedTools[i].Status = "running"
			live.NestedTools[i].Output = ""
			live.NestedTools[i].Details = nil
			live.NestedTools[i].Diagnostics = nil
		}
	}
	h.saveLive(conv, live)
	h.notifyLocked()
}

// runTool executes a resolved tool and settles it.
func (h *Harness) runTool(ctx context.Context, conv ConversationID, taskID TaskID, input ToolTaskInput, call ToolHookCall, tool *Tool, args json.RawMessage, api *ToolAPI) {
	started := time.Now()
	result, err := tool.Execute(ctx, args, api)
	duration := time.Since(started).Milliseconds()
	api.end()
	// Children left running when the invocation returns (also on a throw) end first.
	h.abortNestedOf(taskID)

	if ctx.Err() != nil || h.isDead() {
		// A close is not a crash: the task stays as committed for the next process.
		if h.Closed() {
			return
		}
		h.settleFromSlot(conv, taskID, input, call, tool, "aborted",
			fmt.Sprintf("Tool %s was aborted", call.Name), true)
		return
	}
	ending := Outcome{Status: OutcomeCompleted}
	if err != nil {
		msg := err.Error()
		result = ToolResult{
			IsError:     true,
			Diagnostics: []Diagnostic{diag("tool_error", msg)},
		}
		ending = Outcome{Status: OutcomeFailed, Error: &TaskError{Message: fmt.Sprintf("Tool %s threw", call.Name)}}
	}
	// What the tool reported through its api is the fallback for what the
	// result omits.
	reportedText, reportedDetails, reportedDiagnostics := api.snapshot()
	final := h.finalResult(ctx, input, call, tool, result, reportedText, reportedDetails, reportedDiagnostics, api)
	h.finishToolTask(conv, taskID, input, call, ending, &final, duration)
}

// finalResult applies afterTool, output bounds and structured-output rules to an
// executed tool result.
func (h *Harness) finalResult(ctx context.Context, input ToolTaskInput, call ToolHookCall, tool *Tool, result ToolResult, reportedText string, reportedDetails any, reportedDiagnostics []Diagnostic, api *ToolAPI) ToolResult {
	limits := tool.OutputLimits.resolved()
	var output agentcore.ContentList
	retained := ""
	droppedBytes, droppedLines := 0, 0
	if result.HasOutput {
		output = result.Output
	} else {
		var dropped int
		var lostLines int
		retained, dropped, lostLines = boundText(reportedText, limits)
		droppedBytes, droppedLines = dropped, lostLines
		if retained != "" {
			output = agentcore.ContentList{agentcore.NewTextContent(retained)}
		} else {
			output = agentcore.ContentList{}
		}
	}
	final := ToolResult{
		Output:           output,
		HasOutput:        true,
		StructuredOutput: cloneRaw(result.StructuredOutput),
		IsError:          result.IsError,
		Details:          result.Details,
		Diagnostics:      append(append([]Diagnostic(nil), reportedDiagnostics...), result.Diagnostics...),
		Usage:            result.Usage,
		Control:          result.Control,
	}
	if final.Details == nil {
		final.Details = reportedDetails
	}
	for _, hook := range h.toolHooks() {
		if hook.AfterTool == nil {
			continue
		}
		replacement, err := hook.AfterTool(ctx, call, final, api)
		if err != nil {
			h.report(err)
			continue
		}
		if replacement != nil {
			final = *replacement
		}
	}
	harnessDiags := []Diagnostic{}
	// The retained output's truncation applies only while afterTool kept that
	// output; a replacement is bounded anew below.
	if !result.HasOutput && droppedBytes > 0 && contentEqual(final.Output, output) {
		harnessDiags = append(harnessDiags, truncatedDiagnostic(droppedLines, droppedBytes, limits.Retain))
	}
	bounded, bBytes, bLines := boundContent(final.Output, limits)
	if bBytes > 0 {
		harnessDiags = append(harnessDiags, truncatedDiagnostic(bLines, bBytes, limits.Retain))
	}
	final.Output = bounded
	// Structured output contract.
	if broken := structuredOutputError(tool, final); broken != "" {
		final.StructuredOutput = nil
		if input.Kind == "nested" {
			final.IsError = true
			harnessDiags = append(harnessDiags, diag("invalid_structured_output", broken))
		} else {
			h.report(errors.New(broken))
		}
	}
	if input.Kind == "nested" && len(tool.StructuredOutputSchema) == 0 {
		final.StructuredOutput = rawJSON(outputValue(bounded))
	}
	if input.Kind == "nested" && len(tool.StructuredOutputSchema) > 0 && final.IsError && final.StructuredOutput == nil {
		hasErrorDiag := false
		for _, d := range append(append([]Diagnostic(nil), final.Diagnostics...), harnessDiags...) {
			if d.Severity == "error" {
				hasErrorDiag = true
			}
		}
		if !hasErrorDiag {
			if text := OutputText(bounded); text != "" {
				harnessDiags = append(harnessDiags, diag("tool_error", text))
			}
		}
	}
	final.Diagnostics = append(append([]Diagnostic(nil), final.Diagnostics...), harnessDiags...)
	return final
}

// afterHarnessResult normalizes a result the Harness wrote itself: afterTool,
// bounds, no structured output the tool never produced.
func (h *Harness) afterHarnessResult(ctx context.Context, call ToolHookCall, tool *Tool, result ToolResult, api *ToolAPI) ToolResult {
	limits := OutputLimits{}.resolved()
	if tool != nil {
		limits = tool.OutputLimits.resolved()
	}
	final := result
	for _, hook := range h.toolHooks() {
		if hook.AfterTool == nil {
			continue
		}
		replacement, err := hook.AfterTool(ctx, call, final, api)
		if err != nil {
			h.report(err)
			continue
		}
		if replacement != nil {
			final = *replacement
		}
	}
	harnessDiags := []Diagnostic{}
	bounded, droppedBytes, droppedLines := boundContent(final.Output, limits)
	if droppedBytes > 0 {
		harnessDiags = append(harnessDiags, truncatedDiagnostic(droppedLines, droppedBytes, limits.Retain))
	}
	final.Output = bounded
	final.HasOutput = true
	final.StructuredOutput = nil
	if call.Parent != nil && !final.IsError && tool != nil && len(tool.StructuredOutputSchema) == 0 {
		final.StructuredOutput = rawJSON(outputValue(bounded))
	}
	final.Diagnostics = append(append([]Diagnostic(nil), final.Diagnostics...), harnessDiags...)
	return final
}

// settleFromSlot builds an error result from the slot's durable partial output.
func (h *Harness) settleFromSlot(conv ConversationID, taskID TaskID, input ToolTaskInput, call ToolHookCall, tool *Tool, code, message string, runHooks bool) {
	live := h.loadLive(conv)
	result := ToolResult{Output: agentcore.ContentList{}, HasOutput: true, IsError: true}
	api := &ToolAPI{TaskID: taskID, ConversationID: conv, CallID: call.ID, Parent: call.Parent, h: h}
	if tool == nil {
		tool = &Tool{Name: call.Name}
	}
	api.tool = tool
	if input.Kind == "model" {
		for _, slot := range live.Tools {
			if slot.TaskID != taskID {
				continue
			}
			if slot.Status == "done" {
				// Already settled: nothing to do.
				return
			}
			if slot.Output != "" {
				result.Output = agentcore.ContentList{agentcore.NewTextContent(slot.Output)}
			}
			if slot.Details != nil {
				result.Details = json.RawMessage(slot.Details)
			}
			result.Diagnostics = append([]Diagnostic(nil), slot.Diagnostics...)
		}
	}
	if input.Kind == "nested" {
		for _, slot := range live.NestedTools {
			if slot.TaskID != taskID {
				continue
			}
			if slot.Output != "" {
				result.Output = agentcore.ContentList{agentcore.NewTextContent(slot.Output)}
			}
			if slot.Details != nil {
				result.Details = json.RawMessage(slot.Details)
			}
			result.Diagnostics = append([]Diagnostic(nil), slot.Diagnostics...)
		}
	}
	result.Diagnostics = append(result.Diagnostics, diag(code, message))
	var final ToolResult
	if runHooks {
		final = h.afterHarnessResult(context.Background(), call, tool, result, api)
	} else {
		final = result
	}
	ending := Outcome{Status: OutcomeCompleted}
	switch code {
	case "aborted":
		ending = Outcome{Status: OutcomeAborted}
	case "interrupted", "abandoned", "restart_limit":
		ending = Outcome{Status: OutcomeFailed, Error: &TaskError{
			Message: message,
			Detail:  rawJSON(map[string]string{"reason": code}),
		}}
	}
	h.finishToolTask(conv, taskID, input, call, ending, &final, -1)
}

// finishToolTask commits a tool task's terminal state and its result entry or
// nested document. duration < 0 means the tool never executed.
func (h *Harness) finishToolTask(conv ConversationID, taskID TaskID, input ToolTaskInput, call ToolHookCall, ending Outcome, result *ToolResult, duration int64) {
	h.mu.Lock()
	if h.dead || h.closed {
		h.mu.Unlock()
		return
	}
	task, ok := h.store.GetTask(taskID)
	if !ok || task.Terminal() {
		h.mu.Unlock()
		return
	}
	live := h.loadLive(conv)
	var settled ToolTaskResult
	if input.Kind == "nested" {
		nested := NestedResult{
			TaskID:      taskID,
			IsError:     result.IsError,
			Diagnostics: append([]Diagnostic(nil), result.Diagnostics...),
			Usage:       result.Usage,
		}
		if result.StructuredOutput != nil {
			nested.StructuredOutput = cloneRaw(result.StructuredOutput)
		}
		if result.Details != nil {
			if raw, ok := result.Details.(json.RawMessage); ok {
				nested.Details = cloneRaw(raw)
			} else if raw, err := json.Marshal(result.Details); err == nil {
				nested.Details = raw
			} else {
				h.report(err)
			}
		}
		if duration >= 0 {
			d := duration
			nested.DurationMs = &d
		}
		doc, _ := h.loadNested(conv, input.Parent, input.Key)
		doc.Result = &nested
		h.saveNested(conv, input.Parent, input.Key, doc)
		for i := range live.NestedTools {
			if live.NestedTools[i].TaskID == taskID {
				live.NestedTools[i].Status = "done"
				live.NestedTools[i].Summary = summaryOf(nested, result.Output)
				live.NestedTools[i].Details = nested.Details
			}
		}
		settled = ToolTaskResult{Kind: "nested"}
	} else {
		entry := h.appendToolResultLocked(conv, call, *result, duration)
		for i := range live.Tools {
			if live.Tools[i].TaskID == taskID {
				live.Tools[i].Status = "done"
				live.Tools[i].Entry = entry.ID
			}
		}
		settled = ToolTaskResult{Kind: "model", EntryID: entry.ID}
		if ending.Status == OutcomeCompleted && result.Control != nil {
			settled.Control = result.Control
		}
		// The caller's nested receipts retire with it.
		h.retireNestedLocked(conv, taskID, &live)
	}
	h.saveLive(conv, live)
	outcome := ending
	outcome.Result = rawJSON(settled)
	_, _ = h.store.UpdateTask(taskID, func(t *TaskRecord) {
		t.State.Status = TaskTerminal
		t.EndedAt = h.now()
		t.State.Checkpoint = nil
		t.State.Outcome = &outcome
	})
	h.notifyLocked()
	h.mu.Unlock()
}

// retireNestedLocked removes a settled caller's nested slots and documents.
func (h *Harness) retireNestedLocked(conv ConversationID, parent TaskID, live *LiveState) {
	prefix := string(parent) + "/"
	for key := range h.store.ListDocs(conv, docNested) {
		if strings.HasPrefix(key, prefix) {
			h.deleteNested(conv, parent, strings.TrimPrefix(key, prefix))
		}
	}
	kept := live.NestedTools[:0]
	for _, slot := range live.NestedTools {
		if slot.ParentTaskID != parent {
			kept = append(kept, slot)
		}
	}
	live.NestedTools = kept
}

func (h *Harness) appendToolResultLocked(conv ConversationID, call ToolHookCall, result ToolResult, duration int64) EntryRecord {
	diagnostics := append([]Diagnostic(nil), result.Diagnostics...)
	content := append(agentcore.ContentList(nil), result.Output...)
	if len(diagnostics) > 0 {
		content = append(content, agentcore.NewTextContent(renderDiagnostics(diagnostics)))
	}
	msg := agentcore.ToolResultMessage{
		RoleField:  agentcore.RoleToolResult,
		ToolCallID: call.ID,
		ToolName:   call.Name,
		Content:    content,
		IsError:    result.IsError,
		Timestamp:  h.now(),
	}
	if result.Details != nil {
		msg.Details = result.Details
	}
	if duration > 0 {
		d := duration
		msg.DurationMs = &d
	}
	return h.store.AppendEntry(conv, EntryRecord{
		Kind:  "pi.tool-result",
		Model: []agentcore.Message{msg},
		Data:  rawJSON(map[string]any{"diagnostics": diagnostics}),
	})
}

func renderDiagnostics(diagnostics []Diagnostic) string {
	var b strings.Builder
	b.WriteString("<harness>\n")
	for _, d := range diagnostics {
		b.WriteString("[" + d.Severity + "] " + d.Message + "\n")
	}
	b.WriteString("</harness>")
	return b.String()
}

// faultTool records a task that could not even decode its input.
func (h *Harness) faultTool(taskID TaskID, err error) {
	h.mu.Lock()
	if h.dead {
		h.mu.Unlock()
		return
	}
	_, _ = h.store.UpdateTask(taskID, func(t *TaskRecord) {
		t.State.Status = TaskTerminal
		t.EndedAt = h.now()
		t.State.Outcome = &Outcome{Status: OutcomeFaulted, Error: &TaskError{Message: err.Error()}}
	})
	h.notifyLocked()
	h.mu.Unlock()
}

// harnessError builds a Harness-written error result.
func harnessError(code, message string) ToolResult {
	return ToolResult{Output: agentcore.ContentList{}, HasOutput: true, IsError: true,
		Diagnostics: []Diagnostic{diag(code, message)}}
}

func truncatedDiagnostic(lines, bytes int, retain string) Diagnostic {
	kept := ""
	if retain == "head" {
		kept = " to its beginning"
	} else if retain == "tail" {
		kept = " to its end"
	}
	return Diagnostic{Severity: "warn", Code: "truncated",
		Message: fmt.Sprintf("Output truncated%s: %d lines, %d bytes dropped", kept, lines, bytes)}
}

// outputValue maps bounded output to the value programs get from a schema-less
// tool.
func outputValue(content agentcore.ContentList) any {
	if len(content) == 0 {
		return ""
	}
	if len(content) > 1 {
		return content
	}
	if t, ok := content[0].(agentcore.TextContent); ok {
		return t.Text
	}
	return content[0]
}

// boundContent bounds the text items of result content.
func boundContent(content agentcore.ContentList, limits OutputLimits) (agentcore.ContentList, int, int) {
	var texts []string
	for _, item := range content {
		if t, ok := item.(agentcore.TextContent); ok {
			texts = append(texts, t.Text)
		}
	}
	if len(texts) == 0 {
		return content, 0, 0
	}
	bounded, droppedBytes, droppedLines := boundText(strings.Join(texts, ""), limits)
	if droppedBytes == 0 {
		return content, 0, 0
	}
	var keepIndex = -1
	if limits.Retain == "tail" {
		for i := len(content) - 1; i >= 0; i-- {
			if _, ok := content[i].(agentcore.TextContent); ok {
				keepIndex = i
				break
			}
		}
	} else {
		for i := 0; i < len(content); i++ {
			if _, ok := content[i].(agentcore.TextContent); ok {
				keepIndex = i
				break
			}
		}
	}
	out := make(agentcore.ContentList, 0, len(content))
	for i, item := range content {
		if _, ok := item.(agentcore.TextContent); !ok {
			out = append(out, item)
			continue
		}
		if i == keepIndex {
			out = append(out, agentcore.NewTextContent(bounded))
		}
	}
	return out, droppedBytes, droppedLines
}

// boundText applies PI's chord output window (whole lines; UTF-8 safe).
func boundText(text string, limits OutputLimits) (string, int, int) {
	totalBytes := len(text)
	lines := splitLineCount(text)
	if lines <= limits.MaxLines && totalBytes <= limits.MaxBytes {
		return text, 0, 0
	}
	data := []byte(text)
	var from, to int
	if limits.Retain == "tail" {
		to = len(data)
		// Find the start of the last maxLines lines.
		count := 1
		last := len(data) - 1
		if len(data) > 0 && data[len(data)-1] == '\n' {
			last--
		}
		start := 0
		for i := last; i >= 0; i-- {
			if data[i] == '\n' {
				if count == limits.MaxLines {
					start = i + 1
					break
				}
				count++
			}
		}
		from = start
		if len(data)-from > limits.MaxBytes {
			from = len(data) - limits.MaxBytes
			if nl := bytes.IndexByte(data[from:], '\n'); nl >= 0 && from+nl+1 < len(data) {
				from += nl + 1
			} else {
				for from < len(data) && data[from]&0xc0 == 0x80 {
					from++
				}
			}
		}
	} else {
		from = 0
		to = len(data)
		count := 0
		for i := 0; i < len(data); i++ {
			if data[i] == '\n' {
				count++
				if count == limits.MaxLines {
					to = i + 1
					break
				}
			}
		}
		if to > limits.MaxBytes {
			if nl := bytes.LastIndexByte(data[:limits.MaxBytes], '\n'); nl >= 0 {
				to = nl + 1
			} else {
				to = limits.MaxBytes
				for to > 0 && data[to]&0xc0 == 0x80 {
					to--
				}
			}
		}
	}
	kept := string(data[from:to])
	return kept, totalBytes - len(kept), lines - splitLineCount(kept)
}

func contentEqual(a, b agentcore.ContentList) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if OutputText(agentcore.ContentList{a[i]}) != OutputText(agentcore.ContentList{b[i]}) {
			return false
		}
		if _, ok := a[i].(agentcore.TextContent); ok {
			if _, ok := b[i].(agentcore.TextContent); !ok {
				return false
			}
		}
	}
	return true
}

func splitLineCount(text string) int {
	if text == "" {
		return 0
	}
	n := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		n++
	}
	return n
}

// summaryOf builds a nested slot summary.
func summaryOf(result NestedResult, output agentcore.ContentList) *NestedSummary {
	summary := &NestedSummary{IsError: result.IsError, DurationMs: result.DurationMs, Usage: result.Usage}
	if result.IsError {
		text := ""
		for _, d := range result.Diagnostics {
			if d.Severity == "error" {
				text += d.Message + "\n"
			}
		}
		if text == "" {
			text = OutputText(output)
		}
		text = strings.TrimSuffix(text, "\n")
		if len(text) > 500 {
			text = text[:500]
		}
		summary.Error = text
	}
	return summary
}
