package pluginpkg

import (
	"strings"
	"testing"
)

// Published tool names (change keel-plugin-core, spec: 发布名必须是 wire 安全的
// `<a-zA-Z0-9_->{1,64}`). These tests pin the two halves of the rule:
//
//	normalization  a name the host derives (or an in-process plugin authored) is
//	               rewritten character by character, and an already compliant
//	               name is returned verbatim — that is the compatibility red line
//	               for `example_status` / `spawn_subagent`.
//	validation     a manifest whose published name cannot satisfy the wire
//	               grammar (over 64 characters, or two declarations colliding
//	               after normalization) is refused at install with a field error,
//	               not at call time.

func TestNormalizePublishedToolName(t *testing.T) {
	cases := map[string]string{
		// The compatibility red line: compliant names are byte-identical.
		"example_status": "example_status",
		"spawn_subagent": "spawn_subagent",
		"tool-1":         "tool-1",
		"ABCdef123_-":    "ABCdef123_-",
		"":               "",
		// The namespaced form the host derives for a sidecar.
		"demo.sidecar.greet":    "demo_sidecar_greet",
		"demo.sidecar.own_tool": "demo_sidecar_own_tool",
		// Other characters a declared name may carry (the id grammar plus ':').
		"cap:foo.bar": "cap_foo_bar",
		"a b":         "a_b",
		// Non-ASCII bytes are not wire safe either; each rune becomes one '_'.
		"工具":   "__",
		"a工具b": "a__b",
	}
	for input, want := range cases {
		if got := NormalizePublishedToolName(input); got != want {
			t.Errorf("NormalizePublishedToolName(%q) = %q, want %q", input, got, want)
		}
	}
	// Idempotence: normalizing a normalized name changes nothing.
	for input := range cases {
		once := NormalizePublishedToolName(input)
		if twice := NormalizePublishedToolName(once); twice != once {
			t.Errorf("normalization is not idempotent for %q: %q -> %q", input, once, twice)
		}
	}
}

func TestManifestPublishedToolsQualifyByKind(t *testing.T) {
	// A sidecar speaks local names; the host namespaces and normalizes them, and
	// both natural declarations of one tool converge on one published name.
	sidecar := &Manifest{ID: "demo.sidecar", Kind: KindSidecar, Provides: Provides{Tools: []string{"greet", "demo.sidecar.own_tool"}}}
	got := sidecar.PublishedTools()
	if len(got) != 2 {
		t.Fatalf("published tools = %+v", got)
	}
	if got[0].Declared != "greet" || got[0].Local != "greet" || got[0].Published != "demo_sidecar_greet" {
		t.Fatalf("bare declaration = %+v", got[0])
	}
	if got[1].Declared != "demo.sidecar.own_tool" || got[1].Local != "own_tool" || got[1].Published != "demo_sidecar_own_tool" {
		t.Fatalf("qualified declaration = %+v", got[1])
	}
	if NamespacedPublishedToolName("demo.sidecar", "greet") != "demo_sidecar_greet" {
		t.Fatalf("NamespacedPublishedToolName = %q", NamespacedPublishedToolName("demo.sidecar", "greet"))
	}

	// An in-process plugin names its tools directly: the declared name is the
	// published name, with only the wire normalization applied.
	inProcess := &Manifest{ID: "keel.creation", Kind: KindInProcess, Provides: Provides{Tools: []string{"creation_scaffold", "legacy.dotted"}}}
	got = inProcess.PublishedTools()
	if got[0].Published != "creation_scaffold" {
		t.Fatalf("compliant in-process tool was renamed: %+v", got[0])
	}
	if got[1].Published != "legacy_dotted" {
		t.Fatalf("dotted in-process tool = %+v", got[1])
	}
	if inProcess.NamespacesTools() {
		t.Fatal("an in-process plugin must not namespace its tools")
	}
}

func TestManifestRefusesUnpublishableToolNames(t *testing.T) {
	// A sidecar id of 60 characters leaves room for only 3 more characters once
	// the host joins `<id>.<local>` (60 + 1 + local <= 64).
	longID := "a" + strings.Repeat("b", 59)
	if !ValidatePluginID(longID) {
		t.Fatalf("fixture id %q is not grammar legal", longID)
	}
	// A three-character local name still fits (60 + 1 + 3 = 64): the boundary is
	// the published LENGTH, not the presence of punctuation.
	fitting := validManifest()
	fitting.ID = longID
	fitting.Kind = KindSidecar
	fitting.Provides.Tools = []string{"toolong"[:3]}
	fitting.Provides.UI = nil
	fitting.Entry = Entry{Command: []string{"node", "backend.mjs"}}
	if err := fitting.Validate(testEnv()); err != nil {
		t.Fatalf("a boundary-length published name was refused: %v", err)
	}
	if published := fitting.PublishedTools()[0].Published; len(published) != MaxPublishedToolName {
		t.Fatalf("boundary published name = %q (%d characters)", published, len(published))
	}

	// 60 + 1 + 7 = 68 characters once namespaced and normalized.
	manifest := validManifest()
	manifest.ID = longID
	manifest.Kind = KindSidecar
	manifest.Provides.Tools = []string{"toolong"}
	manifest.Provides.UI = nil
	manifest.Entry = Entry{Command: []string{"node", "backend.mjs"}}
	err := manifest.Validate(testEnv())
	requireField(t, err, "provides.tools[0]")
	if !strings.Contains(err.Error(), "must match") {
		t.Fatalf("error = %v, want the wire grammar", err)
	}

	// An in-process plugin: the declared name itself must survive normalization
	// inside 64 characters.
	manifest = validManifest()
	manifest.Provides.Tools = []string{strings.Repeat("x", MaxPublishedToolName+1)}
	requireField(t, manifest.Validate(testEnv()), "provides.tools[0]")
}

func TestManifestRefusesCollidingPublishedNames(t *testing.T) {
	// Two declarations that normalize onto one wire name would publish one tool
	// twice; the collision must be refused at install (fail-closed), whether the
	// collision comes from punctuation or from the qualified spelling.
	manifest := validManifest()
	manifest.Provides.Tools = []string{"a.b", "a_b"}
	err := manifest.Validate(testEnv())
	requireField(t, err, "provides.tools[1]")
	if !strings.Contains(err.Error(), "publishes the same name") {
		t.Fatalf("error = %v, want the collision reason", err)
	}

	sidecar := validManifest()
	sidecar.Kind = KindSidecar
	sidecar.Provides.Tools = []string{"greet", "vendor.thing.greet"}
	sidecar.Entry = Entry{Command: []string{"node", "backend.mjs"}}
	requireField(t, sidecar.Validate(testEnv()), "provides.tools[1]")
}

func TestPublishedToolNameGrammar(t *testing.T) {
	if !ValidPublishedToolName("demo_sidecar_greet") || !ValidPublishedToolName("a") {
		t.Fatal("a compliant name was refused")
	}
	if ValidPublishedToolName("") || ValidPublishedToolName("demo.sidecar") ||
		ValidPublishedToolName(strings.Repeat("a", MaxPublishedToolName+1)) {
		t.Fatal("a non-compliant name was accepted")
	}
}
