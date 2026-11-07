package agentlooptest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentloop"
	"github.com/jixoai/keel/internal/agentloop/testloop"
	"github.com/jixoai/keel/internal/provider"
)

// TestManifestMatchesSuiteCases keeps the manifest and the implementation in
// lockstep: the manifest is the contract the loadout pins, so it can never
// advertise a case the suite lacks (or hide a case the suite runs).
func TestManifestMatchesSuiteCases(t *testing.T) {
	manifest := Manifest()
	byName := map[string]bool{}
	for _, c := range manifest {
		if c.Name == "" || c.Title == "" || c.Source == "" {
			t.Fatalf("manifest case has an empty field: %+v", c)
		}
		if byName[c.Name] {
			t.Fatalf("manifest case %q declared twice", c.Name)
		}
		byName[c.Name] = true
	}
	for name := range cases {
		if !byName[name] {
			t.Fatalf("suite case %q is not in the manifest", name)
		}
	}
	for name := range byName {
		if _, ok := cases[name]; !ok {
			t.Fatalf("manifest case %q has no suite implementation", name)
		}
	}
	if len(manifest) != len(cases) {
		t.Fatalf("manifest cases = %d, suite cases = %d", len(manifest), len(cases))
	}
}

// TestManifestHashIsCanonical: the hash is sha256 over the canonical JSON,
// independent of declaration order, and sensitive to any case change.
func TestManifestHashIsCanonical(t *testing.T) {
	cases := Manifest()
	blob, err := json.Marshal(cases)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	sum := sha256.Sum256(blob)
	want := hex.EncodeToString(sum[:])
	if got := ManifestHash(); got != want {
		t.Fatalf("ManifestHash = %s, want sha256(canonical JSON) = %s", got, want)
	}
	if len(want) != 64 {
		t.Fatalf("hash length = %d, want 64 hex characters", len(want))
	}
	if ManifestHash() != ManifestHash() {
		t.Fatal("ManifestHash is not stable across calls")
	}

	shuffled := append([]Case(nil), cases...)
	for i, j := 0, len(shuffled)-1; i < j; i, j = i+1, j-1 {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	if got := HashCases(shuffled); got != ManifestHash() {
		t.Fatalf("HashCases(shuffled) = %s, want declaration-order independence %s", got, ManifestHash())
	}

	tampered := append([]Case(nil), cases...)
	tampered[0].Title += " (tampered)"
	if got := HashCases(tampered); got == ManifestHash() {
		t.Fatal("HashCases ignored a changed PI title")
	}

	// Manifest returns a copy: a caller cannot mutate the contract.
	mutated := Manifest()
	mutated[0].Name = "mutated"
	if Manifest()[0].Name == "mutated" {
		t.Fatal("Manifest returned the shared slice")
	}
}

// recorder is a testing.T stand-in that captures the suite's assertion
// failures, so a test can prove a bad loop fails on an assertion rather than at
// compile time.
type recorder struct {
	mu       sync.Mutex
	failures []string
}

type caseFailed struct{}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.mu.Lock()
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
	r.mu.Unlock()
	panic(caseFailed{})
}

func (r *recorder) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.failures...)
}

// runCaseCaptured runs one manifest case and reports whether it failed through
// an assertion (Fatalf), plus the assertion messages.
func runCaseCaptured(t *testing.T, mk func(stream provider.StreamFn) agentloop.Loop, name string) (failed bool, messages []string) {
	t.Helper()
	rec := &recorder{}
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(caseFailed); !ok {
					panic(r)
				}
				failed = true
			}
		}()
		runCase(rec, mk, name, "captured")
	}()
	return failed, rec.messages()
}

// brokenLoop is a deliberately contract-violating implementation: it emits
// agent_start and settles without agent_end or any provider call.
type brokenLoop struct{}

var _ agentloop.Loop = brokenLoop{}

func (brokenLoop) Run(ctx context.Context, in agentloop.Input) (*agentloop.RunStream, error) {
	stream := agentloop.NewRunStream(2, nil)
	go func() {
		_ = stream.Emit(ctx, agentcore.AgentStartEvent{SessionID: in.SessionID})
		stream.Finish(nil)
	}()
	return stream, nil
}

// TestViolatingLoopFailsOnAssertion: the suite rejects an implementation that
// violates the event contract, and it does so through a case assertion (the
// loop builds and runs; its behavior is what fails).
func TestViolatingLoopFailsOnAssertion(t *testing.T) {
	failed, messages := runCaseCaptured(t, func(provider.StreamFn) agentloop.Loop {
		return brokenLoop{}
	}, "agent-message-events")
	if !failed {
		t.Fatal("the suite passed a loop that never emits agent_end")
	}
	joined := strings.Join(messages, "\n")
	if !strings.Contains(joined, "agent_end") {
		t.Fatalf("failure was not the event-order assertion:\n%s", joined)
	}
	t.Logf("captured assertion: %s", joined)
}

// TestCannedLoopFailsConformance: the canned alternate loop (testloop) cannot
// pass the PI suite — it never calls the model and emits no prompt lifecycle.
// This is the deliberate counter-example the replacement gate exists for.
func TestCannedLoopFailsConformance(t *testing.T) {
	failed, messages := runCaseCaptured(t, func(provider.StreamFn) agentloop.Loop {
		return testloop.New("canned: ")
	}, "agent-message-events")
	if !failed {
		t.Fatal("the suite passed the canned testloop, which cannot be PI-conformant")
	}
	joined := strings.Join(messages, "\n")
	if !strings.Contains(joined, "prompt message lifecycle") && !strings.Contains(joined, "produced messages") {
		t.Fatalf("failure was not a transcript assertion:\n%s", joined)
	}
	t.Logf("captured assertion: %s", joined)
}
