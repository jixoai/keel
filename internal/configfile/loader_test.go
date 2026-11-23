package configfile_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jixoai/keel/internal/assembly"
	"github.com/jixoai/keel/internal/capturepolicy"
	"github.com/jixoai/keel/internal/configfile"
	"github.com/jixoai/keel/internal/host"
)

const (
	manualConfig = `version = 1

[filesystem.capture]
mode = "manual"

[[rows]]
id = "keel.shell#bash-local"
impl = "manual-impl"
`
	checkpointConfig = `version = 1

[filesystem.capture]
mode = "checkpoint"

[[rows]]
id = "keel.shell#bash-local"
impl = "checkpoint-impl"
`
)

func openLoader(t *testing.T, path string) *configfile.Loader {
	t.Helper()
	loader, err := configfile.Open(configfile.Options{Path: path})
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	return loader
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestOpenMissingFileIsAnEmptyOptionalConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), configfile.FileName)
	loader := openLoader(t, path)
	status := loader.Status()
	if status.Exists {
		t.Fatal("missing file reported as existing")
	}
	if status.Revision != 1 || !strings.HasPrefix(status.Hash, "sha256:") {
		t.Fatalf("status = %+v", status)
	}
	if status.CaptureMode != "" {
		t.Fatalf("missing file has capture mode %q", status.CaptureMode)
	}
	if _, ok := loader.Pin().Config.Settings.Get(assembly.SettingFilesystemCaptureMode); ok {
		t.Fatal("missing file declared a capture setting")
	}
}

func TestOpenRequiresAPath(t *testing.T) {
	if _, err := configfile.Open(configfile.Options{}); err == nil {
		t.Fatal("Open accepted an empty path")
	}
	if _, err := configfile.Open(configfile.Options{Path: "  "}); err == nil {
		t.Fatal("Open accepted a whitespace path")
	}
}

func TestOpenRejectsInvalidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), configfile.FileName)
	writeFile(t, path, "version = 1\n[filesystem.capture]\nmode = \"sometimes\"\n")
	if _, err := configfile.Open(configfile.Options{Path: path}); err == nil {
		t.Fatal("Open accepted an invalid capture mode")
	}
	// Fail closed: the file is still there and unchanged, nothing was adopted.
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "sometimes") {
		t.Fatalf("invalid file was modified: %q / %v", data, err)
	}
}

func TestRotateReplacesTheFileAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configfile.FileName)
	writeFile(t, path, manualConfig)
	loader := openLoader(t, path)

	before := loader.Pin()
	if got := before.Config.Rows[0].Impl; got != "manual-impl" {
		t.Fatalf("initial pin impl = %q", got)
	}

	fact, err := loader.Rotate([]byte(checkpointConfig), 9)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if fact.Revision != 2 || fact.AnchorEventSeq != 9 || fact.PreviousRevision != 1 {
		t.Fatalf("fact = %+v", fact)
	}
	if fact.Bytes != len(checkpointConfig) {
		t.Fatalf("fact bytes = %d, want %d", fact.Bytes, len(checkpointConfig))
	}
	if fact.Hash != loader.Pin().Version.Hash {
		t.Fatalf("fact hash = %s, pin hash = %s", fact.Hash, loader.Pin().Version.Hash)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read active file: %v", err)
	}
	if string(onDisk) != checkpointConfig {
		t.Fatalf("active file = %q, want the rotated document", onDisk)
	}
	if _, err := os.Stat(path + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging file left behind: %v", err)
	}

	status := loader.Status()
	if status.Rotations != 1 || status.LastRotation == nil {
		t.Fatalf("status = %+v", status)
	}
	if status.LastRotation.Hash != fact.Hash || status.CaptureMode != string(capturepolicy.CaptureModeCheckpoint) {
		t.Fatalf("status = %+v", status)
	}

	// A run that pinned before the rotation keeps both content and version.
	if got := before.Config.Rows[0].Impl; got != "manual-impl" {
		t.Fatalf("in-flight pin changed to %q", got)
	}
	if before.Version.Revision != 1 || before.Version.Hash == loader.Pin().Version.Hash {
		t.Fatalf("in-flight version = %+v, current = %+v", before.Version, loader.Pin().Version)
	}
}

