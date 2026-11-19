package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Bash job statuses. A job is either running (the in-process goroutine exists)
// or terminal. `interrupted` is the terminal state recovery assigns to a job
// whose owning process died; it never means "still running" (the goroutine
// cannot survive the process, docs/GBASH-CONTRACT.md §5.4).
const (
	BashJobRunning     = "running"
	BashJobExited      = "exited"
	BashJobInterrupted = "interrupted"
	BashJobCanceled    = "canceled"
	BashJobFailed      = "failed"
)

// ErrBashJobLimit reports that a new job would exceed the global or per-group
// running cap. The caller refuses the new job; existing jobs are never killed
// to make room.
var ErrBashJobLimit = errors.New("store: bash job limit reached")

// ErrNoBashJob means the job does not exist for the tenant.
var ErrNoBashJob = errors.New("store: bash job not found")

// BashJob is one durable background-job fact.
type BashJob struct {
	TenantID        uuid.UUID
	JobID           uuid.UUID
	SessionID       *uuid.UUID
	GroupKey        string
	Command         string
	Cwd             string
	Status          string
	ExitCode        *int
	StdoutBlobID    *uuid.UUID
	StderrBlobID    *uuid.UUID
	StdoutBytes     int64
	StderrBytes     int64
	StdoutHead      string
	StdoutTail      string
	StderrHead      string
	StderrTail      string
	OutputTruncated bool
	Reason          string
	CreatedAt       time.Time
	StartedAt       time.Time
	FinishedAt      *time.Time
}

// bashJobColumns is the read projection. Nullable columns are rendered as
// sentinel text so scanning never depends on driver pointer support.
const bashJobColumns = `tenant_id, job_id, COALESCE(session_id::text, ''), group_key, command, cwd,
	status, COALESCE(exit_code, -1), COALESCE(stdout_blob_id::text, ''), COALESCE(stderr_blob_id::text, ''),
	stdout_bytes, stderr_bytes, stdout_head, stdout_tail, stderr_head, stderr_tail,
	output_truncated, reason, created_at, started_at, finished_at`

func scanBashJob(row rowScanner) (BashJob, error) {
	var (
		job          BashJob
		sessionText  string
		exitCode     int
		stdoutIDText string
		stderrIDText string
	)
	err := row.Scan(
		&job.TenantID, &job.JobID, &sessionText, &job.GroupKey, &job.Command, &job.Cwd,
		&job.Status, &exitCode, &stdoutIDText, &stderrIDText,
		&job.StdoutBytes, &job.StderrBytes, &job.StdoutHead, &job.StdoutTail, &job.StderrHead, &job.StderrTail,
		&job.OutputTruncated, &job.Reason, &job.CreatedAt, &job.StartedAt, &job.FinishedAt,
	)
	if err != nil {
		return BashJob{}, err
	}
	if sessionText != "" {
		parsed, err := uuid.Parse(sessionText)
		if err != nil {
			return BashJob{}, fmt.Errorf("store: parse bash job session id: %w", err)
		}
		job.SessionID = &parsed
	}
	if exitCode >= 0 {
		job.ExitCode = &exitCode
	}
	if stdoutIDText != "" {
		parsed, err := uuid.Parse(stdoutIDText)
		if err != nil {
			return BashJob{}, fmt.Errorf("store: parse stdout blob id: %w", err)
		}
		job.StdoutBlobID = &parsed
	}
	if stderrIDText != "" {
		parsed, err := uuid.Parse(stderrIDText)
		if err != nil {
			return BashJob{}, fmt.Errorf("store: parse stderr blob id: %w", err)
		}
		job.StderrBlobID = &parsed
	}
	return job, nil
}

