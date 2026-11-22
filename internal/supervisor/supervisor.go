package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/durable"
	"github.com/jixoai/keel/internal/kernel"
)

// RunRecord is the supervisor's projection of one run row. It is read-only
// input: the run row remains the queue's truth and the ledger remains the
// recovery truth.
type RunRecord struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	Status    string
	Attempts  int
	// LeaseUntil nil or in the past means the executing process died.
	LeaseUntil *time.Time
	CursorSeq  int64
	LastError  string
	UpdatedAt  time.Time
}

// RequeueOutcome is the store's answer to an idempotent resume request.
type RequeueOutcome struct {
	Status   string
	Attempts int
}

// Store is the narrow persistence seam the supervisor needs. All methods are
// idempotent or read-only; the supervisor never claims a run.
type Store interface {
	// ListSupervisorRuns returns non-terminal runs plus failed runs that may be
	// resumed, oldest activity first.
	ListSupervisorRuns(ctx context.Context, limit int) ([]RunRecord, error)
	// RequeueRun re-enqueues one session's existing run row. It must be safe to
	// call while a worker claims the same row.
	RequeueRun(ctx context.Context, tenantID, sessionID uuid.UUID) (RequeueOutcome, error)
	// ParkRun moves a pending/expired-running row to a resumable failed state so
	// no worker claims it. It reports whether the row changed.
	ParkRun(ctx context.Context, tenantID, sessionID uuid.UUID, reason string) (bool, error)
	// ResumeAttemptCount counts every committed resume.attempt fact of a
	// session (operator and supervisor), used only to mint a unique fact key.
	ResumeAttemptCount(ctx context.Context, tenantID, sessionID uuid.UUID) (int, error)
	// AutoResumeCount counts supervisor automatic attempts that were counted
	// against the retry budget.
	AutoResumeCount(ctx context.Context, tenantID, sessionID uuid.UUID) (int, error)
	// LastAutoResume returns when the newest counted automatic resume landed.
	LastAutoResume(ctx context.Context, tenantID, sessionID uuid.UUID) (time.Time, bool, error)
	// EscalationCount counts committed supervision.escalated facts.
	EscalationCount(ctx context.Context, tenantID, sessionID uuid.UUID) (int, error)
	// EscalationsSince counts escalations committed at or after the given run
	// failure. It is the dedupe check that keeps one failure epoch from
	// escalating on every scan.
	EscalationsSince(ctx context.Context, tenantID, sessionID uuid.UUID, since time.Time) (int, error)
	// LatestSelection returns the newest selection fact of the session (the
	// input's model identity), or nil when no input named one.
	LatestSelection(ctx context.Context, tenantID, sessionID uuid.UUID) (json.RawMessage, error)
}

// PolicySource returns the policy and configuration identity for one scan.
// Reading it per scan is what makes a rotation affect only later attempts:
// facts already committed keep the version they recorded.
type PolicySource func() (Policy, ConfigVersion)

// Notification is an owner escalation.
type Notification struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	RequestID string
	Title     string
	Text      string
}

// NotificationResult reports whether the notification was persisted and its id.
type NotificationResult struct {
	ID     string
	Status string
}

// Notifier is the `keel.notify` owner channel.
type Notifier interface {
	Notify(ctx context.Context, req Notification) (NotificationResult, error)
}

// ParentResolver finds the durable parent session of a sub-agent. A main agent
// has no parent: escalation then goes straight to the owner.
type ParentResolver func(ctx context.Context, tenantID, childID uuid.UUID) (uuid.UUID, bool, error)

// ParentEscalator writes the parent-agent hand-off (the parent card's
// task.note). It is the L1 rung of the ladder.
type ParentEscalator interface {
	Escalate(ctx context.Context, parent, child kernel.Scope, reason string) (string, error)
}

// SuperAgentRequest is the L3 hand-off. The seam exists so an installed
// `keel.super-agent` can recover a main agent; without one the ladder stops at
// owner notification.
type SuperAgentRequest struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	Reason    string
	Attempts  int
	Config    ConfigVersion
}

