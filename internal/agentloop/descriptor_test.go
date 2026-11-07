package agentloop_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentloop"
	"github.com/jixoai/keel/internal/agentloop/testloop"
	"github.com/jixoai/keel/internal/loadout"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/servicegraph"
)

// TestDescriptorMatchesLoadoutBlueprint: the loop's own descriptor and the
// frozen default-loadout blueprint must describe the same service. Drift in
// name, version, capabilities or effects fails here instead of surfacing as a
// silently different assembly.
func TestDescriptorMatchesLoadoutBlueprint(t *testing.T) {
	if agentloop.ServiceName != loadout.ServicePiLoop {
		t.Fatalf("ServiceName = %q, loadout.ServicePiLoop = %q", agentloop.ServiceName, loadout.ServicePiLoop)
	}
	vocabulary := []struct{ got, want, what string }{
		{agentloop.Version, loadout.Version, "version"},
		{agentloop.CapLoop, loadout.CapLoop, "provides"},
		{agentloop.CapLLM, loadout.CapLLM, "llm capability"},
		{agentloop.CapTools, loadout.CapTools, "tools capability"},
		{agentloop.CapCompaction, loadout.CapCompaction, "compaction capability"},
	}
	for _, v := range vocabulary {
		if v.got != v.want {
			t.Errorf("%s: agentloop %q != loadout %q", v.what, v.got, v.want)
		}
	}

	var blueprint servicegraph.ServiceDescriptor
	for _, d := range loadout.DefaultServices() {
		if d.Name == agentloop.ServiceName {
			blueprint = d
		}
	}
	if blueprint.Name == "" {
		t.Fatalf("loadout blueprint has no %q service", agentloop.ServiceName)
	}
	got := agentloop.Descriptor()
	if got.Name != blueprint.Name || got.Version != blueprint.Version || got.Role != blueprint.Role {
		t.Fatalf("identity: agentloop %s@%s role=%q, loadout %s@%s role=%q",
			got.Name, got.Version, got.Role, blueprint.Name, blueprint.Version, blueprint.Role)
	}
	if !reflect.DeepEqual(sorted(got.Provides), sorted(blueprint.Provides)) {
		t.Fatalf("provides: agentloop %v, loadout %v", got.Provides, blueprint.Provides)
	}
	if !reflect.DeepEqual(sorted(got.Requires), sorted(blueprint.Requires)) {
		t.Fatalf("requires: agentloop %v, loadout %v", got.Requires, blueprint.Requires)
	}
	if !reflect.DeepEqual(sorted(got.Optional), sorted(blueprint.Optional)) {
		t.Fatalf("optional: agentloop %v, loadout %v", got.Optional, blueprint.Optional)
	}
	if !reflect.DeepEqual(sorted(got.Effects), sorted(blueprint.Effects)) {
		t.Fatalf("effects: agentloop %v, loadout %v", got.Effects, blueprint.Effects)
	}
	if got.Factory == nil {
		t.Fatal("descriptor has no factory")
	}
}

