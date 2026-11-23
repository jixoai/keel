package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/bashbridge"
	"github.com/jixoai/keel/internal/capturepolicy"
	"github.com/jixoai/keel/internal/configfile"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

type fakeTenantConfigReader struct {
	record store.TenantConfigRecord
	err    error
}

func (f fakeTenantConfigReader) GetTenantConfig(context.Context, uuid.UUID) (store.TenantConfigRecord, error) {
	return f.record, f.err
}

func TestEffectiveCaptureModeUsesStoredDocumentOverBoot(t *testing.T) {
	scope := kernel.Scope{TenantID: uuid.New()}
	defaults := host.TenantConfig{}
	defaults.Filesystem.Capture.Mode = "per-tool-call"

	reader := fakeTenantConfigReader{record: store.TenantConfigRecord{
		Config:   json.RawMessage(`{"filesystem":{"capture":{"mode":"checkpoint"}}}`),
		Revision: 7,
	}}
	mode, source, err := effectiveCaptureMode(context.Background(), reader, defaults, nil, scope)
	if err != nil {
		t.Fatalf("effectiveCaptureMode: %v", err)
	}
	if mode != capturepolicy.CaptureModeCheckpoint {
		t.Fatalf("mode = %q, want checkpoint", mode)
	}
	if source != "stored-document@7" {
		t.Fatalf("source = %q, want the stored revision", source)
	}

	// A tenant without a stored document runs on the boot document.
	mode, source, err = effectiveCaptureMode(context.Background(),
		fakeTenantConfigReader{err: store.ErrNoTenant}, defaults, nil, scope)
	if err != nil {
		t.Fatalf("effectiveCaptureMode(no tenant): %v", err)
	}
	if mode != capturepolicy.CaptureModePerToolCall || source != "boot-default" {
		t.Fatalf("mode/source = %q/%q, want per-tool-call from boot-default", mode, source)
	}

	// No reader at all is the same boot default, not an invented stored source.
	mode, source, err = effectiveCaptureMode(context.Background(), nil, defaults, nil, scope)
	if err != nil {
		t.Fatalf("effectiveCaptureMode(nil reader): %v", err)
	}
	if mode != capturepolicy.CaptureModePerToolCall || source != "boot-default" {
		t.Fatalf("mode/source = %q/%q, want per-tool-call from boot-default", mode, source)
	}
}

func TestEffectiveCaptureModeFailsClosedOnBadDocumentOrRead(t *testing.T) {
	scope := kernel.Scope{TenantID: uuid.New()}
	for name, reader := range map[string]fakeTenantConfigReader{
		"parse": {record: store.TenantConfigRecord{Config: json.RawMessage(`{"filesystem":`)}},
		"read":  {err: errors.New("database unavailable")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := effectiveCaptureMode(context.Background(), reader, host.TenantConfig{}, nil, scope); err == nil {
				t.Fatal("an unreadable or unparsable policy must fail closed")
			}
		})
	}
}

