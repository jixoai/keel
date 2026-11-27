package superagentboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/superagent"

	"github.com/jixoai/keel-board/protocol"
)

// PrincipalResolver resolves the authenticated board caller. It is
// *boardintegration.Identity in production; tests inject a fake.
type PrincipalResolver interface {
	Actor(r *http.Request) (protocol.Actor, error)
	Tenant(r *http.Request) (string, error)
}

// TaskAuthorizer is the board's authorization for a mirrored-task action. It is
// *boardintegration.Authorizer in production. A mirrored task's channel is the
// supervisor queue's projection and carries its own relation tuples; hiding an
// action in the UI is not authorization, so the bridge asks the channel-level
// decision before any supervisor command. AuthorizeAnywhere remains for callers
// that name no channel; the bridge no longer relies on it.
type TaskAuthorizer interface {
	Authorize(ctx context.Context, tenant string, actor protocol.Actor, channel string, permission protocol.Permission) error
	AuthorizeAnywhere(ctx context.Context, tenant string, actor protocol.Actor, permission protocol.Permission) error
}

// TaskWriteGate is the supervisor's operator boundary for a board action. The
// bridge asks it before calling any supervisor command, so complete/fail/block
// cannot bypass the task.write requirement the supervisor applies to its own
// operator routes. It is action-agnostic; the supervisor still checks the
// ticket-specific operator permission for claim/assign/cancel.
type TaskWriteGate interface {
	AuthorizeTaskWrite(ctx context.Context, tenantID uuid.UUID) error
}

// Bridge wraps the board HTTP surface and routes actions on mirrored tasks to
// the supervisor first. It is the 1b command bridge; it never owns a second
// board mutation path for mirrored tasks.
type Bridge struct {
	Next       http.Handler
	Identity   PrincipalResolver
	Authorizer TaskAuthorizer
	Operator   TaskWriteGate
	Logger     *slog.Logger
	// Timeout bounds one supervisor command. Zero uses 5s.
	Timeout time.Duration

	mu       sync.Mutex
	links    LinkStore
	config   ConfigReader
	mirror   *Mirror
	commands *SupervisorCommands
	board    BoardPort
}

// Attach wires the supervisor-side seams after the agent runtime exists. The
// HTTP mount happens before the runtime, so the bridge starts as a pass-through
// and becomes active when Attach lands.
func (b *Bridge) Attach(links LinkStore, config ConfigReader, mirror *Mirror, commands *SupervisorCommands, board BoardPort) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.links = links
	b.config = config
	b.mirror = mirror
	b.commands = commands
	b.board = board
}

func (b *Bridge) snapshot() (LinkStore, ConfigReader, *Mirror, *SupervisorCommands, BoardPort) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.links, b.config, b.mirror, b.commands, b.board
}

