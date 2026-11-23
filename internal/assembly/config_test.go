package assembly_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/assembly"
)

func mustParseConfig(t *testing.T, source string) assembly.Config {
	t.Helper()
	config, err := assembly.ParseConfig([]byte(source))
	if err != nil {
		t.Fatalf("ParseConfig: %v\n%s", err, source)
	}
	return config
}

func configIssues(t *testing.T, source string) *assembly.Error {
	t.Helper()
	_, err := assembly.ParseConfig([]byte(source))
	if err == nil {
		t.Fatalf("ParseConfig accepted:\n%s", source)
	}
	return issueError(t, err)
}

func TestParseConfigRoundTripsTheDocumentedSubset(t *testing.T) {
	config := mustParseConfig(t, `
# the config stage is overrides only
version = 1

[[rows]]
id = "keel.shell#bash-local"   # no id, no override
impl = "ssh"
params.host = "devbox"
params.port = '22'

[[rows]]
id = "keel.ui#shell"
absent = true
`)
	if config.Version != assembly.ConfigFormatVersion {
		t.Fatalf("version = %d", config.Version)
	}
	if len(config.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(config.Rows))
	}
	first := config.Rows[0]
	if first.ID != "keel.shell#bash-local" || first.Layer != "" || first.Absent {
		t.Fatalf("row 0 = %+v", first)
	}
	if first.Impl != "ssh" {
		t.Fatalf("row 0 impl = %q", first.Impl)
	}
	if !reflect.DeepEqual(first.Params, map[string]string{"host": "devbox", "port": "22"}) {
		t.Fatalf("row 0 params = %v", first.Params)
	}
	if !config.Rows[1].Absent || config.Rows[1].Impl != "" {
		t.Fatalf("row 1 = %+v, want absent", config.Rows[1])
	}

	document := config.Document()
	if document.Stage != assembly.StageConfig {
		t.Fatalf("document stage = %s", document.Stage)
	}
	if len(document.Rows) != 2 || document.Rows[0].ID != first.ID {
		t.Fatalf("document = %+v", document)
	}
}

func TestParseConfigRejectsFieldLevelMistakes(t *testing.T) {
	cases := []struct {
		name   string
		source string
		code   string
	}{
		{
			name:   "missing version",
			source: "[[rows]]\nid = \"keel.a#row\"\nimpl = \"v1\"\n",
			code:   assembly.IssueConfigVersion,
		},
		{
			name:   "wrong version",
			source: "version = 2\n",
			code:   assembly.IssueConfigVersion,
		},
		{
			name:   "non-integer version",
			source: "version = \"1\"\n",
			code:   assembly.IssueConfigVersion,
		},
		{
			name:   "unknown table",
			source: "version = 1\n[assembly]\nfoo = 1\n",
			code:   assembly.IssueConfigTable,
		},
		{
			name:   "unknown top-level key",
			source: "version = 1\nmode = \"serve\"\n",
			code:   assembly.IssueConfigKey,
		},
		{
			name:   "unknown row key",
			source: "version = 1\n[[rows]]\nid = \"keel.a#row\"\nimpl = \"v1\"\nlayer = \"harness\"\n",
			code:   assembly.IssueConfigKey,
		},
		{
			name:   "id is not a string",
			source: "version = 1\n[[rows]]\nid = 7\nimpl = \"v1\"\n",
			code:   assembly.IssueConfigValue,
		},
		{
			name:   "param is not a string",
			source: "version = 1\n[[rows]]\nid = \"keel.a#row\"\nimpl = \"v1\"\nparams.host = [\"x\"]\n",
			code:   assembly.IssueConfigValue,
		},
		{
			name:   "duplicate key",
			source: "version = 1\n[[rows]]\nid = \"keel.a#row\"\nimpl = \"v1\"\nimpl = \"v2\"\n",
			code:   assembly.IssueConfigDuplicate,
		},
		{
			name:   "duplicate row",
			source: "version = 1\n[[rows]]\nid = \"keel.a#row\"\nimpl = \"v1\"\n\n[[rows]]\nid = \"keel.a#row\"\nimpl = \"v2\"\n",
			code:   assembly.IssueConfigDuplicate,
		},
		{
			name:   "absent with impl",
			source: "version = 1\n[[rows]]\nid = \"keel.a#row\"\nabsent = true\nimpl = \"v1\"\n",
			code:   assembly.IssueConfigAbsentImpl,
		},
		{
			name:   "absent with params",
			source: "version = 1\n[[rows]]\nid = \"keel.a#row\"\nabsent = true\nparams.host = \"x\"\n",
			code:   assembly.IssueConfigParamsAbs,
		},
		{
			name:   "empty row table",
			source: "version = 1\n[[rows]]\n",
			code:   assembly.IssueConfigEmptyRow,
		},
		{
			name:   "row without id",
			source: "version = 1\n[[rows]]\nimpl = \"v1\"\n",
			code:   assembly.IssueConfigEmptyRow,
		},
		{
			name:   "row with invalid id",
			source: "version = 1\n[[rows]]\nid = \"keel.a\"\nimpl = \"v1\"\n",
			code:   assembly.IssueRowID,
		},
		{
			name:   "present row without impl",
			source: "version = 1\n[[rows]]\nid = \"keel.a#row\"\n",
			code:   assembly.IssueRowImpl,
		},
		{
			name:   "invalid param key",
			source: "version = 1\n[[rows]]\nid = \"keel.a#row\"\nimpl = \"v1\"\nparams.Bad = \"x\"\n",
			code:   assembly.IssueConfigKey,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			issues := configIssues(t, test.source)
			if !issues.HasCode(test.code) {
				t.Fatalf("codes = %v, want %s\n%s", issues.Codes(), test.code, issues.Error())
			}
		})
	}
}

