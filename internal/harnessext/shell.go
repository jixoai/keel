package harnessext

// keel.shell is the built-in execution plugin. The harness-owned `bash`
// aggregate tool stays in the model surface even when this plugin is disabled;
// the plugin is the action backend (and the lifecycle gate: a disabled plugin
// makes bash actions answer plugin_disabled). It delegates the actual sandbox
// and job work to internal/bashbridge and internal/jobqueue, so this file owns
// only the model contract: argument validation, bounded rendering and the
// typed error vocabulary.
//
// The tool names below are internal (ToolSpec.Internal): they never appear in
// the model-visible manifest. The model reaches them through bash {action:...}.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/jobqueue"
	"github.com/jixoai/keel/internal/store"
)

const (
	// ShellPluginID is the built-in plugin id.
	ShellPluginID      = "keel.shell"
	ShellPluginVersion = "1.0.0"

	shellRunTool  = "keel.shell.run"
	shellJobsTool = "keel.shell.jobs"
)

// Wire error codes. These mirror toolsurface's typed codes; harnessext cannot
// import toolsurface (toolsurface imports harnessext), so the vocabulary is
// duplicated here and pinned by tests.
const (
	shellCodeUnknownAction    = "unknown_action"
	shellCodeInvalidArguments = "invalid_arguments"
	shellCodeNotFound         = "not_found"
	shellCodePluginDisabled   = "plugin_disabled"
	shellCodeBackendFailed    = "backend_error"
)

// ShellOptions configures the shell plugin.
type ShellOptions struct {
	// Service returns the job manager. It is a function because the plugin is
	// assembled before the product layer that owns it.
	Service func() *jobqueue.Manager
	// TenantID is the fallback tenant when no run scope is installed (tests,
	// CLI-driven calls). uuid.Nil is the no-auth dev tenant.
	TenantID uuid.UUID
}

// NewShellPlugin builds keel.shell. It contributes no model-facing tools: its
// two internal tools are the bash aggregate tool's backend.
func NewShellPlugin(opts ShellOptions) *Plugin {
	return &Plugin{
		ID:       ShellPluginID,
		Version:  ShellPluginVersion,
		Provides: []string{"cap:shell"},
		// No PromptSection on purpose: the timeout/job semantics live in the
		// harness-owned bash tool description, and the prompt-seam inventory
		// test treats plugin sections as an ordered set owned by keel.creation.
		Tools: []ToolSpec{
			{
				Name:   shellRunTool,
				Schema: shellRunSchema(),
				// Internal: backend of the bash aggregate tool, never published.
				Internal: true,
				Execute: func(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
					return shellInvoke(ctx, opts, "run", args)
				},
			},
			{
				Name:     shellJobsTool,
				Schema:   shellJobsSchema(),
				Internal: true,
				Execute: func(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
					return shellInvoke(ctx, opts, "jobs", args)
				},
			},
		},
	}
}

// ShellBackend is the bash aggregate tool's backend. It routes through the
// plugin manager's InvokeToolResult, so lifecycle gating (disabled plugin ->
// ErrPluginState -> plugin_disabled) is the manager's, exactly like
// keel.creation.
type ShellBackend struct {
	Manager func() *Manager
}

// Invoke implements the bash aggregate tool's backend contract.
func (b ShellBackend) Invoke(ctx context.Context, action string, args json.RawMessage) (agentcore.AgentToolResult, error) {
	manager := (*Manager)(nil)
	if b.Manager != nil {
		manager = b.Manager()
	}
	if manager == nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("harnessext: %w: the plugin manager is not wired", ErrPluginState)
	}
	tool := shellRunTool
	switch action {
	case "run":
	case "jobs":
		tool = shellJobsTool
	default:
		return agentcore.AgentToolResult{}, fmt.Errorf("harnessext: unknown bash action %q", action)
	}
	return manager.InvokeToolResult(ctx, ShellPluginID, tool, args)
}

func shellInvoke(ctx context.Context, opts ShellOptions, action string, raw json.RawMessage) (agentcore.AgentToolResult, error) {
	var manager *jobqueue.Manager
	if opts.Service != nil {
		manager = opts.Service()
	}
	if manager == nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("harnessext: %w: the bash job manager is not wired", ErrPluginState)
	}
	tenant, session := shellScope(ctx, opts.TenantID)
	switch action {
	case "run":
		return shellRun(ctx, manager, tenant, session, raw)
	case "jobs":
		return shellJobs(ctx, manager, tenant, raw)
	default:
		return shellError(shellCodeUnknownAction, "unknown bash action "+action, map[string]any{
			"availableActions": []string{"run", "jobs"},
		}), nil
	}
}

