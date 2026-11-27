package store

// Command invocation records (change keel-plugin-commands).
//
// One chat command call is one row keyed by (tenant, session, requestId). The
// row is the durable idempotency/audit ledger of the call:
// command/source/actor/status/decisionId/duration, the argument hash and length
// (never the argument document), and the bounded result the first call
// produced. A replay reads this row instead of executing the command again.
//
// The event log carries the user-facing `command.invoked` projection (the row
// the chat renders); THIS TABLE is the audit/idempotency truth. The two carry
// the same identity fields; only the table owns the running/pending phase and
// the conditional transitions that make one requestId execute exactly once.
//
// State machine:
//
//	          insert (owner)
//	                v
//	            running --idle/plugin/permission failure--> terminal
//	                |
//	                +--danger=destructive--> pending_decision
//	                                            |
//	                        answered allow ---> running (ClaimDecisionResult)
//	                        deny/timeout/cancel -> terminal (conditional)
//
// A `running` row carries a lease. A replay that finds an expired lease may
// reclaim the execution (ClaimCommandInvocation); a live executor renews its
// lease (RenewCommandInvocation), so a healthy long command is never stolen.

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
	// Actor is the calling principal label (a Bot id for a bot principal, else
	// an account id).
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
	// LeaseUntil is the running owner's lease (nil for a terminal state or a
	// pending decision).
	LeaseUntil *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
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

// commandInvocationColumns is the one projection every read/write returns, so a
// new column cannot be forgotten in one of them.
const commandInvocationColumns = `tenant_id, session_id, request_id, command, source, actor, status,
	COALESCE(decision_id::text, ''), args_hash, args_bytes, duration_ms, event_seq,
	result, truncated, error_code, error_message, lease_until, created_at, updated_at`

func scanCommandInvocation(row interface{ Scan(...any) error }) (CommandInvocation, error) {
	var invitation CommandInvocation
	var result []byte
	err := row.Scan(
		&invitation.TenantID, &invitation.SessionID, &invitation.RequestID,
		&invitation.Command, &invitation.Source, &invitation.Actor, &invitation.Status,
		&invitation.DecisionID, &invitation.ArgsHash, &invitation.ArgsBytes,
		&invitation.DurationMs, &invitation.EventSeq, &result, &invitation.Truncated,
		&invitation.ErrorCode, &invitation.ErrorMessage, &invitation.LeaseUntil,
		&invitation.CreatedAt, &invitation.UpdatedAt,
	)
	if err != nil {
		return CommandInvocation{}, err
	}
	if len(result) > 0 {
		invitation.Result = json.RawMessage(result)
	}
	return invitation, nil
}

