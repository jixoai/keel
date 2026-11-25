package compaction

// This file (change keel-checkpoint) defines the checkpoint policy: the
// tenant/session knobs that decide when a durable checkpoint is produced and
// how many projections are kept. The policy never changes the truth: a
// checkpoint is still a projection over the append-only event log and the
// compaction event remains the durable fact.
//
// The policy deliberately stays a small value type. A tenant document supplies
// the effective policy; a session may only narrow it through an explicit
// override document (CheckpointPolicyOverride). Narrowing is a pure function so
// the same rules hold for the HTTP surface, the settings page and tests.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Checkpoint policy version prefix. A future semantic change to the fields or
// narrowing rules must bump it so a checkpoint's recorded policyVersion can
// never be mistaken for the current rules.
const checkpointPolicyScheme = "cp1"

// CheckpointPolicy is the effective checkpoint policy of one session.
//
// Zero values have these meanings:
//   - ThresholdTokens 0: fall back to the context-window/reserve trigger
//     (CompactionSettings/ShouldCompact), not "compact at zero tokens".
//   - RetainTokens 0: retain nothing beyond the safety rules (never the pending
//     user message); production defaults to DefaultCompactionSettings.
//   - MaxCheckpoints 0: unlimited projections.
//   - TTL 0: no expiry.
type CheckpointPolicy struct {
	// Enabled gates automatic checkpoint compaction.
	Enabled bool
	// ThresholdTokens is the absolute context-token threshold at which an
	// automatic checkpoint compaction starts. Zero derives the threshold from
	// the context window and the reserve (the historical behavior).
	ThresholdTokens int
	// RetainTokens is the approximate recent-token budget kept verbatim after a
	// compaction.
	RetainTokens int
	// MaxCheckpoints bounds how many checkpoint projections one session keeps.
	// Zero means unlimited.
	MaxCheckpoints int
	// TTL expires old checkpoint projections. Zero means no expiry.
	TTL time.Duration
	// ManualCompact allows POST /v1/sessions/{id}/compact.
	ManualCompact bool
}

// DefaultCheckpointPolicy is the policy a deployment serves when the tenant
// document is silent: compaction and manual compaction on, the historical
// retained budget, unlimited projections and no expiry.
func DefaultCheckpointPolicy() CheckpointPolicy {
	return CheckpointPolicy{
		Enabled:       true,
		RetainTokens:  DefaultCompactionSettings.KeepRecentTokens,
		ManualCompact: true,
	}
}

// Validate rejects negative knobs. A zero value is meaningful (see the type
// documentation), so only impossible values fail.
func (p CheckpointPolicy) Validate() error {
	if p.ThresholdTokens < 0 {
		return fmt.Errorf("compaction: checkpoint thresholdTokens must be >= 0")
	}
	if p.RetainTokens < 0 {
		return fmt.Errorf("compaction: checkpoint retainTokens must be >= 0")
	}
	if p.MaxCheckpoints < 0 {
		return fmt.Errorf("compaction: checkpoint maxCheckpoints must be >= 0")
	}
	if p.TTL < 0 {
		return fmt.Errorf("compaction: checkpoint ttl must be >= 0")
	}
	return nil
}

// Version is the stable identity of the effective policy recorded on every
// checkpoint it produced. It covers the knobs only (never tenant/session ids or
// the summary), so two checkpoints under the same rules share a version.
func (p CheckpointPolicy) Version() string {
	b := strings.Join([]string{
		checkpointPolicyScheme,
		strconv.FormatBool(p.Enabled),
		strconv.Itoa(p.ThresholdTokens),
		strconv.Itoa(p.RetainTokens),
		strconv.Itoa(p.MaxCheckpoints),
		strconv.FormatInt(int64(p.TTL/time.Second), 10),
		strconv.FormatBool(p.ManualCompact),
	}, "\x00")
	sum := sha256.Sum256([]byte(b))
	return checkpointPolicyScheme + "-" + hex.EncodeToString(sum[:6])
}