func shellScope(ctx context.Context, fallback uuid.UUID) (uuid.UUID, uuid.UUID) {
	tenant := fallback
	session := uuid.Nil
	if scope, ok := RunScopeFromContext(ctx); ok {
		if scope.TenantID != uuid.Nil {
			tenant = scope.TenantID
		}
		session = scope.SessionID
	}
	return tenant, session
}

type shellRunInput struct {
	Command   string            `json:"command"`
	Cwd       string            `json:"cwd"`
	TimeoutMs int64             `json:"timeoutMs"`
	Env       map[string]string `json:"env"`
}

const (
	// shellMaxTimeoutMs bounds the wait before the command becomes a job. It is
	// not a command lifetime: a job may run longer and is stopped explicitly.
	shellMaxTimeoutMs  = 10 * 60 * 1000
	shellMaxEnvEntries = 32
	shellMaxEnvValue   = 4 << 10
	shellMaxCommand    = 64 << 10
)

// shellEnvAllowlist is the model's env surface. Script-local assignments
// (FOO=bar cmd) remain available inside the command; the tool-level map is
// limited to the variables that describe the sandbox session, so a caller
// cannot inject loader/startup hooks.
var shellEnvAllowlist = map[string]bool{
	"PATH": true, "HOME": true, "TMPDIR": true, "TEMP": true, "TMP": true,
	"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "LC_MESSAGES": true,
	"LC_NUMERIC": true, "LC_TIME": true, "TZ": true, "TERM": true,
	"NO_COLOR": true, "FORCE_COLOR": true, "COLUMNS": true, "LINES": true,
}

func shellRun(ctx context.Context, manager *jobqueue.Manager, tenant, session uuid.UUID, raw json.RawMessage) (agentcore.AgentToolResult, error) {
	var input shellRunInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return shellError(shellCodeInvalidArguments, "bash run arguments are not valid JSON: "+err.Error(), nil), nil
	}
	if strings.TrimSpace(input.Command) == "" {
		return shellError(shellCodeInvalidArguments, "bash run needs a non-empty command", nil), nil
	}
	if len(input.Command) > shellMaxCommand {
		return shellError(shellCodeInvalidArguments, fmt.Sprintf("command is too large (max %d bytes)", shellMaxCommand), nil), nil
	}
	if input.TimeoutMs < 0 {
		return shellError(shellCodeInvalidArguments, "timeoutMs must be >= 0", nil), nil
	}
	if input.TimeoutMs > shellMaxTimeoutMs {
		return shellError(shellCodeInvalidArguments, fmt.Sprintf("timeoutMs must be <= %d", shellMaxTimeoutMs), nil), nil
	}
	if err := validateShellEnv(input.Env); err != nil {
		return shellError(shellCodeInvalidArguments, err.Error(), nil), nil
	}
	timeout := time.Duration(input.TimeoutMs) * time.Millisecond
	result, err := manager.Run(ctx, tenant, session, jobqueue.RunRequest{
		Command: input.Command,
		Cwd:     input.Cwd,
		Env:     input.Env,
		Timeout: timeout,
	})
	if err != nil {
		if errors.Is(err, jobqueue.ErrLimit) {
			return shellError(shellCodeBackendFailed,
				"the background job limit is reached; stop a running job with bash {action:\"jobs\", operation:\"kill\"} before starting another",
				map[string]any{"reason": "job_limit_reached"}), nil
		}
		return shellError(shellCodeBackendFailed, err.Error(), nil), nil
	}
	return shellRunResult(result), nil
}

func validateShellEnv(env map[string]string) error {
	if len(env) > shellMaxEnvEntries {
		return fmt.Errorf("env has %d entries; at most %d are allowed", len(env), shellMaxEnvEntries)
	}
	for name, value := range env {
		if !shellEnvAllowlist[name] {
			return fmt.Errorf("env %q is not allowed; the tool-level env surface is %s (use a script-local assignment for other values)",
				name, strings.Join(shellEnvNames(), ", "))
		}
		if len(value) > shellMaxEnvValue {
			return fmt.Errorf("env %q is too large (max %d bytes)", name, shellMaxEnvValue)
		}
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("env %q contains a NUL byte", name)
		}
	}
	return nil
}

func shellEnvNames() []string {
	names := make([]string, 0, len(shellEnvAllowlist))
	for name := range shellEnvAllowlist {
		names = append(names, name)
	}
	// Stable order for the error message.
	sortStrings(names)
	return names
}