// TestRotationKeepsInFlightAttemptOnItsPinnedVersion rotates while an attempt
// is in flight and asserts the pinned policy cannot move.
func TestRotationKeepsInFlightAttemptOnItsPinnedVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), configfile.FileName)
	writeFile(t, path, manualConfig)
	loader := openLoader(t, path)

	mode, ok, err := loader.CaptureMode()
	if err != nil || !ok || mode != capturepolicy.CaptureModeManual {
		t.Fatalf("initial mode = %q/%v/%v", mode, ok, err)
	}
	if _, err := loader.Rotate([]byte(checkpointConfig), 1); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	pin := loader.Pin()
	if mode, ok, err := configfile.CaptureModeOf(pin.Config); err != nil || !ok || mode != capturepolicy.CaptureModeCheckpoint {
		t.Fatalf("next pin mode = %q/%v/%v", mode, ok, err)
	}
}

func TestRotateRejectsInvalidDocumentsWithoutTouchingTheActiveFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configfile.FileName)
	writeFile(t, path, manualConfig)
	loader := openLoader(t, path)
	before := loader.Status()

	invalid := []string{
		"version = 1\n[filesystem.capture]\nmode = \"sometimes\"\n",
		"version = 1\n[unknown]\nkey = \"x\"\n",
		"version = 1\n[[rows]]\nid = \"keel.a#row\"\nimpl = \"v\"\nparams.x = [1]\n",
		"not = \"toml\"\n",
	}
	for _, document := range invalid {
		if _, err := loader.Rotate([]byte(document), 3); err == nil {
			t.Fatalf("Rotate accepted invalid document:\n%s", document)
		}
		onDisk, err := os.ReadFile(path)
		if err != nil || string(onDisk) != manualConfig {
			t.Fatalf("active file changed after rejected rotation: %q / %v", onDisk, err)
		}
		if _, err := os.Stat(path + ".new"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("staging file left after rejected rotation: %v", err)
		}
		if got := loader.Status(); got.Revision != before.Revision || got.Hash != before.Hash {
			t.Fatalf("version advanced after rejection: %+v -> %+v", before, got)
		}
	}
}

func TestRotateRejectsAnAnchorThatDoesNotAdvance(t *testing.T) {
	path := filepath.Join(t.TempDir(), configfile.FileName)
	writeFile(t, path, manualConfig)
	loader := openLoader(t, path)
	if _, err := loader.Rotate([]byte(checkpointConfig), 5); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, anchor := range []int64{5, 4, -1} {
		if _, err := loader.Rotate([]byte(manualConfig), anchor); err == nil {
			t.Fatalf("Rotate accepted anchor %d", anchor)
		}
		onDisk, err := os.ReadFile(path)
		if err != nil || string(onDisk) != string(current) {
			t.Fatalf("file changed after anchor rejection: %q / %v", onDisk, err)
		}
	}
}

// TestRotationWriteFailureLeavesNoPartialFile blocks the staging path with a
// directory: the write fails before rename, the active file stays byte-identical
// and the version does not advance.
func TestRotationWriteFailureLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configfile.FileName)
	writeFile(t, path, manualConfig)
	loader := openLoader(t, path)
	before := loader.Status()

	staging := path + ".new"
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatalf("block staging path: %v", err)
	}
	if _, err := loader.Rotate([]byte(checkpointConfig), 1); err == nil {
		t.Fatal("Rotate succeeded with a blocked staging path")
	}
	onDisk, err := os.ReadFile(path)
	if err != nil || string(onDisk) != manualConfig {
		t.Fatalf("active file changed after write failure: %q / %v", onDisk, err)
	}
	after := loader.Status()
	if after.Revision != before.Revision || after.Hash != before.Hash || after.Rotations != 0 {
		t.Fatalf("version advanced after write failure: %+v -> %+v", before, after)
	}
}

// TestRotateOverwritesAStaleStagingFile is the crash-leftover case: a partial
// config.toml.new never becomes visible and a later valid rotation replaces it
// completely.
func TestRotateOverwritesAStaleStagingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configfile.FileName)
	writeFile(t, path, manualConfig)
	loader := openLoader(t, path)
	writeFile(t, path+".new", "version = 1\n[[rows]]\nid = \"broken")

	if _, err := loader.Rotate([]byte(checkpointConfig), 1); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil || string(onDisk) != checkpointConfig {
		t.Fatalf("active file = %q / %v", onDisk, err)
	}
	if _, err := os.Stat(path + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale staging file survived: %v", err)
	}
}