// CreateBashJob reserves a running job. The limit check and the insert run in
// one transaction serialized by a per-tenant advisory lock, so two concurrent
// starts cannot both slip past the cap. maxGlobal/maxGroup <= 0 means no cap
// for that dimension; exceeding it returns ErrBashJobLimit and writes nothing.
func (s *Store) CreateBashJob(ctx context.Context, job BashJob, maxGlobal, maxGroup int) error {
	// uuid.Nil is a legitimate tenant here: the no-auth dev server has no tenant
	// and its bash jobs still need the same durable facts. Row-level security
	// treats it as an ordinary tenant_id value.
	if job.JobID == uuid.Nil {
		return errors.New("store: bash job needs an id")
	}
	if job.Status == "" {
		job.Status = BashJobRunning
	}
	err := retryTxBudget(ctx, 6, func() error {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, job.TenantID.String()); err != nil {
			return err
		}
		if maxGlobal > 0 {
			var running int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM bash_jobs WHERE tenant_id = $1 AND status = 'running'`,
				job.TenantID).Scan(&running); err != nil {
				return err
			}
			if running >= maxGlobal {
				return ErrBashJobLimit
			}
		}
		if maxGroup > 0 && job.GroupKey != "" {
			var running int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM bash_jobs
				  WHERE tenant_id = $1 AND group_key = $2 AND status = 'running'`,
				job.TenantID, job.GroupKey).Scan(&running); err != nil {
				return err
			}
			if running >= maxGroup {
				return ErrBashJobLimit
			}
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO bash_jobs (tenant_id, job_id, session_id, group_key, command, cwd, status,
			                       exit_code, stdout_blob_id, stderr_blob_id, stdout_bytes, stderr_bytes,
			                       stdout_head, stdout_tail, stderr_head, stderr_tail, output_truncated, reason)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
			job.TenantID, job.JobID, job.SessionID, job.GroupKey, job.Command, job.Cwd, job.Status,
			job.ExitCode, job.StdoutBlobID, job.StderrBlobID, job.StdoutBytes, job.StderrBytes,
			job.StdoutHead, job.StdoutTail, job.StderrHead, job.StderrTail, job.OutputTruncated, job.Reason)
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		return fmt.Errorf("store: create bash job: %w", err)
	}
	return nil
}

// BashJobFinish is the terminal update of a job.
type BashJobFinish struct {
	Status          string
	ExitCode        *int
	StdoutBlobID    *uuid.UUID
	StderrBlobID    *uuid.UUID
	StdoutBytes     int64
	StderrBytes     int64
	StdoutHead      string
	StdoutTail      string
	StderrHead      string
	StderrTail      string
	OutputTruncated bool
	Reason          string
}