// ClaimCommandInvocation inserts the running row, reclaims an expired running
// lease, or returns the existing row. owned reports that this caller must
// execute; false means the caller replays or waits for the stored state.
func (s *Store) ClaimCommandInvocation(ctx context.Context, row CommandInvocation, lease time.Duration) (CommandInvocation, bool, error) {
	if row.TenantID == uuid.Nil || row.SessionID == uuid.Nil || row.RequestID == "" {
		return CommandInvocation{}, false, errors.New("store: command invocation needs tenant, session and requestId")
	}
	if row.Status == "" {
		row.Status = CommandStatusRunning
	}
	created, err := scanCommandInvocation(s.pool.QueryRow(ctx,
		`INSERT INTO command_invocations
		    (tenant_id, session_id, request_id, command, source, actor, status,
		     decision_id, args_hash, args_bytes, lease_until)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, '')::uuid, $9, $10,
		         now() + make_interval(secs => $11))
		 ON CONFLICT (tenant_id, session_id, request_id) DO NOTHING
		 RETURNING `+commandInvocationColumns,
		row.TenantID, row.SessionID, row.RequestID, row.Command, row.Source, row.Actor, row.Status,
		row.DecisionID, row.ArgsHash, row.ArgsBytes, leaseSeconds(lease)))
	if err == nil {
		return created, true, nil
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
	if existing.Status == CommandStatusRunning {
		// An expired lease belongs to a crashed/abandoned executor. The
		// conditional UPDATE is the CAS: exactly one replay reclaims it.
		reclaimed, err := scanCommandInvocation(s.pool.QueryRow(ctx,
			`UPDATE command_invocations
			    SET status = 'running',
			        lease_until = now() + make_interval(secs => $4),
			        updated_at = now()
			  WHERE tenant_id = $1 AND session_id = $2 AND request_id = $3
			    AND status = 'running'
			    AND (lease_until IS NULL OR lease_until <= now())
			  RETURNING `+commandInvocationColumns,
			row.TenantID, row.SessionID, row.RequestID, leaseSeconds(lease)))
		if err == nil {
			return reclaimed, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return CommandInvocation{}, false, fmt.Errorf("store: reclaim command invocation: %w", err)
		}
		// A live lease: read the row again (the winner may have advanced it).
		existing, found, err = s.GetCommandInvocation(ctx, row.TenantID, row.SessionID, row.RequestID)
		if err != nil {
			return CommandInvocation{}, false, err
		}
		if !found {
			return CommandInvocation{}, false, fmt.Errorf("store: command invocation %q disappeared", row.RequestID)
		}
	}
	return existing, false, nil
}

// ClaimDecisionResult is the CAS that turns one pending destructive decision's
// approval into exactly one execution: pending_decision -> running. Only the
// caller that gets owned=true may call the tool; every other replay waits for
// the terminal row.
func (s *Store) ClaimDecisionResult(ctx context.Context, tenantID, sessionID uuid.UUID, requestID string, lease time.Duration) (CommandInvocation, bool, error) {
	claimed, err := scanCommandInvocation(s.pool.QueryRow(ctx,
		`UPDATE command_invocations
		    SET status = 'running',
		        lease_until = now() + make_interval(secs => $4),
		        updated_at = now()
		  WHERE tenant_id = $1 AND session_id = $2 AND request_id = $3
		    AND status = 'pending_decision'
		  RETURNING `+commandInvocationColumns,
		tenantID, sessionID, requestID, leaseSeconds(lease)))
	if err == nil {
		return claimed, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CommandInvocation{}, false, fmt.Errorf("store: claim command decision result: %w", err)
	}
	existing, found, err := s.GetCommandInvocation(ctx, tenantID, sessionID, requestID)
	if err != nil {
		return CommandInvocation{}, false, err
	}
	if !found {
		return CommandInvocation{}, false, fmt.Errorf("store: command invocation %q not found", requestID)
	}
	return existing, false, nil
}

// RenewCommandInvocation renews a live execution's lease.
func (s *Store) RenewCommandInvocation(ctx context.Context, tenantID, sessionID uuid.UUID, requestID string, lease time.Duration) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE command_invocations
		    SET lease_until = now() + make_interval(secs => $4),
		        updated_at = now()
		  WHERE tenant_id = $1 AND session_id = $2 AND request_id = $3
		    AND status = 'running'`,
		tenantID, sessionID, requestID, leaseSeconds(lease))
	if err != nil {
		return false, fmt.Errorf("store: renew command invocation: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// TransitionCommandInvocation persists a state transition only from the status
// the caller observed. It is the guard that keeps a terminal write from
// overwriting a concurrent one; false means the row moved and the caller must
// read the current state.
func (s *Store) TransitionCommandInvocation(ctx context.Context, row CommandInvocation, from string) (bool, error) {
	if row.TenantID == uuid.Nil || row.SessionID == uuid.Nil || row.RequestID == "" {
		return false, errors.New("store: command invocation needs tenant, session and requestId")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE command_invocations
		    SET status = $5, decision_id = NULLIF($6, '')::uuid, duration_ms = $7,
		        event_seq = $8, result = $9, truncated = $10, error_code = $11, error_message = $12,
		        lease_until = CASE WHEN $5 = 'running' THEN lease_until ELSE NULL END,
		        updated_at = now()
		  WHERE tenant_id = $1 AND session_id = $2 AND request_id = $3 AND status = $4`,
		row.TenantID, row.SessionID, row.RequestID, from, row.Status, row.DecisionID,
		row.DurationMs, row.EventSeq, nullableJSON(row.Result), row.Truncated,
		row.ErrorCode, row.ErrorMessage)
	if err != nil {
		return false, fmt.Errorf("store: transition command invocation: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// GetCommandInvocation reads one row. A foreign tenant/session answers found
// false, never another row.
func (s *Store) GetCommandInvocation(ctx context.Context, tenantID, sessionID uuid.UUID, requestID string) (CommandInvocation, bool, error) {
	row, err := scanCommandInvocation(s.pool.QueryRow(ctx,
		`SELECT `+commandInvocationColumns+`
		   FROM command_invocations
		  WHERE tenant_id = $1 AND session_id = $2 AND request_id = $3`,
		tenantID, sessionID, requestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return CommandInvocation{}, false, nil
	}
	if err != nil {
		return CommandInvocation{}, false, fmt.Errorf("store: get command invocation: %w", err)
	}
	return row, true, nil
}

// leaseSeconds renders a lease for make_interval(secs => ...). A non-positive
// lease still gets a small positive value so a claim never inserts an
// immediately expired row by accident.
func leaseSeconds(lease time.Duration) float64 {
	seconds := lease.Seconds()
	if seconds <= 0 {
		seconds = 1
	}
	return seconds
}

// nullableJSON maps an empty result onto SQL NULL.
func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}
