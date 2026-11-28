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
	"regexp"
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
	commandCatalogMax         = 256
	commandSkillCatalogMax    = 128
	commandRequestIDMax       = 200
	commandNameMax            = 256
	commandArgsMaxBytes       = 64 << 10
	commandResultMaxBytes     = 64 << 10
	commandWaitDefault        = 30 * time.Second
	commandDecisionDefault    = 5 * time.Minute
	commandLeaseDefault       = 2 * time.Minute
	commandSkillBodyMax       = 256 << 10
	commandDecisionPreviewMax = 512
	commandDecisionKind       = "command.invoke"
	commandDecisionOptionOK   = "allow"
	commandDecisionOptionNo   = "deny"
	commandDecisionRequestID  = "command:"
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
	CommandCodeDecisionConflict  = "decision_conflict"
	CommandCodeCompactDisabled   = "compaction_disabled"
	CommandCodeCompactConflict   = "compaction_conflict"
	CommandCodeQuota             = "quota_exceeded"
	CommandCodeState             = "command_state"
)

// CommandInvocationStore is the durable requestId state machine. The
// production implementation is *store.Store; tests use an in-memory double.
type CommandInvocationStore interface {
	// ClaimCommandInvocation inserts the running row or reclaims an expired
	// running lease. owned=true means this caller must execute.
	ClaimCommandInvocation(ctx context.Context, row store.CommandInvocation, lease time.Duration) (store.CommandInvocation, bool, error)
	// ClaimDecisionResult is the pending_decision -> running CAS: exactly one
	// replay of an approved destructive command may execute.
	ClaimDecisionResult(ctx context.Context, tenantID, sessionID uuid.UUID, requestID string, lease time.Duration) (store.CommandInvocation, bool, error)
	// RenewCommandInvocation renews a live execution's lease.
	RenewCommandInvocation(ctx context.Context, tenantID, sessionID uuid.UUID, requestID string, lease time.Duration) (bool, error)
	// TransitionCommandInvocation writes a state only if it is still `from`.
	TransitionCommandInvocation(ctx context.Context, row store.CommandInvocation, from string) (bool, error)
	GetCommandInvocation(ctx context.Context, tenantID, sessionID uuid.UUID, requestID string) (store.CommandInvocation, bool, error)
}

