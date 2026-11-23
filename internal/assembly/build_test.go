package assembly_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/jixoai/keel/internal/assembly"
)

func row(id, impl string, layer assembly.Layer) assembly.Row {
	return assembly.Row{ID: assembly.RowID(id), Layer: layer, Impl: impl}
}

func base(rows ...assembly.Row) assembly.Document {
	return assembly.Document{Stage: assembly.StageBase, Name: "base", Rows: rows}
}

func overlay(stage assembly.Stage, name string, rows ...assembly.Row) assembly.Document {
	return assembly.Document{Stage: stage, Name: name, Rows: rows}
}

func mustBuild(t *testing.T, docs ...assembly.Document) *assembly.Assembly {
	t.Helper()
	built, err := assembly.Build(docs)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return built
}

func issueError(t *testing.T, err error) *assembly.Error {
	t.Helper()
	var issues *assembly.Error
	if !errors.As(err, &issues) {
		t.Fatalf("error = %T %v, want *assembly.Error", err, err)
	}
	return issues
}

func ids(rows []assembly.EffectiveRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, string(r.ID))
	}
	return out
}

func stepIDs(steps []assembly.Step) []string {
	out := make([]string, 0, len(steps))
	for _, step := range steps {
		out = append(out, string(step.ID))
	}
	return out
}

// TestActivationIsAvailabilityDrivenNotDeclarationOrder pins the two orders:
// the effective row order never moves, while activation follows declared
// availability (a row becomes available as soon as its requirements are).
func TestActivationIsAvailabilityDrivenNotDeclarationOrder(t *testing.T) {
	c := row("keel.c#row", "v1", assembly.LayerHarness)
	c.Requires = []assembly.RowID{"keel.a#row", "keel.b#row"}
	built := mustBuild(t,
		base(c,
			row("keel.a#row", "v1", assembly.LayerHarness),
			row("keel.b#row", "v1", assembly.LayerHarness)),
	)

	if got, want := ids(built.Effective()), []string{"keel.c#row", "keel.a#row", "keel.b#row"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("effective order = %v, want declaration order %v", got, want)
	}
	if got, want := stepIDs(built.Activation()), []string{"keel.a#row", "keel.b#row", "keel.c#row"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("activation = %v, want availability order %v", got, want)
	}
}

// TestModeOverridesOnlyTheMatchingRow is the headless scenario: the UI row is
// absent, every other base row is byte-identical, and no startup order is
// hand-written.
func TestModeOverridesOnlyTheMatchingRow(t *testing.T) {
	ui := row("keel.ui#shell", "webview", assembly.LayerUI)
	shell := row("keel.shell#bash-local", "local", assembly.LayerHarness)
	workspace := row("keel.fs#workspace", "layerfs", assembly.LayerHarness)
	mode := overlay(assembly.StageMode, "headless", assembly.Row{
		ID: "keel.ui#shell", Absent: true,
	})

	built := mustBuild(t, base(ui, shell, workspace), mode)

	got, ok := built.Row("keel.ui#shell")
	if !ok || !got.Absent {
		t.Fatalf("ui row = %+v, want absent", got)
	}
	if got.Layer != assembly.LayerUI {
		t.Fatalf("an absent row keeps its architectural layer, got %q", got.Layer)
	}
	if got.Source != assembly.StageMode || !got.Overridden {
		t.Fatalf("ui row provenance = %s/%v, want mode override", got.Source, got.Overridden)
	}
	for _, id := range []assembly.RowID{"keel.shell#bash-local", "keel.fs#workspace"} {
		effective, ok := built.Row(id)
		if !ok {
			t.Fatalf("row %s disappeared", id)
		}
		original, _ := built.Row(id)
		if effective.Impl != original.Impl || effective.Source != assembly.StageBase || effective.Overridden {
			t.Fatalf("row %s changed without an override: %+v", id, effective)
		}
	}
	if got, want := stepIDs(built.Activation()), []string{"keel.shell#bash-local", "keel.fs#workspace"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("activation = %v, want the non-absent rows %v", got, want)
	}
	if got := built.Blocked(); len(got) != 0 {
		t.Fatalf("blocked = %+v, want none", got)
	}
}