// SuperAgent is the optional `keel.super-agent` capability.
type SuperAgent interface {
	Recover(ctx context.Context, req SuperAgentRequest) (string, error)
}

// Config assembles a Supervisor.
type Config struct {
	Store  Store
	Ledger kernel.Committer
	Policy PolicySource
	Logger *slog.Logger
	Now    func() time.Time
	// Interval is the periodic scan interval (2s default).
	Interval time.Duration
	// Limit bounds one scan (200 default).
	Limit int

	ParentOf        ParentResolver
	ParentEscalator ParentEscalator
	Notifier        Notifier
	SuperAgent      SuperAgent
	// ValidateSelection validates a providerFallback selection before it is
	// written into the resume fact. Nil refuses fallback selections (fail
	// closed): an unvalidatable model must not be forced on the next attempt.
	ValidateSelection func(json.RawMessage) error
}

// Report is one scan's accounting.
type Report struct {
	Scanned   int      `json:"scanned"`
	Queued    int      `json:"queued"`
	Recovered int      `json:"recovered"`
	Parked    int      `json:"parked"`
	Escalated int      `json:"escalated"`
	Backoff   int      `json:"backoff"`
	Cancelled int      `json:"cancelled"`
	Live      int      `json:"live"`
	Skipped   int      `json:"skipped"`
	Errors    []string `json:"errors,omitempty"`
}

// Supervisor is the recovery orchestrator.
type Supervisor struct {
	store    Store
	ledger   kernel.Committer
	policy   PolicySource
	log      *slog.Logger
	now      func() time.Time
	interval time.Duration
	limit    int

	parentOf        ParentResolver
	parentEscalator ParentEscalator
	notifier        Notifier
	superAgent      SuperAgent
	validate        func(json.RawMessage) error

	scanMu sync.Mutex
	// bootID scopes the read-only "a worker owns the queue" observations, so a
	// restart can record its own recovery observation without duplicating the
	// previous process's fact.
	bootID string
}

// New validates and builds a Supervisor.
func New(cfg Config) (*Supervisor, error) {
	if cfg.Store == nil {
		return nil, errors.New("supervisor: a store is required")
	}
	if cfg.Ledger == nil {
		return nil, errors.New("supervisor: a kernel committer is required")
	}
	if cfg.Policy == nil {
		defaultPolicy := DefaultPolicy()
		cfg.Policy = func() (Policy, ConfigVersion) { return defaultPolicy, ConfigVersion{} }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
	}
	if cfg.Limit <= 0 {
		cfg.Limit = 200
	}
	return &Supervisor{
		store:           cfg.Store,
		ledger:          cfg.Ledger,
		policy:          cfg.Policy,
		log:             cfg.Logger,
		now:             cfg.Now,
		interval:        cfg.Interval,
		limit:           cfg.Limit,
		parentOf:        cfg.ParentOf,
		parentEscalator: cfg.ParentEscalator,
		notifier:        cfg.Notifier,
		superAgent:      cfg.SuperAgent,
		validate:        cfg.ValidateSelection,
		bootID:          uuid.NewString(),
	}, nil
}

// Run scans periodically until ctx is cancelled. The startup scan is the
// caller's explicit first call; Run only handles later ticks.
func (s *Supervisor) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.Scan(ctx); err != nil {
				s.log.Warn("supervisor scan", "err", err)
			}
		}
	}
}

