package main

import (
	"context"
	"log/slog"
	"os"
	"sync"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/notify"
	"github.com/jixoai/keel/internal/servicehost"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/supervisor"
	"github.com/jixoai/keel/internal/tasklog"
)

// newRecoverySupervisor wires the W12 recovery slice into `keel serve`:
// automatic startup scan, periodic reconciliation, the parent/owner escalation
// ladder and the providerFallback policy. It is deliberately a product-layer
// object (it needs notifications and the task log), while the run queue and
// lease stay with the worker.
func newRecoverySupervisor(ctx context.Context, st *store.Store, kern *kernel.Kernel, rt *servicehost.Runtime, product *productLayer, policyReader *supervisorPolicyReader, log *slog.Logger) (*supervisor.Supervisor, error) {
	if st == nil || kern == nil || product == nil || policyReader == nil {
		return nil, nil
	}
	policyReader.setRuntime(rt)
	cfg := supervisor.Config{
		Store:  supervisor.NewSQLStore(st),
		Ledger: kern,
		Policy: policyReader.policy,
		Logger: log,
		// The parent link is durable (the child's first input), so escalation
		// survives a restart even when the in-process sub-agent registry is cold.
		ParentOf: func(ctx context.Context, tenantID, childID uuid.UUID) (uuid.UUID, bool, error) {
			return harnessext.DurableParentSession(ctx, kern, tenantID, childID)
		},
		// The parent hand-off is a task.note on the card the child card points
		// at; the same writer the normal sub-agent return uses.
		ParentEscalator: supervisorParentEscalator{notes: product.taskLog},
		// The owner rung is the durable notification service; `keel.notify`
		// consumers see the same row the tool would create.
		ValidateSelection: selectFromCurrentGeneration(rt),
	}
	if product.notifications != nil {
		cfg.Notifier = supervisorNotifier{service: product.notifications}
	}
	sup, err := supervisor.New(cfg)
	if err != nil {
		return nil, err
	}
	report, err := sup.Scan(ctx)
	if err != nil {
		log.Warn("supervisor startup scan", "err", err)
	} else {
		log.Info("supervisor startup scan",
			"scanned", report.Scanned, "queued", report.Queued, "recovered", report.Recovered,
			"parked", report.Parked, "escalated", report.Escalated,
			"backoff", report.Backoff, "live", report.Live, "errors", len(report.Errors))
	}
	go sup.Run(ctx)
	return sup, nil
}

// supervisorNotifier adapts notify.Service to the owner escalation rung.
type supervisorNotifier struct{ service *notify.Service }

func (n supervisorNotifier) Notify(ctx context.Context, req supervisor.Notification) (supervisor.NotificationResult, error) {
	view, err := n.service.Push(ctx, notify.PushRequest{
		TenantID:  req.TenantID,
		SessionID: req.SessionID,
		RequestID: req.RequestID,
		Title:     req.Title,
		Text:      req.Text,
		Consumers: []notify.ConsumerSpec{{
			ID:    "owner",
			Kind:  notify.ConsumerClient,
			Label: "owner",
		}},
	})
	if err != nil {
		return supervisor.NotificationResult{}, err
	}
	return supervisor.NotificationResult{ID: view.NotificationID, Status: view.Status}, nil
}

// supervisorParentEscalator writes the L1 hand-off as a task.note on the card
// the child card points at. It reuses the sub-agent return writer, so the
// parent's task board shows one converged thread (not a second card).
type supervisorParentEscalator struct{ notes *tasklog.Service }

func (p supervisorParentEscalator) Escalate(ctx context.Context, parent, child kernel.Scope, reason string) (string, error) {
	if p.notes == nil {
		return "", errSupervisorNotWired
	}
	if err := p.notes.NoteSubagentFinished(ctx, parent, child.SessionID, "escalated", reason); err != nil {
		return "", err
	}
	return "parent_notified", nil
}

var errSupervisorNotWired = &supervisorWiringError{"the task log is not wired"}

type supervisorWiringError struct{ message string }

func (e *supervisorWiringError) Error() string { return e.message }

// supervisorPolicyReader reads the policy document per call. It is created
// before servicehost.Boot so the runner's guardrail policy comes from the same
// document as the supervisor's resume policy; the runtime is attached after
// Boot, when the loadout identity exists.
type supervisorPolicyReader struct {
	log *slog.Logger

	mu      sync.Mutex
	rt      *servicehost.Runtime
	lastErr string
}

func newSupervisorPolicyReader(log *slog.Logger) *supervisorPolicyReader {
	return &supervisorPolicyReader{log: log}
}

func (s *supervisorPolicyReader) setRuntime(rt *servicehost.Runtime) {
	s.mu.Lock()
	s.rt = rt
	s.mu.Unlock()
}

// guardrails is the runner's per-attempt source.
func (s *supervisorPolicyReader) guardrails() supervisor.GuardrailPolicy {
	policy, _ := s.policy()
	return policy.Guardrails
}

func (s *supervisorPolicyReader) policy() (supervisor.Policy, supervisor.ConfigVersion) {
	policy, err := supervisor.LoadPolicy(os.Getenv)
	if err != nil {
		s.mu.Lock()
		changed := err.Error() != s.lastErr
		s.lastErr = err.Error()
		s.mu.Unlock()
		if changed {
			// Fail closed: an unreadable policy must not silently auto-resume
			// with defaults. An explicit resume still works.
			s.log.Error("supervisor policy is invalid; automatic recovery is manual until it is fixed", "err", err)
		}
		policy = supervisor.DefaultPolicy()
		policy.Resume.Mode = supervisor.ModeManual
	}
	version := supervisor.ConfigVersion{Hash: policy.Hash()}
	s.mu.Lock()
	rt := s.rt
	s.mu.Unlock()
	if rt != nil {
		if snapshot := rt.Snapshot(); snapshot != nil {
			version.Hash = agent.ConfigHashForLoadout(snapshot.LoadoutID, snapshot.Revision)
			version.Revision = snapshot.Revision
			version.Generation = snapshot.Generation
		}
	}
	return policy, version
}
