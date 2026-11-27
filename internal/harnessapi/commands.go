package harnessapi

// Chat command catalog and execution (change keel-plugin-commands).
//
// A command is a named USER action: the Composer/TUI sends `/name`, the host
// validates the session boundary, permissions and arguments, and executes one
// existing seam:
//
//   - `compact`    → the manual compaction seam (CompactSession), the same one
//     POST /v1/sessions/{id}/compact drives;
//   - `skill:<n>`  → the skills library, pinning the content for the next turn
//     (a durable skill.loaded event the runner reads);
//   - plugin       → Manager.InvokePluginTool, the approved plugin tool
//     dispatch (plugin.invoke + the plugin's own permissions).
//
// A command NEVER creates user/assistant events and never expands text into a
// prompt. `/learn` is deliberately absent: it is a client prompt template.
//
// Idempotency and the destructive-decision phase are durable:
//
//	POST .../commands/{name} {args, requestId}
//	  → inserted row status=running
//	  → destructive: open decision, row=pending_decision, answer 202
//	  → replay (same requestId) after an answer: execute once, answer 200
//	  → any terminal state is replayed from the row, never re-executed
//
// Spec: openspec/changes/keel-plugin-commands/specs/plugin-commands/spec.md.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/logsafe"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/skilllib"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/toolsurface"
)

// Catalog bounds. The order is builtin → plugin (plugin id) → skill, and the
// total is capped; skills alone may occupy at most half the catalog.
const (
	commandCatalogMax        = 256
	commandSkillCatalogMax   = 128
	commandRequestIDMax      = 200
	commandNameMax           = 256
	commandArgsMaxBytes      = 64 << 10
	commandResultMaxBytes    = 64 << 10
	commandWaitDefault       = 30 * time.Second
	commandDecisionDefault   = 5 * time.Minute
	commandDecisionKind      = "command.invoke"
	commandDecisionOptionOK  = "allow"
	commandDecisionOptionNo  = "deny"
	commandDecisionRequestID = "command:"
)

// Command availability reasons beyond the plugin registry's own.
const (
	// CommandReasonNotWired reports a catalog entry whose seam is not wired in
	// this deployment.
	CommandReasonNotWired = "not_wired"
	// CommandReasonMissingPermissions reports required keys the caller lacks.
	CommandReasonMissingPermissions = "missing_permissions"
	// CommandReasonTargetUnsupported reports a command not offered on the
	// caller's client target.
	CommandReasonTargetUnsupported = "target_unsupported"
)

// Command error codes on the wire. They are stable.
const (
	CommandCodeUnknown           = "unknown_command"
	CommandCodeSessionNotFound   = "session_not_found"
	CommandCodeInvalidArgs       = "invalid_arguments"
	CommandCodePermissionDenied  = "permission_denied"
	CommandCodeBusy              = "session_busy"
	CommandCodeRequestConflict   = "request_id_conflict"
	CommandCodeInProgress        = "command_in_progress"
	CommandCodeNotWired          = "not_wired"
	CommandCodePluginUnavailable = "plugin_unavailable"
	CommandCodeSkillNotFound     = "skill_not_found"
	CommandCodeToolError         = "tool_error"
	CommandCodeDecisionRefused   = "decision_refused"
	CommandCodeDecisionTimeout   = "decision_timeout"
	CommandCodeDecisionCancelled = "decision_cancelled"
	CommandCodeCompactDisabled   = "compaction_disabled"
	CommandCodeCompactConflict   = "compaction_conflict"
	CommandCodeQuota             = "quota_exceeded"
	CommandCodeState             = "command_state"
)

// CommandInvocationStore is the durable requestId state machine. The
// production implementation is *store.Store; tests use an in-memory double.
type CommandInvocationStore interface {
	ClaimCommandInvocation(ctx context.Context, row store.CommandInvocation) (store.CommandInvocation, bool, error)
	GetCommandInvocation(ctx context.Context, tenantID, sessionID uuid.UUID, requestID string) (store.CommandInvocation, bool, error)
	UpdateCommandInvocation(ctx context.Context, row store.CommandInvocation) error
}

// CommandPluginRegistry is the live plugin command surface. Nil means this
// deployment contributes no plugin commands (builtins and skills still work).
type CommandPluginRegistry interface {
	ListCommands() harnessext.CommandRegistry
}

// CommandSkills is the skill catalog and loader behind `/skill:<name>`.
// *skilllib.Library satisfies it.
type CommandSkills interface {
	List(tenantID uuid.UUID, check bool) (skilllib.Snapshot, error)
	Show(tenantID uuid.UUID, name string) (skilllib.Detail, error)
}

// SessionIdleFunc reports whether the session has no in-flight turn or
// compaction. A false answer makes a command 409 session_busy before any side
// effect; an error fails closed (500).
type SessionIdleFunc func(ctx context.Context, tenantID, sessionID uuid.UUID) (bool, error)

// CommandView is one catalog entry.
type CommandView struct {
	// Name is the command as the client sends it: compact, skill:<name> or
	// <plugin-id>:<name> (the client renders a leading '/').
	Name string `json:"name"`
	// Source is builtin|plugin|skill.
	Source string `json:"source"`
	// Summary is the one-line listing text.
	Summary string `json:"summary"`
	// ArgsSchema is the JSON object schema (always an object).
	ArgsSchema json.RawMessage `json:"argsSchema"`
	// Danger is read|write|destructive.
	Danger string `json:"danger"`
	// Idempotent reports whether replaying the same arguments has no additional
	// effect.
	Idempotent bool `json:"idempotent"`
	// Targets are the client targets the command is offered on.
	Targets []string `json:"targets"`
	// RequiredPermissions are the keys an invocation needs. Plugin commands
	// derive them from the tool registry, never from the plugin's own claims.
	RequiredPermissions []string `json:"requiredPermissions"`
	// Available reports whether this caller can run it now.
	Available bool `json:"available"`
	// Reason is a stable unavailability reason.
	Reason string `json:"reason,omitempty"`
	// MissingPermissions names the keys the caller lacks.
	MissingPermissions []string `json:"missingPermissions,omitempty"`
}

