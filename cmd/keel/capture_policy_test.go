package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/bashbridge"
	"github.com/jixoai/keel/internal/capturepolicy"
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
	mode, source, err := effectiveCaptureMode(context.Background(), reader, defaults, scope)
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
		fakeTenantConfigReader{err: store.ErrNoTenant}, defaults, scope)
	if err != nil {
		t.Fatalf("effectiveCaptureMode(no tenant): %v", err)
	}
	if mode != capturepolicy.CaptureModePerToolCall || source != "boot-default" {
		t.Fatalf("mode/source = %q/%q, want per-tool-call from boot-default", mode, source)
	}

	// No reader at all is the same boot default, not an invented stored source.
	mode, source, err = effectiveCaptureMode(context.Background(), nil, defaults, scope)
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
			if _, _, err := effectiveCaptureMode(context.Background(), reader, host.TenantConfig{}, scope); err == nil {
				t.Fatal("an unreadable or unparsable policy must fail closed")
			}
		})
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