func shellRunResult(result jobqueue.RunResult) agentcore.AgentToolResult {
	document := map[string]any{
		"status":     result.Status,
		"timedOut":   result.TimedOut,
		"cwd":        result.Cwd,
		"durationMs": result.Duration.Milliseconds(),
		"stdout":     shellOutputDocument(result.Stdout),
		"stderr":     shellOutputDocument(result.Stderr),
	}
	if result.JobID != nil {
		document["jobId"] = result.JobID.String()
	}
	if result.ExitCode != nil {
		document["exitCode"] = *result.ExitCode
	}
	if result.Reason != "" {
		document["reason"] = result.Reason
	}
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(shellRunText(result))},
		StructuredContent: document,
	}
}

func shellOutputDocument(out jobqueue.Output) map[string]any {
	document := map[string]any{
		"bytes": out.Bytes,
	}
	if out.Head != "" {
		document["head"] = out.Head
	}
	if out.Tail != "" {
		document["tail"] = out.Tail
	}
	if out.Truncated {
		document["truncated"] = true
	}
	if out.BlobID != nil {
		document["blobId"] = out.BlobID.String()
	}
	if out.Error != "" {
		document["error"] = out.Error
	}
	return document
}

// shellRunText renders the bounded model view. It never grows with the output:
// only the head/tail windows and fixed labels are included.
func shellRunText(result jobqueue.RunResult) string {
	var b strings.Builder
	if result.TimedOut {
		jobID := ""
		if result.JobID != nil {
			jobID = result.JobID.String()
		}
		fmt.Fprintf(&b, "Command exceeded timeoutMs and continues as background job %s.\n", jobID)
		fmt.Fprintf(&b, "Query it with bash {action:\"jobs\", operation:\"status\", jobId:%q}; stop it with operation:\"kill\".\n", jobID)
		b.WriteString("The output below is what it had produced when the tool returned.\n\n")
	}
	shellWriteStream(&b, "stdout", result.Stdout)
	shellWriteStream(&b, "stderr", result.Stderr)
	b.WriteString("\nstatus=" + result.Status)
	if result.ExitCode != nil {
		fmt.Fprintf(&b, " exitCode=%d", *result.ExitCode)
	}
	if result.Reason != "" {
		b.WriteString(" reason=" + result.Reason)
	}
	b.WriteString("\n")
	return b.String()
}

func shellWriteStream(b *strings.Builder, label string, out jobqueue.Output) {
	fmt.Fprintf(b, "%s (%d bytes", label, out.Bytes)
	if out.Truncated {
		b.WriteString(", truncated")
	}
	if out.BlobID != nil {
		b.WriteString(", blobId=" + out.BlobID.String())
	}
	b.WriteString("):\n")
	if out.Head == "" && out.Tail == "" {
		b.WriteString("(empty)\n")
		return
	}
	b.WriteString(out.Head)
	omitted := out.Bytes - int64(len(out.Head)) - int64(len(out.Tail))
	if omitted > 0 {
		fmt.Fprintf(b, "\n... [%d bytes omitted] ...\n", omitted)
	}
	b.WriteString(out.Tail)
	if !strings.HasSuffix(out.Tail, "\n") {
		b.WriteString("\n")
	}
}

type shellJobsInput struct {
	Operation string `json:"operation"`
	JobID     string `json:"jobId"`
}

func shellJobs(ctx context.Context, manager *jobqueue.Manager, tenant uuid.UUID, raw json.RawMessage) (agentcore.AgentToolResult, error) {
	var input shellJobsInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return shellError(shellCodeInvalidArguments, "bash jobs arguments are not valid JSON: "+err.Error(), nil), nil
	}
	switch input.Operation {
	case "list":
		jobs, err := manager.List(ctx, tenant, 20)
		if err != nil {
			return shellError(shellCodeBackendFailed, err.Error(), nil), nil
		}
		documents := make([]map[string]any, 0, len(jobs))
		for _, job := range jobs {
			documents = append(documents, shellJobSummary(job))
		}
		document := map[string]any{"jobs": documents}
		return shellJSONResult(document, shellJobsListText(jobs)), nil
	case "status", "adopt", "kill":
		jobID, err := uuid.Parse(strings.TrimSpace(input.JobID))
		if err != nil {
			return shellError(shellCodeInvalidArguments, "jobId must be a job id returned by bash run", nil), nil
		}
		var (
			job jobqueue.JobView
		)
		switch input.Operation {
		case "status":
			job, err = manager.Status(ctx, tenant, jobID)
		case "adopt":
			job, err = manager.Adopt(ctx, tenant, jobID)
		case "kill":
			job, err = manager.Kill(ctx, tenant, jobID)
		}
		if err != nil {
			if errors.Is(err, store.ErrNoBashJob) {
				return shellError(shellCodeNotFound, "no such job in this tenant: "+jobID.String(), nil), nil
			}
			return shellError(shellCodeBackendFailed, err.Error(), nil), nil
		}
		document := map[string]any{"job": shellJobDetail(job)}
		return shellJSONResult(document, shellJobDetailText(job)), nil
	default:
		return shellError(shellCodeInvalidArguments,
			fmt.Sprintf("unknown jobs operation %q", input.Operation),
			map[string]any{"availableOperations": []string{"list", "status", "adopt", "kill"}}), nil
	}
}

