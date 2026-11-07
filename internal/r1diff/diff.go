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
type Report struct {
	Paths      int       `json:"paths"`
	Unexpected []Diff    `json:"unexpected"`
	Allowed    []Allowed `json:"allowed"`
	AllowRules int       `json:"allowRules"`
	Equal      bool      `json:"equal"`
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

// Filter splits diffs into allowed and unexpected using the rules. A rule
// matches when the diff path equals its path or sits under it (prefix), so
// "/http/5" whitelists every field of the sixth HTTP step while "/http/2/status"
// whitelists exactly one field.
func Filter(diffs []Diff, rules []AllowRule) (allowed []Allowed, unexpected []Diff) {
	for _, d := range diffs {
		matched := false
		for _, r := range rules {
			if d.Path != r.Path && !strings.HasPrefix(d.Path, strings.TrimSuffix(r.Path, "/")+"/") {
				continue
			}
			// A pinned value must match exactly; an unpinned one is a
			// deliberate "formatting only" allowance.
			if (r.Old != "" && r.Old != d.Old) || (r.New != "" && r.New != d.New) {
				continue
			}
			allowed = append(allowed, Allowed{Diff: d, Reason: r.Reason})
			matched = true
			break
		}
		if !matched {
			unexpected = append(unexpected, d)
		}
	}
	return allowed, unexpected
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
	allowed, unexpected := Filter(diffs, rules)
	return Report{
		Paths:      paths,
		Unexpected: unexpected,
		Allowed:    allowed,
		AllowRules: len(rules),
		Equal:      len(unexpected) == 0,
	}
}

// Summary renders a human-readable verdict for the terminal log.
func (r Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "R1 differential: %d leaf paths compared, %d difference(s), %d allowed, %d unexpected\n",
		r.Paths, len(r.Allowed)+len(r.Unexpected), len(r.Allowed), len(r.Unexpected))
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
	if r.Equal {
		b.WriteString("verdict: PASS (no difference outside the allowlist)\n")
	} else {
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
