// The run-path capture policy adapter (change keel-harness-v2, W3 capture
// policy override).
//
// The tenant document is the single configuration vocabulary: the admin API
// stores `filesystem.capture.mode` on the tenant row, and the run path reads it
// once per attempt. The resolver is deliberately per attempt, not per boot or
// per tool call: a configuration rotation applies to the next run, and the
// worker pins the resolved mode on that attempt's tool context so an in-flight
// run can never switch policy mid-flight.
//
// The boot-time tenant document is the fallback (it carries deployment
// defaults). A stored document overlays it, so quota-style tenant writes and the
// capture policy share the same precedence and the same validation.

package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/bashbridge"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/layerfs"
	"github.com/jixoai/keel/internal/store"
)

// tenantConfigReader is the narrow store seam the capture resolver needs: the
// admin-written tenant document and its revision. *store.Store satisfies it.
type tenantConfigReader interface {
	GetTenantConfig(ctx context.Context, tenantID uuid.UUID) (store.TenantConfigRecord, error)
}

// newRunCaptureResolver builds the runner's per-attempt capture policy source.
// documents may be nil (no stored-document seam): the boot tenant document is
// then the only source. workspace is the execution space whose explicit
// checkpoint is the manual/checkpoint trigger; nil keeps the modes fail-closed
// at policy validation instead of pretending a trigger exists.
func newRunCaptureResolver(documents tenantConfigReader, defaults host.TenantConfig,
	workspace *bashbridge.LayerWorkspaces) func(context.Context, kernel.Scope) (agent.RunCapture, error) {
	return func(ctx context.Context, scope kernel.Scope) (agent.RunCapture, error) {
		mode, source, err := effectiveCaptureMode(ctx, documents, defaults, scope)
		if err != nil {
			return agent.RunCapture{}, err
		}
		capture := agent.RunCapture{Mode: mode, Source: source}
		if mode == layerfs.CaptureModeCheckpoint || mode == layerfs.CaptureModeManual {
			if workspace != nil {
				capture.Checkpoint = func(ctx context.Context, scope kernel.Scope, callID string) error {
					_, _, err := workspace.Checkpoint(ctx, bashbridge.SessionIdentity{
						TenantID: scope.TenantID, SessionID: scope.SessionID, CallID: callID,
					})
					return err
				}
			}
		}
		return capture, nil
	}
}

// effectiveCaptureMode merges the stored tenant document over the boot
// document and returns the capture mode. A read or parse failure is returned to
// the runner, which fails the attempt before any tool executes: running under
// an unknown policy is worse than not running.
func effectiveCaptureMode(ctx context.Context, documents tenantConfigReader,
	defaults host.TenantConfig, scope kernel.Scope) (layerfs.CaptureMode, string, error) {
	mode := defaults.Filesystem.Capture.EffectiveMode()
	if documents == nil {
		return mode, "boot-default", nil
	}
	record, err := documents.GetTenantConfig(ctx, scope.TenantID)
	switch {
	case err == nil:
		document, parseErr := host.ParseTenantConfig(record.Config)
		if parseErr != nil {
			return "", "", fmt.Errorf("stored tenant config: %w", parseErr)
		}
		merged := host.Merge(defaults, document)
		return merged.Filesystem.Capture.EffectiveMode(),
			fmt.Sprintf("stored-document@%d", record.Revision), nil
	case errors.Is(err, store.ErrNoTenant):
		// A tenant without a stored document runs on the boot defaults.
		return mode, "boot-default", nil
	default:
		return "", "", fmt.Errorf("read stored tenant config: %w", err)
	}
}

// checkpointDocument is the model-visible bash {action:"checkpoint"} surface.
// The bash backend wrapper has stamped the ambient session identity (the same
// one the run's shell executions use), so the checkpoint settles with the right
// fence, attribution and idempotency key.
func checkpointDocument(ctx context.Context, workspace *bashbridge.LayerWorkspaces) (map[string]any, error) {
	if workspace == nil {
		return nil, errors.New("the execution workspace is not wired; no checkpoint surface exists")
	}
	identity, ok := bashbridge.SessionIdentityFromContext(ctx)
	if !ok || !identity.Valid() {
		return nil, errors.New("checkpoint needs the ambient run session; call it from a tool call inside a run")
	}
	result, captured, err := workspace.Checkpoint(ctx, identity)
	if err != nil {
		return nil, err
	}
	document := map[string]any{"captured": captured}
	if identity.TaskID != "" {
		document["taskId"] = identity.TaskID
	}
	if captured {
		document["treeHash"] = result.Tree
		document["count"] = result.Count
		document["seq"] = result.Seq
		document["baseSeq"] = result.BaseSeq
		document["snapshotVersion"] = result.SnapshotVersion
		document["replayed"] = result.Replayed
	}
	return document, nil
}
