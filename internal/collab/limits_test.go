package collab_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

func TestLimitsNarrowedBy(t *testing.T) {
	ceiling := collab.Limits{
		MaxDepth: 4, MaxChildren: 8, MaxConcurrent: 16,
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
				MaxDepth: 2, MaxChildren: 4, MaxConcurrent: 5,
				MaxTurns: 10, MaxTokens: 100, MaxDurationMs: 1000,
			},
			want: collab.Limits{
				MaxDepth: 2, MaxChildren: 4, MaxConcurrent: 5,
				MaxTurns: 10, MaxTokens: 100, MaxDurationMs: 1000,
			},
		},
		{
			name: "requested wider is clamped",
			requested: collab.Limits{
				MaxDepth: 9, MaxChildren: 99, MaxConcurrent: 999,
				MaxTurns: 999, MaxTokens: 999_999, MaxDurationMs: 999_999,
			},
			want: ceiling,
		},
		{
			name: "mixed dimensions merge per field",
			requested: collab.Limits{
				MaxDepth: 0, MaxChildren: 2, MaxConcurrent: 0,
				MaxTurns: 50, MaxTokens: 0, MaxDurationMs: 1000,
			},
			want: collab.Limits{
				MaxDepth: 4, MaxChildren: 2, MaxConcurrent: 16,
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
	requested := collab.Limits{MaxDepth: 3, MaxTurns: 7}
	if got := requested.NarrowedBy(collab.Limits{}); got != requested {
		t.Fatalf("unlimited ceiling changed the request: %+v", got)
	}
}

func TestLimitsExceeds(t *testing.T) {
	ceiling := collab.Limits{MaxDepth: 2, MaxTurns: 5}
	cases := []struct {
		name      string
		requested collab.Limits
		wantField string
	}{
		{name: "within", requested: collab.Limits{MaxDepth: 2, MaxTurns: 5}},
		{name: "zero inherits", requested: collab.Limits{}},
		{name: "depth over", requested: collab.Limits{MaxDepth: 3}, wantField: "maxDepth"},
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
	if violation := (collab.Limits{MaxDepth: 1 << 20}).Exceeds(collab.Limits{}); violation != nil {
		t.Fatalf("unlimited ceiling exceeded: %+v", violation)
	}
}

func TestLimitsValidate(t *testing.T) {
	if err := (collab.Limits{}).Validate(); err != nil {
		t.Fatalf("zero limits: %v", err)
	}
	if err := (collab.Limits{MaxDurationMs: -1}).Validate(); !errors.Is(err, collab.ErrInvalidRequest) {
		t.Fatalf("negative duration = %v, want ErrInvalidRequest", err)
	}
	if err := (collab.Budget{MaxTurns: -1}).Validate(); !errors.Is(err, collab.ErrInvalidRequest) {
		t.Fatalf("negative budget = %v, want ErrInvalidRequest", err)
	}
}

func TestNewModeRegistryRejectsNegativeCeiling(t *testing.T) {
	if _, err := collab.NewModeRegistry(uuid.New(), collab.Limits{MaxDepth: -1}); !errors.Is(err, collab.ErrInvalidMode) {
		t.Fatalf("NewModeRegistry = %v, want ErrInvalidMode", err)
	}
	if _, err := collab.NewModeRegistry(uuid.Nil, collab.Limits{}); !errors.Is(err, collab.ErrTenantRequired) {
		t.Fatalf("NewModeRegistry with no tenant = %v, want ErrTenantRequired", err)
	}
}