// shellJobsListText renders the list into the model-visible text block. The
// structured document carries the same facts for UIs; the model only reads
// Content, so a bare count would hide every job. Commands are truncated and the
// list is capped by the manager (20), keeping this block bounded.
func shellJobsListText(jobs []jobqueue.JobView) string {
	if len(jobs) == 0 {
		return "no background jobs"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d background job(s):\n", len(jobs))
	for _, job := range jobs {
		state := job.Status
		if job.Live {
			state += " live"
		}
		fmt.Fprintf(&b, "- %s %s cwd=%s created=%s command=%q",
			job.JobID, state, job.Cwd, job.CreatedAt.UTC().Format(time.RFC3339),
			truncateShellText(job.Command, 160))
		if job.ExitCode != nil {
			fmt.Fprintf(&b, " exitCode=%d", *job.ExitCode)
		}
		if job.Reason != "" {
			b.WriteString(" reason=" + job.Reason)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func truncateShellText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-3] + "..."
}

func shellJobSummary(job jobqueue.JobView) map[string]any {
	document := map[string]any{
		"jobId":       job.JobID.String(),
		"status":      job.Status,
		"command":     truncateShellText(job.Command, 400),
		"cwd":         job.Cwd,
		"createdAt":   job.CreatedAt.UTC().Format(time.RFC3339),
		"stdoutBytes": job.Stdout.Bytes,
		"stderrBytes": job.Stderr.Bytes,
		"live":        job.Live,
	}
	if job.ExitCode != nil {
		document["exitCode"] = *job.ExitCode
	}
	if job.Reason != "" {
		document["reason"] = job.Reason
	}
	return document
}

func shellJobDetail(job jobqueue.JobView) map[string]any {
	document := shellJobSummary(job)
	document["stdout"] = shellOutputDocument(job.Stdout)
	document["stderr"] = shellOutputDocument(job.Stderr)
	if job.FinishedAt != nil {
		document["finishedAt"] = job.FinishedAt.UTC().Format(time.RFC3339)
	}
	if job.SessionID != nil {
		document["sessionId"] = job.SessionID.String()
	}
	return document
}

func shellJobDetailText(job jobqueue.JobView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "job %s status=%s live=%t\ncommand: %s\ncwd: %s\n", job.JobID, job.Status, job.Live, job.Command, job.Cwd)
	if job.ExitCode != nil {
		fmt.Fprintf(&b, "exitCode: %d\n", *job.ExitCode)
	}
	if job.Reason != "" {
		b.WriteString("reason: " + job.Reason + "\n")
	}
	shellWriteStream(&b, "stdout", job.Stdout)
	shellWriteStream(&b, "stderr", job.Stderr)
	return b.String()
}

func shellJSONResult(document map[string]any, text string) agentcore.AgentToolResult {
	encoded, _ := json.Marshal(document)
	if strings.TrimSpace(text) == "" {
		text = string(encoded)
	}
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(text)},
		StructuredContent: document,
	}
}

// shellError mirrors toolsurface.ErrorResult's wire shape without importing
// toolsurface (which would be a cycle).
func shellError(code, message string, extra map[string]any) agentcore.AgentToolResult {
	document := map[string]any{
		"code":    code,
		"tool":    "bash",
		"message": strings.TrimSpace(message),
	}
	for key, value := range extra {
		document[key] = value
	}
	encoded, _ := json.Marshal(map[string]any{"error": document})
	isError := true
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(encoded))},
		StructuredContent: map[string]any{"error": document},
		IsError:           &isError,
	}
}

func shellRunSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"},"command":{"type":"string"},"cwd":{"type":"string"},"timeoutMs":{"type":"integer","minimum":0},"env":{"type":"object"}},"required":["command"],"additionalProperties":true}`)
}

func shellJobsSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"},"operation":{"type":"string"},"jobId":{"type":"string"}},"required":["operation"],"additionalProperties":true}`)
}