// CommandCatalog is GET /v1/sessions/{id}/commands.
type CommandCatalog struct {
	Commands  []CommandView `json:"commands"`
	Truncated bool          `json:"truncated"`
	// Generation is the plugin runtime generation the plugin entries came from.
	Generation int64 `json:"generation"`
}

// CommandResult is the 200 body of a command execution.
type CommandResult struct {
	RequestID  string          `json:"requestId"`
	Command    string          `json:"command"`
	Source     string          `json:"source"`
	Status     string          `json:"status"`
	Data       json.RawMessage `json:"data,omitempty"`
	EventSeq   int64           `json:"eventSeq,omitempty"`
	DurationMs int64           `json:"durationMs"`
	Truncated  bool            `json:"truncated,omitempty"`
	DecisionID string          `json:"decisionId,omitempty"`
	ErrorCode  string          `json:"errorCode,omitempty"`
	Error      string          `json:"error,omitempty"`
}

// resolvedCommand is one executable catalog resolution.
type resolvedCommand struct {
	// name is the published name.
	name string
	// source is builtin|plugin|skill.
	source string
	// summary/danger/idempotent/argsSchema/targets describe the command.
	summary    string
	danger     string
	idempotent bool
	argsSchema map[string]any
	targets    []string
	// pluginID/tool are set for plugin commands.
	pluginID      string
	tool          string
	publishedTool string
	// skill is set for skill commands.
	skill string
	// available/reason mirror the registry state (plugins).
	available bool
	reason    string
}

func (s *Server) commandRoutes() {
	s.mux.HandleFunc("GET /v1/sessions/{id}/commands", s.listSessionCommands)
	s.mux.HandleFunc("POST /v1/sessions/{id}/commands/{name}", s.executeSessionCommand)
}

// commandWired reports whether the execution route has its minimum seams. The
// catalog can still be served without them (entries are marked unavailable).
func (s *Server) commandWired() bool {
	return s.cfg.CommandStore != nil && s.cfg.SessionIdle != nil && s.cfg.Access != nil
}

// listSessionCommands implements GET /v1/sessions/{id}/commands.
func (s *Server) listSessionCommands(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	sessionID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	if !s.requireSession(w, r, tenantID, sessionID) {
		return
	}
	if !s.authorizeSessionResource(w, r, tenantID, sessionID, access.KeySessionRead) {
		return
	}
	target, ok := commandTarget(w, r)
	if !ok {
		return
	}
	principal, err := s.cfg.Access.Principal(r.Context(), tenantID)
	if err != nil {
		writeIdentityError(w, http.StatusUnauthorized, "invalid_session", "authentication required")
		return
	}
	grants, err := s.cfg.Access.Effective(r.Context(), principal)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	catalog, err := s.commandCatalog(r.Context(), tenantID, sessionID, target, grants)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, catalog)
}

// commandTarget parses the optional ?target= client surface.
func commandTarget(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("target"))
	switch raw {
	case "", "web", "native", "tui":
		return raw, true
	}
	writeErr(w, http.StatusBadRequest, errors.New("target must be one of web|native|tui"))
	return "", false
}

// commandCatalog aggregates builtins, plugin commands and skills in the fixed
// order and applies availability without hiding an entry.
func (s *Server) commandCatalog(ctx context.Context, tenantID, sessionID uuid.UUID, target string, grants access.Grants) (CommandCatalog, error) {
	catalog := CommandCatalog{Commands: []CommandView{}, Generation: 0}
	dropped := false

	// 1. Builtins, in declaration order.
	catalog.Commands = append(catalog.Commands, s.builtinCommandViews(grants, target)...)

	// 2. Plugin commands: plugin id order, declaration order inside a plugin.
	if s.cfg.CommandPlugins != nil {
		registry := s.cfg.CommandPlugins.ListCommands()
		catalog.Generation = registry.Generation
		for _, command := range registry.Commands {
			schema := command.Decl.EffectiveArgsSchema()
			required := pluginCommandPermissions(command)
			view := CommandView{
				Name:                command.Published,
				Source:              "plugin",
				Summary:             command.Decl.Summary,
				ArgsSchema:          commandSchemaJSON(schema),
				Danger:              string(command.Decl.Danger),
				Idempotent:          command.Decl.Idempotent,
				Targets:             slotTargetStrings(command.Decl.EffectiveTargets()),
				RequiredPermissions: required,
			}
			missing := missingKeys(grants, required)
			switch {
			case !command.Available:
				view.Available = false
				view.Reason = command.Reason
			case len(missing) > 0:
				view.Available = false
				view.Reason = CommandReasonMissingPermissions
				view.MissingPermissions = missing
			case target != "" && !targetIn(view.Targets, target):
				view.Available = false
				view.Reason = CommandReasonTargetUnsupported
			default:
				view.Available = true
			}
			if len(missing) > 0 && view.MissingPermissions == nil {
				view.MissingPermissions = missing
			}
			catalog.Commands = append(catalog.Commands, view)
		}
	}

	// 3. Skills, sorted by name, at most half the catalog.
	if s.cfg.CommandSkills != nil {
		snapshot, err := s.cfg.CommandSkills.List(tenantID, false)
		if err != nil {
			return CommandCatalog{}, err
		}
		names := make([]string, 0, len(snapshot.Skills))
		for _, record := range snapshot.Skills {
			names = append(names, record.Name)
		}
		sort.Strings(names)
		if len(names) > commandSkillCatalogMax {
			names = names[:commandSkillCatalogMax]
			dropped = true
		}
		for _, name := range names {
			view := CommandView{
				Name:                "skill:" + name,
				Source:              "skill",
				Summary:             "Load skill " + name + " for the next turn.",
				ArgsSchema:          commandSchemaJSON(pluginpkg.CommandDecl{}.EffectiveArgsSchema()),
				Danger:              string(pluginpkg.DangerRead),
				Idempotent:          true,
				Targets:             slotTargetStrings(pluginpkg.DefaultCommandTargets()),
				RequiredPermissions: []string{access.KeySessionWrite, access.KeySkillRead},
			}
			missing := missingKeys(grants, view.RequiredPermissions)
			switch {
			case len(missing) > 0:
				view.Available = false
				view.Reason = CommandReasonMissingPermissions
				view.MissingPermissions = missing
			case target != "" && !targetIn(view.Targets, target):
				view.Available = false
				view.Reason = CommandReasonTargetUnsupported
			default:
				view.Available = true
			}
			catalog.Commands = append(catalog.Commands, view)
		}
	}

	if len(catalog.Commands) > commandCatalogMax {
		catalog.Commands = catalog.Commands[:commandCatalogMax]
		dropped = true
	}
	catalog.Truncated = dropped
	return catalog, nil
}