// FinishBashJob records a terminal state. Only a still-running row can finish,
// so a duplicate finish (a sweeper racing the goroutine) is a no-op and the
// first terminal fact wins. It reports whether this call performed the update.
func (s *Store) FinishBashJob(ctx context.Context, tenantID, jobID uuid.UUID, finish BashJobFinish) (bool, error) {
	if finish.Status == BashJobRunning || finish.Status == "" {
		return false, errors.New("store: finish bash job needs a terminal status")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE bash_jobs
		    SET status = $3, exit_code = $4, stdout_blob_id = $5, stderr_blob_id = $6,
		        stdout_bytes = $7, stderr_bytes = $8, stdout_head = $9, stdout_tail = $10,
		        stderr_head = $11, stderr_tail = $12, output_truncated = $13, reason = $14,
		        finished_at = now()
		  WHERE tenant_id = $1 AND job_id = $2 AND status = 'running'`,
		tenantID, jobID, finish.Status, finish.ExitCode, finish.StdoutBlobID, finish.StderrBlobID,
		finish.StdoutBytes, finish.StderrBytes, finish.StdoutHead, finish.StdoutTail,
		finish.StderrHead, finish.StderrTail, finish.OutputTruncated, finish.Reason)
	if err != nil {
		return false, fmt.Errorf("store: finish bash job: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// UpdateBashJobProgress persists the bounded head/tail snapshot of a running
// job. It exists so a process crash still leaves readable output facts behind:
// the goroutine dies with the process, and the full blob is only completed when
// the stream ends, but the last persisted window survives. A terminal row is
// never overwritten.
func (s *Store) UpdateBashJobProgress(ctx context.Context, tenantID, jobID uuid.UUID, stdoutHead, stdoutTail, stderrHead, stderrTail string, stdoutBytes, stderrBytes int64, truncated bool) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE bash_jobs
		    SET stdout_head = $3, stdout_tail = $4, stderr_head = $5, stderr_tail = $6,
		        stdout_bytes = $7, stderr_bytes = $8, output_truncated = $9
		  WHERE tenant_id = $1 AND job_id = $2 AND status = 'running'`,
		tenantID, jobID, stdoutHead, stdoutTail, stderrHead, stderrTail,
		stdoutBytes, stderrBytes, truncated)
	if err != nil {
		return false, fmt.Errorf("store: update bash job progress: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// GetBashJob reads one job.
func (s *Store) GetBashJob(ctx context.Context, tenantID, jobID uuid.UUID) (BashJob, error) {
	job, err := scanBashJob(s.pool.QueryRow(ctx,
		`SELECT `+bashJobColumns+` FROM bash_jobs WHERE tenant_id = $1 AND job_id = $2`,
		tenantID, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return BashJob{}, ErrNoBashJob
	}
	if err != nil {
		return BashJob{}, fmt.Errorf("store: get bash job: %w", err)
	}
	return job, nil
}

// ListBashJobs returns the tenant's jobs, running first, newest first. status
// filters by exact value when non-empty; limit defaults to 20 and caps at 200.
func (s *Store) ListBashJobs(ctx context.Context, tenantID uuid.UUID, status string, limit int) ([]BashJob, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	query := `SELECT ` + bashJobColumns + ` FROM bash_jobs WHERE tenant_id = $1`
	args := []any{tenantID}
	if status != "" {
		query += ` AND status = $2`
		args = append(args, status)
	}
	query += ` ORDER BY CASE WHEN status = 'running' THEN 0 ELSE 1 END, created_at DESC LIMIT ` +
		fmt.Sprintf("%d", limit)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list bash jobs: %w", err)
	}
	defer rows.Close()
	out := make([]BashJob, 0, limit)
	for rows.Next() {
		job, err := scanBashJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan bash job: %w", err)
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// RunningBashJobsBySession lists the session's running jobs (orphan recovery).
func (s *Store) RunningBashJobsBySession(ctx context.Context, tenantID, sessionID uuid.UUID) ([]BashJob, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+bashJobColumns+`
		   FROM bash_jobs
		  WHERE tenant_id = $1 AND session_id = $2 AND status = 'running'
		  ORDER BY created_at`,
		tenantID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: running bash jobs by session: %w", err)
	}
	defer rows.Close()
	var out []BashJob
	for rows.Next() {
		job, err := scanBashJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan bash job: %w", err)
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// ConvergeRunningBashJobs marks running jobs terminal after an event that
// invalidates their carrier (harness restart, session deletion). A nil tenant
// converges every tenant (process restart); a nil-safe pointer lets the
// no-auth dev tenant (uuid.Nil) be addressed explicitly. The return lists the
// rows exactly as they were converged so a caller can log or attach facts.
func (s *Store) ConvergeRunningBashJobs(ctx context.Context, tenantID *uuid.UUID, status, reason string) ([]BashJob, error) {
	if status == "" || status == BashJobRunning {
		return nil, errors.New("store: converge needs a terminal status")
	}
	query := `UPDATE bash_jobs
	             SET status = $1, reason = $2, finished_at = now()
	           WHERE status = 'running'`
	args := []any{status, reason}
	if tenantID != nil {
		query += ` AND tenant_id = $3`
		args = append(args, *tenantID)
	}
	query += ` RETURNING ` + bashJobColumns
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: converge bash jobs: %w", err)
	}
	defer rows.Close()
	var out []BashJob
	for rows.Next() {
		job, err := scanBashJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan converged bash job: %w", err)
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// SessionActive reports whether the session exists and is not archived. A
// missing session returns false: its jobs are orphans.
func (s *Store) SessionActive(ctx context.Context, tenantID, sessionID uuid.UUID) (bool, error) {
	var active bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM sessions
		                 WHERE tenant_id = $1 AND session_id = $2 AND archived_at IS NULL)`,
		tenantID, sessionID).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("store: session active: %w", err)
	}
	return active, nil
}
