package collab_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

func TestDepthPolicyStates(t *testing.T) {
	if collab.ForbiddenDepth().Allows(1) {
		t.Fatal("forbidden depth allowed a child")
	}
	unlimited := collab.UnlimitedDepth()
	if !unlimited.Allows(1) || !unlimited.Allows(1_000) {
		t.Fatal("unlimited depth rejected a child")
	}
	limited := collab.LimitedDepth(3)
	if !limited.Allows(1) || !limited.Allows(3) {
		t.Fatal("limited depth rejected an allowed child")
	}
	if limited.Allows(4) {
		t.Fatal("limited depth allowed depth 4")
	}
	if !(collab.DepthPolicy{}).Allows(1) {
		t.Fatal("unset policy should behave as no-effective-cap")
	}
}

func TestDepthPolicyValidate(t *testing.T) {
	valid := []collab.DepthPolicy{
		collab.ForbiddenDepth(), collab.UnlimitedDepth(), collab.LimitedDepth(1), {},
	}
	for _, policy := range valid {
		if err := policy.Validate(); err != nil {
			t.Fatalf("valid policy %+v: %v", policy, err)
		}
	}
	invalid := []collab.DepthPolicy{
		{Mode: collab.DepthLimited, Max: 0},
		{Mode: collab.DepthLimited, Max: -1},
		{Mode: collab.DepthUnlimited, Max: 2},
		{Mode: collab.DepthForbidden, Max: 1},
		{Mode: "galaxy"},
		{Mode: collab.DepthUnset, Max: 3},
	}
	for _, policy := range invalid {
		if err := policy.Validate(); !errors.Is(err, collab.ErrInvalidRequest) {
			t.Fatalf("invalid policy %+v = %v, want ErrInvalidRequest", policy, err)
		}
	}
}