// builtinCommandViews renders the two builtin commands. They are always listed
// (a client must be able to explain why one is unavailable), never learn.
func (s *Server) builtinCommandViews(grants access.Grants, target string) []CommandView {
	compact := CommandView{
		Name:                "compact",
		Source:              "builtin",
		Summary:             "Compact the current session context.",
		ArgsSchema:          commandSchemaJSON(pluginpkg.CommandDecl{}.EffectiveArgsSchema()),
		Danger:              string(pluginpkg.DangerWrite),
		Idempotent:          true,
		Targets:             slotTargetStrings(pluginpkg.DefaultCommandTargets()),
		RequiredPermissions: []string{access.KeySessionWrite, access.KeyModelInvoke},
	}
	missing := missingKeys(grants, compact.RequiredPermissions)
	switch {
	case s.cfg.CompactSession == nil:
		compact.Available = false
		compact.Reason = CommandReasonNotWired
	case len(missing) > 0:
		compact.Available = false
		compact.Reason = CommandReasonMissingPermissions
		compact.MissingPermissions = missing
	case target != "" && !targetIn(compact.Targets, target):
		compact.Available = false
		compact.Reason = CommandReasonTargetUnsupported
	default:
		compact.Available = true
	}
	if len(missing) > 0 && compact.MissingPermissions == nil {
		compact.MissingPermissions = missing
	}
	return []CommandView{compact}
}

// pluginCommandPermissions derives the keys a plugin command needs: the route's
// session.write, plugin.invoke (the approval boundary) and any key the tool
// registry declares for the backend tool. The plugin never self-reports keys.
func pluginCommandPermissions(command harnessext.PluginCommand) []string {
	// plugin.invoke is always required: it is the approval boundary that
	// installed the plugin. A tool-registry key, when the registry declares one,
	// is added beside it (a plugin command never replaces the boundary).
	keys := []string{access.KeySessionWrite, access.KeyPluginInvoke}
	if derived, ok := toolsurface.RequiredKey(command.PublishedTool, ""); ok && !containsString(keys, derived) {
		keys = append(keys, derived)
	}
	return keys
}