func TestConfigFileCaptureModeOverlaysTheTenantDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), configfile.FileName)
	writeFile(t, path, checkpointConfig)
	loader := openLoader(t, path)

	// The stored/admin document says manual; the file's checkpoint is the
	// outermost user layer and wins for the key it declares.
	stored := host.TenantConfig{Filesystem: host.FilesystemConfig{Capture: host.CaptureConfig{Mode: "manual"}}}
	overlaid, applied, err := configfile.OverlayTenant(loader.Pin(), stored)
	if err != nil {
		t.Fatalf("OverlayTenant: %v", err)
	}
	if !applied || overlaid.Filesystem.Capture.Mode != "checkpoint" {
		t.Fatalf("overlaid = %+v applied=%v", overlaid, applied)
	}
	if stored.Filesystem.Capture.Mode != "manual" {
		t.Fatal("OverlayTenant mutated its input")
	}
	if got := overlaid.Filesystem.Capture.EffectiveMode(); got != capturepolicy.CaptureModeCheckpoint {
		t.Fatalf("effective mode = %q", got)
	}

	// A file without the setting leaves the tenant document untouched, so the
	// admin JSON path keeps governing capture.
	plain := filepath.Join(t.TempDir(), configfile.FileName)
	writeFile(t, plain, "version = 1\n")
	plainLoader := openLoader(t, plain)
	overlaid, applied, err = configfile.OverlayTenant(plainLoader.Pin(), stored)
	if err != nil || applied || overlaid.Filesystem.Capture.Mode != "manual" {
		t.Fatalf("plain overlay = %+v applied=%v err=%v", overlaid, applied, err)
	}
}

func TestRestartPicksTheRotatedVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configfile.FileName)
	writeFile(t, path, manualConfig)
	first := openLoader(t, path)
	fact, err := first.Rotate([]byte(checkpointConfig), 4)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	restarted := openLoader(t, path)
	status := restarted.Status()
	if !status.Exists || status.Hash != fact.Hash {
		t.Fatalf("restart status = %+v, rotated hash = %s", status, fact.Hash)
	}
	if status.CaptureMode != string(capturepolicy.CaptureModeCheckpoint) {
		t.Fatalf("restart capture mode = %q", status.CaptureMode)
	}
	if got := restarted.Pin().Config.Rows[0].Impl; got != "checkpoint-impl" {
		t.Fatalf("restart pin impl = %q", got)
	}
}

func TestReloadAdoptsAnExternallyRotatedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configfile.FileName)
	writeFile(t, path, manualConfig)
	loader := openLoader(t, path)

	replacement := filepath.Join(dir, "replacement")
	writeFile(t, replacement, checkpointConfig)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("external rotate: %v", err)
	}
	fact, err := loader.Reload(2)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if fact.Revision != 2 || fact.Hash != loader.Pin().Version.Hash {
		t.Fatalf("reload fact = %+v", fact)
	}
	if mode, ok, err := loader.CaptureMode(); err != nil || !ok || mode != capturepolicy.CaptureModeCheckpoint {
		t.Fatalf("reloaded mode = %q/%v/%v", mode, ok, err)
	}
	if _, err := loader.Reload(2); err == nil {
		t.Fatal("Reload accepted a non-advancing anchor")
	}
}

// TestConcurrentPinAndRotate exercises the lock: every pin is internally
// consistent (hash belongs to the version's content) while rotations replace
// the file.
func TestConcurrentPinAndRotate(t *testing.T) {
	path := filepath.Join(t.TempDir(), configfile.FileName)
	writeFile(t, path, manualConfig)
	loader := openLoader(t, path)

	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		for anchor := int64(1); anchor <= 20; anchor++ {
			document := manualConfig
			if anchor%2 == 0 {
				document = checkpointConfig
			}
			if _, err := loader.Rotate([]byte(document), anchor); err != nil {
				t.Errorf("Rotate(%d): %v", anchor, err)
				return
			}
		}
	}()
	for i := 0; i < 200; i++ {
		pin := loader.Pin()
		if pin.Hash() != pin.Config.Hash() {
			t.Errorf("pin hash %s does not match its content %s", pin.Hash(), pin.Config.Hash())
			return
		}
	}
	wait.Wait()
}