func sorted(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

// TestDescriptorFactoryBindsTheLLMBinding: the factory resolves the provider
// stream from the CapLLM binding on every assembly, so a reassembled loadout
// rebinds the loop to the new provider instead of keeping a captured stream.
func TestDescriptorFactoryBindsTheLLMBinding(t *testing.T) {
	desc := agentloop.Descriptor()
	ctx := context.Background()

	t.Run("bare stream function binding", func(t *testing.T) {
		prov := newFakeProvider(assistantText("bound", agentcore.StopReasonEndTurn))
		instance, err := desc.Factory(ctx, servicegraph.Resolved{
			Service:  desc.Name,
			Bindings: map[string]any{agentloop.CapLLM: prov.stream},
		})
		if err != nil {
			t.Fatalf("factory: %v", err)
		}
		loop, ok := instance.(agentloop.Loop)
		if !ok {
			t.Fatalf("instance %T does not implement agentloop.Loop", instance)
		}
		if err := instance.Start(ctx, nil); err != nil {
			t.Fatalf("start: %v", err)
		}
		_, result := runLoopOnce(t, loop, baseInput())
		assertShapesEqual(t, "bound loop produced", shapesOf(result), []msgShape{
			{Role: agentcore.RoleUser, Text: "hello"},
			{Role: agentcore.RoleAssistant, Text: "bound", StopReason: agentcore.StopReasonEndTurn},
		})
		if err := instance.Stop(ctx); err != nil {
			t.Fatalf("stop: %v", err)
		}
	})

	t.Run("stream provider binding", func(t *testing.T) {
		prov := newFakeProvider(assistantText("via provider", agentcore.StopReasonEndTurn))
		instance, err := desc.Factory(ctx, servicegraph.Resolved{
			Service:  desc.Name,
			Bindings: map[string]any{agentloop.CapLLM: streamBinding{stream: prov.stream}},
		})
		if err != nil {
			t.Fatalf("factory: %v", err)
		}
		loop, ok := instance.(agentloop.Loop)
		if !ok {
			t.Fatalf("instance %T does not implement agentloop.Loop", instance)
		}
		_, result := runLoopOnce(t, loop, baseInput())
		assertShapesEqual(t, "bound loop produced", shapesOf(result), []msgShape{
			{Role: agentcore.RoleUser, Text: "hello"},
			{Role: agentcore.RoleAssistant, Text: "via provider", StopReason: agentcore.StopReasonEndTurn},
		})
	})

	t.Run("missing or mis-shaped binding fails the factory", func(t *testing.T) {
		cases := map[string]map[string]any{
			"unbound":         nil,
			"wrong type":      {agentloop.CapLLM: 42},
			"nil stream":      {agentloop.CapLLM: provider.StreamFn(nil)},
			"empty provider":  {agentloop.CapLLM: streamBinding{}},
			"tool-like value": {agentloop.CapLLM: &echoTool{}},
		}
		for name, bindings := range cases {
			t.Run(name, func(t *testing.T) {
				_, err := desc.Factory(ctx, servicegraph.Resolved{Service: desc.Name, Bindings: bindings})
				if !errors.Is(err, agentloop.ErrNoStream) {
					t.Fatalf("err = %v, want ErrNoStream", err)
				}
			})
		}
	})
}

// TestDescriptorResolvesInGraph: the descriptor participates in loadout
// resolution like any other service: the static graph reports the missing
// required capabilities, and with providers present it orders the loop after
// them (dependency gating itself is W2's engine).
func TestDescriptorResolvesInGraph(t *testing.T) {
	t.Run("missing providers", func(t *testing.T) {
		lo, err := servicegraph.NewLoadout(servicegraph.LoadoutConfig{
			ID:       "solo",
			Services: []servicegraph.ServiceDescriptor{agentloop.Descriptor()},
		})
		if err != nil {
			t.Fatalf("loadout: %v", err)
		}
		graph, err := servicegraph.Resolve(lo)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		missing := sorted(graph.MissingRequires(agentloop.ServiceName))
		if want := []string{agentloop.CapLLM, agentloop.CapTools}; !reflect.DeepEqual(missing, want) {
			t.Fatalf("missing requires = %v, want %v", missing, want)
		}
	})

	t.Run("providers present", func(t *testing.T) {
		lo, err := servicegraph.NewLoadout(servicegraph.LoadoutConfig{
			ID: "with-providers",
			Services: []servicegraph.ServiceDescriptor{
				{Name: "llm", Version: "v-test", Provides: []string{agentloop.CapLLM}},
				{Name: "tools", Version: "v-test", Provides: []string{agentloop.CapTools}},
				{Name: "compaction", Version: "v-test", Provides: []string{agentloop.CapCompaction}},
				agentloop.Descriptor(),
			},
		})
		if err != nil {
			t.Fatalf("loadout: %v", err)
		}
		graph, err := servicegraph.Resolve(lo)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got := graph.MissingRequires(agentloop.ServiceName); len(got) != 0 {
			t.Fatalf("missing requires = %v, want none", got)
		}
		if got := graph.MissingOptionals(agentloop.ServiceName); len(got) != 0 {
			t.Fatalf("missing optionals = %v, want none", got)
		}
		order := graph.Order()
		index := map[string]int{}
		for i, name := range order {
			index[name] = i
		}
		for _, provider := range []string{"llm", "tools", "compaction"} {
			if index[provider] >= index[agentloop.ServiceName] {
				t.Fatalf("activation order %v: %s must precede %s", order, provider, agentloop.ServiceName)
			}
		}
	})
}

// TestLoopSlotAcceptsAnotherImplementation: the same service slot can host a
// different loop. Here the canned test loop is installed through
// DescriptorWithFactory — identity unchanged, factory swapped — and it runs
// through the identical interface.
func TestLoopSlotAcceptsAnotherImplementation(t *testing.T) {
	desc := agentloop.DescriptorWithFactory(func(context.Context, servicegraph.Resolved) (servicegraph.Instance, error) {
		return testloop.New("swapped: "), nil
	})
	if desc.Name != agentloop.ServiceName || desc.Version != agentloop.Version {
		t.Fatalf("replacement changed the service identity: %s@%s", desc.Name, desc.Version)
	}
	instance, err := desc.Factory(context.Background(), servicegraph.Resolved{Service: desc.Name})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	loop, ok := instance.(agentloop.Loop)
	if !ok {
		t.Fatalf("instance %T does not implement agentloop.Loop", instance)
	}
	_, result := checkLoopContract(t, "swapped loop", loop, baseInput())
	assertShapesEqual(t, "swapped loop produced", shapesOf(result), []msgShape{
		{Role: agentcore.RoleAssistant, Text: "swapped: hello", StopReason: agentcore.StopReasonEndTurn},
	})
}