// missingKeys returns the required keys the principal does not hold.
func missingKeys(grants access.Grants, required []string) []string {
	missing := make([]string, 0)
	for _, key := range required {
		if _, held := grants.ScopeOf(key); !held {
			missing = append(missing, key)
		}
	}
	return missing
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func targetIn(targets []string, want string) bool {
	return containsString(targets, want)
}

func slotTargetStrings(targets []pluginpkg.SlotTarget) []string {
	out := make([]string, 0, len(targets))
	for _, target := range targets {
		out = append(out, string(target))
	}
	return out
}

func commandSchemaJSON(schema map[string]any) json.RawMessage {
	raw, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return raw
}

// resolveCommand finds the executable entry for a published command name. The
// catalog and the execution route share the same registry, so a name that is
// not in the catalog is a 404 here.
func (s *Server) resolveCommand(ctx context.Context, tenantID, sessionID uuid.UUID, name string) (resolvedCommand, bool, error) {
	switch {
	case name == "compact":
		return resolvedCommand{
			name:       "compact",
			source:     "builtin",
			summary:    "Compact the current session context.",
			danger:     string(pluginpkg.DangerWrite),
			idempotent: true,
			argsSchema: pluginpkg.CommandDecl{}.EffectiveArgsSchema(),
			targets:    slotTargetStrings(pluginpkg.DefaultCommandTargets()),
			available:  s.cfg.CompactSession != nil,
		}, true, nil
	case strings.HasPrefix(name, "skill:"):
		skill := strings.TrimPrefix(name, "skill:")
		if !validSkillCommandName(skill) {
			return resolvedCommand{}, false, nil
		}
		return resolvedCommand{
			name:       name,
			source:     "skill",
			summary:    "Load skill " + skill + " for the next turn.",
			danger:     string(pluginpkg.DangerRead),
			idempotent: true,
			argsSchema: pluginpkg.CommandDecl{}.EffectiveArgsSchema(),
			targets:    slotTargetStrings(pluginpkg.DefaultCommandTargets()),
			skill:      skill,
			available:  s.cfg.CommandSkills != nil,
		}, true, nil
	case s.cfg.CommandPlugins != nil:
		for _, candidate := range s.cfg.CommandPlugins.ListCommands().Commands {
			if candidate.Published != name {
				continue
			}
			return resolvedCommand{
				name:          candidate.Published,
				source:        "plugin",
				summary:       candidate.Decl.Summary,
				danger:        string(candidate.Decl.Danger),
				idempotent:    candidate.Decl.Idempotent,
				argsSchema:    candidate.Decl.EffectiveArgsSchema(),
				targets:       slotTargetStrings(candidate.Decl.EffectiveTargets()),
				pluginID:      candidate.PluginID,
				tool:          candidate.Tool,
				publishedTool: candidate.PublishedTool,
				available:     candidate.Available,
				reason:        candidate.Reason,
			}, true, nil
		}
	}
	return resolvedCommand{}, false, nil
}

// validSkillCommandName mirrors the skills package grammar.
func validSkillCommandName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// commandExecuteRequest is the POST body.
type commandExecuteRequest struct {
	Args      json.RawMessage `json:"args"`
	RequestID string          `json:"requestId"`
}

// executeSessionCommand implements POST /v1/sessions/{id}/commands/{name}.
func (s *Server) executeSessionCommand(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	sessionID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	rawName := strings.TrimSpace(r.PathValue("name"))
	if rawName == "" || len(rawName) > commandNameMax || !utf8.ValidString(rawName) {
		writeCommandError(w, http.StatusNotFound, CommandCodeUnknown, "unknown command")
		return
	}
	// The session boundary first: a foreign or missing session is the same 404.
	if !s.requireSession(w, r, tenantID, sessionID) {
		return
	}
	if !s.authorizeSessionResource(w, r, tenantID, sessionID, access.KeySessionWrite) {
		return
	}
	if s.cfg.CommandStore == nil || s.cfg.SessionIdle == nil {
		writeCommandError(w, http.StatusServiceUnavailable, CommandCodeNotWired,
			"command execution is not wired in this deployment")
		return
	}
	resolved, found, err := s.resolveCommand(r.Context(), tenantID, sessionID, rawName)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		writeCommandError(w, http.StatusNotFound, CommandCodeUnknown, "unknown command")
		return
	}
	var body commandExecuteRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeCommandError(w, http.StatusBadRequest, CommandCodeInvalidArgs, err.Error())
		return
	}
	requestID := strings.TrimSpace(body.RequestID)
	if requestID == "" {
		writeCommandError(w, http.StatusBadRequest, CommandCodeInvalidArgs, "requestId is required")
		return
	}
	if len(requestID) > commandRequestIDMax {
		writeCommandError(w, http.StatusBadRequest, CommandCodeInvalidArgs, "requestId is too long")
		return
	}
	args, normalized, fieldErrors := normalizeCommandArgs(resolved.argsSchema, body.Args)
	if len(fieldErrors) > 0 {
		writeCommandFieldErrors(w, CommandCodeInvalidArgs, fieldErrors)
		return
	}
	if len(normalized) > commandArgsMaxBytes {
		writeCommandFieldErrors(w, CommandCodeInvalidArgs, []pluginpkg.FieldError{{
			Field: "args", Message: fmt.Sprintf("arguments exceed %d bytes", commandArgsMaxBytes),
		}})
		return
	}
	principal, err := s.cfg.Access.Principal(r.Context(), tenantID)
	if err != nil {
		writeIdentityError(w, http.StatusUnauthorized, "invalid_session", "authentication required")
		return
	}
	for _, need := range commandRequiredKeys(resolved) {
		if need == access.KeySessionWrite {
			continue // the route gate above already decided it
		}
		if !s.authorizeSessionResource(w, r, tenantID, sessionID, need) {
			return
		}
	}
	hash := sha256Hex(normalized)
	started := time.Now()
	invocation, inserted, err := s.cfg.CommandStore.ClaimCommandInvocation(r.Context(), store.CommandInvocation{
		TenantID: tenantID, SessionID: sessionID, RequestID: requestID,
		Command: resolved.name, Source: resolved.source, Actor: commandActor(principal),
		Status: store.CommandStatusRunning, ArgsHash: hash, ArgsBytes: len(normalized),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !inserted {
		s.replayCommand(w, r, invocation, resolved, args, hash, started)
		return
	}
	// Idle-only commands (the whole first version): a live turn or compaction is
	// 409 without touching a tool.
	idle, err := s.cfg.SessionIdle(r.Context(), tenantID, sessionID)
	if err != nil {
		s.finishCommand(r.Context(), &invocation, store.CommandStatusError, CommandCodeState,
			"the session idle check failed", nil, started, "")
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !idle {
		s.finishCommand(r.Context(), &invocation, store.CommandStatusRefused, CommandCodeBusy,
			"session is busy", nil, started, "")
		writeCommandError(w, http.StatusConflict, CommandCodeBusy, "session is busy; retry after the active turn settles")
		return
	}
	if strings.EqualFold(resolved.danger, string(pluginpkg.DangerDestructive)) {
		s.openCommandDecision(w, r, invocation, resolved, started)
		return
	}
	s.runCommand(w, r, &invocation, resolved, args, started, "")
}

// commandRequiredKeys is the permission set an execution needs. The route key
// is included so the catalog and the execution route cannot drift.
func commandRequiredKeys(command resolvedCommand) []string {
	switch command.source {
	case "builtin":
		return []string{access.KeySessionWrite, access.KeyModelInvoke}
	case "skill":
		return []string{access.KeySessionWrite, access.KeySkillRead}
	case "plugin":
		keys := []string{access.KeySessionWrite, access.KeyPluginInvoke}
		if derived, ok := toolsurface.RequiredKey(command.publishedTool, ""); ok && !containsString(keys, derived) {
			keys = append(keys, derived)
		}
		return keys
	}
	return []string{access.KeySessionWrite}
}

// replayCommand answers a repeated requestId from its durable row.
func (s *Server) replayCommand(w http.ResponseWriter, r *http.Request, invocation store.CommandInvocation, resolved resolvedCommand, args json.RawMessage, argsHash string, started time.Time) {
	if invocation.Command != resolved.name || invocation.ArgsHash != argsHash {
		writeCommandError(w, http.StatusConflict, CommandCodeRequestConflict,
			"requestId was already used by a different command or arguments")
		return
	}
	switch invocation.Status {
	case store.CommandStatusOK:
		writeJSON(w, http.StatusOK, commandResultFromRow(invocation))
	case store.CommandStatusError, store.CommandStatusRefused,
		store.CommandStatusTimedOut, store.CommandStatusCancelled:
		// The stored error code is the first outcome, including a pre-execution
		// refusal (busy) and a decision outcome.
		writeStoredCommandError(w, invocation)
	case store.CommandStatusPendingDecision:
		s.resumeCommandDecision(w, r, invocation, resolved, args, started)
	case store.CommandStatusRunning:
		// Another caller owns this requestId. Serialize on the row: poll for its
		// terminal state rather than executing twice.
		s.waitForCommand(w, r, invocation, resolved, args, started)
	default:
		writeCommandError(w, http.StatusConflict, CommandCodeState, "the command invocation is in an unknown state")
	}
}

// resumeCommandDecision inspects the durable decision a pending destructive
// command opened: still open → 202 with the same decision id; answered allow →
// execute once; any other terminal state → its error.
func (s *Server) resumeCommandDecision(w http.ResponseWriter, r *http.Request, invocation store.CommandInvocation, resolved resolvedCommand, args json.RawMessage, started time.Time) {
	if s.cfg.Decisions == nil {
		writeCommandError(w, http.StatusServiceUnavailable, CommandCodeNotWired,
			"destructive commands need the durable decision primitive, which is not wired")
		return
	}
	decisionID, err := uuid.Parse(invocation.DecisionID)
	if err != nil {
		writeCommandError(w, http.StatusConflict, CommandCodeState, "the stored decision id is unusable")
		return
	}
	decision, found := s.cfg.Decisions.Get(r.Context(), invocation.TenantID, invocation.SessionID, decisionID)
	if !found {
		writeCommandError(w, http.StatusConflict, CommandCodeState, "the stored decision no longer exists")
		return
	}
	switch decision.Status {
	case harnessext.DecisionOpen:
		writeJSON(w, http.StatusAccepted, commandPendingResult(invocation, decision.DecisionID, s.commandDecisionTimeout()))
	case harnessext.DecisionAnswered:
		// The answer alone is not enough: the stored row must be the question
		// this command asked (kind and derived requestId), so a generic decision
		// opened under a chosen requestId cannot authorize the command.
		grant := decision.Kind == commandDecisionKind &&
			decision.RequestID == commandDecisionRequestID+invocation.RequestID &&
			decision.Answer != nil && decision.Answer.OptionID == commandDecisionOptionOK
		if grant {
			s.runCommand(w, r, &invocation, resolved, args, started, decision.DecisionID)
			return
		}
		s.finishCommand(r.Context(), &invocation, store.CommandStatusRefused, CommandCodeDecisionRefused,
			"the destructive command was refused", nil, started, decision.DecisionID)
		writeCommandErrorWithDecision(w, http.StatusForbidden, CommandCodeDecisionRefused,
			"the destructive command was refused", decision.DecisionID, invocation.EventSeq)
	case harnessext.DecisionTimedOut:
		s.finishCommand(r.Context(), &invocation, store.CommandStatusTimedOut, CommandCodeDecisionTimeout,
			"the destructive command's decision timed out", nil, started, decision.DecisionID)
		writeCommandErrorWithDecision(w, http.StatusRequestTimeout, CommandCodeDecisionTimeout,
			"the destructive command's decision timed out", decision.DecisionID, invocation.EventSeq)
	case harnessext.DecisionCancelled:
		s.finishCommand(r.Context(), &invocation, store.CommandStatusCancelled, CommandCodeDecisionCancelled,
			"the destructive command's decision was cancelled", nil, started, decision.DecisionID)
		writeCommandErrorWithDecision(w, http.StatusConflict, CommandCodeDecisionCancelled,
			"the destructive command's decision was cancelled", decision.DecisionID, invocation.EventSeq)
	default:
		writeCommandError(w, http.StatusConflict, CommandCodeState, "the stored decision is in an unknown state")
	}
}

// openCommandDecision opens the durable question of a destructive command. The
// decision's requestId is derived from the command requestId, so a repeated
// open returns the same row (no second question).
func (s *Server) openCommandDecision(w http.ResponseWriter, r *http.Request, invocation store.CommandInvocation, resolved resolvedCommand, started time.Time) {
	if s.cfg.Decisions == nil {
		// Fail closed: a deployment without the decision primitive must not run a
		// destructive command.
		s.finishCommand(r.Context(), &invocation, store.CommandStatusError, CommandCodeNotWired,
			"destructive commands need the durable decision primitive", nil, started, "")
		writeCommandError(w, http.StatusServiceUnavailable, CommandCodeNotWired,
			"destructive commands need the durable decision primitive, which is not wired")
		return
	}
	decision, _, err := s.cfg.Decisions.Open(r.Context(), harnessext.OpenDecisionRequest{
		TenantID:  invocation.TenantID,
		SessionID: invocation.SessionID,
		RequestID: commandDecisionRequestID + invocation.RequestID,
		Kind:      commandDecisionKind,
		Prompt: fmt.Sprintf("Run the destructive command /%s? It is declared danger=destructive; approving it will execute the command once.",
			resolved.name),
		Options: []harnessext.DecisionOption{
			{ID: commandDecisionOptionOK, Label: "Run"},
			{ID: commandDecisionOptionNo, Label: "Cancel"},
		},
		Timeout: s.commandDecisionTimeout(),
	})
	if err != nil {
		s.finishCommand(r.Context(), &invocation, store.CommandStatusError, CommandCodeState,
			"the command decision could not be opened", nil, started, "")
		if errors.Is(err, harnessext.ErrInvalidDecision) {
			writeCommandError(w, http.StatusBadRequest, CommandCodeInvalidArgs, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	eventSeq := s.recordCommandEvent(r.Context(), invocation, store.CommandStatusPendingDecision,
		CommandCodeState, "", nil, 0, decision.DecisionID)
	invocation.Status = store.CommandStatusPendingDecision
	invocation.DecisionID = decision.DecisionID
	invocation.EventSeq = eventSeq
	invocation.DurationMs = time.Since(started).Milliseconds()
	if err := s.cfg.CommandStore.UpdateCommandInvocation(r.Context(), invocation); err != nil {
		s.cfg.Logger.Error("command invocation decision state not persisted",
			"tenant_id", invocation.TenantID, "session_id", invocation.SessionID,
			"request_id", invocation.RequestID, "err", err)
	}
	writeJSON(w, http.StatusAccepted, commandPendingResult(invocation, decision.DecisionID, s.commandDecisionTimeout()))
}

// runCommand executes the resolved command and records its terminal state.
func (s *Server) runCommand(w http.ResponseWriter, r *http.Request, invocation *store.CommandInvocation, resolved resolvedCommand, args json.RawMessage, started time.Time, decisionID string) {
	status, code, message, data := s.dispatchCommand(r.Context(), invocation, resolved, args)
	if decisionID != "" {
		invocation.DecisionID = decisionID
	}
	if status == store.CommandStatusOK {
		s.finishCommandData(r.Context(), invocation, status, "", "", data, started, decisionID)
		writeJSON(w, http.StatusOK, commandResultFromRow(*invocation))
		return
	}
	s.finishCommand(r.Context(), invocation, status, code, message, data, started, decisionID)
	statusCode := commandErrorStatus(code)
	writeCommandErrorWithDecision(w, statusCode, code, message, decisionID, invocation.EventSeq)
}

// dispatchCommand runs the seam and maps its failure onto a command outcome.
func (s *Server) dispatchCommand(ctx context.Context, invocation *store.CommandInvocation, resolved resolvedCommand, args json.RawMessage) (status, code, message string, data json.RawMessage) {
	switch resolved.source {
	case "builtin":
		if s.cfg.CompactSession == nil {
			return store.CommandStatusError, CommandCodeNotWired, "the manual compaction seam is not wired", nil
		}
		result, err := s.cfg.CompactSession(ctx, invocation.TenantID, invocation.SessionID, invocation.RequestID)
		if err != nil {
			switch {
			case errors.Is(err, ErrSessionNotFound):
				return store.CommandStatusError, CommandCodeSessionNotFound, "session not found", nil
			case errors.Is(err, ErrCompactDisabled):
				return store.CommandStatusError, CommandCodeCompactDisabled, "manual compaction is disabled by the checkpoint policy", nil
			case errors.Is(err, ErrCompactBusy):
				return store.CommandStatusError, CommandCodeBusy, "session is busy", nil
			case errors.Is(err, ErrCompactQuota):
				return store.CommandStatusError, CommandCodeQuota, "run admission quota exceeded", nil
			case errors.Is(err, ErrCompactConflict):
				return store.CommandStatusError, CommandCodeCompactConflict, "session context cannot be compacted safely", nil
			case errors.Is(err, ErrExternalSession):
				return store.CommandStatusError, CommandCodeCompactConflict, "session is owned by an external backend", nil
			default:
				s.cfg.Logger.Warn("command compact failed", "tenant_id", invocation.TenantID,
					"session_id", invocation.SessionID, "request_id", invocation.RequestID, "err", err)
				return store.CommandStatusError, CommandCodeToolError, "the compaction seam failed", nil
			}
		}
		// Mirror the HTTP route's identifiers: the result is the same seam.
		result.SessionID = invocation.SessionID.String()
		result.RequestID = invocation.RequestID
		raw, err := json.Marshal(result)
		if err != nil {
			return store.CommandStatusError, CommandCodeToolError, "the compaction result could not be encoded", nil
		}
		return store.CommandStatusOK, "", "", raw
	case "skill":
		if s.cfg.CommandSkills == nil {
			return store.CommandStatusError, CommandCodeNotWired, "the skill library is not wired", nil
		}
		detail, err := s.cfg.CommandSkills.Show(invocation.TenantID, resolved.skill)
		if err != nil {
			var libraryErr *skilllib.Error
			if errors.As(err, &libraryErr) && libraryErr.Code == skilllib.CodeNotFound {
				return store.CommandStatusError, CommandCodeSkillNotFound, "skill not found", nil
			}
			s.cfg.Logger.Warn("command skill load failed", "tenant_id", invocation.TenantID,
				"session_id", invocation.SessionID, "skill", resolved.skill, "err", err)
			return store.CommandStatusError, CommandCodeToolError, "the skill library failed", nil
		}
		payload := map[string]any{
			"name":        detail.Name,
			"description": detail.Description,
			"hash":        detail.CurrentHash,
			"body":        detail.Body,
		}
		if _, _, err := s.appendSessionEvent(ctx, *invocation, store.EventSkillLoaded, payload, commandEventKey(*invocation, "skill")); err != nil {
			return store.CommandStatusError, CommandCodeToolError, "the skill load could not be recorded", nil
		}
		data, _ := json.Marshal(map[string]any{
			"skill": detail.Name, "hash": detail.CurrentHash, "loadedFor": "next-turn",
		})
		return store.CommandStatusOK, "", "", data
	case "plugin":
		if !resolved.available {
			reason := resolved.reason
			if reason == "" {
				reason = harnessext.CommandReasonPluginFailed
			}
			return store.CommandStatusError, CommandCodePluginUnavailable,
				"the plugin command is unavailable (" + reason + ")", nil
		}
		if s.cfg.CommandTools == nil {
			return store.CommandStatusError, CommandCodeNotWired, "the plugin tool dispatch is not wired", nil
		}
		result, err := s.cfg.CommandTools.InvokePluginTool(ctx, resolved.pluginID, resolved.tool, args)
		if err != nil {
			switch {
			case errors.Is(err, harnessext.ErrPermissionDenied):
				return store.CommandStatusError, CommandCodePermissionDenied, "the plugin refused the tool call", nil
			case errors.Is(err, harnessext.ErrPluginNotFound), errors.Is(err, harnessext.ErrPluginState):
				return store.CommandStatusError, CommandCodePluginUnavailable, "the plugin is no longer available", nil
			default:
				s.cfg.Logger.Warn("command plugin tool failed", "plugin", resolved.pluginID,
					"tool", resolved.tool, "tenant_id", invocation.TenantID,
					"session_id", invocation.SessionID, "err", err)
				return store.CommandStatusError, CommandCodeToolError, "the plugin tool failed", nil
			}
		}
		if !json.Valid(result) {
			return store.CommandStatusError, CommandCodeToolError, "the plugin tool returned invalid JSON", nil
		}
		return store.CommandStatusOK, "", "", result
	}
	return store.CommandStatusError, CommandCodeState, "the command source is unknown", nil
}

// finishCommand records a failed/refused terminal state and its projection.
func (s *Server) finishCommand(ctx context.Context, invocation *store.CommandInvocation, status, code, message string, _ json.RawMessage, started time.Time, decisionID string) {
	eventSeq := s.recordCommandEvent(ctx, *invocation, status, code, message, nil, time.Since(started).Milliseconds(), decisionID)
	invocation.Status = status
	invocation.ErrorCode = code
	invocation.ErrorMessage = message
	invocation.EventSeq = eventSeq
	invocation.DurationMs = time.Since(started).Milliseconds()
	if decisionID != "" {
		invocation.DecisionID = decisionID
	}
	if err := s.cfg.CommandStore.UpdateCommandInvocation(ctx, *invocation); err != nil {
		s.cfg.Logger.Error("command invocation state not persisted",
			"tenant_id", invocation.TenantID, "session_id", invocation.SessionID,
			"request_id", invocation.RequestID, "err", err)
	}
}

// finishCommandData records a successful terminal state and its projection.
func (s *Server) finishCommandData(ctx context.Context, invocation *store.CommandInvocation, status, code, message string, data json.RawMessage, started time.Time, decisionID string) {
	bounded, truncated := boundCommandResult(data)
	eventSeq := s.recordCommandEvent(ctx, *invocation, status, code, message, bounded, time.Since(started).Milliseconds(), decisionID)
	invocation.Status = status
	invocation.Result = bounded
	invocation.Truncated = truncated
	invocation.EventSeq = eventSeq
	invocation.DurationMs = time.Since(started).Milliseconds()
	if decisionID != "" {
		invocation.DecisionID = decisionID
	}
	if err := s.cfg.CommandStore.UpdateCommandInvocation(ctx, *invocation); err != nil {
		s.cfg.Logger.Error("command invocation state not persisted",
			"tenant_id", invocation.TenantID, "session_id", invocation.SessionID,
			"request_id", invocation.RequestID, "err", err)
	}
}

// recordCommandEvent appends the command.invoked projection beside the row. The
// payload never carries the argument document or a secret; Data is the bounded
// command result.
func (s *Server) recordCommandEvent(ctx context.Context, invocation store.CommandInvocation, status, code, message string, data json.RawMessage, durationMs int64, decisionID string) int64 {
	phase := "result"
	if status == store.CommandStatusPendingDecision {
		phase = "decision"
	}
	payload := commandInvokedPayload{
		RequestID:  invocation.RequestID,
		Command:    invocation.Command,
		Source:     invocation.Source,
		Status:     status,
		Actor:      invocation.Actor,
		DecisionID: decisionID,
		ArgsHash:   invocation.ArgsHash,
		ArgsBytes:  invocation.ArgsBytes,
		DurationMs: durationMs,
		Data:       data,
		ErrorCode:  code,
		Error:      message,
	}
	seq, _, err := s.appendSessionEvent(ctx, invocation, store.EventCommandInvoked, payload, commandEventKey(invocation, phase))
	if err != nil {
		s.cfg.Logger.Error("command invoked event not appended",
			"tenant_id", invocation.TenantID, "session_id", invocation.SessionID,
			"request_id", invocation.RequestID, "err", err)
		return 0
	}
	return seq
}

// appendSessionEvent appends one bookkeeping event through the store. The event
// dedupe key makes a crashed/retried terminal write exactly-once.
type commandEventAppender interface {
	AppendEventDedup(ctx context.Context, tenantID, sessionID uuid.UUID, kind string, payload any, dedupeKey string) (int64, bool, error)
}

func (s *Server) appendSessionEvent(ctx context.Context, invocation store.CommandInvocation, kind string, payload any, dedupeKey string) (int64, bool, error) {
	appender, ok := s.cfg.CommandStore.(commandEventAppender)
	if !ok {
		return 0, false, errors.New("command store cannot append session events")
	}
	return appender.AppendEventDedup(ctx, invocation.TenantID, invocation.SessionID, kind, payload, dedupeKey)
}

// commandEventKey scopes the dedupe key to the requestId and phase.
func commandEventKey(invocation store.CommandInvocation, phase string) string {
	return "command:" + invocation.RequestID + ":" + phase
}

// commandInvokedPayload is the durable command.invoked line. It never carries
// the argument document: only its hash and byte length.
type commandInvokedPayload struct {
	RequestID  string          `json:"requestId"`
	Command    string          `json:"command"`
	Source     string          `json:"source"`
	Status     string          `json:"status"`
	Actor      string          `json:"actor,omitempty"`
	DecisionID string          `json:"decisionId,omitempty"`
	ArgsHash   string          `json:"argsHash"`
	ArgsBytes  int             `json:"argsBytes"`
	DurationMs int64           `json:"durationMs"`
	Data       json.RawMessage `json:"data,omitempty"`
	ErrorCode  string          `json:"errorCode,omitempty"`
	Error      string          `json:"error,omitempty"`
}

// waitForCommand polls the claimed row until it settles. A same-process caller
// holding a mutex would be faster, but the row is the authority across
// processes; polling for the short execution window is the honest serialization.
func (s *Server) waitForCommand(w http.ResponseWriter, r *http.Request, invocation store.CommandInvocation, resolved resolvedCommand, args json.RawMessage, started time.Time) {
	deadline := time.Now().Add(s.commandWaitTimeout())
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, found, err := s.cfg.CommandStore.GetCommandInvocation(r.Context(), invocation.TenantID, invocation.SessionID, invocation.RequestID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if found && current.Status != store.CommandStatusRunning {
			s.replayCommand(w, r, current, resolved, args, current.ArgsHash, started)
			return
		}
		if time.Now().After(deadline) {
			writeCommandError(w, http.StatusConflict, CommandCodeInProgress,
				"another request with this requestId is still running")
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

// commandResultFromRow renders a stored terminal result.
func commandResultFromRow(invocation store.CommandInvocation) CommandResult {
	result := CommandResult{
		RequestID:  invocation.RequestID,
		Command:    invocation.Command,
		Source:     invocation.Source,
		Status:     invocation.Status,
		Data:       invocation.Result,
		EventSeq:   invocation.EventSeq,
		DurationMs: invocation.DurationMs,
		Truncated:  invocation.Truncated,
		DecisionID: invocation.DecisionID,
	}
	return result
}

// commandPendingResult is the 202 answer of a destructive command.
func commandPendingResult(invocation store.CommandInvocation, decisionID string, timeout time.Duration) map[string]any {
	body := map[string]any{
		"requestId":  invocation.RequestID,
		"command":    invocation.Command,
		"source":     invocation.Source,
		"status":     store.CommandStatusPendingDecision,
		"decisionId": decisionID,
		"pending":    true,
		"eventSeq":   invocation.EventSeq,
	}
	if timeout > 0 {
		body["timeoutMs"] = timeout.Milliseconds()
	}
	return body
}

// writeStoredCommandError replays a stored failure with its original status.
func writeStoredCommandError(w http.ResponseWriter, invocation store.CommandInvocation) {
	statusCode := commandErrorStatus(invocation.ErrorCode)
	writeCommandErrorWithDecision(w, statusCode, invocation.ErrorCode, invocation.ErrorMessage,
		invocation.DecisionID, invocation.EventSeq)
}

// commandErrorStatus maps a stable command error code onto HTTP.
func commandErrorStatus(code string) int {
	switch code {
	case CommandCodeInvalidArgs:
		return http.StatusBadRequest
	case CommandCodePermissionDenied, CommandCodeDecisionRefused:
		return http.StatusForbidden
	case CommandCodeUnknown, CommandCodeSkillNotFound, CommandCodeSessionNotFound:
		return http.StatusNotFound
	case CommandCodeDecisionTimeout:
		return http.StatusRequestTimeout
	case CommandCodeBusy, CommandCodeRequestConflict, CommandCodeInProgress,
		CommandCodeDecisionCancelled, CommandCodeCompactDisabled, CommandCodeCompactConflict:
		return http.StatusConflict
	case CommandCodeQuota:
		return http.StatusTooManyRequests
	case CommandCodeNotWired, CommandCodePluginUnavailable:
		return http.StatusServiceUnavailable
	case CommandCodeToolError:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

func writeCommandError(w http.ResponseWriter, status int, code, message string) {
	writeCommandErrorWithDecision(w, status, code, message, "", 0)
}

func writeCommandErrorWithDecision(w http.ResponseWriter, status int, code, message, decisionID string, eventSeq int64) {
	body := map[string]any{"error": logsafe.Redact(message), "code": code}
	if decisionID != "" {
		body["decisionId"] = decisionID
	}
	if eventSeq != 0 {
		body["eventSeq"] = eventSeq
	}
	writeJSON(w, status, body)
}

func writeCommandFieldErrors(w http.ResponseWriter, code string, fields []pluginpkg.FieldError) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error":  "the command arguments are invalid",
		"code":   code,
		"fields": fields,
	})
}

// normalizeCommandArgs decodes and validates the argument document against the
// command's schema. It returns the raw document (default `{}`) and a canonical
// encoding used for the idempotency hash; unknown fields are always refused.
func normalizeCommandArgs(schema map[string]any, raw json.RawMessage) (json.RawMessage, []byte, []pluginpkg.FieldError) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil, []pluginpkg.FieldError{{Field: "args", Message: "arguments must be a JSON object"}}
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, nil, []pluginpkg.FieldError{{Field: "args", Message: "arguments are not valid JSON: " + err.Error()}}
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, nil, []pluginpkg.FieldError{{Field: "args", Message: "arguments must be a JSON object"}}
	}
	fields := validateCommandArgs(schema, object, "args", 0)
	if len(fields) > 0 {
		return nil, nil, fields
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, nil, []pluginpkg.FieldError{{Field: "args", Message: "arguments could not be encoded"}}
	}
	return json.RawMessage(trimmed), canonical, nil
}

func validateCommandArgs(schema map[string]any, object map[string]any, path string, depth int) []pluginpkg.FieldError {
	if depth > 8 {
		return []pluginpkg.FieldError{{Field: path, Message: "arguments are nested too deeply"}}
	}
	properties, _ := schema["properties"].(map[string]any)
	var fields []pluginpkg.FieldError
	for name, value := range object {
		property, declared := properties[name]
		if !declared {
			fields = append(fields, pluginpkg.FieldError{Field: path + "." + name, Message: "unknown argument"})
			continue
		}
		subSchema, _ := property.(map[string]any)
		fields = append(fields, validateCommandValue(subSchema, value, path+"."+name, depth+1)...)
	}
	if required, ok := schema["required"].([]any); ok {
		for _, raw := range required {
			name, _ := raw.(string)
			if name == "" {
				continue
			}
			if _, present := object[name]; !present {
				fields = append(fields, pluginpkg.FieldError{Field: path + "." + name, Message: "is required"})
			}
		}
	}
	return fields
}

func validateCommandValue(schema map[string]any, value any, path string, depth int) []pluginpkg.FieldError {
	if schema == nil {
		return nil
	}
	if types, ok := schema["type"].(string); ok && types != "" {
		if !valueMatchesCommandType(types, value) {
			return []pluginpkg.FieldError{{Field: path, Message: "must be a " + types}}
		}
	}
	if options, ok := schema["enum"].([]any); ok && len(options) > 0 {
		matched := false
		encoded, _ := json.Marshal(value)
		for _, option := range options {
			candidate, _ := json.Marshal(option)
			if string(encoded) == string(candidate) {
				matched = true
				break
			}
		}
		if !matched {
			return []pluginpkg.FieldError{{Field: path, Message: "is not one of the allowed values"}}
		}
	}
	switch typed := value.(type) {
	case map[string]any:
		return validateCommandArgs(schema, typed, path, depth)
	case []any:
		items, _ := schema["items"].(map[string]any)
		if items == nil {
			return nil
		}
		var fields []pluginpkg.FieldError
		for index, item := range typed {
			fields = append(fields, validateCommandValue(items, item, fmt.Sprintf("%s[%d]", path, index), depth+1)...)
		}
		return fields
	}
	return nil
}

func valueMatchesCommandType(kind string, value any) bool {
	switch kind {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		switch value.(type) {
		case json.Number, float64:
			return true
		}
		return false
	case "integer":
		switch typed := value.(type) {
		case json.Number:
			_, err := typed.Int64()
			return err == nil
		case float64:
			return typed == float64(int64(typed))
		}
		return false
	}
	// An unknown schema type is not the command's argument error to guess.
	return true
}

// boundCommandResult caps the stored/projected result at 64 KiB. An oversized
// result is replaced by a small marker: the state itself is never lost.
func boundCommandResult(data json.RawMessage) (json.RawMessage, bool) {
	if len(data) <= commandResultMaxBytes {
		return data, false
	}
	marker, _ := json.Marshal(map[string]any{"truncated": true, "bytes": len(data)})
	return marker, true
}

func commandActor(principal access.Principal) string {
	switch {
	case principal.AccountID != uuid.Nil:
		return principal.AccountID.String()
	case principal.BotID != "":
		return principal.BotID
	default:
		return ""
	}
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *Server) commandDecisionTimeout() time.Duration {
	if s.cfg.CommandDecisionTimeout > 0 {
		return s.cfg.CommandDecisionTimeout
	}
	return commandDecisionDefault
}

func (s *Server) commandWaitTimeout() time.Duration {
	if s.cfg.CommandWaitTimeout > 0 {
		return s.cfg.CommandWaitTimeout
	}
	return commandWaitDefault
}