// TestPresetChangesBehaviorWithoutRewritingBase pins whole-row replacement:
// the preset changes the implementation and its params, the base document is
// untouched, and no other row moves.
func TestPresetChangesBehaviorWithoutRewritingBase(t *testing.T) {
	bash := row("keel.shell#bash-local", "local", assembly.LayerHarness)
	bash.Params = map[string]string{"profile": "default", "timeout": "30"}
	read := row("keel.file#read", "local", assembly.LayerHarness)
	baseDoc := base(bash, read)
	preset := overlay(assembly.StagePreset, "safe", assembly.Row{
		ID:     "keel.shell#bash-local",
		Impl:   "restricted",
		Params: map[string]string{"profile": "safe"},
	})

	built := mustBuild(t, baseDoc, preset)

	effective, ok := built.Row("keel.shell#bash-local")
	if !ok {
		t.Fatal("bash row missing")
	}
	if effective.Impl != "restricted" {
		t.Fatalf("impl = %q, want restricted", effective.Impl)
	}
	if !reflect.DeepEqual(effective.Params, map[string]string{"profile": "safe"}) {
		t.Fatalf("params = %v, want whole-row replacement without merging", effective.Params)
	}
	if effective.Source != assembly.StagePreset {
		t.Fatalf("source = %s, want preset", effective.Source)
	}
	if baseDoc.Rows[0].Impl != "local" || baseDoc.Rows[0].Params["timeout"] != "30" {
		t.Fatalf("the base document was rewritten: %+v", baseDoc.Rows[0])
	}
	other, _ := built.Row("keel.file#read")
	if other.Impl != "local" || other.Source != assembly.StageBase {
		t.Fatalf("an unrelated row changed: %+v", other)
	}
	if got, want := stepIDs(built.Activation()), []string{"keel.shell#bash-local", "keel.file#read"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("activation = %v, want %v", got, want)
	}
}

// TestAbsentRowRendersAsAbsent pins the absent semantics: the row stays in the
// effective set with provenance, is excluded from activation, and carries no
// stale implementation.
func TestAbsentRowRendersAsAbsent(t *testing.T) {
	ui := row("keel.ui#shell", "webview", assembly.LayerUI)
	shell := row("keel.shell#bash-local", "local", assembly.LayerHarness)
	built := mustBuild(t,
		base(ui, shell),
		overlay(assembly.StageMode, "headless", assembly.Row{ID: "keel.ui#shell", Absent: true}),
	)

	effective, ok := built.Row("keel.ui#shell")
	if !ok {
		t.Fatal("an absent row must remain queryable")
	}
	if !effective.Absent || effective.Impl != "" || len(effective.Params) != 0 {
		t.Fatalf("absent row = %+v, want Absent with no implementation/params", effective)
	}
	for _, active := range built.Active() {
		if active.ID == "keel.ui#shell" {
			t.Fatal("an absent row activated")
		}
	}
}

func TestRowIdentityCollisionsAreRejected(t *testing.T) {
	duplicateBase := []assembly.Document{base(
		row("keel.a#row", "v1", assembly.LayerHarness),
		row("keel.a#row", "v2", assembly.LayerHarness),
	)}
	_, err := assembly.Build(duplicateBase)
	if !issueError(t, err).HasCode(assembly.IssueRowDuplicate) {
		t.Fatalf("base duplicate was not rejected: %v", err)
	}

	duplicateOverlay := []assembly.Document{
		base(row("keel.a#row", "v1", assembly.LayerHarness)),
		overlay(assembly.StageMode, "serve",
			assembly.Row{ID: "keel.a#row", Impl: "v2"},
			assembly.Row{ID: "keel.a#row", Impl: "v3"},
		),
	}
	_, err = assembly.Build(duplicateOverlay)
	if !issueError(t, err).HasCode(assembly.IssueRowDuplicate) {
		t.Fatalf("overlay duplicate was not rejected: %v", err)
	}
}

func TestUnknownRowIsRejected(t *testing.T) {
	_, err := assembly.Build([]assembly.Document{
		base(row("keel.a#row", "v1", assembly.LayerHarness)),
		overlay(assembly.StagePreset, "p", assembly.Row{ID: "keel.missing#row", Impl: "v2"}),
	})
	if !issueError(t, err).HasCode(assembly.IssueRowUnknown) {
		t.Fatalf("unknown row was not rejected: %v", err)
	}
}

func TestReorderingIsRejected(t *testing.T) {
	baseDoc := base(
		row("keel.a#row", "v1", assembly.LayerHarness),
		row("keel.b#row", "v1", assembly.LayerHarness),
		row("keel.c#row", "v1", assembly.LayerHarness),
	)
	_, err := assembly.Build([]assembly.Document{
		baseDoc,
		overlay(assembly.StagePreset, "p",
			assembly.Row{ID: "keel.c#row", Impl: "v2"},
			assembly.Row{ID: "keel.a#row", Impl: "v2"},
		),
	})
	if !issueError(t, err).HasCode(assembly.IssueRowOrder) {
		t.Fatalf("row reordering was not rejected: %v", err)
	}

	_, err = assembly.Build([]assembly.Document{
		baseDoc,
		overlay(assembly.StagePreset, "p"),
		overlay(assembly.StageMode, "serve"),
	})
	if !issueError(t, err).HasCode(assembly.IssueStageOrder) {
		t.Fatalf("stage reordering was not rejected: %v", err)
	}
}

