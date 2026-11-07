package servicegraph

import (
	"strings"
	"testing"
)

func TestNewLoadoutValidation(t *testing.T) {
	cases := []struct {
		name     string
		services []ServiceDescriptor
		wantCode string
	}{
		{
			name:     "empty service name",
			services: []ServiceDescriptor{{Name: "", Provides: []string{"a"}}},
			wantCode: IssueEmptyName,
		},
		{
			name: "duplicate service name",
			services: []ServiceDescriptor{
				{Name: "a", Provides: []string{"c1"}},
				{Name: "a", Provides: []string{"c2"}},
			},
			wantCode: IssueDuplicateService,
		},
		{
			name:     "missing provides",
			services: []ServiceDescriptor{{Name: "a"}},
			wantCode: IssueMissingProvides,
		},
		{
			name:     "duplicate provides",
			services: []ServiceDescriptor{{Name: "a", Provides: []string{"c", "c"}}},
			wantCode: IssueDuplicateProvides,
		},
		{
			name:     "duplicate requires",
			services: []ServiceDescriptor{{Name: "a", Provides: []string{"c"}, Requires: []string{"d", "d"}}},
			wantCode: IssueDuplicateRequires,
		},
		{
			name:     "duplicate optional",
			services: []ServiceDescriptor{{Name: "a", Provides: []string{"c"}, Optional: []string{"d", "d"}}},
			wantCode: IssueDuplicateOptional,
		},
		{
			name:     "empty capability",
			services: []ServiceDescriptor{{Name: "a", Provides: []string{""}}},
			wantCode: IssueEmptyCapability,
		},
		{
			name:     "invalid role",
			services: []ServiceDescriptor{{Name: "a", Provides: []string{"c"}, Role: "secondary"}},
			wantCode: IssueInvalidRole,
		},
		{
			name:     "requires and optional overlap",
			services: []ServiceDescriptor{{Name: "a", Provides: []string{"c"}, Requires: []string{"d"}, Optional: []string{"d"}}},
			wantCode: IssueRequiresOptional,
		},
		{
			name:     "self dependency",
			services: []ServiceDescriptor{{Name: "a", Provides: []string{"c"}, Requires: []string{"c"}}},
			wantCode: IssueSelfDependency,
		},
		{
			name:     "self dependency through optional",
			services: []ServiceDescriptor{{Name: "a", Provides: []string{"c"}, Optional: []string{"c"}}},
			wantCode: IssueSelfDependency,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewLoadout(LoadoutConfig{ID: "x", Revision: 1, Services: tc.services})
			if err == nil {
				t.Fatalf("expected rejection with %s", tc.wantCode)
			}
			if !hasIssue(err, tc.wantCode) {
				t.Fatalf("want issue %s, got %v", tc.wantCode, err)
			}
		})
	}
}

func TestNewLoadoutFreezesSnapshot(t *testing.T) {
	provides := []string{"ctx.a"}
	descs := []ServiceDescriptor{{Name: "a", Version: "v1", Provides: provides}}
	lo, err := NewLoadout(LoadoutConfig{ID: "l", Revision: 3, Version: "test@v1", Summary: "s", Services: descs})
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the caller's slices must not change the snapshot.
	provides[0] = "ctx.mutated"
	descs[0].Name = "mutated"
	got, ok := lo.Service("a")
	if !ok || got.Provides[0] != "ctx.a" || got.Name != "a" {
		t.Fatalf("loadout was mutated through caller slices: %+v", got)
	}
	got.Provides[0] = "ctx.other"
	again, _ := lo.Service("a")
	if again.Provides[0] != "ctx.a" {
		t.Fatalf("loadout was mutated through Services() copy: %+v", again)
	}
	if lo.ID() != "l" || lo.Revision() != 3 || lo.Version() != "test@v1" || lo.Summary() != "s" || lo.Len() != 1 {
		t.Fatalf("accessors wrong: %s %d %s %s %d", lo.ID(), lo.Revision(), lo.Version(), lo.Summary(), lo.Len())
	}
}