// ServeHTTP implements the board surface wrapper.
func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if b.Next == nil {
		http.NotFound(w, r)
		return
	}
	links, configReader, mirror, commands, board := b.snapshot()
	if links == nil || configReader == nil || mirror == nil || commands == nil || board == nil {
		b.Next.ServeHTTP(w, r)
		return
	}
	action, taskID, ok := parseTaskAction(r)
	if !ok {
		if b.rejectMessageMutation(w, r, links, board) || b.rejectReplyOnMirroredThread(w, r, links, board) {
			return
		}
		b.Next.ServeHTTP(w, r)
		return
	}
	tenantID, tenant, actor, ok := b.principal(r)
	if !ok {
		// Authentication is the board's to answer, not the bridge's.
		b.Next.ServeHTTP(w, r)
		return
	}
	config, err := configReader.SuperagentConfig(r.Context(), tenantID)
	if err != nil {
		writeBridgeError(w, http.StatusServiceUnavailable, "config_unavailable", "the board.superagent configuration could not be read")
		return
	}
	if action == "claim-ready" {
		// claim-ready has no task id, so a channel that overlaps the mirror
		// channel in either direction could select a mirrored task. The guard
		// applies even when both switches are off: a link from an earlier
		// enabled period is still a mirrored task the supervisor owns.
		if claimReadyTouchesMirror(r, config.Channel) {
			writeMirrorReadOnly(w, "claim-ready would bypass the supervisor; claim the mirrored task by id")
			return
		}
		b.Next.ServeHTTP(w, r)
		return
	}

	link, found, err := links.GetBoardLinkByTask(r.Context(), tenantID, BoardSource, store.BoardLinkTicket, taskID)
	if err != nil {
		writeBridgeError(w, http.StatusServiceUnavailable, "link_unavailable", "the mirrored task mapping could not be read")
		return
	}
	if !found {
		b.Next.ServeHTTP(w, r)
		return
	}
	if !config.Mirror || !config.Bridge || mirror.Secret == nil {
		writeMirrorReadOnly(w, "the mirrored task is read-only while board.superagent.bridge is off")
		return
	}
	switch action {
	case "claim", "complete", "fail", "block", "unblock", "assign", "cancel":
	default:
		writeMirrorReadOnly(w, "this board action is not part of the supervisor command bridge")
		return
	}

	task, err := board.Task(r.Context(), tenantID, taskID)
	if err != nil {
		writeBridgeError(w, http.StatusNotFound, string(protocol.CodeNotFound), "the mirrored task does not exist")
		return
	}
	if b.Authorizer != nil {
		// The channel relation is the authority. LogBoard.Task bypasses the
		// board's HTTP authorizer, so a caller who may write elsewhere must not
		// reach this ticket's supervisor command: the UI hiding a move is not
		// authorization.
		if authErr := b.Authorizer.Authorize(r.Context(), tenant, actor, task.Channel, protocol.PermissionLabel); authErr != nil {
			writeAuthorizerError(w, authErr)
			return
		}
	}
	if b.Operator == nil {
		writeBridgeError(w, http.StatusServiceUnavailable, "task_write_gate_unwired",
			"the supervisor operator gate is not wired in this deployment")
		return
	}
	if gateErr := b.Operator.AuthorizeTaskWrite(r.Context(), tenantID); gateErr != nil {
		writeBridgeError(w, http.StatusForbidden, string(protocol.CodeForbidden),
			"supervisor task.write at tenant scope is required for a mirrored task action")
		return
	}

	body, err := decodeActionBody(r)
	if err != nil {
		writeBridgeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	operator := strings.TrimSpace(actor.Principal)
	if operator == "" {
		operator = strings.TrimSpace(actor.ID)
	}
	// Validate the board-only half before the supervisor writes: assign's `for`
	// is board presentation, and a malformed value must be a 400 without a
	// supervisor side effect.
	var assignTarget string
	if action == "assign" {
		assignTarget, err = assignFor(task, body.For)
		if err != nil {
			writeBridgeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
	}

	timeout := b.Timeout
	if timeout <= 0 {
		timeout = DefaultCommandLimit
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	ticketID, err := uuid.Parse(link.SourceID)
	if err != nil {
		writeBridgeError(w, http.StatusServiceUnavailable, "link_invalid", "the mirrored ticket id is invalid")
		return
	}
	if err := b.applyCommand(ctx, commands, action, ticketID, task, body, operator); err != nil {
		writeSupervisorError(w, err)
		return
	}
	// The supervisor wrote. Now the board may change: assign is board-only
	// presentation (the supervisor's assignment is durable already), and every
	// other action is reflected by a mirror pass of the new truth.
	if action == "assign" {
		if err := board.Assign(r.Context(), tenantID, AssignRequest{
			TaskID: taskID, For: assignTarget, Actor: ServiceActor(tenantID),
		}); err != nil {
			// The supervisor accepted; only the board projection failed. Say so
			// instead of returning 200 for a half-applied action.
			b.logger().Warn("superagent board bridge assign mirror failed", "ticket", ticketID, "err", err)
			writeJSON(w, http.StatusAccepted, map[string]any{
				"supervisorAccepted": true,
				"boardSynced":        false,
				"boardError":         "assign_not_mirrored",
			})
			return
		}
	}
	if _, err := mirror.SyncTicket(ctx, ticketID); err != nil {
		// The supervisor accepted and is the truth; the board will catch up on
		// the next mirror tick. Say so instead of pretending the board landed.
		writeJSON(w, http.StatusAccepted, map[string]any{
			"supervisorAccepted": true,
			"boardSynced":        false,
		})
		return
	}
	view, err := board.TaskView(r.Context(), tenantID, taskID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"task": map[string]any{"id": strconv.FormatUint(taskID, 10), "state": task.State},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": view})
}

func (b *Bridge) applyCommand(ctx context.Context, commands *SupervisorCommands, action string, ticketID uuid.UUID, task TaskInfo, body actionBody, operator string) error {
	switch action {
	case "claim":
		return commands.ClaimTicket(ctx, commands.TenantID, ticketID, operator)
	case "assign":
		return commands.AssignTicket(ctx, commands.TenantID, ticketID, body.forValue(), operator)
	case "complete":
		return commands.CompleteTicket(ctx, commands.TenantID, ticketID, body.resultValue(), operator)
	case "fail":
		return commands.FailTicket(ctx, commands.TenantID, ticketID, body.reasonValue(), operator)
	case "block":
		return commands.BlockTicket(ctx, commands.TenantID, ticketID, body.kindValue(), body.reasonValue(), task.Version, operator)
	case "unblock":
		return commands.UnblockTicket(ctx, commands.TenantID, ticketID, body.optionValue(), body.reasonValue(), operator)
	case "cancel":
		return commands.CancelTicket(ctx, commands.TenantID, ticketID, body.reasonValue(), operator)
	default:
		return fmt.Errorf("superagentboard: unsupported action %q", action)
	}
}

// rejectReplyOnMirroredThread refuses a new reply whose `re` names a mirrored
// root. The thread is the supervisor's projection; a board reply would exist
// only on the board and vanish from the supervisor's timeline.
func (b *Bridge) rejectReplyOnMirroredThread(w http.ResponseWriter, r *http.Request, links LinkStore, board BoardPort) bool {
	if r.Method != http.MethodPost || strings.TrimSuffix(r.URL.Path, "/") != "/v1/board/messages" {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var body struct {
		Re string `json:"re"`
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	if err := json.Unmarshal(raw, &body); err != nil || strings.TrimSpace(body.Re) == "" {
		return false
	}
	rootID, ok := boardRef(body.Re)
	if !ok {
		return false
	}
	tenantID, _, _, ok := b.principal(r)
	if !ok {
		return false
	}
	root, _, found, err := board.MessageRoot(r.Context(), tenantID, rootID)
	if err != nil || !found {
		return false
	}
	_, linked, err := links.GetBoardLinkByTask(r.Context(), tenantID, BoardSource, store.BoardLinkTicket, root)
	if err != nil || !linked {
		return false
	}
	writeMirrorReadOnly(w, "a mirrored task thread is read-only; replies belong to the supervisor timeline")
	return true
}

// boardRef parses a board decimal ref for a guard that only needs the number.
func boardRef(value string) (uint64, bool) {
	id, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}

// rejectMessageMutation refuses edit/delete/label/reaction writes on a mirrored thread.
// It returns true when it answered the request.
func (b *Bridge) rejectMessageMutation(w http.ResponseWriter, r *http.Request, links LinkStore, board BoardPort) bool {
	if r.Method != http.MethodPost {
		return false
	}
	id, action, ok := parseMessageAction(r.URL.Path)
	if !ok {
		return false
	}
	switch action {
	case "edit", "delete", "labels", "reactions":
	default:
		return false
	}
	tenantID, _, _, ok := b.principal(r)
	if !ok {
		return false
	}
	rootID, channel, found, err := board.MessageRoot(r.Context(), tenantID, id)
	if err != nil || !found {
		return false
	}
	_, linked, err := links.GetBoardLinkByTask(r.Context(), tenantID, BoardSource, store.BoardLinkTicket, rootID)
	if err != nil || !linked {
		return false
	}
	_ = channel
	writeMirrorReadOnly(w, "a mirrored task thread is read-only; use the supervisor-facing task actions")
	return true
}

func (b *Bridge) principal(r *http.Request) (uuid.UUID, string, protocol.Actor, bool) {
	if b.Identity == nil {
		return uuid.Nil, "", protocol.Actor{}, false
	}
	tenant, err := b.Identity.Tenant(r)
	if err != nil {
		return uuid.Nil, "", protocol.Actor{}, false
	}
	tenantID, err := uuid.Parse(strings.TrimSpace(tenant))
	if err != nil || tenantID == uuid.Nil {
		return uuid.Nil, "", protocol.Actor{}, false
	}
	actor, err := b.Identity.Actor(r)
	if err != nil {
		return uuid.Nil, "", protocol.Actor{}, false
	}
	return tenantID, tenant, actor, true
}

func (b *Bridge) logger() *slog.Logger {
	if b != nil && b.Logger != nil {
		return b.Logger
	}
	return slog.Default()
}

type actionBody struct {
	For      *string `json:"for"`
	Result   *string `json:"result"`
	Reason   *string `json:"reason"`
	Kind     *string `json:"kind"`
	OptionID *string `json:"optionId"`
}

func (a actionBody) forValue() string {
	if a.For == nil {
		return ""
	}
	return *a.For
}

func (a actionBody) resultValue() string {
	if a.Result == nil {
		return ""
	}
	return *a.Result
}

func (a actionBody) reasonValue() string {
	if a.Reason == nil {
		return ""
	}
	return *a.Reason
}

func (a actionBody) kindValue() string {
	if a.Kind == nil {
		return ""
	}
	return *a.Kind
}

func (a actionBody) optionValue() string {
	if a.OptionID == nil {
		return ""
	}
	return *a.OptionID
}

func decodeActionBody(r *http.Request) (actionBody, error) {
	if r.Body == nil {
		return actionBody{}, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return actionBody{}, errors.New("the request body could not be read")
	}
	var body actionBody
	if len(bytes.TrimSpace(raw)) == 0 {
		return body, nil
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return actionBody{}, errors.New("the request body is not a JSON object")
	}
	return body, nil
}

func assignFor(task TaskInfo, forValue *string) (string, error) {
	if forValue == nil || strings.TrimSpace(*forValue) == "" {
		return "", nil
	}
	value := strings.TrimSpace(*forValue)
	if strings.Contains(value, "+") {
		return value, nil
	}
	if task.Owner == "" {
		return "", errors.New("the mirrored task has no owner")
	}
	return task.Owner + "+" + value, nil
}

// parseTaskAction recognizes /v1/board/tasks/{id}/{action} and the
// claim-ready route /v1/board/tasks/claim.
func parseTaskAction(r *http.Request) (string, uint64, bool) {
	if r.Method != http.MethodPost {
		return "", 0, false
	}
	const prefix = "/v1/board/tasks/"
	path := strings.TrimSuffix(r.URL.Path, "/")
	if !strings.HasPrefix(path, prefix) {
		return "", 0, false
	}
	rest := strings.TrimPrefix(path, prefix)
	parts := strings.Split(rest, "/")
	if len(parts) == 1 && parts[0] == "claim" {
		return "claim-ready", 0, true
	}
	if len(parts) != 2 {
		return "", 0, false
	}
	id, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || id == 0 {
		return "", 0, false
	}
	return parts[1], id, true
}

func parseMessageAction(path string) (uint64, string, bool) {
	const prefix = "/v1/board/messages/"
	path = strings.TrimSuffix(path, "/")
	if !strings.HasPrefix(path, prefix) {
		return 0, "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) != 2 {
		return 0, "", false
	}
	id, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || id == 0 {
		return 0, "", false
	}
	return id, parts[1], true
}

func claimReadyTouchesMirror(r *http.Request, mirrorChannel string) bool {
	mirror := strings.Trim(strings.TrimSpace(mirrorChannel), "/")
	if mirror == "" {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return true
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var body struct {
		Channel *string `json:"channel"`
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return true
		}
	}
	if body.Channel == nil {
		// No channel names any channel in the board, including the mirror.
		return true
	}
	want := strings.Trim(strings.TrimSpace(*body.Channel), "/")
	if want == "" {
		return true
	}
	// Any overlap counts: the same channel, the mirror inside the request's
	// subtree (work/ contains work/agents), or the request inside the mirror's
	// subtree (superagent/sub inside superagent).
	return channelsOverlap(want, mirror)
}

func channelsOverlap(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func writeMirrorReadOnly(w http.ResponseWriter, message string) {
	writeBoardError(w, &protocol.Error{
		Status: http.StatusConflict, Code: protocol.Code("mirror_read_only"), Message: message,
	})
}

func writeAuthorizerError(w http.ResponseWriter, err error) {
	if boardErr, ok := protocol.AsError(err); ok {
		writeBoardError(w, boardErr)
		return
	}
	writeBridgeError(w, http.StatusForbidden, string(protocol.CodeForbidden), "permission denied")
}

func writeSupervisorError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeBridgeError(w, http.StatusGatewayTimeout, "supervisor_timeout",
			"the supervisor did not answer in time; the board was not changed")
	case errors.Is(err, ErrNoActiveAssignment):
		writeBridgeError(w, http.StatusConflict, "no_active_assignment",
			"the ticket has no active assignment for this action")
	case errors.Is(err, ErrNoOpenAttention):
		writeBridgeError(w, http.StatusConflict, "no_open_attention",
			"the ticket has no open attention to unblock")
	case errors.Is(err, superagent.ErrOperatorUnavailable), errors.Is(err, collab.ErrPermissionDenied):
		writeBridgeError(w, http.StatusForbidden, string(protocol.CodeForbidden), "the supervisor refused the action")
	case errors.Is(err, superagent.ErrAnswerDeliveryPending):
		// The decision is durable; the worker has not received it yet. The
		// mirror will reflect the answered Attention; 202 says exactly that.
		writeJSON(w, http.StatusAccepted, map[string]any{
			"supervisorAccepted": true,
			"deliveryPending":    true,
			"boardSynced":        false,
		})
	case errors.Is(err, superagent.ErrAnswerDeliveryFailed):
		writeBridgeError(w, http.StatusConflict, "answer_delivery_failed",
			"the answer is recorded but delivery failed; retry or dismiss it on the ticket board")
	case errors.Is(err, superagent.ErrAnswerNotDelivered):
		writeBridgeError(w, http.StatusConflict, "answer_not_delivered",
			"the answer is recorded but the ticket closed before a worker received it")
	case errors.Is(err, collab.ErrWorkItemNotFound), errors.Is(err, collab.ErrAssignmentNotFound),
		errors.Is(err, collab.ErrAttentionNotFound):
		writeBridgeError(w, http.StatusNotFound, string(protocol.CodeNotFound), "the supervisor record does not exist")
	case errors.Is(err, collab.ErrWorkItemClosed), errors.Is(err, collab.ErrAttentionConflict),
		errors.Is(err, collab.ErrAttentionClosed), errors.Is(err, collab.ErrAttentionExpired):
		writeBridgeError(w, http.StatusConflict, "supervisor_rejected", boundedError(err))
	default:
		// A 502 is the honest answer: the supervisor is the authority and it did
		// not accept the action. The board was not written.
		writeBridgeError(w, http.StatusBadGateway, "supervisor_rejected", boundedError(err))
	}
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 300 {
		message = message[:300]
	}
	return message
}

func writeBridgeError(w http.ResponseWriter, status int, code, message string) {
	writeBoardError(w, &protocol.Error{Status: status, Code: protocol.Code(code), Message: message})
}

func writeBoardError(w http.ResponseWriter, err *protocol.Error) {
	writeJSON(w, err.Status, err.Body())
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(body)
}