// TriggerThreshold returns the absolute context-token threshold this policy
// uses for automatic compaction. A positive ThresholdTokens wins; otherwise the
// context-window trigger applies (contextWindow - reserveTokens), mirroring
// CompactionSettings. A non-positive result means "no trigger".
func (p CheckpointPolicy) TriggerThreshold(contextWindow, reserveTokens int) int {
	if p.ThresholdTokens > 0 {
		return p.ThresholdTokens
	}
	if contextWindow <= 0 {
		return 0
	}
	return contextWindow - reserveTokens
}

// ShouldCompact reports whether the policy triggers an automatic checkpoint
// compaction at contextTokens. It is the policy-level companion of
// ShouldCompact(contextTokens, contextWindow, settings); callers that still
// drive the loop through CompactionSettings keep the original function.
func (p CheckpointPolicy) ShouldCompact(contextTokens, contextWindow, reserveTokens int) bool {
	if !p.Enabled {
		return false
	}
	threshold := p.TriggerThreshold(contextWindow, reserveTokens)
	return threshold > 0 && contextTokens > threshold
}

// CheckpointPolicyOverride is a session's explicit narrowing request. A nil
// field inherits the tenant policy, which keeps "zero means inherit" possible
// for booleans (Enabled, ManualCompact).
type CheckpointPolicyOverride struct {
	Enabled         *bool
	ThresholdTokens *int
	RetainTokens    *int
	MaxCheckpoints  *int
	TTL             *time.Duration
	ManualCompact   *bool
}

// CheckpointPolicyOverrideDocument is the JSON document a session policy API
// stores and serves. It is separate from CheckpointPolicyOverride because TTL
// travels as whole hours on the wire (the tenant document uses ttlHours too).
// Every field is optional: an omitted field inherits the tenant policy, and an
// empty document ({}) clears an override.
type CheckpointPolicyOverrideDocument struct {
	Enabled         *bool `json:"enabled,omitempty"`
	ThresholdTokens *int  `json:"thresholdTokens,omitempty"`
	RetainTokens    *int  `json:"retainTokens,omitempty"`
	MaxCheckpoints  *int  `json:"maxCheckpoints,omitempty"`
	TTLHours        *int  `json:"ttlHours,omitempty"`
	ManualCompact   *bool `json:"manualCompact,omitempty"`
}

// Validate rejects negative knobs. Zero is meaningful for every integer field
// (see CheckpointPolicy), so only impossible values fail.
func (d CheckpointPolicyOverrideDocument) Validate() error {
	negative := func(name string, value *int) error {
		if value != nil && *value < 0 {
			return fmt.Errorf("compaction: checkpoint policy %s must be >= 0", name)
		}
		return nil
	}
	for _, check := range []struct {
		name  string
		value *int
	}{
		{"thresholdTokens", d.ThresholdTokens},
		{"retainTokens", d.RetainTokens},
		{"maxCheckpoints", d.MaxCheckpoints},
		{"ttlHours", d.TTLHours},
	} {
		if err := negative(check.name, check.value); err != nil {
			return err
		}
	}
	return nil
}

// IsEmpty reports whether the document requests no override at all.
func (d CheckpointPolicyOverrideDocument) IsEmpty() bool {
	return d.Enabled == nil && d.ThresholdTokens == nil && d.RetainTokens == nil &&
		d.MaxCheckpoints == nil && d.TTLHours == nil && d.ManualCompact == nil
}

// Override converts the wire document into the narrowing input. TTLHours is
// converted to a duration; a zero value means "no expiry" (the narrower request
// is clamped to the tenant TTL by NarrowCheckpointPolicy).
func (d CheckpointPolicyOverrideDocument) Override() CheckpointPolicyOverride {
	out := CheckpointPolicyOverride{
		Enabled:         d.Enabled,
		ThresholdTokens: d.ThresholdTokens,
		RetainTokens:    d.RetainTokens,
		MaxCheckpoints:  d.MaxCheckpoints,
		ManualCompact:   d.ManualCompact,
	}
	if d.TTLHours != nil {
		ttl := time.Duration(*d.TTLHours) * time.Hour
		out.TTL = &ttl
	}
	return out
}

