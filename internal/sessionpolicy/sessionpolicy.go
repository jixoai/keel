// Package sessionpolicy owns the per-session checkpoint policy override: the
// durable document a session writes and the effective policy computed by
// narrowing it against the tenant policy.
//
// The session document is never used directly. Every read funnels through
// compaction.NarrowCheckpointPolicy, so a session can only disable a capability
// or tighten a bound; it can never widen what the tenant allows. The computed
// policy is what the runner records as checkpoint.policy_version and what the
// read API projects.
package sessionpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/store"
)

// ErrSessionNotFound reports a missing or foreign-tenant session.
var ErrSessionNotFound = errors.New("sessionpolicy: session not found")

// ErrInvalid reports an override document the policy rules reject.
var ErrInvalid = errors.New("sessionpolicy: invalid checkpoint policy document")

// Store is the durable half of the service. *store.Store implements it.
type Store interface {
	GetSessionCheckpointPolicy(ctx context.Context, tenantID, sessionID uuid.UUID) (store.SessionCheckpointPolicy, bool, error)
	SetSessionCheckpointPolicy(ctx context.Context, tenantID, sessionID uuid.UUID, document json.RawMessage) (int64, error)
	DeleteSessionCheckpointPolicy(ctx context.Context, tenantID, sessionID uuid.UUID) error
}

// TenantPolicy supplies the current tenant checkpoint policy. The hosting
// process wires it to the live generation so a reload changes later decisions
// without a restart; a nil provider serves the default policy.
type TenantPolicy func() compaction.CheckpointPolicy

// Service computes and persists session checkpoint policies.
type Service struct {
	store  Store
	tenant TenantPolicy
}

// New builds the service. store is required; tenant may be nil.
func New(store Store, tenant TenantPolicy) *Service {
	return &Service{store: store, tenant: tenant}
}

// Policy is the wire projection of one effective policy. TTL travels in hours
// (the tenant document's unit) and the version is the same identity checkpoint
// rows record.
type Policy struct {
	Enabled         bool   `json:"enabled"`
	ThresholdTokens int    `json:"thresholdTokens"`
	RetainTokens    int    `json:"retainTokens"`
	MaxCheckpoints  int    `json:"maxCheckpoints"`
	TTLHours        int64  `json:"ttlHours"`
	ManualCompact   bool   `json:"manualCompact"`
	PolicyVersion   string `json:"policyVersion"`
}

// View is the read projection of a session's policy: the tenant policy, the
// stored override (when present), the effective policy and every field the
// tenant narrowed. It contains no conversation content.
type View struct {
	Tenant    Policy                                       `json:"tenant"`
	Effective Policy                                       `json:"effective"`
	Override  *compaction.CheckpointPolicyOverrideDocument `json:"override,omitempty"`
	Narrowed  []compaction.PolicyNarrowing                 `json:"narrowed"`
	Revision  int64                                        `json:"revision"`
	Source    string                                       `json:"source"` // tenant | session
}

// Get returns the effective policy of one session. A session without an
// override reports the tenant policy at revision 0.
func (s *Service) Get(ctx context.Context, tenantID, sessionID uuid.UUID) (View, error) {
	base := s.basePolicy()
	record, found, err := s.store.GetSessionCheckpointPolicy(ctx, tenantID, sessionID)
	if err != nil {
		return View{}, mapStoreError(err)
	}
	return s.view(base, record, found), nil
}

// Put validates and stores a session override, then returns the effective
// policy. The stored document is the caller's request; narrowing is recomputed
// on every read, so a later tenant change is honored. An empty document clears
// the override.
func (s *Service) Put(ctx context.Context, tenantID, sessionID uuid.UUID, document compaction.CheckpointPolicyOverrideDocument) (View, error) {
	if err := document.Validate(); err != nil {
		return View{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	base := s.basePolicy()
	if document.IsEmpty() {
		// Clearing is a delete: the session returns to the tenant policy with
		// no stored row and no revision of its own.
		if err := s.store.DeleteSessionCheckpointPolicy(ctx, tenantID, sessionID); err != nil {
			return View{}, mapStoreError(err)
		}
		return s.view(base, store.SessionCheckpointPolicy{}, false), nil
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return View{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	revision, err := s.store.SetSessionCheckpointPolicy(ctx, tenantID, sessionID, raw)
	if err != nil {
		return View{}, mapStoreError(err)
	}
	record := store.SessionCheckpointPolicy{Policy: raw, Revision: revision}
	return s.view(base, record, true), nil
}

// CheckpointPolicy is the runner's resolver seam: the effective policy of one
// session. A read failure returns the tenant policy plus the error so a caller
// can log it and continue with the safe base; a missing session is not an error
// for the run path (the claim already proved existence).
func (s *Service) CheckpointPolicy(ctx context.Context, tenantID, sessionID uuid.UUID) (compaction.CheckpointPolicy, error) {
	base := s.basePolicy()
	record, found, err := s.store.GetSessionCheckpointPolicy(ctx, tenantID, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNoSession) {
			return base, nil
		}
		return base, mapStoreError(err)
	}
	if !found {
		return base, nil
	}
	override, err := decodeOverride(record.Policy)
	if err != nil {
		return base, err
	}
	effective, _ := compaction.NarrowCheckpointPolicy(base, override)
	return effective, nil
}

func (s *Service) basePolicy() compaction.CheckpointPolicy {
	if s.tenant == nil {
		return compaction.DefaultCheckpointPolicy()
	}
	policy := s.tenant()
	if err := policy.Validate(); err != nil {
		return compaction.DefaultCheckpointPolicy()
	}
	return policy
}

func (s *Service) view(base compaction.CheckpointPolicy, record store.SessionCheckpointPolicy, found bool) View {
	view := View{
		Tenant:   policyDTO(base),
		Revision: record.Revision,
		Source:   "tenant",
	}
	effective := base
	var narrowed []compaction.PolicyNarrowing
	if found {
		var document compaction.CheckpointPolicyOverrideDocument
		if err := json.Unmarshal(record.Policy, &document); err == nil && !document.IsEmpty() {
			view.Override = &document
			view.Source = "session"
			effective, narrowed = compaction.NarrowCheckpointPolicy(base, document.Override())
		}
	}
	view.Effective = policyDTO(effective)
	view.Narrowed = narrowed
	if view.Narrowed == nil {
		view.Narrowed = []compaction.PolicyNarrowing{}
	}
	return view
}

func policyDTO(policy compaction.CheckpointPolicy) Policy {
	return Policy{
		Enabled:         policy.Enabled,
		ThresholdTokens: policy.ThresholdTokens,
		RetainTokens:    policy.RetainTokens,
		MaxCheckpoints:  policy.MaxCheckpoints,
		TTLHours:        int64(policy.TTL / time.Hour),
		ManualCompact:   policy.ManualCompact,
		PolicyVersion:   policy.Version(),
	}
}

func decodeOverride(raw json.RawMessage) (compaction.CheckpointPolicyOverride, error) {
	var document compaction.CheckpointPolicyOverrideDocument
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &document); err != nil {
			return compaction.CheckpointPolicyOverride{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	if err := document.Validate(); err != nil {
		return compaction.CheckpointPolicyOverride{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return document.Override(), nil
}

func mapStoreError(err error) error {
	if errors.Is(err, store.ErrNoSession) {
		return ErrSessionNotFound
	}
	return err
}