// TestEffectiveCaptureModeAppliesConfigFileLast pins the documented precedence:
// boot defaults < stored admin document < config.toml, and a rotation moves the
// next attempt while the previous pin is untouched.
func TestEffectiveCaptureModeAppliesConfigFileLast(t *testing.T) {
	scope := kernel.Scope{TenantID: uuid.New()}
	defaults := host.TenantConfig{}
	defaults.Filesystem.Capture.Mode = "per-tool-call"
	stored := fakeTenantConfigReader{record: store.TenantConfigRecord{
		Config:   json.RawMessage(`{"filesystem":{"capture":{"mode":"checkpoint"}}}`),
		Revision: 7,
	}}

	path := filepath.Join(t.TempDir(), configfile.FileName)
	if err := os.WriteFile(path, []byte("version = 1\n[filesystem.capture]\nmode = \"manual\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loader, err := configfile.Open(configfile.Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mode, source, err := effectiveCaptureMode(context.Background(), stored, defaults, loader, scope)
	if err != nil {
		t.Fatalf("effectiveCaptureMode: %v", err)
	}
	if mode != capturepolicy.CaptureModeManual || source != "config.toml@revision=1" {
		t.Fatalf("mode/source = %q/%q, want manual from config.toml", mode, source)
	}
	pinned := loader.Pin()
	if _, err := loader.Rotate([]byte("version = 1\n[filesystem.capture]\nmode = \"checkpoint\"\n"), 3); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if mode, _, err := configfile.CaptureModeOf(pinned.Config); err != nil || mode != capturepolicy.CaptureModeManual {
		t.Fatalf("in-flight pin mode = %q/%v, want manual", mode, err)
	}
	mode, source, err = effectiveCaptureMode(context.Background(), stored, defaults, loader, scope)
	if err != nil {
		t.Fatalf("effectiveCaptureMode after rotation: %v", err)
	}
	if mode != capturepolicy.CaptureModeCheckpoint || source != "config.toml@revision=2" {
		t.Fatalf("mode/source after rotation = %q/%q, want checkpoint@2", mode, source)
	}

	// A file without the setting leaves the stored admin document in charge.
	plainPath := filepath.Join(t.TempDir(), configfile.FileName)
	if err := os.WriteFile(plainPath, []byte("version = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plain, err := configfile.Open(configfile.Options{Path: plainPath})
	if err != nil {
		t.Fatalf("Open plain: %v", err)
	}
	mode, source, err = effectiveCaptureMode(context.Background(), stored, defaults, plain, scope)
	if err != nil {
		t.Fatalf("effectiveCaptureMode plain: %v", err)
	}
	if mode != capturepolicy.CaptureModeCheckpoint || source != "stored-document@7" {
		t.Fatalf("plain mode/source = %q/%q, want the stored document", mode, source)
	}
}

// TestEffectiveCaptureModeFallsBackWhenFileSettingIsRemoved is the P1 guard: a
// rotation that removes filesystem.capture.mode must not leave the old file
// value stuck in the resolver's boot defaults. The serve wiring passes the
// PRE-overlay tenant document as defaults; these cases use exactly that shape.
func TestEffectiveCaptureModeFallsBackWhenFileSettingIsRemoved(t *testing.T) {
	scope := kernel.Scope{TenantID: uuid.New()}
	defaults := host.TenantConfig{}
	defaults.Filesystem.Capture.Mode = "per-tool-call"

	path := filepath.Join(t.TempDir(), configfile.FileName)
	if err := os.WriteFile(path, []byte("version = 1\n[filesystem.capture]\nmode = \"manual\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loader, err := configfile.Open(configfile.Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if mode, _, err := effectiveCaptureMode(context.Background(), nil, defaults, loader, scope); err != nil || mode != capturepolicy.CaptureModeManual {
		t.Fatalf("initial file mode = %q/%v, want manual", mode, err)
	}
	if _, err := loader.Rotate([]byte("version = 1\n"), 1); err != nil {
		t.Fatalf("Rotate (remove setting): %v", err)
	}

	// No stored document: the removed setting falls back to the boot default.
	mode, source, err := effectiveCaptureMode(context.Background(), nil, defaults, loader, scope)
	if err != nil {
		t.Fatalf("effectiveCaptureMode(no reader): %v", err)
	}
	if mode != capturepolicy.CaptureModePerToolCall || source != "boot-default" {
		t.Fatalf("no-doc fallback = %q/%q, want per-tool-call from boot-default", mode, source)
	}
	if mode, source, err := effectiveCaptureMode(context.Background(),
		fakeTenantConfigReader{err: store.ErrNoTenant}, defaults, loader, scope); err != nil ||
		mode != capturepolicy.CaptureModePerToolCall || source != "boot-default" {
		t.Fatalf("missing-tenant fallback = %q/%q/%v, want per-tool-call from boot-default", mode, source, err)
	}

	// A silent stored document keeps the boot default, but the source names the
	// document revision that was read.
	silent := fakeTenantConfigReader{record: store.TenantConfigRecord{
		Config: json.RawMessage(`{}`), Revision: 8,
	}}
	mode, source, err = effectiveCaptureMode(context.Background(), silent, defaults, loader, scope)
	if err != nil {
		t.Fatalf("effectiveCaptureMode(silent): %v", err)
	}
	if mode != capturepolicy.CaptureModePerToolCall || source != "stored-document@8" {
		t.Fatalf("silent-doc fallback = %q/%q, want per-tool-call from stored-document@8", mode, source)
	}

	// File removed but the stored document declares checkpoint: the stored
	// document is the next layer, not the removed file value.
	stored := fakeTenantConfigReader{record: store.TenantConfigRecord{
		Config:   json.RawMessage(`{"filesystem":{"capture":{"mode":"checkpoint"}}}`),
		Revision: 7,
	}}
	mode, source, err = effectiveCaptureMode(context.Background(), stored, defaults, loader, scope)
	if err != nil {
		t.Fatalf("effectiveCaptureMode(stored): %v", err)
	}
	if mode != capturepolicy.CaptureModeCheckpoint || source != "stored-document@7" {
		t.Fatalf("stored fallback = %q/%q, want checkpoint from stored-document@7", mode, source)
	}
}

func TestCheckpointDocumentRequiresAnAmbientRun(t *testing.T) {
	if _, err := checkpointDocument(context.Background(), nil); err == nil ||
		!strings.Contains(err.Error(), "no checkpoint surface") {
		t.Fatalf("nil workspace error = %v, want a typed missing-surface refusal", err)
	}
	// A context without a session identity must not settle an arbitrary session.
	if _, err := checkpointDocument(context.Background(), &bashbridge.LayerWorkspaces{}); err == nil {
		t.Fatal("checkpoint outside a run must be refused")
	}
}
