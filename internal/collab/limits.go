package collab

import "fmt"

// Limits is the collaboration budget vocabulary. The same shape is used at
// every level, which makes the one direction explicit: a tenant ceiling is the
// hard upper bound, and a mode, WorkItem or request may only narrow it.
//
// Zero semantics for the numeric dimensions are deliberate:
//   - on a ceiling, 0 means "no cap";
//   - on a request, 0 means "inherit the effective cap".
//
// Depth is different: it has a three-state contract (forbidden / unlimited /
// limited) that a single integer cannot express without overloading zero. It is
// therefore a DepthPolicy, and a ModeDescriptor must declare one explicitly.
type Limits struct {
	// Depth is the explicit delegation-depth policy. Unlike the numeric
	// dimensions, depth is never "zero means inherit and unlimited and
	// forbidden" at the same time.
	Depth DepthPolicy `json:"depth"`
	// MaxChildren bounds the concurrently active children of one session.
	MaxChildren int `json:"maxChildren,omitempty"`
	// MaxConcurrent bounds concurrently active sessions of one mode in the
	// tenant.
	MaxConcurrent int `json:"maxConcurrent,omitempty"`
	// MaxTurns bounds an individual session's assistant turns.
	MaxTurns int `json:"maxTurns,omitempty"`
	// MaxTokens bounds an individual session's input+output tokens.
	MaxTokens int `json:"maxTokens,omitempty"`
	// MaxDurationMs bounds one session's wall-clock duration.
	MaxDurationMs int64 `json:"maxDurationMs,omitempty"`
}

// DepthMode is the closed depth-policy state.
type DepthMode string

const (
	// DepthUnset is the zero policy. At the request/WorkItem level it inherits
	// the effective cap; a ModeDescriptor is rejected without an explicit
	// policy.
	DepthUnset DepthMode = ""
	// DepthLimited caps delegation at Max (main is depth 0, children may reach
	// depth Max).
	DepthLimited DepthMode = "limited"
	// DepthUnlimited removes the depth cap (budget, width, concurrency and
	// permission checks still apply).
	DepthUnlimited DepthMode = "unlimited"
	// DepthForbidden rejects every child creation.
	DepthForbidden DepthMode = "forbidden"
)

// DepthPolicy is the explicit three-state depth contract. Max is meaningful
// only for DepthLimited.
type DepthPolicy struct {
	Mode DepthMode `json:"mode"`
	Max  int       `json:"max,omitempty"`
}

// LimitedDepth builds a finite depth policy. Max must be at least 1 (main is
// depth 0; 0 as a finite cap would be indistinguishable from "forbidden").
func LimitedDepth(max int) DepthPolicy { return DepthPolicy{Mode: DepthLimited, Max: max} }

// UnlimitedDepth builds a policy with no depth cap.
func UnlimitedDepth() DepthPolicy { return DepthPolicy{Mode: DepthUnlimited} }

// ForbiddenDepth builds a policy that rejects every child.
func ForbiddenDepth() DepthPolicy { return DepthPolicy{Mode: DepthForbidden} }

// IsSet reports whether a policy was declared.
func (p DepthPolicy) IsSet() bool { return p.Mode != DepthUnset }

// Validate checks the state/field combination.
func (p DepthPolicy) Validate() error {
	switch p.Mode {
	case DepthUnset:
		if p.Max != 0 {
			return fmt.Errorf("%w: depth.max is only valid with mode=limited", ErrInvalidRequest)
		}
	case DepthLimited:
		if p.Max < 1 {
			return fmt.Errorf("%w: limited depth requires max >= 1 (use forbidden for 0)", ErrInvalidRequest)
		}
	case DepthUnlimited, DepthForbidden:
		if p.Max != 0 {
			return fmt.Errorf("%w: depth.max is only valid with mode=limited", ErrInvalidRequest)
		}
	default:
		return fmt.Errorf("%w: depth mode %q is not in the closed vocabulary", ErrInvalidRequest, p.Mode)
	}
	return nil
}

