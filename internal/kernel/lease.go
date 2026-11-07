package kernel

import (
	"context"
	"fmt"
	"time"
)

// Claim claims the next runnable run for this runtime instance and issues a
// fencing token. "Runnable" keeps the existing semantics: a pending run, or a
// running run whose lease has expired (a crashed process must not block the
// session forever). ErrNoRunnable when nothing is claimable right now.
//
// The tenant admission seam is consulted for the claimed scope; a run that is
// not admitted is released immediately and the claim answers ErrQuota.
func (k *Kernel) Claim(ctx context.Context, owner string, ttl time.Duration) (Lease, error) {
	if err := checkClaimArgs(owner, ttl); err != nil {
		return Lease{}, k.reject(ReasonInvalidRequest, OpClaim, Scope{}, "", OwnerToken{}, err.Error(), ErrInvalidRequest)
	}
	lease, err := k.log.ClaimLease(ctx, owner, ttl)
	if err != nil {
		return Lease{}, k.classifyStoreError(OpClaim, CommitRequest{Token: OwnerToken{Owner: owner}}, err)
	}
	if k.limits != nil {
		if err := k.limits.AdmitRun(ctx, lease.Scope); err != nil {
			// The claim is undone: a rejected run must not consume the lease, and
			// the next attempt (another owner) must see the run as claimable.
			if relErr := k.log.ReleaseLease(ctx, lease.Scope, lease.Token); relErr != nil {
				k.auditViolation(Violation{Reason: ReasonStoreFailure, Op: OpClaim, Scope: lease.Scope, Token: lease.Token, Detail: "release after quota rejection: " + relErr.Error()})
			}
			return Lease{}, k.reject(ReasonQuota, OpClaim, lease.Scope, "", lease.Token, err.Error(), ErrQuota)
		}
	}
	return lease, nil
}

// ClaimScope claims one specific session, the primitive the handoff protocol
// uses ("new runtime claims the session and gets the new epoch"). It fails
// with ErrNotFound for an unknown session, ErrLeaseHeld while another owner
// holds a live lease, and ErrNoRunnable when the run is settled.
//
// The admission seam is consulted only after the claim succeeded, exactly like
// Claim: a failed claim never touches the tenant budget (a handoff Adopt polls
// this call, and admitting before the claim would burn a slot per poll), and an
// admission rejection releases the just-taken claim so the next owner sees it.
func (k *Kernel) ClaimScope(ctx context.Context, scope Scope, owner string, ttl time.Duration) (Lease, error) {
	if err := checkClaimArgs(owner, ttl); err != nil {
		return Lease{}, k.reject(ReasonInvalidRequest, OpClaim, scope, "", OwnerToken{}, err.Error(), ErrInvalidRequest)
	}
	if scope.IsZero() {
		return Lease{}, k.reject(ReasonInvalidRequest, OpClaim, scope, "", OwnerToken{}, "tenant and session are required", ErrInvalidRequest)
	}
	lease, err := k.log.ClaimScope(ctx, scope, owner, ttl)
	if err != nil {
		return Lease{}, k.classifyStoreError(OpClaim, CommitRequest{Scope: scope, Token: OwnerToken{Owner: owner}}, err)
	}
	if k.limits != nil {
		if err := k.limits.AdmitRun(ctx, scope); err != nil {
			if relErr := k.log.ReleaseLease(ctx, lease.Scope, lease.Token); relErr != nil {
				k.auditViolation(Violation{Reason: ReasonStoreFailure, Op: OpClaim, Scope: lease.Scope, Token: lease.Token, Detail: "release after quota rejection: " + relErr.Error()})
			}
			return Lease{}, k.reject(ReasonQuota, OpClaim, scope, "", lease.Token, err.Error(), ErrQuota)
		}
	}
	return lease, nil
}

// Renew extends a live lease. Renewal is only possible while the token is the
// current one and the lease has not expired: once another owner claimed (or the
// expiry passed), the old runtime must stop, not resurrect its claim.
func (k *Kernel) Renew(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	if err := checkClaimArgs(lease.Token.Owner, ttl); err != nil {
		return Lease{}, k.reject(ReasonInvalidRequest, OpRenew, lease.Scope, "", lease.Token, err.Error(), ErrInvalidRequest)
	}
	if lease.Token.IsZero() {
		return Lease{}, k.reject(ReasonMissingToken, OpRenew, lease.Scope, "", lease.Token, "a lease token is required", ErrMissingToken)
	}
	renewed, err := k.log.RenewLease(ctx, lease, ttl)
	if err != nil {
		return Lease{}, k.classifyStoreError(OpRenew, CommitRequest{Scope: lease.Scope, Token: lease.Token}, err)
	}
	return renewed, nil
}

