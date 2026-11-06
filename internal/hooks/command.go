package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

// DefaultCommandTimeout is the default timeout for one command hook invocation.
const DefaultCommandTimeout = 5 * time.Second

// FailurePolicy decides what a command-hook failure (timeout, non-zero exit,
// unparseable output) means for a gating event. "continue" fails open (the run
// continues), "deny" fails closed. The default is continue.
type FailurePolicy string

const (
	FailureContinue FailurePolicy = "continue"
	FailureDeny     FailurePolicy = "deny"
)

// CommandHookConfig describes one command hook. Command is an argv vector (no
// shell); Event is one of the PI event names; Timeout is a Go duration string
// such as "5s" (default DefaultCommandTimeout).
type CommandHookConfig struct {
	Name          string            `json:"name"`
	Event         string            `json:"event"`
	Command       []string          `json:"command"`
	Timeout       string            `json:"timeout,omitempty"`
	FailurePolicy FailurePolicy     `json:"failurePolicy,omitempty"`
	Observer      bool              `json:"observer,omitempty"`
	Cwd           string            `json:"cwd,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
}

// CommandHooksFile is the JSON document accepted by LoadCommandHooks:
//
//	{"hooks": [ {...}, {...} ]}
//
// A bare array is also accepted.
type CommandHooksFile struct {
	Hooks []CommandHookConfig `json:"hooks"`
}

// LoadCommandHooks reads and validates a JSON configuration file.
func LoadCommandHooks(path string) ([]CommandHookConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("hooks: read command config: %w", err)
	}
	return ParseCommandHooks(bytes.NewReader(data))
}

// ParseCommandHooks decodes and validates command hook configuration.
func ParseCommandHooks(reader io.Reader) ([]CommandHookConfig, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("hooks: read command config: %w", err)
	}
	var file CommandHooksFile
	if err := json.Unmarshal(data, &file); err == nil && file.Hooks != nil {
		for i := range file.Hooks {
			if err := file.Hooks[i].validate(); err != nil {
				return nil, err
			}
		}
		return file.Hooks, nil
	}
	var list []CommandHookConfig
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("hooks: decode command config: %w", err)
	}
	for i := range list {
		if err := list[i].validate(); err != nil {
			return nil, err
		}
	}
	return list, nil
}

func (c CommandHookConfig) validate() error {
	if c.Event == "" || !KnownEvent(c.Event) {
		return fmt.Errorf("hooks: command hook %q has unknown event %q", c.Name, c.Event)
	}
	if len(c.Command) == 0 || strings.TrimSpace(c.Command[0]) == "" {
		return fmt.Errorf("hooks: command hook %q has no command", c.Name)
	}
	if _, err := c.timeout(); err != nil {
		return err
	}
	switch c.FailurePolicy {
	case "", FailureContinue, FailureDeny:
	default:
		return fmt.Errorf("hooks: command hook %q has unknown failure policy %q", c.Name, c.FailurePolicy)
	}
	return nil
}

func (c CommandHookConfig) timeout() (time.Duration, error) {
	if strings.TrimSpace(c.Timeout) == "" {
		return DefaultCommandTimeout, nil
	}
	timeout, err := time.ParseDuration(c.Timeout)
	if err != nil {
		return 0, fmt.Errorf("hooks: command hook %q has invalid timeout %q: %w", c.Name, c.Timeout, err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("hooks: command hook %q timeout must be positive", c.Name)
	}
	return timeout, nil
}

func (c CommandHookConfig) errorPolicy() ErrorPolicy {
	if c.FailurePolicy == FailureDeny {
		return ErrorPolicyFailClosed
	}
	return ErrorPolicyFailOpen
}

// commandInput is the JSON written to a command hook's stdin.
type commandInput struct {
	Hook  string          `json:"hook"`
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
	Cwd   string          `json:"cwd,omitempty"`
}

// commandOutput is the JSON a command hook may write to stdout. An empty stdout
// means allow. The Claude Code-style hookSpecificOutput shape is accepted as
// an alias for PreToolUse-style permission decisions.
type commandOutput struct {
	Decision     string            `json:"decision,omitempty"`
	Reason       string            `json:"reason,omitempty"`
	Terminate    bool              `json:"terminate,omitempty"`
	Input        json.RawMessage   `json:"input,omitempty"`
	Result       *ToolResultPatch  `json:"result,omitempty"`
	Messages     agentcoreMessages `json:"messages,omitempty"`
	SystemPrompt *string           `json:"systemPrompt,omitempty"`
	Guidance     []GuidanceMessage `json:"guidance,omitempty"`
	Compaction   *CompactionResult `json:"compaction,omitempty"`
	Entries      []BoundaryDraft   `json:"entries,omitempty"`
	Continue     *bool             `json:"continue,omitempty"`

	HookSpecificOutput *struct {
		PermissionDecision       string          `json:"permissionDecision,omitempty"`
		PermissionDecisionReason string          `json:"permissionDecisionReason,omitempty"`
		UpdatedInput             json.RawMessage `json:"updatedInput,omitempty"`
	} `json:"hookSpecificOutput,omitempty"`
}

// agentcoreMessages decodes a JSON array of messages using the shared
// role-discriminated decoder.
type agentcoreMessages []agentcore.Message

func (m *agentcoreMessages) UnmarshalJSON(data []byte) error {
	var list agentcore.MessageList
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	*m = agentcoreMessages(list)
	return nil
}

// CommandTimeoutError reports a command hook that exceeded its timeout.
type CommandTimeoutError struct {
	Hook    string
	Timeout time.Duration
}

func (e *CommandTimeoutError) Error() string {
	return fmt.Sprintf("command hook %q timed out after %s", e.Hook, e.Timeout)
}

// NewCommandHandler builds an in-process handler for a command hook. Stderr is
// retained on failures and attached to HookError; use newCommandHandler with a
// writer to also log stderr on success.
func NewCommandHandler(cfg CommandHookConfig) (Handler, error) {
	return newCommandHandler(cfg, nil)
}

func newCommandHandler(cfg CommandHookConfig, stderrLog io.Writer) (Handler, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	timeout, _ := cfg.timeout()
	return func(ctx context.Context, event Event) (Decision, error) {
		return runCommandHook(ctx, cfg, timeout, stderrLog, event)
	}, nil
}

// RegisterCommandHooks builds and registers every command hook. On error it
// unregisters the hooks it already added.
func (r *Registry) RegisterCommandHooks(configs []CommandHookConfig) ([]func(), error) {
	var unsubscribe []func()
	rollback := func() {
		for _, fn := range unsubscribe {
			fn()
		}
	}
	for _, cfg := range configs {
		handler, err := newCommandHandler(cfg, r.stderrWriter())
		if err != nil {
			rollback()
			return nil, err
		}
		fn, err := r.Add(Registration{
			Name:        cfg.Name,
			Event:       cfg.Event,
			Handler:     handler,
			Observer:    cfg.Observer,
			ErrorPolicy: cfg.errorPolicy(),
		})
		if err != nil {
			rollback()
			return nil, err
		}
		unsubscribe = append(unsubscribe, fn)
	}
	return unsubscribe, nil
}

func runCommandHook(ctx context.Context, cfg CommandHookConfig, timeout time.Duration, stderrLog io.Writer, event Event) (Decision, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return Decision{}, fmt.Errorf("command hook %q: encode event: %w", cfg.Name, err)
	}
	payload, err := json.Marshal(commandInput{
		Hook:  cfg.Name,
		Event: event.EventType(),
		Data:  data,
		Cwd:   cfg.Cwd,
	})
	if err != nil {
		return Decision{}, fmt.Errorf("command hook %q: encode input: %w", cfg.Name, err)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, cfg.Command[0], cfg.Command[1:]...)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 100 * time.Millisecond
	if cfg.Cwd != "" {
		cmd.Dir = cfg.Cwd
	}
	cmd.Env = buildEnv(cfg.Env)

	runErr := cmd.Run()
	stderrText := strings.TrimSpace(stderr.String())
	if stderrText != "" && stderrLog != nil {
		_, _ = fmt.Fprintf(stderrLog, "hook %s: %s\n", cfg.Name, stderrText)
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return Decision{}, &CommandTimeoutError{Hook: cfg.Name, Timeout: timeout}
	}

	// A structured stdout decision wins even on a blocking exit, matching the
	// Claude Code convention where exit 2 means "block" and stderr is the
	// reason.
	if decision, ok, parseErr := parseCommandOutput(stdout.Bytes()); ok {
		if parseErr == nil {
			return decision, nil
		}
		if runErr == nil {
			return Decision{}, fmt.Errorf("command hook %q: %w", cfg.Name, parseErr)
		}
		// Unparseable stdout on a failing exit falls through to the exit-code
		// handling below (exit 2 still denies).
	}

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) && exitErr.ExitCode() == 2 {
			reason := stderrText
			if reason == "" {
				reason = fmt.Sprintf("command hook %q blocked the operation", cfg.Name)
			}
			return Deny(reason), nil
		}
		if stderrText != "" {
			return Decision{}, fmt.Errorf("command hook %q exited with error: %w: %s", cfg.Name, runErr, stderrText)
		}
		return Decision{}, fmt.Errorf("command hook %q exited with error: %w", cfg.Name, runErr)
	}
	return Allow(), nil
}

// parseCommandOutput parses stdout. ok is false when stdout is empty (allow).
// A non-empty but invalid stdout returns ok=true with a parse error so the
// caller can apply the failure policy instead of silently allowing.
func parseCommandOutput(stdout []byte) (Decision, bool, error) {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return Decision{}, false, nil
	}
	var output commandOutput
	if err := json.Unmarshal(trimmed, &output); err != nil {
		return Decision{}, true, fmt.Errorf("invalid stdout JSON: %w", err)
	}
	decision, err := output.toDecision()
	if err != nil {
		return Decision{}, true, err
	}
	return decision, true, nil
}

func (o commandOutput) toDecision() (Decision, error) {
	action := strings.ToLower(strings.TrimSpace(o.Decision))
	reason := o.Reason
	input := o.Input
	terminate := o.Terminate
	if o.HookSpecificOutput != nil {
		updated := o.HookSpecificOutput.UpdatedInput
		if len(updated) > 0 {
			input = updated
		}
		if action == "" {
			action = strings.ToLower(strings.TrimSpace(o.HookSpecificOutput.PermissionDecision))
			if reason == "" {
				reason = o.HookSpecificOutput.PermissionDecisionReason
			}
		}
	}

	hasModification := len(input) > 0 ||
		o.Result != nil ||
		o.Messages != nil ||
		o.SystemPrompt != nil ||
		len(o.Guidance) > 0 ||
		o.Compaction != nil ||
		o.Entries != nil ||
		o.Continue != nil

	switch action {
	case "", "allow", "approve":
		if hasModification {
			return o.modifyDecision(input, terminate), nil
		}
		return Allow(), nil
	case "deny", "block", "ask":
		// "ask" has no interactive UI here; fail safe by denying with the
		// supplied reason.
		if reason == "" {
			reason = "denied by command hook"
		}
		return Decision{Action: ActionDeny, Reason: reason, Terminate: terminate}, nil
	case "modify", "update":
		return o.modifyDecision(input, terminate), nil
	case "continue":
		continuation := true
		if o.Continue != nil {
			continuation = *o.Continue
		}
		return Decision{Action: ActionContinue, Entries: o.Entries, ContinueOverride: &continuation}, nil
	default:
		return Decision{}, fmt.Errorf("unknown decision %q", o.Decision)
	}
}

func (o commandOutput) modifyDecision(input json.RawMessage, terminate bool) Decision {
	decision := Decision{
		Action:           ActionModify,
		Reason:           o.Reason,
		Terminate:        terminate,
		Input:            input,
		Messages:         []agentcore.Message(o.Messages),
		SystemPrompt:     o.SystemPrompt,
		Guidance:         o.Guidance,
		Compaction:       o.Compaction,
		Entries:          o.Entries,
		ContinueOverride: o.Continue,
	}
	if o.Result != nil {
		decision.Result = o.Result
	}
	return decision
}

// buildEnv returns the parent environment with cfg overrides applied. Existing
// entries for overridden keys are removed so a duplicate never depends on
// platform-dependent last-wins behavior.
func buildEnv(overrides map[string]string) []string {
	base := os.Environ()
	if len(overrides) == 0 {
		return base
	}
	out := base[:0]
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if _, ok := overrides[key]; ok {
			continue
		}
		out = append(out, entry)
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = append(out, key+"="+overrides[key])
	}
	return out
}