func TestConfigHashIsCanonical(t *testing.T) {
	a := mustParseConfig(t, `
version = 1
[[rows]]
id = "keel.a#row"
impl = "v1"
params.x = "1"
params.y = "2"

[[rows]]
id = "keel.b#row"
absent = true
`)
	b := mustParseConfig(t, `
# comments and whitespace are irrelevant
version = 1
[[rows]]
id="keel.b#row"
absent=true
[[rows]]
id="keel.a#row"
impl="v1"
params.y="2"
params.x="1"
`)
	if a.Hash() != b.Hash() {
		t.Fatalf("canonical hash differs:\n%s\n%s", a.Hash(), b.Hash())
	}
	if !strings.HasPrefix(a.Hash(), "sha256:") || len(a.Hash()) != len("sha256:")+64 {
		t.Fatalf("hash shape = %q", a.Hash())
	}

	c := mustParseConfig(t, "version = 1\n[[rows]]\nid = \"keel.a#row\"\nimpl = \"v2\"\n")
	if c.Hash() == a.Hash() {
		t.Fatal("a changed implementation must change the config hash")
	}
}

// TestRotationKeepsInFlightRunsOnTheirVersion is the frozen rotation contract:
// a run pins one version, a rotation affects only the next pin, and every
// version records its ledger anchor.
func TestRotationKeepsInFlightRunsOnTheirVersion(t *testing.T) {
	first := mustParseConfig(t, "version = 1\n[[rows]]\nid = \"keel.shell#bash-local\"\nimpl = \"local\"\n")
	rotation, err := assembly.NewRotation(first, 10)
	if err != nil {
		t.Fatalf("NewRotation: %v", err)
	}
	inFlight := rotation.Pin()
	if inFlight.Version.Revision != 1 || inFlight.Version.AnchorEventSeq != 10 {
		t.Fatalf("first pin = %s", inFlight.Version)
	}

	second := mustParseConfig(t, "version = 1\n[[rows]]\nid = \"keel.shell#bash-local\"\nimpl = \"ssh\"\n")
	version, err := rotation.Rotate(second, 11)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if version.Revision != 2 || version.AnchorEventSeq != 11 || version.Hash != second.Hash() {
		t.Fatalf("rotated version = %s", version)
	}
	next := rotation.Pin()
	if next.Version.Revision != 2 {
		t.Fatalf("next run revision = %d, want 2", next.Version.Revision)
	}

	if got := inFlight.Config.Rows[0].Impl; got != "local" {
		t.Fatalf("in-flight run sees %q, want the pinned local implementation", got)
	}
	if got := next.Config.Rows[0].Impl; got != "ssh" {
		t.Fatalf("next run sees %q, want the rotated ssh implementation", got)
	}
	if inFlight.Version.Hash == next.Version.Hash {
		t.Fatal("the two runs must pin different config hashes")
	}

	previous, ok := rotation.Previous()
	if !ok || previous.Version.Revision != 1 || previous.Config.Rows[0].Impl != "local" {
		t.Fatalf("previous = %+v/%v", previous.Version, ok)
	}

	// An identical document still advances the revision: the rotation event is
	// the version fact, while the content hash stays the same.
	third, err := rotation.Rotate(first, 12)
	if err != nil {
		t.Fatalf("Rotate identical: %v", err)
	}
	if third.Revision != 3 || third.AnchorEventSeq != 12 {
		t.Fatalf("third version = %s", third)
	}
	if third.Hash != inFlight.Version.Hash {
		t.Fatalf("identical content hashes differ: %s != %s", third.Hash, inFlight.Version.Hash)
	}

	pin := rotation.Pin()
	pin.Config.Rows[0].Impl = "mutated"
	again := rotation.Pin()
	if again.Config.Rows[0].Impl != "local" {
		t.Fatalf("Pin returned a mutable view: %q", again.Config.Rows[0].Impl)
	}
}