// Finish settles a claimed run: it validates the token, applies the store's
// outcome policy and clears the lease. A stale owner's finish is rejected, so a
// drained runtime can never clear the new owner's lease or overwrite its run
// state.
func (k *Kernel) Finish(ctx context.Context, req FinishRequest) error {
	if req.Lease.Token.IsZero() {
		return k.reject(ReasonMissingToken, OpFinish, req.Lease.Scope, "", req.Lease.Token, "a lease token is required", ErrMissingToken)
	}
	if req.Lease.Scope.IsZero() {
		return k.reject(ReasonInvalidRequest, OpFinish, req.Lease.Scope, "", req.Lease.Token, "tenant and session are required", ErrInvalidRequest)
	}
	// The attempt ends here whatever the store answers: a stale owner that
	// cannot settle still stops consuming its admission slot.
	defer k.releaseAdmission(ctx, req.Lease.Scope)
	if req.Loadout.OwnerEpoch == 0 {
		req.Loadout.OwnerEpoch = req.Lease.Token.Epoch
	}
	if err := k.log.FinishLease(ctx, req); err != nil {
		return k.classifyStoreError(OpFinish, CommitRequest{Scope: req.Lease.Scope, Token: req.Lease.Token}, err)
	}
	return nil
}

// Release gives a claim back without settling the run: it clears the lease and
// returns the run to pending so any owner may claim it again. It is the
// "shutdown before starting" primitive (drain, or a claim that failed
// admission) and it too is fenced: a stale owner cannot release a live claim.
func (k *Kernel) Release(ctx context.Context, lease Lease) error {
	if lease.Token.IsZero() {
		return k.reject(ReasonMissingToken, OpRelease, lease.Scope, "", lease.Token, "a lease token is required", ErrMissingToken)
	}
	if lease.Scope.IsZero() {
		return k.reject(ReasonInvalidRequest, OpRelease, lease.Scope, "", lease.Token, "tenant and session are required", ErrInvalidRequest)
	}
	defer k.releaseAdmission(ctx, lease.Scope)
	if err := k.log.ReleaseLease(ctx, lease.Scope, lease.Token); err != nil {
		return k.classifyStoreError(OpRelease, CommitRequest{Scope: lease.Scope, Token: lease.Token}, err)
	}
	return nil
}

// releaseAdmission returns the run admission slot of a settled attempt.
func (k *Kernel) releaseAdmission(ctx context.Context, scope Scope) {
	if k.limits == nil || scope.IsZero() {
		return
	}
	k.limits.ReleaseRun(ctx, scope)
}

// LeaseState answers the session's fencing state (for audits, handoff
// queries and tests). ErrNotFound for a foreign or missing session.
func (k *Kernel) LeaseState(ctx context.Context, scope Scope) (LeaseState, error) {
	if scope.IsZero() {
		return LeaseState{}, k.reject(ReasonInvalidRequest, OpRead, scope, "", OwnerToken{}, "tenant and session are required", ErrInvalidRequest)
	}
	exists, err := k.log.SessionExists(ctx, scope.TenantID, scope.SessionID)
	if err != nil {
		return LeaseState{}, k.classifyStoreError(OpRead, CommitRequest{Scope: scope}, err)
	}
	if !exists {
		return LeaseState{}, k.reject(ReasonNotFound, OpRead, scope, "", OwnerToken{}, "", ErrNotFound)
	}
	state, err := k.log.FindLease(ctx, scope)
	if err != nil {
		return LeaseState{}, k.classifyStoreError(OpRead, CommitRequest{Scope: scope}, err)
	}
	return state, nil
}

func checkClaimArgs(owner string, ttl time.Duration) error {
	if owner == "" {
		return fmt.Errorf("owner identity is required")
	}
	if ttl <= 0 {
		return fmt.Errorf("lease ttl must be positive, got %s", ttl)
	}
	return nil
}