func TestDepthPolicyNarrowedBy(t *testing.T) {
	ceiling := collab.LimitedDepth(4)
	cases := []struct {
		name      string
		requested collab.DepthPolicy
		ceiling   collab.DepthPolicy
		want      collab.DepthPolicy
	}{
		{name: "unset inherits limited ceiling", requested: collab.DepthPolicy{}, ceiling: ceiling, want: ceiling},
		{name: "narrow limited survives", requested: collab.LimitedDepth(2), ceiling: ceiling, want: collab.LimitedDepth(2)},
		{name: "wide limited clamps", requested: collab.LimitedDepth(9), ceiling: ceiling, want: ceiling},
		{name: "unlimited clamps to limited", requested: collab.UnlimitedDepth(), ceiling: ceiling, want: ceiling},
		{name: "forbidden survives", requested: collab.ForbiddenDepth(), ceiling: ceiling, want: collab.ForbiddenDepth()},
		{name: "forbidden ceiling wins", requested: collab.LimitedDepth(2), ceiling: collab.ForbiddenDepth(), want: collab.ForbiddenDepth()},
		{name: "unlimited ceiling leaves request", requested: collab.LimitedDepth(9), ceiling: collab.UnlimitedDepth(), want: collab.LimitedDepth(9)},
		{name: "unset ceiling leaves request", requested: collab.UnlimitedDepth(), ceiling: collab.DepthPolicy{}, want: collab.UnlimitedDepth()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.requested.NarrowedBy(tc.ceiling); got != tc.want {
				t.Fatalf("NarrowedBy = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDepthPolicyExceeds(t *testing.T) {
	ceiling := collab.LimitedDepth(2)
	cases := []struct {
		name      string
		requested collab.DepthPolicy
		ceiling   collab.DepthPolicy
		want      bool
	}{
		{name: "unset inherits", requested: collab.DepthPolicy{}, ceiling: ceiling},
		{name: "narrow within", requested: collab.LimitedDepth(2), ceiling: ceiling},
		{name: "narrow over", requested: collab.LimitedDepth(3), ceiling: ceiling, want: true},
		{name: "unlimited over limited", requested: collab.UnlimitedDepth(), ceiling: ceiling, want: true},
		{name: "forbidden never exceeds", requested: collab.ForbiddenDepth(), ceiling: ceiling},
		{name: "unlimited ceiling never exceeds", requested: collab.UnlimitedDepth(), ceiling: collab.UnlimitedDepth()},
		{name: "nothing exceeds forbidden", requested: collab.ForbiddenDepth(), ceiling: collab.ForbiddenDepth()},
		{name: "unset does not exceed forbidden", requested: collab.DepthPolicy{}, ceiling: collab.ForbiddenDepth()},
		{name: "limited exceeds forbidden", requested: collab.LimitedDepth(1), ceiling: collab.ForbiddenDepth(), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			violation := tc.requested.Exceeds(tc.ceiling)
			if tc.want != (violation != nil) {
				t.Fatalf("Exceeds = %+v, want violation=%v", violation, tc.want)
			}
			if violation != nil && violation.Field != "depth" {
				t.Fatalf("violation field = %q", violation.Field)
			}
		})
	}
}

func TestLimitsNarrowedBy(t *testing.T) {
	ceiling := collab.Limits{
		Depth:       collab.LimitedDepth(4),
		MaxChildren: 8, MaxConcurrent: 16,
		MaxTurns: 100, MaxTokens: 1000, MaxDurationMs: 60_000,
	}
	cases := []struct {
		name      string
		requested collab.Limits
		want      collab.Limits
	}{
		{
			name:      "zero inherits the ceiling",
			requested: collab.Limits{},
			want:      ceiling,
		},
		{
			name: "requested narrower survives",
			requested: collab.Limits{
				Depth:       collab.LimitedDepth(2),
				MaxChildren: 4, MaxConcurrent: 5,
				MaxTurns: 10, MaxTokens: 100, MaxDurationMs: 1000,
			},
			want: collab.Limits{
				Depth:       collab.LimitedDepth(2),
				MaxChildren: 4, MaxConcurrent: 5,
				MaxTurns: 10, MaxTokens: 100, MaxDurationMs: 1000,
			},
		},
		{
			name: "requested wider is clamped",
			requested: collab.Limits{
				Depth:       collab.UnlimitedDepth(),
				MaxChildren: 99, MaxConcurrent: 999,
				MaxTurns: 999, MaxTokens: 999_999, MaxDurationMs: 999_999,
			},
			want: ceiling,
		},
		{
			name: "mixed dimensions merge per field",
			requested: collab.Limits{
				Depth:       collab.ForbiddenDepth(),
				MaxChildren: 2, MaxConcurrent: 0,
				MaxTurns: 50, MaxTokens: 0, MaxDurationMs: 1000,
			},
			want: collab.Limits{
				Depth:       collab.ForbiddenDepth(),
				MaxChildren: 2, MaxConcurrent: 16,
				MaxTurns: 50, MaxTokens: 1000, MaxDurationMs: 1000,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.requested.NarrowedBy(ceiling); got != tc.want {
				t.Fatalf("NarrowedBy = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLimitsNarrowedByUnlimitedCeiling(t *testing.T) {
	requested := collab.Limits{Depth: collab.LimitedDepth(3), MaxTurns: 7}
	if got := requested.NarrowedBy(collab.Limits{}); got != requested {
		t.Fatalf("unlimited ceiling changed the request: %+v", got)
	}
}

func TestLimitsExceeds(t *testing.T) {
	ceiling := collab.Limits{Depth: collab.LimitedDepth(2), MaxTurns: 5}
	cases := []struct {
		name      string
		requested collab.Limits
		wantField string
	}{
		{name: "within", requested: collab.Limits{Depth: collab.LimitedDepth(2), MaxTurns: 5}},
		{name: "zero inherits", requested: collab.Limits{}},
		{name: "depth over", requested: collab.Limits{Depth: collab.LimitedDepth(3)}, wantField: "depth"},
		{name: "unlimited depth over", requested: collab.Limits{Depth: collab.UnlimitedDepth()}, wantField: "depth"},
		{name: "turns over", requested: collab.Limits{MaxTurns: 6}, wantField: "maxTurns"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			violation := tc.requested.Exceeds(ceiling)
			if tc.wantField == "" {
				if violation != nil {
					t.Fatalf("Exceeds = %+v, want nil", violation)
				}
				return
			}
			if violation == nil || violation.Field != tc.wantField {
				t.Fatalf("Exceeds = %+v, want field %s", violation, tc.wantField)
			}
		})
	}
	// An unlimited ceiling can never be exceeded.
	if violation := (collab.Limits{Depth: collab.UnlimitedDepth(), MaxChildren: 1 << 20}).Exceeds(collab.Limits{}); violation != nil {
		t.Fatalf("unlimited ceiling exceeded: %+v", violation)
	}
}

func TestLimitsValidate(t *testing.T) {
	if err := (collab.Limits{}).Validate(); err != nil {
		t.Fatalf("zero limits: %v", err)
	}
	if err := (collab.Limits{Depth: collab.LimitedDepth(0)}).Validate(); !errors.Is(err, collab.ErrInvalidRequest) {
		t.Fatalf("limited depth 0 = %v, want ErrInvalidRequest", err)
	}
	if err := (collab.Limits{MaxDurationMs: -1}).Validate(); !errors.Is(err, collab.ErrInvalidRequest) {
		t.Fatalf("negative duration = %v, want ErrInvalidRequest", err)
	}
	if err := (collab.Budget{MaxTurns: -1}).Validate(); !errors.Is(err, collab.ErrInvalidRequest) {
		t.Fatalf("negative budget = %v, want ErrInvalidRequest", err)
	}
}

func TestNewModeRegistryRejectsNegativeCeiling(t *testing.T) {
	if _, err := collab.NewModeRegistry(uuid.New(), collab.Limits{MaxChildren: -1}); !errors.Is(err, collab.ErrInvalidMode) {
		t.Fatalf("NewModeRegistry = %v, want ErrInvalidMode", err)
	}
	if _, err := collab.NewModeRegistry(uuid.Nil, collab.Limits{}); !errors.Is(err, collab.ErrTenantRequired) {
		t.Fatalf("NewModeRegistry with no tenant = %v, want ErrTenantRequired", err)
	}
}
