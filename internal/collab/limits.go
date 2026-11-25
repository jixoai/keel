package collab

import "fmt"

// Limits is the collaboration budget vocabulary. The same shape is used at
// every level, which makes the one direction explicit: a tenant ceiling is the
// hard upper bound, and a mode, WorkItem or request may only narrow it.
//
// Zero semantics are deliberate:
//   - on a ceiling, 0 means "no cap";
//   - on a request, 0 means "inherit the effective cap".
//
// NarrowedBy resolves both: when either side is 0 the other wins; when both
// are non-zero the smaller wins.
type Limits struct {
	// MaxDepth bounds nested mode sessions. A top-level session has depth 0,
	// so MaxDepth=3 permits children through depth 3. The core zero means
	// "inherit" on a request and "no core cap" on a ceiling; mode-specific
	// encodings (the sub-agent contract's -1/unlimited and 0/forbidden
	// settings) are resolved by the mode change before it calls Host.
	MaxDepth int `json:"maxDepth,omitempty"`
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

// Validate rejects negative dimensions. Zero is valid everywhere.
func (l Limits) Validate() error {
	fields := []struct {
		name  string
		value int64
	}{
		{"maxDepth", int64(l.MaxDepth)},
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
		MaxDepth:      minNonZero(l.MaxDepth, ceiling.MaxDepth),
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
}

func (v Violation) String() string {
	return fmt.Sprintf("%s: requested %d, limit %d", v.Field, v.Requested, v.Limit)
}

// Exceeds reports the first dimension on which l asks for more than ceiling.
// A zero request inherits and never exceeds; a zero ceiling is unlimited.
func (l Limits) Exceeds(ceiling Limits) *Violation {
	checks := []struct {
		field     string
		requested int64
		limit     int64
	}{
		{"maxDepth", int64(l.MaxDepth), int64(ceiling.MaxDepth)},
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