// Scan reconciles every non-terminal run with the resume policy. It never
// claims: a worker still owns execution; the supervisor decides what the next
// attempt is and records why.
func (s *Supervisor) Scan(ctx context.Context) (Report, error) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()

	var report Report
	policy, version := s.policy()
	if err := policy.Validate(); err != nil {
		return report, err
	}
	runs, err := s.store.ListSupervisorRuns(ctx, s.limit)
	if err != nil {
		return report, fmt.Errorf("supervisor: list runs: %w", err)
	}
	now := s.now()
	for _, run := range runs {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		report.Scanned++
		switch run.Status {
		case "done":
			continue
		case "pending":
			report.Queued++
			if policy.Resume.Mode == ModeAuto {
				// The run is already in the durable queue; a worker claim is the
				// resume, and the reclaimed-lease path records the recovery fact.
				continue
			}
			s.park(ctx, &report, policy, version, run, now)
		case "running":
			if run.LeaseUntil != nil && run.LeaseUntil.After(now) {
				report.Live++
				continue
			}
			if policy.Resume.Mode == ModeAuto {
				// A running row with an expired lease is claimable. Before letting
				// the worker reclaim it, enforce the automatic-recovery budget: a
				// crash loop must escalate, not recur forever.
				attempts, err := s.store.AutoResumeCount(ctx, run.TenantID, run.SessionID)
				if err != nil {
					s.fail(&report, run, "count attempts", err)
					continue
				}
				if attempts >= policy.Resume.MaxAttempts {
					reason := fmt.Sprintf("resume budget exhausted after %d automatic attempts", attempts)
					if changed, err := s.store.ParkRun(ctx, run.TenantID, run.SessionID, reason); err != nil {
						s.fail(&report, run, "park exhausted run", err)
					} else if changed {
						report.Parked++
						s.exhaust(ctx, &report, policy, version, run, attempts, now)
					}
					continue
				}
				// A worker claim is the resume; the reclaimed-lease path records
				// the recovery fact under its new epoch and consumes one attempt.
				continue
			}
			s.park(ctx, &report, policy, version, run, now)
		case "failed":
			if isCancelled(run) {
				report.Cancelled++
				continue
			}
			s.recoverFailed(ctx, &report, policy, version, run, now)
		default:
			report.Skipped++
		}
	}
	return report, nil
}

// park moves an auto-disabled run to a resumable failed state and records the
// decision. Parking is the honest "pending recovery" state: the row stays in
// the queue's table, no worker claims it, and the explicit resume endpoint
// (RequeueRun) can continue it.
func (s *Supervisor) park(ctx context.Context, report *Report, policy Policy, version ConfigVersion, run RunRecord, now time.Time) {
	reason := fmt.Sprintf("resume.mode=%s: automatic recovery is disabled", policy.Resume.Mode)
	changed, err := s.store.ParkRun(ctx, run.TenantID, run.SessionID, reason)
	if err != nil {
		s.fail(report, run, "park", err)
		return
	}
	if !changed {
		return
	}
	report.Parked++
	s.recordResume(ctx, report, run, version, resumeFact{
		Reason:       "policy_" + string(policy.Resume.Mode),
		Result:       "parked",
		Counted:      false,
		StatusBefore: run.Status,
		At:           now,
	})
}

// recoverFailed applies the retry budget, backoff and provider fallback to one
// failed run.
func (s *Supervisor) recoverFailed(ctx context.Context, report *Report, policy Policy, version ConfigVersion, run RunRecord, now time.Time) {
	if policy.Resume.Mode != ModeAuto {
		// manual/never also owns failed runs: they stay in the resumable failed
		// state until an operator or the parent agent calls resume.
		report.Skipped++
		return
	}
	attempts, err := s.store.AutoResumeCount(ctx, run.TenantID, run.SessionID)
	if err != nil {
		s.fail(report, run, "count attempts", err)
		return
	}
	if attempts >= policy.Resume.MaxAttempts {
		s.exhaust(ctx, report, policy, version, run, attempts, now)
		return
	}
	if last, ok, err := s.store.LastAutoResume(ctx, run.TenantID, run.SessionID); err != nil {
		s.fail(report, run, "read last attempt", err)
		return
	} else if ok {
		wait := policy.Resume.Backoff.Delay(attempts)
		if next := last.Add(wait); now.Before(next) {
			report.Backoff++
			return
		}
	}

	selection, fallback, fallbackErr := s.fallbackSelection(ctx, policy, attempts, run)
	outcome, err := s.store.RequeueRun(ctx, run.TenantID, run.SessionID)
	if err != nil {
		s.fail(report, run, "requeue", err)
		return
	}
	switch outcome.Status {
	case "requeued", "pending":
	default:
		// A live attempt won the race, the run settled, or the run disappeared.
		// The ladder is not advanced: the row's own truth wins.
		report.Skipped++
		return
	}
	report.Recovered++
	reason := "run_failed"
	if durable.IsProviderExhausted(run.LastError) {
		reason = "provider_exhausted"
	}
	s.recordResume(ctx, report, run, version, resumeFact{
		Reason:           reason,
		Result:           "requeued",
		Counted:          true,
		StatusBefore:     run.Status,
		Attempt:          attempts + 1,
		Selection:        selection,
		ProviderFallback: fallback,
		FallbackError:    fallbackErr,
		At:               now,
	})
}

