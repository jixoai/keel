package r1diff

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Diff is one leaf-level difference between two flattened dumps.
type Diff struct {
	Path string `json:"path"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

// AllowRule whitelists differences under a path prefix. Every rule must carry
// a reason: the allowlist is the change's frozen list of intentional deltas
// (for example the MODIFIED API that turns some edge writes into 400), and an
// unreasoned rule would hide a regression.
//
// Old and New, when set, pin the registered values: a path that matched the
// rule but drifted to another value (400 became 500) is NOT allowed and shows
// up as an unexpected difference. Leave them empty only for values that are not
// part of the contract (error message wording).
type AllowRule struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	Old    string `json:"old,omitempty"`
	New    string `json:"new,omitempty"`
}

// Allowed is a Diff that matched an AllowRule.
type Allowed struct {
	Diff
	Reason string `json:"reason"`
}

// Report is the machine-readable outcome of one differential.
//
// Equal alone would be a one-sided gate: an allowlist that swallows every
// difference (or a comparison run against the wrong dump, where nothing
// differs at all) would pass without proving the registered deltas happened.
// StaleRules closes that side: an allow rule that matched no difference means
// the change it registers is absent, which fails the run even when there is
// nothing unexpected.
type Report struct {
	Paths      int       `json:"paths"`
	Unexpected []Diff    `json:"unexpected"`
	Allowed    []Allowed `json:"allowed"`
	AllowRules int       `json:"allowRules"`
	// StaleRules lists the allowlist entries that matched no difference.
	StaleRules []AllowRule `json:"staleRules"`
	// RulesComplete is true when every allow rule matched at least once.
	RulesComplete bool `json:"rulesComplete"`
	Equal         bool `json:"equal"`
}

// Compare flattens nothing itself: it takes the two flattened dumps and returns
// every path that differs, sorted by path for stable output.
func Compare(oldFlat, newFlat map[string]string) []Diff {
	var diffs []Diff
	for path, oldVal := range oldFlat {
		newVal, ok := newFlat[path]
		if !ok {
			diffs = append(diffs, Diff{Path: path, Old: oldVal, New: "$absent"})
			continue
		}
		if oldVal != newVal {
			diffs = append(diffs, Diff{Path: path, Old: oldVal, New: newVal})
		}
	}
	for path, newVal := range newFlat {
		if _, ok := oldFlat[path]; !ok {
			diffs = append(diffs, Diff{Path: path, Old: "$absent", New: newVal})
		}
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Path < diffs[j].Path })
	return diffs
}

// Filter splits diffs into allowed and unexpected using the rules, and reports
// which rules matched nothing (stale). A rule matches when the diff path equals
// its path or sits under it (prefix), so "/http/5" whitelists every field of the
// sixth HTTP step while "/http/2/status" whitelists exactly one field.
func Filter(diffs []Diff, rules []AllowRule) (allowed []Allowed, unexpected []Diff, stale []AllowRule) {
	hit := make([]bool, len(rules))
	for _, d := range diffs {
		matched := false
		for i, r := range rules {
			if d.Path != r.Path && !strings.HasPrefix(d.Path, strings.TrimSuffix(r.Path, "/")+"/") {
				continue
			}
			// A pinned value must match exactly; an unpinned one is a
			// deliberate "formatting only" allowance.
			if (r.Old != "" && r.Old != d.Old) || (r.New != "" && r.New != d.New) {
				continue
			}
			allowed = append(allowed, Allowed{Diff: d, Reason: r.Reason})
			hit[i] = true
			matched = true
			break
		}
		if !matched {
			unexpected = append(unexpected, d)
		}
	}
	for i, r := range rules {
		// A rule that never fires is not "harmless extra safety": it means the
		// registered delta is missing (wrong baseline, wrong dump, reverted
		// fix), so the caller must fail rather than call the run equal.
		if !hit[i] {
			stale = append(stale, r)
		}
	}
	return allowed, unexpected, stale
}

// LoadRules reads an allowlist file. A missing file is an empty allowlist.
func LoadRules(path string) ([]AllowRule, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rules []AllowRule
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, fmt.Errorf("r1diff: parse allowlist %s: %w", path, err)
	}
	for i, r := range rules {
		if r.Path == "" || r.Reason == "" {
			return nil, fmt.Errorf("r1diff: allowlist entry %d needs both path and reason", i)
		}
	}
	return rules, nil
}

// BuildReport assembles the differential report and its verdict.
func BuildReport(paths int, diffs []Diff, rules []AllowRule) Report {
	allowed, unexpected, stale := Filter(diffs, rules)
	if stale == nil {
		stale = []AllowRule{} // serialise as [] rather than null: "checked, none stale"
	}
	complete := len(stale) == 0
	return Report{
		Paths:         paths,
		Unexpected:    unexpected,
		Allowed:       allowed,
		AllowRules:    len(rules),
		StaleRules:    stale,
		RulesComplete: complete,
		// Both sides must hold: no unregistered drift and every registered
		// delta observed. Identical dumps with a non-empty allowlist fail here.
		Equal: complete && len(unexpected) == 0,
	}
}

// Summary renders a human-readable verdict for the terminal log.
func (r Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "R1 differential: %d leaf paths compared, %d difference(s), %d allowed, %d unexpected\n",
		r.Paths, len(r.Allowed)+len(r.Unexpected), len(r.Allowed), len(r.Unexpected))
	fmt.Fprintf(&b, "  allow rules: %d/%d matched\n", r.AllowRules-len(r.StaleRules), r.AllowRules)
	max := 20
	for i, d := range r.Unexpected {
		if i == max {
			fmt.Fprintf(&b, "  … %d more\n", len(r.Unexpected)-max)
			break
		}
		fmt.Fprintf(&b, "  UNEXPECTED %s\n    old: %s\n    new: %s\n", d.Path, trim(d.Old), trim(d.New))
	}
	for _, a := range r.Allowed {
		fmt.Fprintf(&b, "  allowed %s (%s)\n", a.Path, a.Reason)
	}
	for _, s := range r.StaleRules {
		fmt.Fprintf(&b, "  STALE RULE %s: matched no difference, so the registered delta did not happen (%s)\n",
			s.Path, trim(s.Reason))
	}
	switch {
	case r.Equal:
		b.WriteString("verdict: PASS (no difference outside the allowlist; every allow rule fired)\n")
	case !r.RulesComplete && len(r.Unexpected) == 0:
		b.WriteString("verdict: FAIL (stale allow rules: registered differences did not happen)\n")
	case !r.RulesComplete:
		b.WriteString("verdict: FAIL (unexpected differences and stale allow rules)\n")
	default:
		b.WriteString("verdict: FAIL (unexpected differences)\n")
	}
	return b.String()
}

func trim(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
