package r1diff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestNormalizerCanonicalizesTimeAndIDs(t *testing.T) {
	n := NewNormalizer()
	// An ordered table on purpose: placeholder numbering follows first
	// appearance, so a map iteration order would make this test flaky.
	cases := []struct{ in, want string }{
		{"0f8fad5b-d9cb-469f-a165-70867728950e", "$uuid"},
		{"2026-11-06T12:34:56.789Z", "$ts"},
		{"2026-11-06T12:34:56+08:00", "$ts"},
		{"fake-1728392019283", "$id1"},
		{"call_abc123", "$id2"},
		{"msg_01HXYZ", "$id3"},
		{"hello r1", "hello r1"},
		{"please echo this", "please echo this"},
		{"assistant.message", "assistant.message"},
		{"r1-1", "r1-1"},
		{"used-1", "used-1"},
	}
	for _, c := range cases {
		if got := n.String(c.in); got != c.want {
			t.Errorf("String(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizerKeepsPairingStable(t *testing.T) {
	// The same generated id seen twice must canonicalize to the same value so a
	// toolCall/result pair still lines up across dumps.
	n := NewNormalizer()
	first := n.String("fake-111")
	second := n.String("fake-111")
	if first != second {
		t.Fatalf("same id mapped to %q then %q", first, second)
	}
	if n.String("call_x") == first {
		t.Fatal("different ids collapsed to the same placeholder")
	}
}

func TestNormalizerValueWalksStructure(t *testing.T) {
	n := NewNormalizer()
	v := decode(t, `{"seq":1,"ts":"2026-11-06T00:00:00Z","at":1762345678901,"ids":["call_a"],"ok":true,"none":null}`)
	got := n.Value(v).(map[string]any)
	if got["ts"] != "$ts" {
		t.Errorf("ts = %v", got["ts"])
	}
	if got["at"] != "$tsnum" {
		t.Errorf("at = %v", got["at"])
	}
	if got["ok"] != true || got["none"] != nil {
		t.Errorf("bool/null passthrough: %v %v", got["ok"], got["none"])
	}
	if ids := got["ids"].([]any); ids[0] != "$id1" {
		t.Errorf("ids = %v", ids)
	}
	if got["seq"] != float64(1) {
		t.Errorf("small numbers must stay verbatim, got %v", got["seq"])
	}
}

func TestFlattenShapeAndPaths(t *testing.T) {
	flat := map[string]string{}
	var v any
	if err := json.Unmarshal([]byte(`{"events":[{"seq":1},{"seq":2}],"empty":[]}`), &v); err != nil {
		t.Fatal(err)
	}
	Flatten(v, "", flat)
	if flat["/events/0/seq"] != "1" || flat["/events/1/seq"] != "2" {
		t.Fatalf("array paths: %v", flat)
	}
	if flat["/empty"] != "[]" {
		t.Fatalf("empty array must be a leaf, got %q", flat["/empty"])
	}
}

func TestCompareDetectsAbsenceAndValues(t *testing.T) {
	old := map[string]string{"/a": "1", "/b": "\"x\""}
	new := map[string]string{"/a": "2", "/c": "\"y\""}
	diffs := Compare(old, new)
	if len(diffs) != 3 {
		t.Fatalf("diffs = %+v", diffs)
	}
	if diffs[0].Path != "/a" || diffs[1].Path != "/b" || diffs[2].Path != "/c" {
		t.Fatalf("diffs not sorted by path: %+v", diffs)
	}
	if diffs[1].New != "$absent" || diffs[2].Old != "$absent" {
		t.Fatalf("absence not reported: %+v", diffs)
	}
}

func TestFilterUsesPathPrefixesAndNeedsReasons(t *testing.T) {
	diffs := []Diff{
		{Path: "/http/4/status", Old: "201", New: "400"},
		{Path: "/http/4/body/seq", Old: "2", New: "$absent"},
		{Path: "/events/3/payload/text", Old: "\"hi\"", New: "\"hey\""},
	}
	rules := []AllowRule{{Path: "/http/4", Reason: "MODIFIED API: edge tightening"}}
	allowed, unexpected, stale := Filter(diffs, rules)
	if len(allowed) != 2 || len(unexpected) != 1 {
		t.Fatalf("allowed=%+v unexpected=%+v", allowed, unexpected)
	}
	if len(stale) != 0 {
		t.Fatalf("fired rule reported stale: %+v", stale)
	}
	if allowed[0].Reason == "" {
		t.Fatal("allowed entries must carry the rule reason")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "allow.json")
	if err := os.WriteFile(path, []byte(`[{"path":"/x"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRules(path); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("rule without reason must be rejected, got %v", err)
	}
	if rules, err := LoadRules(""); err != nil || rules != nil {
		t.Fatalf("empty allowlist: %v %v", rules, err)
	}
}

func TestBuildReportVerdict(t *testing.T) {
	pass := BuildReport(10, nil, nil)
	if !pass.Equal || !pass.RulesComplete || !strings.Contains(pass.Summary(), "PASS") {
		t.Fatalf("pass report: %+v", pass)
	}
	fail := BuildReport(10, []Diff{{Path: "/a", Old: "1", New: "2"}}, nil)
	if fail.Equal || !strings.Contains(fail.Summary(), "FAIL") {
		t.Fatalf("fail report: %+v", fail)
	}
	if !strings.Contains(fail.Summary(), "/a") {
		t.Fatal("summary must name the unexpected path")
	}
	// No unexpected difference means the allowlist (empty here) is complete.
	if !fail.RulesComplete {
		t.Fatalf("rule completeness must ignore unexpected diffs: %+v", fail)
	}
}

func TestBuildReportFailsStaleRulesOnIdenticalDumps(t *testing.T) {
	// The one-sided gate this test pins down: identical dumps have zero diffs,
	// so every allow rule is stale and the run must fail even though nothing is
	// "unexpected". This is the wrong-baseline / same-binary / wrong-ref case.
	rules := []AllowRule{
		{Path: "/http/events.owner_scoped/status", Old: "201", New: "400", Reason: "registered tightening"},
		{Path: "/http/input.user1/body/status", Old: "\"pending\"", New: "\"committed\"", Reason: "registered status fix"},
	}
	report := BuildReport(102, nil, rules)
	if report.Equal {
		t.Fatalf("identical dumps with an allowlist must not be equal: %+v", report)
	}
	if report.RulesComplete {
		t.Fatalf("rule completeness must be false: %+v", report)
	}
	if len(report.StaleRules) != len(rules) {
		t.Fatalf("stale rules = %d, want %d: %+v", len(report.StaleRules), len(rules), report.StaleRules)
	}
	summary := report.Summary()
	if !strings.Contains(summary, "FAIL") || !strings.Contains(summary, "STALE RULE") {
		t.Fatalf("summary must fail and name stale rules:\n%s", summary)
	}
	if !strings.Contains(summary, "0/2 matched") {
		t.Fatalf("summary must count matched rules:\n%s", summary)
	}
	// Both fields are part of the machine-readable contract: a consumer reading
	// only "equal" or only "rulesComplete" must see the failure.
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"rulesComplete":false`, `"equal":false`, `"staleRules":[`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("report JSON missing %s:\n%s", want, raw)
		}
	}
}

func TestBuildReportPassesWhenEveryRuleFires(t *testing.T) {
	diffs := []Diff{
		{Path: "/http/events.owner_scoped/status", Old: "201", New: "400"},
		{Path: "/http/input.user1/body/status", Old: "\"pending\"", New: "\"committed\""},
	}
	rules := []AllowRule{
		{Path: "/http/events.owner_scoped/status", Old: "201", New: "400", Reason: "registered tightening"},
		{Path: "/http/input.user1/body/status", Old: "\"pending\"", New: "\"committed\"", Reason: "registered status fix"},
	}
	report := BuildReport(102, diffs, rules)
	if !report.Equal || !report.RulesComplete {
		t.Fatalf("every rule fired, want PASS: %+v", report)
	}
	if len(report.StaleRules) != 0 {
		t.Fatalf("stale rules: %+v", report.StaleRules)
	}
	if !strings.Contains(report.Summary(), "2/2 matched") || !strings.Contains(report.Summary(), "PASS") {
		t.Fatalf("summary:\n%s", report.Summary())
	}
}

func TestFilterMarksUnfiredAndDriftedRulesStale(t *testing.T) {
	diffs := []Diff{
		// This rule's path matches but its pinned new value drifted (400 -> 500):
		// the diff is unexpected AND the rule did not fire.
		{Path: "/http/edge/status", Old: "201", New: "500"},
	}
	rules := []AllowRule{
		{Path: "/http/edge/status", Old: "201", New: "400", Reason: "registered tightening"},
		{Path: "/http/never/seen", Reason: "declared but absent"},
	}
	allowed, unexpected, stale := Filter(diffs, rules)
	if len(allowed) != 0 || len(unexpected) != 1 {
		t.Fatalf("allowed=%+v unexpected=%+v", allowed, unexpected)
	}
	if len(stale) != 2 {
		t.Fatalf("drifted and unfired rules must both be stale, got %+v", stale)
	}
	if stale[0].Path != "/http/edge/status" || stale[1].Path != "/http/never/seen" {
		t.Fatalf("stale order must follow the allowlist: %+v", stale)
	}
}

func TestVerifyHealthOwner(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		pid     int
		wantErr string // empty means success
	}{
		{name: "matching pid", body: `{"ok":true,"pid":4242}`, pid: 4242},
		{name: "mismatched pid", body: `{"ok":true,"pid":4242}`, pid: 4343, wantErr: "pid 4242, not 4343"},
		{name: "missing pid", body: `{"ok":true}`, pid: 4242, wantErr: "no pid"},
		{name: "zero pid", body: `{"pid":0}`, pid: 4242, wantErr: "no pid"},
		{name: "not json", body: `not json`, pid: 4242, wantErr: "not JSON"},
		{name: "pid as string", body: `{"pid":"4242"}`, pid: 4242, wantErr: "not JSON"},
	}
	for _, c := range cases {
		err := VerifyHealthOwner([]byte(c.body), c.pid)
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: err = %v, want it to contain %q", c.name, err, c.wantErr)
		}
	}
}

func TestFilterPinnedValuesRejectDrift(t *testing.T) {
	rules := []AllowRule{{Path: "/http/edge/status", Old: "201", New: "400", Reason: "registered tightening"}}
	// The registered delta matches: allowed, rule fired.
	allowed, unexpected, stale := Filter([]Diff{{Path: "/http/edge/status", Old: "201", New: "400"}}, rules)
	if len(allowed) != 1 || len(unexpected) != 0 || len(stale) != 0 {
		t.Fatalf("registered delta: allowed=%v unexpected=%v stale=%v", allowed, unexpected, stale)
	}
	// The same path drifting to a different value must NOT be allowed: a 500 is
	// a regression, not the registered 400. The rule therefore matched nothing.
	allowed, unexpected, stale = Filter([]Diff{{Path: "/http/edge/status", Old: "201", New: "500"}}, rules)
	if len(allowed) != 0 || len(unexpected) != 1 || len(stale) != 1 {
		t.Fatalf("drifted delta must be unexpected and leave the rule stale: allowed=%v unexpected=%v stale=%v",
			allowed, unexpected, stale)
	}
	// Child paths inherit the prefix but still honour the pins.
	_, unexpected, _ = Filter([]Diff{{Path: "/http/edge/status/extra", Old: "201", New: "500"}}, rules)
	if len(unexpected) != 1 {
		t.Fatalf("child path with drifted value must be unexpected: %v", unexpected)
	}
}

func TestNormalizerKeepsDatePrefixedContentVerbatim(t *testing.T) {
	// A user message that happens to start with a date must not be normalized
	// away: only real timestamps (parseable layouts) become $ts.
	n := NewNormalizer()
	in := "2026-11-07 please remember the milk"
	if got := n.String(in); got != in {
		t.Fatalf("String(%q) = %q, want the text verbatim", in, got)
	}
}