// fallbackSelection builds the providerFallback selection for the next
// attempt. The selection travels in the resume.attempt fact, which is the same
// per-input fact the runner resolves (newest of user.message/resume.attempt),
// so no second endpoint or config write is invented.
func (s *Supervisor) fallbackSelection(ctx context.Context, policy Policy, attempts int, run RunRecord) (json.RawMessage, string, string) {
	if len(policy.Resume.ProviderFallback) == 0 || !durable.IsProviderExhausted(run.LastError) {
		return nil, "", ""
	}
	index := attempts
	if index >= len(policy.Resume.ProviderFallback) {
		index = len(policy.Resume.ProviderFallback) - 1
	}
	choice := policy.Resume.ProviderFallback[index]
	current, err := s.store.LatestSelection(ctx, run.TenantID, run.SessionID)
	if err != nil {
		return nil, choice.String(), err.Error()
	}
	var selection map[string]any
	if len(current) > 0 {
		if err := json.Unmarshal(current, &selection); err != nil {
			selection = nil
		}
	}
	if selection == nil {
		selection = map[string]any{}
	}
	selection["providerId"] = choice.ProviderID
	if choice.ModelID != "" {
		selection["modelId"] = choice.ModelID
	}
	if model, _ := selection["modelId"].(string); strings.TrimSpace(model) == "" {
		return nil, choice.String(), "no modelId to carry over; name provider/model in providerFallback"
	}
	encoded, err := json.Marshal(selection)
	if err != nil {
		return nil, choice.String(), err.Error()
	}
	if s.validate == nil {
		return nil, choice.String(), "selection validation is not wired"
	}
	if err := s.validate(encoded); err != nil {
		return nil, choice.String(), err.Error()
	}
	return encoded, choice.String(), ""
}