func TestStackShapeIsValidated(t *testing.T) {
	_, err := assembly.Build(nil)
	if !issueError(t, err).HasCode(assembly.IssueMissingBase) {
		t.Fatalf("missing base was not rejected: %v", err)
	}

	_, err = assembly.Build([]assembly.Document{
		base(row("keel.a#row", "v1", assembly.LayerHarness)),
		base(row("keel.b#row", "v1", assembly.LayerHarness)),
	})
	if !issueError(t, err).HasCode(assembly.IssueStageDuplicate) {
		t.Fatalf("duplicate stage was not rejected: %v", err)
	}

	_, err = assembly.Build([]assembly.Document{{Stage: "bundle"}})
	if !issueError(t, err).HasCode(assembly.IssueStageUnknown) {
		t.Fatalf("unknown stage was not rejected: %v", err)
	}

	_, err = assembly.Build([]assembly.Document{
		base(row("keel.a#row", "v1", assembly.LayerHarness)),
		overlay(assembly.StageMode, "browser"),
	})
	if !issueError(t, err).HasCode(assembly.IssueMode) {
		t.Fatalf("unknown mode was not rejected: %v", err)
	}
}

// TestAnonymousBaseRowsNeverParticipateInOverlay pins the "no row id, no
// override" rule: the row survives untouched and cannot become an overlay
// target.
func TestAnonymousBaseRowsNeverParticipateInOverlay(t *testing.T) {
	anonymous := assembly.Row{Layer: assembly.LayerHarness, Impl: "builtin"}
	built := mustBuild(t,
		base(anonymous, row("keel.a#row", "v1", assembly.LayerHarness)),
		overlay(assembly.StagePreset, "p", assembly.Row{ID: "keel.a#row", Impl: "v2"}),
	)
	effective := built.Effective()
	if len(effective) != 2 {
		t.Fatalf("effective rows = %d, want 2", len(effective))
	}
	if effective[0].ID != "" || effective[0].Impl != "builtin" || effective[0].Overridden {
		t.Fatalf("anonymous row changed: %+v", effective[0])
	}
	if effective[1].Impl != "v2" {
		t.Fatalf("named row override = %q, want v2", effective[1].Impl)
	}
	if got, want := stepIDs(built.Activation()), []string{"", "keel.a#row"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("activation = %v, want %v", got, want)
	}

	_, err := assembly.Build([]assembly.Document{
		base(row("keel.a#row", "v1", assembly.LayerHarness)),
		overlay(assembly.StageMode, "serve", assembly.Row{Layer: assembly.LayerHarness, Impl: "new"}),
	})
	if !issueError(t, err).HasCode(assembly.IssueRowID) {
		t.Fatalf("anonymous overlay row was not rejected: %v", err)
	}
}

func TestOverlayCannotChangeStructure(t *testing.T) {
	baseDoc := base(row("keel.a#row", "v1", assembly.LayerHarness))

	_, err := assembly.Build([]assembly.Document{
		baseDoc,
		overlay(assembly.StagePreset, "p", assembly.Row{ID: "keel.a#row", Layer: assembly.LayerUI, Impl: "v2"}),
	})
	if !issueError(t, err).HasCode(assembly.IssueRowLayer) {
		t.Fatalf("layer change was not rejected: %v", err)
	}

	_, err = assembly.Build([]assembly.Document{
		baseDoc,
		overlay(assembly.StagePreset, "p", assembly.Row{
			ID:       "keel.a#row",
			Impl:     "v2",
			Requires: []assembly.RowID{"keel.a#row"},
		}),
	})
	if !issueError(t, err).HasCode(assembly.IssueRowRequires) {
		t.Fatalf("overlay requires were not rejected: %v", err)
	}

	_, err = assembly.Build([]assembly.Document{base(assembly.Row{
		ID: "keel.a#row", Layer: assembly.LayerHarness, Impl: "v1", Absent: true,
	})})
	if !issueError(t, err).HasCode(assembly.IssueRowBaseAbsent) {
		t.Fatalf("base absent row was not rejected: %v", err)
	}

	_, err = assembly.Build([]assembly.Document{base(assembly.Row{
		ID: "keel.a#row", Layer: assembly.LayerHarness,
	})})
	if !issueError(t, err).HasCode(assembly.IssueRowImpl) {
		t.Fatalf("missing implementation was not rejected: %v", err)
	}

	_, err = assembly.Build([]assembly.Document{base(assembly.Row{
		ID: "keel.a#row", Layer: assembly.LayerHarness, Impl: "v1",
		Params: map[string]string{"Bad Key": "x"},
	})})
	if !issueError(t, err).HasCode(assembly.IssueRowParams) {
		t.Fatalf("invalid param key was not rejected: %v", err)
	}
}

