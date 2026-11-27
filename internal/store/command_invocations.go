package store

// Command invocation records (change keel-plugin-commands).
//
// One chat command call is one row keyed by (tenant, session, requestId). The
// row is the durable idempotency fact and the audit account of the call:
// command/source/actor/status/decisionId/duration, the argument hash and length
// (never the argument document), and the bounded result the first call
// produced. A replay reads this row instead of executing the command again.
//
// The event log carries the user-facing `command.invoked` projection; this table
// carries the requestId state machine, including the phase where a destructive
// command waits for its durable decision.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Command invocation statuses. They are stable wire values (the API answers
// them and the projection records them).
const (
	// CommandStatusRunning is the claim: one caller owns the execution.
	CommandStatusRunning = "running"
	// CommandStatusPendingDecision is a destructive command waiting for its
	// durable decision. A replay answers 202 with the same decision.
	CommandStatusPendingDecision = "pending_decision"
	// CommandStatusOK is a completed execution.
	CommandStatusOK = "ok"
	// CommandStatusError is a failed execution (a tool error, a seam failure).
	CommandStatusError = "error"
	// CommandStatusRefused is a durable decision answered "deny".
	CommandStatusRefused = "refused"
	// CommandStatusTimedOut is a decision whose deadline passed.
	CommandStatusTimedOut = "timed_out"
	// CommandStatusCancelled is a decision cancelled before an answer.
	CommandStatusCancelled = "cancelled"
)