// exhaust applies onExhausted: escalate up the ladder once per failure epoch,
// or stop with an auditable fact.
func (s *Supervisor) exhaust(ctx context.Context, report *Report, policy Policy, version ConfigVersion, run RunRecord, attempts int, now time.Time) {
	escalations, err := s.store.EscalationsSince(ctx, run.TenantID, run.SessionID, run.UpdatedAt)
	if err != nil {
		s.fail(report, run, "read escalations", err)
		return
	}
	if escalations > 0 {
		return // this failure epoch was already escalated/stopped
	}
	if policy.Resume.OnExhausted == ExhaustedStop {
		s.recordEscalation(ctx, report, run, version, escalationFact{
			Level:        "stop",
			Reason:       "attempts_exhausted",
			Attempts:     attempts,
			MaxAttempts:  policy.Resume.MaxAttempts,
			Result:       "stopped",
			StatusBefore: run.Status,
			At:           now,
		})
		return
	}

	fact := escalationFact{
		Level:        "owner",
		Reason:       "attempts_exhausted",
		Attempts:     attempts,
		MaxAttempts:  policy.Resume.MaxAttempts,
		StatusBefore: run.Status,
		At:           now,
	}
	parentDone := false
	if s.parentOf != nil {
		parentID, ok, err := s.parentOf(ctx, run.TenantID, run.SessionID)
		if err != nil {
			fact.ParentError = err.Error()
		} else if ok && parentID != uuid.Nil {
			fact.ParentSessionID = parentID.String()
			fact.Level = "parent"
			if s.parentEscalator == nil {
				fact.ParentError = "parent escalation is not wired"
			} else if result, err := s.parentEscalator.Escalate(ctx,
				kernel.Scope{TenantID: run.TenantID, SessionID: parentID},
				kernel.Scope{TenantID: run.TenantID, SessionID: run.SessionID},
				fmt.Sprintf("sub-agent attempts exhausted after %d automatic resumes; resume it in place after fixing its configuration (%s)",
					attempts, trimError(run.LastError))); err != nil {
				fact.ParentError = err.Error()
			} else {
				fact.Result = "parent_notified"
				fact.ParentResult = result
				parentDone = true
			}
		}
	}
	if !parentDone {
		fact.Level = "owner"
		if s.notifier == nil {
			fact.Result = "notifier_not_wired"
		} else {
			notification, err := s.notifier.Notify(ctx, Notification{
				TenantID:  run.TenantID,
				SessionID: run.SessionID,
				RequestID: fmt.Sprintf("supervision:%s:%d", run.SessionID, run.UpdatedAt.UnixNano()),
				Title:     "Agent run needs attention",
				Text: fmt.Sprintf("Session %s exhausted its automatic recovery budget (%d). Last error: %s. Resume it after fixing the configuration, for example the provider that ran out of quota.",
					run.SessionID, attempts, trimError(run.LastError)),
			})
			if err != nil {
				fact.Result = "notify_failed"
				fact.NotificationError = err.Error()
			} else {
				fact.Result = "owner_notified"
				fact.NotificationID = notification.ID
				fact.NotificationStatus = notification.Status
			}
		}
	}
	// L3 is an explicit capability seam: with no installed super-agent the
	// ladder stops at the owner and the feature set loses only that rung. It is
	// tried only when the parent rung did not already take the hand-off.
	if s.superAgent != nil && !parentDone {
		result, err := s.superAgent.Recover(ctx, SuperAgentRequest{
			TenantID: run.TenantID, SessionID: run.SessionID,
			Reason: "attempts_exhausted", Attempts: attempts, Config: version,
		})
		fact.SuperAgentResult = result
		if err != nil {
			fact.SuperAgentError = err.Error()
		} else {
			fact.Level = "super-agent"
		}
	}
	s.recordEscalation(ctx, report, run, version, fact)
}

// recordResume commits one `resume.attempt` fact.
func (s *Supervisor) recordResume(ctx context.Context, report *Report, run RunRecord, version ConfigVersion, fact resumeFact) {
	if fact.At.IsZero() {
		fact.At = s.now()
	}
	count, err := s.store.ResumeAttemptCount(ctx, run.TenantID, run.SessionID)
	if err != nil {
		s.fail(report, run, "count resume facts", err)
		return
	}
	payload := map[string]any{
		"actor":            "supervisor",
		"reason":           fact.Reason,
		"result":           fact.Result,
		"counted":          fact.Counted,
		"attempt":          fact.Attempt,
		"statusBefore":     fact.StatusBefore,
		"lastError":        trimError(run.LastError),
		"configHash":       version.Hash,
		"configRevision":   version.Revision,
		"configGeneration": version.Generation,
		"at":               fact.At.UTC().Format(time.RFC3339Nano),
	}
	if len(fact.Selection) > 0 {
		payload["selection"] = fact.Selection
	}
	if fact.ProviderFallback != "" {
		payload["providerFallback"] = fact.ProviderFallback
	}
	if fact.FallbackError != "" {
		payload["fallbackError"] = fact.FallbackError
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		s.fail(report, run, "encode resume fact", err)
		return
	}
	if _, err := s.ledger.Commit(ctx, kernel.CommitRequest{
		Scope:     kernel.Scope{TenantID: run.TenantID, SessionID: run.SessionID},
		Kind:      kernel.KindResumeAttempt,
		System:    true,
		ResumeKey: fmt.Sprintf("supervisor:%s:resume:%d", run.SessionID, count+1),
		Payload:   encoded,
	}); err != nil {
		s.fail(report, run, "commit resume fact", err)
		return
	}
	s.log.Info("supervisor resume attempt recorded",
		"tenant_id", run.TenantID, "session_id", run.SessionID,
		"reason", fact.Reason, "result", fact.Result, "attempt", fact.Attempt)
}

