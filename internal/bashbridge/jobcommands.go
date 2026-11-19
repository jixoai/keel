package bashbridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ewhauser/gbash/commands"
	"github.com/google/uuid"
)

// This file implements the keel-owned job view commands injected into every
// sandbox: `jobs` (this session's background jobs), `ps` (the tenant's
// background jobs) and `nohup` (start a background job and return its id).
// They read keel's JobManager, never the sandbox's process table: gbash has no
// real processes, `$!` is a virtual label and `$$` is 1, so a host-shaped view
// would be a lie (GBASH-CONTRACT §4.1(b), §5; tool-surface "JobManager").
//
// `jobs` is registered explicitly because upstream ships a jobs name without an
// implementation: without this override `jobs` answers 127 (GBASH-CONTRACT
// §4.1(b)). `fg`/`bg` are deliberately not registered: gbash does not implement
// job control and a fake foreground switch would invent semantics.

// JobInfo is one background job as the sandbox job view sees it. It is the
// keel JobManager's persisted fact, never an OS process.
type JobInfo struct {
	ID        string
	Command   string
	Cwd       string
	Status    string
	Reason    string
	SessionID string
	ExitCode  *int
	CreatedAt time.Time
	// Live is true while this harness process still owns the job goroutine.
	Live bool
}

// JobBackend connects the sandbox job commands to the keel JobManager. List is
// tenant-scoped (the command decides whether to narrow to the session); Start
// registers one background job and returns its durable fact.
type JobBackend interface {
	ListJobs(ctx context.Context, tenantID uuid.UUID, limit int) ([]JobInfo, error)
	StartJob(ctx context.Context, tenantID, sessionID uuid.UUID, command string) (JobInfo, error)
}

const (
	jobCommandLimit     = 100
	jobCommandTruncate  = 120
	jobCommandExitUsage = 2
	jobCommandExitError = 1
)

// NewJobCommands returns the three keel job view commands. They are injected
// through Config.Commands; a nil backend still registers the names (so a
// deployment without a JobManager answers a diagnostic instead of 127).
func NewJobCommands(backend JobBackend) []Command {
	return []Command{
		{Name: "jobs", Run: func(ctx context.Context, inv Invocation) error {
			return runJobList(ctx, inv, backend, true)
		}},
		{Name: "nohup", Run: func(ctx context.Context, inv Invocation) error {
			return runNohup(ctx, inv, backend)
		}},
		{Name: "ps", Run: func(ctx context.Context, inv Invocation) error {
			return runJobList(ctx, inv, backend, false)
		}},
	}
}

// runJobList renders the JobManager view. `jobs` narrows to the ambient
// session (a shell's own jobs); `ps` shows the tenant's jobs. Both render the
// same persisted facts the `bash {action:"jobs"}` tool returns.
func runJobList(ctx context.Context, inv Invocation, backend JobBackend, sessionOnly bool) error {
	name := "jobs"
	if !sessionOnly {
		name = "ps"
	}
	identity, ok := SessionIdentityFromContext(ctx)
	if !ok {
		fmt.Fprintln(inv.Stdout, "no background jobs (this sandbox has no keel session)")
		return nil
	}
	if backend == nil {
		return jobCommandError(name, jobCommandExitError, errors.New("the keel job manager is not wired"))
	}
	jobs, err := backend.ListJobs(ctx, identity.TenantID, jobCommandLimit)
	if err != nil {
		return jobCommandError(name, jobCommandExitError, err)
	}
	if sessionOnly {
		filtered := make([]JobInfo, 0, len(jobs))
		for _, job := range jobs {
			if job.SessionID == identity.SessionID.String() {
				filtered = append(filtered, job)
			}
		}
		jobs = filtered
	}
	if len(jobs) == 0 {
		fmt.Fprintln(inv.Stdout, "no background jobs")
		return nil
	}
	if sessionOnly {
		fmt.Fprintf(inv.Stdout, "%d background job(s) in this session:\n", len(jobs))
	} else {
		fmt.Fprintf(inv.Stdout, "%d background job(s) in this tenant:\n", len(jobs))
	}
	for _, job := range jobs {
		status := job.Status
		if status == "" {
			status = "unknown"
		}
		fmt.Fprintf(inv.Stdout, "- job %s status=%s live=%t", job.ID, status, job.Live)
		if job.SessionID != "" && !sessionOnly {
			fmt.Fprintf(inv.Stdout, " session=%s", job.SessionID)
		}
		fmt.Fprintf(inv.Stdout, " cwd=%s", job.Cwd)
		if !job.CreatedAt.IsZero() {
			fmt.Fprintf(inv.Stdout, " created=%s", job.CreatedAt.UTC().Format(time.RFC3339))
		}
		if job.ExitCode != nil {
			fmt.Fprintf(inv.Stdout, " exitCode=%d", *job.ExitCode)
		}
		if job.Reason != "" {
			fmt.Fprintf(inv.Stdout, " reason=%s", job.Reason)
		}
		fmt.Fprintf(inv.Stdout, " command=%q\n", truncateJobCommand(job.Command))
	}
	return nil
}