// Allows reports whether a session at childDepth may be created. A top-level
// session is depth 0, so a limited policy with Max=3 allows depths 1..3.
func (p DepthPolicy) Allows(childDepth int) bool {
	switch p.Mode {
	case DepthForbidden:
		return false
	case DepthLimited:
		return childDepth <= p.Max
	default:
		// Unlimited and unset (no effective policy) allow; a mode descriptor
		// cannot be unset, so unset only reaches here through a bare request.
		return true
	}
}

// NarrowedBy returns the effective policy under a ceiling. A ceiling narrows a
// wider request; it never widens one. Unset/unlimited ceilings do not change
// the request.
func (p DepthPolicy) NarrowedBy(ceiling DepthPolicy) DepthPolicy {
	if !ceiling.IsSet() || ceiling.Mode == DepthUnlimited {
		return p
	}
	if ceiling.Mode == DepthForbidden {
		return ForbiddenDepth()
	}
	// A limited ceiling.
	switch p.Mode {
	case DepthForbidden:
		return ForbiddenDepth()
	case DepthLimited:
		if p.Max < ceiling.Max {
			return p
		}
		return ceiling
	default:
		// Unset/unlimited inherits the finite ceiling.
		return ceiling
	}
}

// Exceeds reports whether the policy asks for more than a ceiling. An unset
// policy inherits and never exceeds; forbidden never exceeds.
func (p DepthPolicy) Exceeds(ceiling DepthPolicy) *Violation {
	if !ceiling.IsSet() || ceiling.Mode == DepthUnlimited {
		return nil
	}
	if ceiling.Mode == DepthForbidden {
		if !p.IsSet() || p.Mode == DepthForbidden {
			return nil
		}
		return &Violation{Field: "depth", Detail: "the ceiling forbids delegation"}
	}
	// A limited ceiling.
	switch p.Mode {
	case DepthUnset, DepthForbidden:
		return nil
	case DepthUnlimited:
		return &Violation{Field: "depth", Detail: fmt.Sprintf("unlimited depth exceeds the ceiling of %d", ceiling.Max)}
	case DepthLimited:
		if p.Max > ceiling.Max {
			return &Violation{Field: "depth", Requested: int64(p.Max), Limit: int64(ceiling.Max),
				Detail: fmt.Sprintf("depth %d exceeds the ceiling of %d", p.Max, ceiling.Max)}
		}
	}
	return nil
}

// Budget is one session's request-level budget. It is a narrower view of
// Limits: only the per-session dimensions are request-settable.
type Budget struct {
	MaxTurns  int   `json:"maxTurns,omitempty"`
	MaxTokens int   `json:"maxTokens,omitempty"`
	TimeoutMs int64 `json:"timeoutMs,omitempty"`
}

// Limits projects the budget onto the shared limit shape.
func (b Budget) Limits() Limits {
	return Limits{MaxTurns: b.MaxTurns, MaxTokens: b.MaxTokens, MaxDurationMs: b.TimeoutMs}
}

// Validate rejects negative dimensions. Zero is valid everywhere.
func (b Budget) Validate() error {
	switch {
	case b.MaxTurns < 0:
		return fmt.Errorf("%w: maxTurns must be non-negative", ErrInvalidRequest)
	case b.MaxTokens < 0:
		return fmt.Errorf("%w: maxTokens must be non-negative", ErrInvalidRequest)
	case b.TimeoutMs < 0:
		return fmt.Errorf("%w: timeoutMs must be non-negative", ErrInvalidRequest)
	}
	return nil
}