// recordEscalation commits one `supervision.escalated` fact.
func (s *Supervisor) recordEscalation(ctx context.Context, report *Report, run RunRecord, version ConfigVersion, fact escalationFact) {
	if fact.At.IsZero() {
		fact.At = s.now()
	}
	count, err := s.store.EscalationCount(ctx, run.TenantID, run.SessionID)
	if err != nil {
		s.fail(report, run, "count escalation facts", err)
		return
	}
	payload := map[string]any{
		"actor":            "supervisor",
		"level":            fact.Level,
		"reason":           fact.Reason,
		"result":           fact.Result,
		"attempts":         fact.Attempts,
		"maxAttempts":      fact.MaxAttempts,
		"statusBefore":     fact.StatusBefore,
		"lastError":        trimError(run.LastError),
		"configHash":       version.Hash,
		"configRevision":   version.Revision,
		"configGeneration": version.Generation,
		"at":               fact.At.UTC().Format(time.RFC3339Nano),
	}
	if fact.ParentSessionID != "" {
		payload["parentSessionId"] = fact.ParentSessionID
	}
	if fact.ParentResult != "" {
		payload["parentResult"] = fact.ParentResult
	}
	if fact.ParentError != "" {
		payload["parentError"] = fact.ParentError
	}
	if fact.NotificationID != "" {
		payload["notificationId"] = fact.NotificationID
	}
	if fact.NotificationStatus != "" {
		payload["notificationStatus"] = fact.NotificationStatus
	}
	if fact.NotificationError != "" {
		payload["notificationError"] = fact.NotificationError
	}
	if fact.SuperAgentResult != "" {
		payload["superAgentResult"] = fact.SuperAgentResult
	}
	if fact.SuperAgentError != "" {
		payload["superAgentError"] = fact.SuperAgentError
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		s.fail(report, run, "encode escalation fact", err)
		return
	}
	if _, err := s.ledger.Commit(ctx, kernel.CommitRequest{
		Scope:          kernel.Scope{TenantID: run.TenantID, SessionID: run.SessionID},
		Kind:           kernel.KindSupervisionEscalated,
		System:         true,
		SupervisionKey: fmt.Sprintf("supervisor:%s:escalate:%d", run.SessionID, count+1),
		Payload:        encoded,
	}); err != nil {
		s.fail(report, run, "commit escalation fact", err)
		return
	}
	report.Escalated++
	s.log.Warn("supervisor escalated a run",
		"tenant_id", run.TenantID, "session_id", run.SessionID,
		"level", fact.Level, "attempts", fact.Attempts, "result", fact.Result)
}

func (s *Supervisor) fail(report *Report, run RunRecord, stage string, err error) {
	message := fmt.Sprintf("%s %s/%s: %v", stage, run.TenantID, run.SessionID, err)
	report.Errors = append(report.Errors, message)
	s.log.Warn("supervisor step failed", "stage", stage,
		"tenant_id", run.TenantID, "session_id", run.SessionID, "err", err)
}

type resumeFact struct {
	Reason           string
	Result           string
	Counted          bool
	StatusBefore     string
	Attempt          int
	Selection        json.RawMessage
	ProviderFallback string
	FallbackError    string
	At               time.Time
}

type escalationFact struct {
	Level              string
	Reason             string
	Result             string
	Attempts           int
	MaxAttempts        int
	StatusBefore       string
	ParentSessionID    string
	ParentResult       string
	ParentError        string
	NotificationID     string
	NotificationStatus string
	NotificationError  string
	SuperAgentResult   string
	SuperAgentError    string
	At                 time.Time
}

func isCancelled(run RunRecord) bool {
	message := strings.ToLower(run.LastError)
	return strings.Contains(message, "context canceled") || strings.Contains(message, "context cancelled")
}

func trimError(message string) string {
	trimmed := strings.TrimSpace(message)
	if len(trimmed) <= 500 {
		return trimmed
	}
	return trimmed[:500] + "…"
}