// CommandPluginRegistry is the live plugin command surface. Nil means this
// deployment contributes no plugin commands (builtins and skills still work).
type CommandPluginRegistry interface {
	ListCommands() harnessext.CommandRegistry
	// TenantID is the tenant this live registry is bound to. The catalog and
	// the execution route refuse a registry bound to another tenant.
	TenantID() uuid.UUID
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
	// Warnings are stable degradation codes (for example skills_unavailable).
	// They are codes, never raw error text: a catalog read must not leak a
	// library failure into the client.
	Warnings []string `json:"warnings,omitempty"`
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
	catalog, err := s.commandCatalog(r.Context(), principal, tenantID, sessionID, target)
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

// commandKeyChecker decides each command key once per catalog request against
// the actual session resource. A key grant alone is not enough: the relation
// (owner/viewer/editor) is what the execution gate will require, so the
// catalog must not promise a command the execution route would refuse. One
// decision per distinct key also bounds the denial-audit rows a catalog read
// can produce.
type commandKeyChecker struct {
	ctx       context.Context
	server    *Server
	principal access.Principal
	tenantID  uuid.UUID
	sessionID uuid.UUID
	cache     map[string]bool
	err       error
}

func newCommandKeyChecker(ctx context.Context, server *Server, principal access.Principal, tenantID, sessionID uuid.UUID) *commandKeyChecker {
	return &commandKeyChecker{
		ctx: ctx, server: server, principal: principal,
		tenantID: tenantID, sessionID: sessionID, cache: map[string]bool{},
	}
}

func (c *commandKeyChecker) allowed(key string) bool {
	if c.err != nil {
		return false
	}
	if allowed, seen := c.cache[key]; seen {
		return allowed
	}
	decision, err := c.server.cfg.Access.Authorize(c.ctx, c.principal, key, access.Resource{
		TenantID: c.tenantID, Type: "session", ID: c.sessionID.String(),
	})
	if err != nil {
		c.err = err
		return false
	}
	allowed := !decision.Denied()
	c.cache[key] = allowed
	return allowed
}

func (c *commandKeyChecker) missing(required []string) []string {
	missing := make([]string, 0)
	for _, key := range required {
		if !c.allowed(key) {
			missing = append(missing, key)
		}
	}
	return missing
}

// commandCatalog aggregates builtins, plugin commands and skills in the fixed
// order and applies availability without hiding an entry.
func (s *Server) commandCatalog(ctx context.Context, principal access.Principal, tenantID, sessionID uuid.UUID, target string) (CommandCatalog, error) {
	catalog := CommandCatalog{Commands: []CommandView{}, Generation: 0}
	dropped := false
	keys := newCommandKeyChecker(ctx, s, principal, tenantID, sessionID)
	wired := s.commandWired()

	// 1. Builtins, in declaration order.
	catalog.Commands = append(catalog.Commands, s.builtinCommandViews(keys, wired, target)...)

	// 2. Plugin commands: plugin id order, declaration order inside a plugin.
	if plugins := s.commandPluginsForTenant(tenantID); plugins != nil {
		registry := plugins.ListCommands()
		catalog.Generation = registry.Generation
		pluginWired := wired && s.cfg.CommandTools != nil
		for _, command := range registry.Commands {
			schema := command.Decl.EffectiveArgsSchema()
			required := pluginCommandPermissions(command)
			commandWired := pluginWired
			if strings.EqualFold(string(command.Decl.Danger), string(pluginpkg.DangerDestructive)) && s.cfg.Decisions == nil {
				commandWired = false
			}
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
			missing := keys.missing(required)
			switch {
			case !commandWired:
				view.Available = false
				view.Reason = CommandReasonNotWired
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

	// 3. Skills, sorted by name, at most half the catalog. A failing skill
	// library degrades the catalog (the builtins/plugins still answer) instead
	// of failing the whole page.
	if s.cfg.CommandSkills != nil {
		snapshot, err := s.cfg.CommandSkills.List(tenantID, false)
		if err != nil {
			s.cfg.Logger.Warn("command catalog: skill library unavailable",
				"tenant_id", tenantID, "err", err)
			catalog.Warnings = append(catalog.Warnings, "skills_unavailable")
		} else {
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
				missing := keys.missing(view.RequiredPermissions)
				switch {
				case !wired:
					view.Available = false
					view.Reason = CommandReasonNotWired
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
	}
	if keys.err != nil {
		return CommandCatalog{}, keys.err
	}

	if len(catalog.Commands) > commandCatalogMax {
		catalog.Commands = catalog.Commands[:commandCatalogMax]
		dropped = true
	}
	catalog.Truncated = dropped
	return catalog, nil
}

// builtinCommandViews renders the builtin commands. They are always listed (a
// client must be able to explain why one is unavailable), never learn.
func (s *Server) builtinCommandViews(keys *commandKeyChecker, wired bool, target string) []CommandView {
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
	missing := keys.missing(compact.RequiredPermissions)
	switch {
	case !wired:
		compact.Available = false
		compact.Reason = CommandReasonNotWired
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

// commandPluginsForTenant returns the plugin command registry only when the
// manager is bound to the request's tenant. One process serves one tenant
// today, but the boundary is enforced here rather than left to the deployment:
// a manager for tenant B must never leak a command into tenant A's catalog or
// resolve an execution for it.
func (s *Server) commandPluginsForTenant(tenantID uuid.UUID) CommandPluginRegistry {
	registry := s.cfg.CommandPlugins
	if registry == nil || registry.TenantID() != tenantID {
		return nil
	}
	return registry
}

// resolveCommand finds the executable entry for a published command name. The
// catalog and the execution route share the same registry, so a name that is
// not in the catalog is a 404 here. The execution route deliberately takes no
// client target: `targets` is a catalog presentation filter (a client must not
// run an unavailable row), not an authorization boundary.
func (s *Server) resolveCommand(ctx context.Context, tenantID, sessionID uuid.UUID, name string) (resolvedCommand, bool, error) {
	if name == "compact" {
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
	}
	if strings.HasPrefix(name, "skill:") {
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
	}
	// Plugin commands: a manager bound to another tenant contributes neither
	// catalog entries nor an execution entry.
	if plugins := s.commandPluginsForTenant(tenantID); plugins != nil {
		for _, candidate := range plugins.ListCommands().Commands {
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
	// Permissions run before the body is read: an unauthorized caller learns
	// nothing about the command's argument contract (and the requestId cannot be
	// probed). The session.write gate above already ran; this adds the command's
	// own keys.
	principal, err := s.cfg.Access.Principal(r.Context(), tenantID)
	if err != nil {
		writeIdentityError(w, http.StatusUnauthorized, "invalid_session", "authentication required")
		return
	}
	for _, need := range commandRequiredKeys(resolved) {
		if need == access.KeySessionWrite {
			continue
		}
		if !s.authorizeSessionResource(w, r, tenantID, sessionID, need) {
			return
		}
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
	_, normalized, fieldErrors := normalizeCommandArgs(resolved.argsSchema, body.Args)
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
	// Execute the canonical encoding: the idempotency hash, the plugin tool
	// arguments and the destructive approval preview then describe the same
	// document even when the client reserialized it with different whitespace.
	args := json.RawMessage(normalized)
	hash := sha256Hex(normalized)
	started := time.Now()
	invocation, inserted, err := s.cfg.CommandStore.ClaimCommandInvocation(r.Context(), store.CommandInvocation{
		TenantID: tenantID, SessionID: sessionID, RequestID: requestID,
		Command: resolved.name, Source: resolved.source, Actor: commandActor(principal),
		Status: store.CommandStatusRunning, ArgsHash: hash, ArgsBytes: len(normalized),
	}, s.commandLease())
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
		s.finishCommandFrom(r.Context(), &invocation, store.CommandStatusRunning, store.CommandStatusError,
			CommandCodeState, "the session idle check failed", nil, started, "")
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !idle {
		if s.finishCommandFrom(r.Context(), &invocation, store.CommandStatusRunning, store.CommandStatusRefused,
			CommandCodeBusy, "session is busy", nil, started, "") {
			// The refusal wrote a durable command.invoked row: name its seq so
			// the client does not mistake a durable busy refusal for a local
			// request-phase failure.
			writeCommandErrorWithDecision(w, http.StatusConflict, CommandCodeBusy,
				"session is busy; retry after the active turn settles", "", invocation.EventSeq)
		} else {
			s.writeCommandRow(w, r, invocation)
		}
		return
	}
	if strings.EqualFold(resolved.danger, string(pluginpkg.DangerDestructive)) {
		s.openCommandDecision(w, r, invocation, resolved, args, started)
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

// commandDecisionGrants reports whether an answered decision is the question
// this command asked. Matching only the option would let a decision opened
// under the derived requestId with a different kind/prompt authorize a
// command, so both identity fields are part of the grant.
func commandDecisionGrants(decision harnessext.Decision, invocation store.CommandInvocation) bool {
	return decision.Kind == commandDecisionKind &&
		decision.RequestID == commandDecisionRequestID+invocation.RequestID &&
		strings.HasPrefix(decision.Prompt, commandDecisionPromptPrefix(invocation.Command)) &&
		decision.Status == harnessext.DecisionAnswered &&
		decision.Answer != nil && decision.Answer.OptionID == commandDecisionOptionOK
}

// commandDecisionPromptPrefix is the stable opening of the destructive
// question. The grant check requires it, so a decision opened under the derived
// requestId with an unrelated prompt cannot authorize the command even when the
// kind and requestId were reproduced.
func commandDecisionPromptPrefix(command string) string {
	return "Run the destructive command /" + command + "?"
}

// resumeCommandDecision inspects the durable decision a pending destructive
// command opened: still open → 202 with the same decision id; answered allow →
// exactly one CAS claim executes; any other terminal state → its error.
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
		if !commandDecisionGrants(decision, invocation) {
			s.finishDecisionFailure(w, r, invocation, store.CommandStatusRefused,
				CommandCodeDecisionRefused, "the destructive command was refused",
				http.StatusForbidden, decision.DecisionID, started)
			return
		}
		// Approval does not bypass the idle rule: if a turn started while the
		// question was open, leave the pending row untouched so the same
		// requestId can execute after the turn settles.
		idle, err := s.cfg.SessionIdle(r.Context(), invocation.TenantID, invocation.SessionID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !idle {
			// The pending invocation (and its decision event) is durable; keep
			// its seq on the retryable answer so the client does not relabel it
			// as a local failure.
			writeCommandErrorWithDecision(w, http.StatusConflict, CommandCodeBusy,
				"session is busy; retry after the active turn settles", "", invocation.EventSeq)
			return
		}
		claimed, owned, err := s.cfg.CommandStore.ClaimDecisionResult(
			r.Context(), invocation.TenantID, invocation.SessionID, invocation.RequestID, s.commandLease())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !owned {
			s.waitForCommand(w, r, claimed, resolved, args, started)
			return
		}
		s.runCommand(w, r, &claimed, resolved, args, started, decision.DecisionID)
	case harnessext.DecisionTimedOut:
		s.finishDecisionFailure(w, r, invocation, store.CommandStatusTimedOut,
			CommandCodeDecisionTimeout, "the destructive command's decision timed out",
			http.StatusRequestTimeout, decision.DecisionID, started)
	case harnessext.DecisionCancelled:
		s.finishDecisionFailure(w, r, invocation, store.CommandStatusCancelled,
			CommandCodeDecisionCancelled, "the destructive command's decision was cancelled",
			http.StatusConflict, decision.DecisionID, started)
	default:
		writeCommandError(w, http.StatusConflict, CommandCodeState, "the stored decision is in an unknown state")
	}
}

// finishDecisionFailure records the first decision outcome once (conditional
// from pending_decision). A concurrent replay that lost the transition answers
// the stored terminal state instead of overwriting it.
func (s *Server) finishDecisionFailure(w http.ResponseWriter, r *http.Request, invocation store.CommandInvocation, status, code, message string, httpStatus int, decisionID string, started time.Time) {
	if s.finishCommandFrom(r.Context(), &invocation, store.CommandStatusPendingDecision,
		status, code, message, nil, started, decisionID) {
		writeCommandErrorWithDecision(w, httpStatus, code, message, decisionID, invocation.EventSeq)
		return
	}
	s.writeCommandRow(w, r, invocation)
}

// writeCommandRow renders the current durable state after a lost transition.
func (s *Server) writeCommandRow(w http.ResponseWriter, r *http.Request, invocation store.CommandInvocation) {
	current, found, err := s.cfg.CommandStore.GetCommandInvocation(r.Context(),
		invocation.TenantID, invocation.SessionID, invocation.RequestID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		writeCommandError(w, http.StatusConflict, CommandCodeState, "the command invocation disappeared")
		return
	}
	switch {
	case current.Status == store.CommandStatusOK:
		writeJSON(w, http.StatusOK, commandResultFromRow(current))
	case current.Terminal():
		writeStoredCommandError(w, current)
	case current.Status == store.CommandStatusPendingDecision:
		writeJSON(w, http.StatusAccepted, commandPendingResult(current, current.DecisionID, s.commandDecisionTimeout()))
	default:
		writeCommandError(w, http.StatusConflict, CommandCodeInProgress,
			"another request with this requestId is still running")
	}
}

// openCommandDecision opens the durable question of a destructive command. The
// decision's requestId is derived from the command requestId, so a repeated
// open returns the same row (no second question).
func (s *Server) openCommandDecision(w http.ResponseWriter, r *http.Request, invocation store.CommandInvocation, resolved resolvedCommand, args json.RawMessage, started time.Time) {
	if s.cfg.Decisions == nil {
		// Fail closed: a deployment without the decision primitive must not run a
		// destructive command.
		s.finishCommandFrom(r.Context(), &invocation, store.CommandStatusRunning, store.CommandStatusError,
			CommandCodeNotWired, "destructive commands need the durable decision primitive", nil, started, "")
		writeCommandError(w, http.StatusServiceUnavailable, CommandCodeNotWired,
			"destructive commands need the durable decision primitive, which is not wired")
		return
	}
	decision, _, err := s.cfg.Decisions.Open(r.Context(), harnessext.OpenDecisionRequest{
		TenantID:  invocation.TenantID,
		SessionID: invocation.SessionID,
		RequestID: commandDecisionRequestID + invocation.RequestID,
		Kind:      commandDecisionKind,
		Prompt:    commandDecisionPrompt(resolved.name, args),
		Options: []harnessext.DecisionOption{
			{ID: commandDecisionOptionOK, Label: "Run"},
			{ID: commandDecisionOptionNo, Label: "Cancel"},
		},
		Timeout: s.commandDecisionTimeout(),
	})
	if err != nil {
		s.openCommandDecisionError(w, r, invocation, err, started)
		return
	}
	switch {
	case decision.Status == harnessext.DecisionAnswered && commandDecisionGrants(decision, invocation):
		// An earlier attempt crashed between the answer and the pending
		// transition: this execution claim is still ours, so run once.
		s.runCommand(w, r, &invocation, resolved, args, started, decision.DecisionID)
		return
	case decision.Status == harnessext.DecisionAnswered:
		s.finishCommandFrom(r.Context(), &invocation, store.CommandStatusRunning, store.CommandStatusRefused,
			CommandCodeDecisionRefused, "the destructive command was refused", nil, started, decision.DecisionID)
		writeCommandErrorWithDecision(w, http.StatusForbidden, CommandCodeDecisionRefused,
			"the destructive command was refused", decision.DecisionID, invocation.EventSeq)
		return
	case decision.Status == harnessext.DecisionTimedOut:
		s.finishCommandFrom(r.Context(), &invocation, store.CommandStatusRunning, store.CommandStatusTimedOut,
			CommandCodeDecisionTimeout, "the destructive command's decision timed out", nil, started, decision.DecisionID)
		writeCommandErrorWithDecision(w, http.StatusRequestTimeout, CommandCodeDecisionTimeout,
			"the destructive command's decision timed out", decision.DecisionID, invocation.EventSeq)
		return
	case decision.Status == harnessext.DecisionCancelled:
		s.finishCommandFrom(r.Context(), &invocation, store.CommandStatusRunning, store.CommandStatusCancelled,
			CommandCodeDecisionCancelled, "the destructive command's decision was cancelled", nil, started, decision.DecisionID)
		writeCommandErrorWithDecision(w, http.StatusConflict, CommandCodeDecisionCancelled,
			"the destructive command's decision was cancelled", decision.DecisionID, invocation.EventSeq)
		return
	}
	eventSeq := s.recordCommandEvent(r.Context(), invocation, store.CommandStatusPendingDecision,
		"", "", nil, 0, decision.DecisionID)
	invocation.Status = store.CommandStatusPendingDecision
	invocation.DecisionID = decision.DecisionID
	invocation.EventSeq = eventSeq
	invocation.DurationMs = time.Since(started).Milliseconds()
	if ok, err := s.cfg.CommandStore.TransitionCommandInvocation(r.Context(), invocation, store.CommandStatusRunning); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	} else if !ok {
		s.writeCommandRow(w, r, invocation)
		return
	}
	writeJSON(w, http.StatusAccepted, commandPendingResult(invocation, decision.DecisionID, s.commandDecisionTimeout()))
}

// openCommandDecisionError maps a refused Open. A conflicting question on the
// derived requestId is never a grant: an answered allow under a different
// kind/prompt is refused (403), any other conflict is 409, and only an invalid
// request is 400.
func (s *Server) openCommandDecisionError(w http.ResponseWriter, r *http.Request, invocation store.CommandInvocation, err error, started time.Time) {
	expectedID := harnessext.DecisionIDFor(invocation.TenantID, invocation.SessionID,
		commandDecisionRequestID+invocation.RequestID)
	if existing, found := s.cfg.Decisions.Get(r.Context(), invocation.TenantID, invocation.SessionID, expectedID); found {
		switch existing.Status {
		case harnessext.DecisionAnswered:
			// The answer alone is never a grant: the stored question must be this
			// command's question. A mismatched (including forged) allow is a
			// refusal, not a conflict.
			code := CommandCodeDecisionRefused
			message := "the destructive command was refused"
			if commandDecisionGrants(existing, invocation) {
				// Open only reports a conflict when kind/prompt/options differ, so
				// a matching grant here means the stored question was mutated
				// after the answer; fail closed rather than execute without proof.
				message = "the destructive command's stored question was mutated"
			}
			s.finishCommandFrom(r.Context(), &invocation, store.CommandStatusRunning, store.CommandStatusRefused,
				code, message, nil, started, existing.DecisionID)
			writeCommandErrorWithDecision(w, http.StatusForbidden, code, message, existing.DecisionID, invocation.EventSeq)
			return
		case harnessext.DecisionTimedOut:
			s.finishCommandFrom(r.Context(), &invocation, store.CommandStatusRunning, store.CommandStatusTimedOut,
				CommandCodeDecisionTimeout, "the destructive command's decision timed out", nil, started, existing.DecisionID)
			writeCommandErrorWithDecision(w, http.StatusRequestTimeout, CommandCodeDecisionTimeout,
				"the destructive command's decision timed out", existing.DecisionID, invocation.EventSeq)
			return
		case harnessext.DecisionCancelled:
			s.finishCommandFrom(r.Context(), &invocation, store.CommandStatusRunning, store.CommandStatusCancelled,
				CommandCodeDecisionCancelled, "the destructive command's decision was cancelled", nil, started, existing.DecisionID)
			writeCommandErrorWithDecision(w, http.StatusConflict, CommandCodeDecisionCancelled,
				"the destructive command's decision was cancelled", existing.DecisionID, invocation.EventSeq)
			return
		}
		if errors.Is(err, harnessext.ErrDecisionConflict) {
			s.finishCommandFrom(r.Context(), &invocation, store.CommandStatusRunning, store.CommandStatusRefused,
				CommandCodeDecisionConflict, "the command's decision conflicts with an existing question", nil, started, "")
			writeCommandError(w, http.StatusConflict, CommandCodeDecisionConflict,
				"the command's decision conflicts with an existing question")
			return
		}
	}
	s.finishCommandFrom(r.Context(), &invocation, store.CommandStatusRunning, store.CommandStatusError,
		CommandCodeState, "the command decision could not be opened", nil, started, "")
	if errors.Is(err, harnessext.ErrInvalidDecision) {
		writeCommandError(w, http.StatusBadRequest, CommandCodeInvalidArgs, err.Error())
		return
	}
	if errors.Is(err, harnessext.ErrDecisionConflict) || errors.Is(err, harnessext.ErrDecisionClosed) {
		writeCommandError(w, http.StatusConflict, CommandCodeState, "the command decision is already terminal")
		return
	}
	writeErr(w, http.StatusInternalServerError, err)
}

// commandDecisionPrompt is the human-facing destructive question. It carries a
// bounded argument preview so the approver sees what will run; the preview is
// never stored as audit (the invocation row keeps only the hash/length).
func commandDecisionPrompt(command string, args json.RawMessage) string {
	prompt := commandDecisionPromptPrefix(command) + " It is declared danger=destructive; approving it will execute the command once."
	preview := commandArgumentPreview(args)
	if preview == "" {
		return prompt
	}
	return prompt + "\nArguments: " + preview
}

// commandArgumentPreview bounds the arguments shown to an approver. The raw
// JSON is the caller's own input; the bound keeps a pathological argument
// document from flooding the decision UI.
func commandArgumentPreview(args json.RawMessage) string {
	trimmed := strings.TrimSpace(string(args))
	if trimmed == "" || trimmed == "{}" || trimmed == "null" {
		return ""
	}
	if len(trimmed) <= commandDecisionPreviewMax {
		return trimmed
	}
	cut := trimmed[:commandDecisionPreviewMax]
	for len(cut) > 0 && !utf8.RuneStart(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…[truncated]"
}

// runCommand executes the resolved command and records its terminal state. The
// execution holds a renewable lease; a concurrent replay cannot execute the
// same requestId.
func (s *Server) runCommand(w http.ResponseWriter, r *http.Request, invocation *store.CommandInvocation, resolved resolvedCommand, args json.RawMessage, started time.Time, decisionID string) {
	stop := s.keepCommandLease(r.Context(), *invocation)
	defer stop()
	status, code, message, data := s.dispatchCommand(r.Context(), invocation, resolved, args)
	if decisionID != "" {
		invocation.DecisionID = decisionID
	}
	var transitioned bool
	if status == store.CommandStatusOK {
		transitioned = s.finishCommandDataFrom(r.Context(), invocation, store.CommandStatusRunning,
			status, "", "", data, started, decisionID)
		if !transitioned {
			s.writeCommandRow(w, r, *invocation)
			return
		}
		writeJSON(w, http.StatusOK, commandResultFromRow(*invocation))
		return
	}
	transitioned = s.finishCommandFrom(r.Context(), invocation, store.CommandStatusRunning,
		status, code, message, data, started, decisionID)
	if !transitioned {
		s.writeCommandRow(w, r, *invocation)
		return
	}
	writeCommandErrorWithDecision(w, commandErrorStatus(code), code, message, decisionID, invocation.EventSeq)
}

// keepCommandLease renews a running owner's lease until the returned stop
// function is called. A lease that stops advancing is what lets a replay
// reclaim a crashed executor.
func (s *Server) keepCommandLease(ctx context.Context, invocation store.CommandInvocation) func() {
	lease := s.commandLease()
	interval := lease / 3
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				_, err := s.cfg.CommandStore.RenewCommandInvocation(renewCtx,
					invocation.TenantID, invocation.SessionID, invocation.RequestID, lease)
				cancel()
				if err != nil {
					s.cfg.Logger.Warn("command invocation lease renewal failed",
						"tenant_id", invocation.TenantID, "session_id", invocation.SessionID,
						"request_id", invocation.RequestID, "err", err)
				}
			}
		}
	}()
	return func() { close(done) }
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
		// The durable pin is a reference (name/hash/logical location) plus a
		// BOUNDED body: a skill bundle is not copied whole into the event log.
		payload := map[string]any{
			"name":         detail.Name,
			"description":  boundText(detail.Description, 1024),
			"hash":         detail.CurrentHash,
			"location":     detail.Path,
			"body":         boundUTF8Text(detail.Body, commandSkillBodyMax),
			"allowedTools": append([]string(nil), detail.AllowedTools...),
		}
		if _, _, err := s.appendSessionEvent(ctx, *invocation, store.EventSkillLoaded, payload, commandEventKey(*invocation, "skill")); err != nil {
			return store.CommandStatusError, CommandCodeToolError, "the skill load could not be recorded", nil
		}
		data, _ := json.Marshal(map[string]any{
			"skill": detail.Name, "hash": detail.CurrentHash, "location": detail.Path,
			"loadedFor": "next-turn",
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
		var envelope struct {
			IsError *bool `json:"isError"`
		}
		if err := json.Unmarshal(result, &envelope); err == nil && envelope.IsError != nil && *envelope.IsError {
			// A plugin tool's own error is a typed command failure (502), not a
			// successful 200 that hides the failure in the body.
			return store.CommandStatusError, CommandCodeToolError, "the plugin tool reported an error", nil
		}
		return store.CommandStatusOK, "", "", result
	}
	return store.CommandStatusError, CommandCodeState, "the command source is unknown", nil
}

// finishCommandFrom records a failed/refused terminal state and its projection,
// only if the row is still in `from`. The bool reports whether this caller won.
func (s *Server) finishCommandFrom(ctx context.Context, invocation *store.CommandInvocation, from, status, code, message string, _ json.RawMessage, started time.Time, decisionID string) bool {
	eventSeq := s.recordCommandEvent(ctx, *invocation, status, code, message, nil, time.Since(started).Milliseconds(), decisionID)
	invocation.Status = status
	invocation.ErrorCode = code
	invocation.ErrorMessage = message
	invocation.EventSeq = eventSeq
	invocation.DurationMs = time.Since(started).Milliseconds()
	if decisionID != "" {
		invocation.DecisionID = decisionID
	}
	ok, err := s.cfg.CommandStore.TransitionCommandInvocation(ctx, *invocation, from)
	if err != nil {
		s.cfg.Logger.Error("command invocation state not persisted",
			"tenant_id", invocation.TenantID, "session_id", invocation.SessionID,
			"request_id", invocation.RequestID, "err", err)
		return false
	}
	return ok
}

// finishCommandDataFrom is finishCommandFrom for a successful result.
func (s *Server) finishCommandDataFrom(ctx context.Context, invocation *store.CommandInvocation, from, status, code, message string, data json.RawMessage, started time.Time, decisionID string) bool {
	bounded, truncated := boundCommandResult(data)
	// The projection carries the same truncation fact as the ledger row.
	invocation.Truncated = truncated
	eventSeq := s.recordCommandEvent(ctx, *invocation, status, code, message, bounded, time.Since(started).Milliseconds(), decisionID)
	invocation.Status = status
	invocation.Result = bounded
	invocation.EventSeq = eventSeq
	invocation.DurationMs = time.Since(started).Milliseconds()
	if decisionID != "" {
		invocation.DecisionID = decisionID
	}
	ok, err := s.cfg.CommandStore.TransitionCommandInvocation(ctx, *invocation, from)
	if err != nil {
		s.cfg.Logger.Error("command invocation state not persisted",
			"tenant_id", invocation.TenantID, "session_id", invocation.SessionID,
			"request_id", invocation.RequestID, "err", err)
		return false
	}
	return ok
}

// recordCommandEvent appends the command.invoked projection beside the row. The
// projection carries the same identity fields as the invocation ledger; the
// ledger is the audit/idempotency truth, the event is what a chat renders. It
// never carries the argument document or a secret; Data is the bounded result.
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
		Truncated:  invocation.Truncated,
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

// commandInvokedPayload is the durable command.invoked projection. It never
// carries the argument document: only its hash and byte length.
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
	Truncated  bool            `json:"truncated,omitempty"`
	ErrorCode  string          `json:"errorCode,omitempty"`
	Error      string          `json:"error,omitempty"`
}

// waitForCommand polls the claimed row until it settles. A duplicate requestId
// never executes: it waits for the owner, reclaims an expired lease once, and
// otherwise answers the stored state.
func (s *Server) waitForCommand(w http.ResponseWriter, r *http.Request, invocation store.CommandInvocation, resolved resolvedCommand, args json.RawMessage, started time.Time) {
	deadline := time.Now().Add(s.commandWaitTimeout())
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, found, err := s.cfg.CommandStore.GetCommandInvocation(r.Context(),
			invocation.TenantID, invocation.SessionID, invocation.RequestID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !found {
			writeCommandError(w, http.StatusConflict, CommandCodeState, "the command invocation disappeared")
			return
		}
		switch current.Status {
		case store.CommandStatusOK, store.CommandStatusError, store.CommandStatusRefused,
			store.CommandStatusTimedOut, store.CommandStatusCancelled:
			s.replayCommand(w, r, current, resolved, args, current.ArgsHash, started)
			return
		case store.CommandStatusPendingDecision:
			// The owner (or the next replay) will claim it after an approval.
		case store.CommandStatusRunning:
			// An expired lease means the owner died; one waiter reclaims it.
			claimed, owned, err := s.cfg.CommandStore.ClaimCommandInvocation(r.Context(), current, s.commandLease())
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			if owned {
				idle, err := s.cfg.SessionIdle(r.Context(), claimed.TenantID, claimed.SessionID)
				if err != nil {
					writeErr(w, http.StatusInternalServerError, err)
					return
				}
				if !idle {
					writeCommandError(w, http.StatusConflict, CommandCodeBusy,
						"session is busy; retry after the active turn settles")
					return
				}
				s.runCommand(w, r, &claimed, resolved, args, started, claimed.DecisionID)
				return
			}
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
		CommandCodeDecisionCancelled, CommandCodeDecisionConflict,
		CommandCodeCompactDisabled, CommandCodeCompactConflict:
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
	var fields []pluginpkg.FieldError
	switch typed := value.(type) {
	case string:
		if minimum, ok := commandSchemaInt(schema["minLength"]); ok && utf8.RuneCountInString(typed) < minimum {
			fields = append(fields, pluginpkg.FieldError{Field: path, Message: fmt.Sprintf("must be at least %d characters", minimum)})
		}
		if maximum, ok := commandSchemaInt(schema["maxLength"]); ok && utf8.RuneCountInString(typed) > maximum {
			fields = append(fields, pluginpkg.FieldError{Field: path, Message: fmt.Sprintf("must be at most %d characters", maximum)})
		}
		if pattern, ok := schema["pattern"].(string); ok && pattern != "" {
			if compiled, err := regexp.Compile(pattern); err == nil && !compiled.MatchString(typed) {
				fields = append(fields, pluginpkg.FieldError{Field: path, Message: "does not match the declared pattern"})
			}
		}
	case map[string]any:
		return append(fields, validateCommandArgs(schema, typed, path, depth)...)
	case []any:
		if minimum, ok := commandSchemaInt(schema["minItems"]); ok && len(typed) < minimum {
			fields = append(fields, pluginpkg.FieldError{Field: path, Message: fmt.Sprintf("must contain at least %d items", minimum)})
		}
		if maximum, ok := commandSchemaInt(schema["maxItems"]); ok && len(typed) > maximum {
			fields = append(fields, pluginpkg.FieldError{Field: path, Message: fmt.Sprintf("must contain at most %d items", maximum)})
		}
		if unique, _ := schema["uniqueItems"].(bool); unique {
			seen := map[string]bool{}
			for _, item := range typed {
				raw, _ := json.Marshal(item)
				if seen[string(raw)] {
					fields = append(fields, pluginpkg.FieldError{Field: path, Message: "must not contain duplicate items"})
					break
				}
				seen[string(raw)] = true
			}
		}
		items, _ := schema["items"].(map[string]any)
		if items != nil {
			for index, item := range typed {
				fields = append(fields, validateCommandValue(items, item, fmt.Sprintf("%s[%d]", path, index), depth+1)...)
			}
		}
		return fields
	default:
		if number, ok := commandSchemaNumber(value); ok {
			if minimum, ok := commandSchemaNumber(schema["minimum"]); ok && number < minimum {
				fields = append(fields, pluginpkg.FieldError{Field: path, Message: fmt.Sprintf("must be at least %v", minimum)})
			}
			if maximum, ok := commandSchemaNumber(schema["maximum"]); ok && number > maximum {
				fields = append(fields, pluginpkg.FieldError{Field: path, Message: fmt.Sprintf("must be at most %v", maximum)})
			}
		}
	}
	return fields
}

func commandSchemaInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		if typed == float64(int(typed)) {
			return int(typed), true
		}
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return int(parsed), true
		}
	}
	return 0, false
}

func commandSchemaNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	case json.Number:
		if parsed, err := typed.Float64(); err == nil {
			return parsed, true
		}
	}
	return 0, false
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
	// A bot principal is the BOT, not its owner account: the audit line must
	// name the actor that invoked the command, and a Bot key is the credential
	// that did.
	if principal.BotID != "" {
		return principal.BotID
	}
	if principal.AccountID != uuid.Nil {
		return principal.AccountID.String()
	}
	return ""
}

// boundUTF8Text cuts text to max bytes without splitting a UTF-8 rune and marks
// the cut. It is used for event payload copies (skill descriptions/bodies).
func boundUTF8Text(value string, max int) string {
	if len(value) <= max {
		return value
	}
	cut := value[:max]
	for len(cut) > 0 && !utf8.RuneStart(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	return cut + "\n[truncated]"
}

// boundText is boundUTF8Text without a newline marker (short metadata).
func boundText(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return boundUTF8Text(value, max)
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

func (s *Server) commandLease() time.Duration {
	if s.cfg.CommandLease > 0 {
		return s.cfg.CommandLease
	}
	return commandLeaseDefault
}