// PolicyNarrowing reports one field the session asked for that the tenant
// policy refused or clamped. The reason is identifier-only (never a message
// body), so a settings page can render "被 tenant 收窄" without leaking data.
type PolicyNarrowing struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// NarrowCheckpointPolicy applies a session override to the tenant policy.
//
// The tenant policy is the ceiling: a session may disable a capability, request
// less retention, keep fewer checkpoints or expire them sooner, but it can
// never enable what the tenant disabled or exceed a tenant cap. For
// ThresholdTokens the narrowing direction is "compact no earlier than the
// tenant permits" (a larger threshold is the narrower request); when the tenant
// set an absolute threshold a session cannot fall back to an earlier
// window-derived one. RetainTokens 0 is literal "retain no recent tail" and is
// allowed; MaxCheckpoints 0 / TTL 0 mean "unlimited"/"no expiry" and are clamped
// to a tenant cap.
//
// Every adjustment is reported so the API/UI can explain the effective policy.
func NarrowCheckpointPolicy(tenant CheckpointPolicy, override CheckpointPolicyOverride) (CheckpointPolicy, []PolicyNarrowing) {
	effective := tenant
	var narrowed []PolicyNarrowing

	if override.Enabled != nil {
		if *override.Enabled && !tenant.Enabled {
			narrowed = append(narrowed, PolicyNarrowing{Field: "enabled", Reason: "tenant policy disables checkpoints"})
		} else {
			effective.Enabled = tenant.Enabled && *override.Enabled
		}
	}

	if override.ThresholdTokens != nil {
		requested := *override.ThresholdTokens
		switch {
		case requested < 0:
			narrowed = append(narrowed, PolicyNarrowing{Field: "thresholdTokens", Reason: "negative values are ignored"})
		case tenant.ThresholdTokens > 0 && (requested == 0 || requested < tenant.ThresholdTokens):
			effective.ThresholdTokens = tenant.ThresholdTokens
			narrowed = append(narrowed, PolicyNarrowing{Field: "thresholdTokens", Reason: "tenant threshold is higher; the session cannot compact earlier"})
		default:
			effective.ThresholdTokens = requested
		}
	}

	if override.RetainTokens != nil {
		requested := *override.RetainTokens
		switch {
		case requested < 0:
			narrowed = append(narrowed, PolicyNarrowing{Field: "retainTokens", Reason: "negative values are ignored"})
		case tenant.RetainTokens > 0 && requested > tenant.RetainTokens:
			effective.RetainTokens = tenant.RetainTokens
			narrowed = append(narrowed, PolicyNarrowing{Field: "retainTokens", Reason: "tenant retained-token ceiling applies"})
		default:
			effective.RetainTokens = requested
		}
	}

	if override.MaxCheckpoints != nil {
		requested := *override.MaxCheckpoints
		switch {
		case requested < 0:
			narrowed = append(narrowed, PolicyNarrowing{Field: "maxCheckpoints", Reason: "negative values are ignored"})
		case tenant.MaxCheckpoints > 0 && (requested == 0 || requested > tenant.MaxCheckpoints):
			effective.MaxCheckpoints = tenant.MaxCheckpoints
			narrowed = append(narrowed, PolicyNarrowing{Field: "maxCheckpoints", Reason: "tenant maximum checkpoint count applies"})
		default:
			effective.MaxCheckpoints = requested
		}
	}

	if override.TTL != nil {
		requested := *override.TTL
		switch {
		case requested < 0:
			narrowed = append(narrowed, PolicyNarrowing{Field: "ttl", Reason: "negative values are ignored"})
		case tenant.TTL > 0 && (requested == 0 || requested > tenant.TTL):
			effective.TTL = tenant.TTL
			narrowed = append(narrowed, PolicyNarrowing{Field: "ttl", Reason: "tenant expiry ceiling applies"})
		default:
			effective.TTL = requested
		}
	}

	if override.ManualCompact != nil {
		if *override.ManualCompact && !tenant.ManualCompact {
			narrowed = append(narrowed, PolicyNarrowing{Field: "manualCompact", Reason: "tenant policy disables manual compaction"})
		} else {
			effective.ManualCompact = tenant.ManualCompact && *override.ManualCompact
		}
	}

	return effective, narrowed
}