func TestResolveDuplicateProviders(t *testing.T) {
	rec := &recorder{}
	llmA := desc("llm-a", instanceFactory(rec, "llm-a"), []string{"ctx.llm"}, nil, nil)
	llmB := desc("llm-b", instanceFactory(rec, "llm-b"), []string{"ctx.llm"}, nil, nil)
	llmF := desc("llm-f", instanceFactory(rec, "llm-f"), []string{"ctx.llm"}, nil, nil)
	llmF.Role = RoleFallback
	llmF2 := desc("llm-f2", instanceFactory(rec, "llm-f2"), []string{"ctx.llm"}, nil, nil)
	llmF2.Role = RoleFallback

	t.Run("two primaries rejected", func(t *testing.T) {
		lo := mustLoadout(t, "l", 1, llmA, llmB)
		_, err := Resolve(lo)
		if !hasIssue(err, IssueDuplicateProvider) {
			t.Fatalf("want duplicate-provider, got %v", err)
		}
	})
	t.Run("two fallbacks without primary rejected", func(t *testing.T) {
		lo := mustLoadout(t, "l", 1, llmF, llmF2)
		_, err := Resolve(lo)
		if !hasIssue(err, IssueAmbiguousProvider) {
			t.Fatalf("want ambiguous-provider, got %v", err)
		}
		if !strings.Contains(err.Error(), "none is primary") {
			t.Fatalf("message should explain the missing primary: %v", err)
		}
	})
	t.Run("primary plus fallback accepted", func(t *testing.T) {
		lo := mustLoadout(t, "l", 1, llmA, llmF)
		g := mustResolve(t, lo)
		chain := g.Chain("ctx.llm")
		if len(chain) != 2 || chain[0].Name != "llm-a" || chain[0].Role != RolePrimary || chain[1].Name != "llm-f" || chain[1].Role != RoleFallback {
			t.Fatalf("chain wrong: %+v", chain)
		}
		if g.IsStandby("llm-a") {
			t.Fatal("primary must not be standby")
		}
		if !g.IsStandby("llm-f") {
			t.Fatal("fallback must be standby")
		}
		head, ok := g.EffectiveProvider("ctx.llm")
		if !ok || head.Name != "llm-a" {
			t.Fatalf("effective provider wrong: %+v", head)
		}
	})
	t.Run("fallbacks keep declaration order", func(t *testing.T) {
		lo := mustLoadout(t, "l", 1, llmF2, llmA, llmF)
		g := mustResolve(t, lo)
		chain := g.Chain("ctx.llm")
		if len(chain) != 3 || chain[0].Name != "llm-a" || chain[1].Name != "llm-f2" || chain[2].Name != "llm-f" {
			t.Fatalf("chain wrong: %+v", chain)
		}
	})
	t.Run("sole fallback accepted and serves", func(t *testing.T) {
		lo := mustLoadout(t, "l", 1, llmF)
		g := mustResolve(t, lo)
		if g.IsStandby("llm-f") {
			t.Fatal("sole provider must serve even when declared fallback")
		}
	})
}