// CommandInvocation is one durable command call.
type CommandInvocation struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	// RequestID is the caller's idempotency key (unique per session).
	RequestID string
	// Command is the resolved published name (compact, skill:<name> or
	// <plugin-id>:<name>).
	Command string
	// Source is builtin|plugin|skill.
	Source string
	// Actor is the calling principal label (account or bot id).
	Actor string
	// Status is one of the CommandStatus* values.
	Status string
	// DecisionID is the durable decision a destructive command opened.
	DecisionID string
	// ArgsHash is the SHA-256 of the canonical argument document; ArgsBytes its
	// length. The document itself is never stored.
	ArgsHash  string
	ArgsBytes int
	// DurationMs is the execution time (0 while pending).
	DurationMs int64
	// EventSeq is the command.invoked projection sequence this state produced
	// (0 when no event could be appended).
	EventSeq int64
	// Result is the bounded structured result of a completed execution.
	Result json.RawMessage
	// Truncated marks a result that exceeded the API's result bound.
	Truncated bool
	// ErrorCode/ErrorMessage describe a failed execution or refusal. The message
	// is a stable, argument-free description.
	ErrorCode    string
	ErrorMessage string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Terminal reports whether the row reached a final state (a replay answers it
// without re-executing).
func (c CommandInvocation) Terminal() bool {
	switch c.Status {
	case CommandStatusOK, CommandStatusError, CommandStatusRefused, CommandStatusTimedOut, CommandStatusCancelled:
		return true
	}
	return false
}

// ClaimCommandInvocation inserts the running row, or returns the existing row
// when the requestId was already used. inserted reports that this caller owns
// the execution; a false value means the caller must replay the stored state.
func (s *Store) ClaimCommandInvocation(ctx context.Context, row CommandInvocation) (CommandInvocation, bool, error) {
	if row.TenantID == uuid.Nil || row.SessionID == uuid.Nil || row.RequestID == "" {
		return CommandInvocation{}, false, errors.New("store: command invocation needs tenant, session and requestId")
	}
	if row.Status == "" {
		row.Status = CommandStatusRunning
	}
	var result []byte
	err := s.pool.QueryRow(ctx,
		`INSERT INTO command_invocations
		    (tenant_id, session_id, request_id, command, source, actor, status,
		     decision_id, args_hash, args_bytes)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, '')::uuid, $9, $10)
		 ON CONFLICT (tenant_id, session_id, request_id) DO NOTHING
		 RETURNING tenant_id, session_id, request_id, command, source, actor, status,
		           COALESCE(decision_id::text, ''), args_hash, args_bytes, duration_ms, event_seq,
		           result, truncated, error_code, error_message, created_at, updated_at`,
		row.TenantID, row.SessionID, row.RequestID, row.Command, row.Source, row.Actor, row.Status,
		row.DecisionID, row.ArgsHash, row.ArgsBytes).Scan(
		&row.TenantID, &row.SessionID, &row.RequestID, &row.Command, &row.Source, &row.Actor, &row.Status,
		&row.DecisionID, &row.ArgsHash, &row.ArgsBytes, &row.DurationMs, &row.EventSeq,
		&result, &row.Truncated, &row.ErrorCode, &row.ErrorMessage, &row.CreatedAt, &row.UpdatedAt)
	if err == nil {
		if len(result) > 0 {
			row.Result = json.RawMessage(result)
		}
		return row, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CommandInvocation{}, false, fmt.Errorf("store: claim command invocation: %w", err)
	}
	existing, found, err := s.GetCommandInvocation(ctx, row.TenantID, row.SessionID, row.RequestID)
	if err != nil {
		return CommandInvocation{}, false, err
	}
	if !found {
		return CommandInvocation{}, false, fmt.Errorf("store: command invocation %q vanished after a conflict", row.RequestID)
	}
	return existing, false, nil
}

// GetCommandInvocation reads one row. A foreign tenant/session answers found
// false, never another row.
func (s *Store) GetCommandInvocation(ctx context.Context, tenantID, sessionID uuid.UUID, requestID string) (CommandInvocation, bool, error) {
	var row CommandInvocation
	var result []byte
	err := s.pool.QueryRow(ctx,
		`SELECT tenant_id, session_id, request_id, command, source, actor, status,
		        COALESCE(decision_id::text, ''), args_hash, args_bytes, duration_ms, event_seq,
		        result, truncated, error_code, error_message, created_at, updated_at
		   FROM command_invocations
		  WHERE tenant_id = $1 AND session_id = $2 AND request_id = $3`,
		tenantID, sessionID, requestID).Scan(
		&row.TenantID, &row.SessionID, &row.RequestID, &row.Command, &row.Source, &row.Actor, &row.Status,
		&row.DecisionID, &row.ArgsHash, &row.ArgsBytes, &row.DurationMs, &row.EventSeq,
		&result, &row.Truncated, &row.ErrorCode, &row.ErrorMessage, &row.CreatedAt, &row.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CommandInvocation{}, false, nil
	}
	if err != nil {
		return CommandInvocation{}, false, fmt.Errorf("store: get command invocation: %w", err)
	}
	if len(result) > 0 {
		row.Result = json.RawMessage(result)
	}
	return row, true, nil
}

// UpdateCommandInvocation persists a state transition (decision open, terminal
// result). The identity columns are immutable; updated_at advances.
func (s *Store) UpdateCommandInvocation(ctx context.Context, row CommandInvocation) error {
	if row.TenantID == uuid.Nil || row.SessionID == uuid.Nil || row.RequestID == "" {
		return errors.New("store: command invocation needs tenant, session and requestId")
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE command_invocations
		    SET status = $4, decision_id = NULLIF($5, '')::uuid, duration_ms = $6,
		        event_seq = $7, result = $8, truncated = $9, error_code = $10, error_message = $11,
		        updated_at = now()
		  WHERE tenant_id = $1 AND session_id = $2 AND request_id = $3`,
		row.TenantID, row.SessionID, row.RequestID, row.Status, row.DecisionID, row.DurationMs,
		row.EventSeq, nullableJSON(row.Result), row.Truncated, row.ErrorCode, row.ErrorMessage)
	if err != nil {
		return fmt.Errorf("store: update command invocation: %w", err)
	}
	return nil
}

// nullableJSON maps an empty result onto SQL NULL.
func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}