func TestRotationRejectsInvalidAnchorsAndDocuments(t *testing.T) {
	config := mustParseConfig(t, "version = 1\n")
	if _, err := assembly.NewRotation(config, -1); !issueError(t, err).HasCode(assembly.IssueConfigAnchor) {
		t.Fatalf("negative anchor was not rejected: %v", err)
	}
	if _, err := assembly.NewRotation(assembly.Config{}, 1); !issueError(t, err).HasCode(assembly.IssueConfigVersion) {
		t.Fatalf("unversioned config was not rejected: %v", err)
	}
	rotation, err := assembly.NewRotation(config, 5)
	if err != nil {
		t.Fatalf("NewRotation: %v", err)
	}
	if _, err := rotation.Rotate(config, 5); !issueError(t, err).HasCode(assembly.IssueConfigAnchor) {
		t.Fatalf("a non-advancing anchor was not rejected: %v", err)
	}
	if _, err := rotation.Rotate(config, 4); !issueError(t, err).HasCode(assembly.IssueConfigAnchor) {
		t.Fatalf("a regressing anchor was not rejected: %v", err)
	}
	var nilRotation *assembly.Rotation
	if _, err := nilRotation.Rotate(config, 6); !issueError(t, err).HasCode(assembly.IssueConfigAnchor) {
		t.Fatalf("nil rotation was not rejected: %v", err)
	}
}

func TestConfigDocumentOverlaysTheBase(t *testing.T) {
	base := base(row("keel.shell#bash-local", "local", assembly.LayerHarness))
	config := mustParseConfig(t, `
version = 1
[[rows]]
id = "keel.shell#bash-local"
impl = "ssh"
params.host = "devbox"
`)
	built := mustBuild(t, base, config.Document())
	effective, ok := built.Row("keel.shell#bash-local")
	if !ok {
		t.Fatal("row missing")
	}
	if effective.Impl != "ssh" || effective.Params["host"] != "devbox" {
		t.Fatalf("effective = %+v", effective)
	}
	if effective.Source != assembly.StageConfig || !effective.Overridden {
		t.Fatalf("provenance = %s/%v", effective.Source, effective.Overridden)
	}
}
