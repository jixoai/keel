package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// baselinePinSchema is the pin file's contract version.
const baselinePinSchema = "keel.r1diff.pin/1"

// baselinePin freezes the R1 reference: which commit is built as the baseline,
// which scenario the differential runs, and which allowlist registers the
// intentional deltas. `scripts/build-r1-baseline.sh` reads `ref` from here and
// `scripts/acceptance-all.sh` reads `allowlist`; the JSON is the single record of
// "what pre-change means" instead of a comment duplicated across scripts.
type baselinePin struct {
	Schema      string   `json:"schema"`
	Scenario    string   `json:"scenario"`
	Ref         string   `json:"ref"`
	RefSubject  string   `json:"refSubject"`
	Allowlist   string   `json:"allowlist"`
	RepinnedAt  string   `json:"repinnedAt"`
	Reason      string   `json:"reason"`
	V2Kinds     []string `json:"v2Kinds"`
	AllowRules  int      `json:"allowRules"`
	NegativeRun string   `json:"negativeRun"`
}

// loadPin reads and validates the pin. It is deliberately strict: a pin that
// names another scenario than the compiled one would make the differential
// compare unrelated captures.
func loadPin(path string) (baselinePin, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return baselinePin{}, fmt.Errorf("r1diff: read pin %s: %w", path, err)
	}
	var pin baselinePin
	if err := json.Unmarshal(raw, &pin); err != nil {
		return baselinePin{}, fmt.Errorf("r1diff: parse pin %s: %w", path, err)
	}
	if pin.Schema != baselinePinSchema {
		return baselinePin{}, fmt.Errorf("r1diff: pin schema %q, want %q", pin.Schema, baselinePinSchema)
	}
	if strings.TrimSpace(pin.Ref) == "" {
		return baselinePin{}, fmt.Errorf("r1diff: pin %s has no baseline ref", path)
	}
	if pin.Scenario != scenarioName {
		return baselinePin{}, fmt.Errorf("r1diff: pin scenario %q, want %q", pin.Scenario, scenarioName)
	}
	if strings.TrimSpace(pin.Allowlist) == "" {
		return baselinePin{}, fmt.Errorf("r1diff: pin %s has no allowlist", path)
	}
	if pin.AllowRules <= 0 {
		return baselinePin{}, fmt.Errorf("r1diff: pin %s must record the expected allow-rule count", path)
	}
	return pin, nil
}