// runNohup hands the command to the JobManager as one background job and
// returns its id immediately. It never starts a host process; the job runs in
// its own sandbox session and settles its own fs.capture when it ends.
func runNohup(ctx context.Context, inv Invocation, backend JobBackend) error {
	command := joinNohupArgs(inv.Args)
	if command == "" {
		return jobCommandError("nohup", jobCommandExitUsage, errors.New("usage: nohup <command> [args...]"))
	}
	if backend == nil {
		return jobCommandError("nohup", jobCommandExitError, errors.New("the keel job manager is not wired"))
	}
	identity, ok := SessionIdentityFromContext(ctx)
	if !ok {
		return jobCommandError("nohup", jobCommandExitError,
			errors.New("background jobs need a keel session; this sandbox is not attached to a run"))
	}
	// The background run is its own bash execution; give it its own capture
	// identity so its fs.capture cannot dedupe against (and hide behind) the
	// parent run's event.
	identity.CallID = nohupCallID(identity.CallID)
	ctx = WithSessionIdentity(ctx, identity)
	job, err := backend.StartJob(ctx, identity.TenantID, identity.SessionID, command)
	if err != nil {
		return jobCommandError("nohup", jobCommandExitError, err)
	}
	if strings.TrimSpace(job.ID) == "" {
		return jobCommandError("nohup", jobCommandExitError, errors.New("the job manager returned no job id"))
	}
	status := job.Status
	if status == "" {
		status = "running"
	}
	fmt.Fprintf(inv.Stdout, "job %s status=%s command=%q\n", job.ID, status, truncateJobCommand(command))
	return nil
}

// nohupCallID keeps the parent tool-call id as a prefix (the attribution trail
// stays readable) and appends one unique suffix per nohup invocation.
func nohupCallID(parent string) string {
	parent = strings.TrimSpace(parent)
	if parent == "" {
		return "nohup:" + uuid.NewString()
	}
	return parent + ":nohup:" + uuid.NewString()
}

// joinNohupArgs renders the invocation's argv back into one script. Simple
// arguments travel untouched; an argument with whitespace or shell syntax is
// single-quoted so the background shell cannot re-split or re-expand it. The
// parent shell has already performed its own expansions/redirections, exactly
// like a real `nohup command args...` invocation.
func joinNohupArgs(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "" || strings.ContainsAny(arg, " \t\n\"'\\$`*?[]{}()<>|&;!~#") {
			parts = append(parts, "'"+strings.ReplaceAll(arg, "'", `'\''`)+"'")
			continue
		}
		parts = append(parts, arg)
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func truncateJobCommand(command string) string {
	if len(command) <= jobCommandTruncate {
		return command
	}
	return command[:jobCommandTruncate-3] + "..."
}

func jobCommandError(name string, code int, err error) error {
	return &commands.ExitError{Code: code, Err: commands.Diagnosticf("%s: %v", name, err)}
}