// Validate rejects negative dimensions and an invalid depth policy.
func (l Limits) Validate() error {
	if err := l.Depth.Validate(); err != nil {
		return err
	}
	fields := []struct {
		name  string
		value int64
	}{
		{"maxChildren", int64(l.MaxChildren)},
		{"maxConcurrent", int64(l.MaxConcurrent)},
		{"maxTurns", int64(l.MaxTurns)},
		{"maxTokens", int64(l.MaxTokens)},
		{"maxDurationMs", l.MaxDurationMs},
	}
	for _, field := range fields {
		if field.value < 0 {
			return fmt.Errorf("%w: %s must be non-negative", ErrInvalidRequest, field.name)
		}
	}
	return nil
}

// NarrowedBy returns the effective limits under a ceiling. The result can never
// exceed either input on any dimension.
func (l Limits) NarrowedBy(ceiling Limits) Limits {
	return Limits{
		Depth:         l.Depth.NarrowedBy(ceiling.Depth),
		MaxChildren:   minNonZero(l.MaxChildren, ceiling.MaxChildren),
		MaxConcurrent: minNonZero(l.MaxConcurrent, ceiling.MaxConcurrent),
		MaxTurns:      minNonZero(l.MaxTurns, ceiling.MaxTurns),
		MaxTokens:     minNonZero(l.MaxTokens, ceiling.MaxTokens),
		MaxDurationMs: minNonZeroInt64(l.MaxDurationMs, ceiling.MaxDurationMs),
	}
}

// IsZero reports whether every dimension is unset.
func (l Limits) IsZero() bool { return l == Limits{} }

// Violation is one dimension a request asked to exceed. Limit 0 means the
// ceiling is unlimited and can therefore never be exceeded.
type Violation struct {
	Field     string `json:"field"`
	Requested int64  `json:"requested"`
	Limit     int64  `json:"limit"`
	Detail    string `json:"detail,omitempty"`
}

func (v Violation) String() string {
	if v.Detail != "" {
		return v.Detail
	}
	return fmt.Sprintf("%s: requested %d, limit %d", v.Field, v.Requested, v.Limit)
}

// Exceeds reports the first dimension on which l asks for more than ceiling.
// A zero request inherits and never exceeds; a zero ceiling is unlimited.
func (l Limits) Exceeds(ceiling Limits) *Violation {
	if violation := l.Depth.Exceeds(ceiling.Depth); violation != nil {
		return violation
	}
	checks := []struct {
		field     string
		requested int64
		limit     int64
	}{
		{"maxChildren", int64(l.MaxChildren), int64(ceiling.MaxChildren)},
		{"maxConcurrent", int64(l.MaxConcurrent), int64(ceiling.MaxConcurrent)},
		{"maxTurns", int64(l.MaxTurns), int64(ceiling.MaxTurns)},
		{"maxTokens", int64(l.MaxTokens), int64(ceiling.MaxTokens)},
		{"maxDurationMs", l.MaxDurationMs, ceiling.MaxDurationMs},
	}
	for _, check := range checks {
		if check.requested > 0 && check.limit > 0 && check.requested > check.limit {
			return &Violation{Field: check.field, Requested: check.requested, Limit: check.limit}
		}
	}
	return nil
}

// effectiveBudget resolves the request budget against the effective limits and
// returns both the concrete budget handed to the launcher and the typed
// refusal when the request asks for more than the cap allows.
func effectiveBudget(request Budget, limits Limits) (Budget, *Error) {
	if violation := request.Limits().Exceeds(limits); violation != nil {
		collabErr := newError(ErrBudgetExceeded, CodeBudgetExceeded, "create_session", violation.String())
		collabErr.Field = violation.Field
		collabErr.Limit = violation.Limit
		collabErr.Actual = violation.Requested
		return Budget{}, collabErr
	}
	return Budget{
		MaxTurns:  minNonZero(request.MaxTurns, limits.MaxTurns),
		MaxTokens: minNonZero(request.MaxTokens, limits.MaxTokens),
		TimeoutMs: minNonZeroInt64(request.TimeoutMs, limits.MaxDurationMs),
	}, nil
}

func minNonZero(a, b int) int {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

func minNonZeroInt64(a, b int64) int64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}