func TestOverlayKeepsTheBaseDependencyShape(t *testing.T) {
	dependency := row("keel.dep#row", "v1", assembly.LayerHarness)
	target := row("keel.a#row", "v1", assembly.LayerHarness)
	target.Requires = []assembly.RowID{"keel.dep#row"}

	built := mustBuild(t,
		base(dependency, target),
		overlay(assembly.StagePreset, "p", assembly.Row{ID: "keel.a#row", Impl: "v2"}),
	)
	effective, ok := built.Row("keel.a#row")
	if !ok {
		t.Fatal("overridden row is missing from the effective set")
	}
	if effective.Impl != "v2" {
		t.Fatalf("impl = %q, want v2", effective.Impl)
	}
	if got, want := effective.Requires, []assembly.RowID{"keel.dep#row"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("overridden row requires = %v, want the base dependency %v", got, want)
	}

	// A dependency that did not survive the overlay must block the overridden
	// row instead of letting it activate with a silently dropped edge.
	absentDependency := assembly.Row{ID: "keel.dep#row", Absent: true}
	blocked := mustBuild(t,
		base(dependency, target),
		overlay(assembly.StagePreset, "p", absentDependency, assembly.Row{ID: "keel.a#row", Impl: "v2"}),
	)
	blockedRows := blocked.Blocked()
	if len(blockedRows) != 1 || blockedRows[0].ID != "keel.a#row" {
		t.Fatalf("blocked = %+v, want keel.a#row blocked by its missing dependency", blockedRows)
	}
}

func TestRequirementErrorsAndBlockedRows(t *testing.T) {
	a := row("keel.a#row", "v1", assembly.LayerHarness)
	b := row("keel.b#row", "v1", assembly.LayerHarness)
	b.Requires = []assembly.RowID{"keel.a#row"}
	c := row("keel.c#row", "v1", assembly.LayerHarness)
	c.Requires = []assembly.RowID{"keel.b#row"}

	built := mustBuild(t,
		base(a, b, c),
		overlay(assembly.StageMode, "serve", assembly.Row{ID: "keel.a#row", Absent: true}),
	)
	if got := built.Activation(); len(got) != 0 {
		t.Fatalf("activation = %v, want none (a is absent, b/c are blocked)", stepIDs(got))
	}
	blocked := built.Blocked()
	if len(blocked) != 2 {
		t.Fatalf("blocked = %+v, want b and c", blocked)
	}
	if blocked[0].ID != "keel.b#row" || blocked[0].Reason != assembly.BlockedReasonAbsent {
		t.Fatalf("b blocked = %+v", blocked[0])
	}
	if blocked[1].ID != "keel.c#row" || blocked[1].Reason != assembly.BlockedReasonDependency {
		t.Fatalf("c blocked = %+v", blocked[1])
	}

	unknown := row("keel.d#row", "v1", assembly.LayerHarness)
	unknown.Requires = []assembly.RowID{"keel.missing#row"}
	if _, err := assembly.Build([]assembly.Document{base(unknown)}); !issueError(t, err).HasCode(assembly.IssueRowRequires) {
		t.Fatalf("unknown requirement was not rejected: %v", err)
	}

	cycleA := row("keel.cycle-a#row", "v1", assembly.LayerHarness)
	cycleA.Requires = []assembly.RowID{"keel.cycle-b#row"}
	cycleB := row("keel.cycle-b#row", "v1", assembly.LayerHarness)
	cycleB.Requires = []assembly.RowID{"keel.cycle-a#row"}
	if _, err := assembly.Build([]assembly.Document{base(cycleA, cycleB)}); !issueError(t, err).HasCode(assembly.IssueRowCycle) {
		t.Fatalf("requirement cycle was not rejected: %v", err)
	}

	self := row("keel.self#row", "v1", assembly.LayerHarness)
	self.Requires = []assembly.RowID{"keel.self#row"}
	if _, err := assembly.Build([]assembly.Document{base(self)}); !issueError(t, err).HasCode(assembly.IssueRowRequires) {
		t.Fatalf("self requirement was not rejected: %v", err)
	}
}

func TestEffectiveRowsAreCopies(t *testing.T) {
	built := mustBuild(t, base(row("keel.a#row", "v1", assembly.LayerHarness)))
	rows := built.Effective()
	rows[0].Impl = "mutated"
	again, _ := built.Row("keel.a#row")
	if again.Impl != "v1" {
		t.Fatalf("Effective returned a mutable view: %q", again.Impl)
	}
	steps := built.Activation()
	steps[0].Row.Impl = "mutated"
	again, _ = built.Row("keel.a#row")
	if again.Impl != "v1" {
		t.Fatalf("Activation returned a mutable view: %q", again.Impl)
	}
}