func TestResolveRejectsCycles(t *testing.T) {
	rec := &recorder{}
	t.Run("two node cycle", func(t *testing.T) {
		lo := mustLoadout(t, "l", 1,
			desc("a", instanceFactory(rec, "a"), []string{"ctx.a"}, []string{"ctx.b"}, nil),
			desc("b", instanceFactory(rec, "b"), []string{"ctx.b"}, []string{"ctx.a"}, nil),
		)
		_, err := Resolve(lo)
		var re *ResolveError
		if !asResolveError(err, &re) {
			t.Fatalf("want ResolveError, got %v", err)
		}
		var cycle *ResolveIssue
		for i := range re.Issues {
			if re.Issues[i].Code == IssueCycle {
				cycle = &re.Issues[i]
			}
		}
		if cycle == nil {
			t.Fatalf("want cycle issue, got %v", re)
		}
		if len(cycle.Cycle) != 3 || cycle.Cycle[0] != cycle.Cycle[2] {
			t.Fatalf("cycle path not closed: %v", cycle.Cycle)
		}
		if cycle.Cycle[0] != "a" || cycle.Cycle[1] != "b" {
			t.Fatalf("cycle path wrong: %v", cycle.Cycle)
		}
		if !strings.Contains(cycle.Message, "a -> b -> a") {
			t.Fatalf("message should show the path: %q", cycle.Message)
		}
	})
	t.Run("three node cycle", func(t *testing.T) {
		lo := mustLoadout(t, "l", 1,
			desc("a", instanceFactory(rec, "a"), []string{"ctx.a"}, []string{"ctx.b"}, nil),
			desc("b", instanceFactory(rec, "b"), []string{"ctx.b"}, []string{"ctx.c"}, nil),
			desc("c", instanceFactory(rec, "c"), []string{"ctx.c"}, []string{"ctx.a"}, nil),
		)
		_, err := Resolve(lo)
		if !hasIssue(err, IssueCycle) {
			t.Fatalf("want cycle, got %v", err)
		}
	})
	t.Run("cycle through optional edge", func(t *testing.T) {
		lo := mustLoadout(t, "l", 1,
			desc("a", instanceFactory(rec, "a"), []string{"ctx.a"}, nil, []string{"ctx.b"}),
			desc("b", instanceFactory(rec, "b"), []string{"ctx.b"}, []string{"ctx.a"}, nil),
		)
		_, err := Resolve(lo)
		if !hasIssue(err, IssueCycle) {
			t.Fatalf("want cycle, got %v", err)
		}
	})
}

func TestResolveOrderAndStaticAnalysis(t *testing.T) {
	rec := &recorder{}
	lo := mustLoadout(t, "l", 1,
		desc("worker", instanceFactory(rec, "worker"), []string{"ctx.worker"}, []string{"ctx.store", "ctx.loop"}, nil),
		desc("loop", instanceFactory(rec, "loop"), []string{"ctx.loop"}, []string{"ctx.llm", "ctx.tools"}, []string{"ctx.compaction"}),
		desc("tools", instanceFactory(rec, "tools"), []string{"ctx.tools"}, nil, []string{"ctx.mcp"}),
		desc("llm", instanceFactory(rec, "llm"), []string{"ctx.llm"}, nil, nil),
		desc("store", instanceFactory(rec, "store"), []string{"ctx.store"}, nil, nil),
	)
	g := mustResolve(t, lo)

	order := g.Order()
	if len(order) != 5 {
		t.Fatalf("order: %v", order)
	}
	pos := map[string]int{}
	for i, name := range order {
		pos[name] = i
	}
	for _, name := range order {
		for _, dep := range g.Dependencies(name) {
			if pos[dep] >= pos[name] {
				t.Fatalf("provider %s must precede dependent %s in %v", dep, name, order)
			}
		}
	}
	if deps := g.Dependencies("loop"); len(deps) != 2 || deps[0] != "llm" || deps[1] != "tools" {
		t.Fatalf("loop deps: %v", deps)
	}
	if miss := g.MissingRequires("loop"); len(miss) != 0 {
		t.Fatalf("loop missing: %v", miss)
	}
	if opt := g.MissingOptionals("loop"); len(opt) != 1 || opt[0] != "ctx.compaction" {
		t.Fatalf("loop optional missing: %v", opt)
	}
	if opt := g.MissingOptionals("tools"); len(opt) != 1 || opt[0] != "ctx.mcp" {
		t.Fatalf("tools optional missing: %v", opt)
	}
}

func TestResolveMissingRequiresStatically(t *testing.T) {
	rec := &recorder{}
	lo := mustLoadout(t, "l", 1,
		desc("loop", instanceFactory(rec, "loop"), []string{"ctx.loop"}, []string{"ctx.tools"}, nil),
	)
	g := mustResolve(t, lo)
	if miss := g.MissingRequires("loop"); len(miss) != 1 || miss[0] != "ctx.tools" {
		t.Fatalf("missing requires: %v", miss)
	}
	// A statically missing provider is not a rejection: the service stays PENDING.
	if len(g.Order()) != 1 || g.Order()[0] != "loop" {
		t.Fatalf("order: %v", g.Order())
	}
}
