W3 capture tail (delete reconciliation + concurrent ordering) — evidence
Lane: H3B, worktree .worktrees/keel-w3-capture, base main 0f22064
Date: 2026-11-21

Commits
- b0d4998 feat(fileplugin): reconcile deleted paths in capture settlements
- 1b88784 feat(fileplugin): order capture settlements per session
- c96051d refactor(fileplugin): make the execution space the deletion truth
- a8c1181 docs(keel-harness-v2): W3.H3B capture delete/ordering evidence + mode blocker
- 8a6415b docs(fileplugin): snapshotVersion is zero on replay

1. Deletion reconciliation (done)
- internal/fileplugin/service.go:588-705: one capture now lists the manifest,
  diffs it against the execution-space snapshot, commits exactly one fs.capture
  (payload keeps count/treeHash plus deleted/deletedCount and optional
  deletedTruncated), upserts the snapshot rows, then deletes the disappeared
  paths. The event document's deleted list is bounded by both
  CaptureDeletedPayloadLimit = 256 and CaptureDeletedPayloadBytes = 32 KiB
  (service.go:539-553); deletedCount reports the full number. The manifest delete
  (DeleteStale) is guarded with updated_seq < event seq, so a row a concurrent
  newer settlement refreshed survives (internal/fileplugin/manifest.go:24-27,
  259-284; memory guard manifest.go:101-114).
- The execution space is the capture truth: any manifest path absent from the
  snapshot is removed, including a write-tool file removed by bash. The fs.write
  event and its blob reference stay in the ledger; only the current projection
  follows the workspace.
- Multiple deleted paths do not create per-file events: one fs.capture per
  settlement, unchanged. count/treeHash always come from the same snapshot the
  manifest is reconciled to, and the manifest delete runs after the event commit
  under the same settlement critical section.
- Historical replay: old fs.capture events/session snapshots are untouched and
  still materialize deleted files by seq (tests below).

2. Concurrent capture ordering (done)
- internal/fileplugin/service.go:791-812: every (tenant, session) has one
  settlement critical section. Writes/edits (service.go:366-369, 500-503) and
  captures (service.go:564-575) hold it. A capture's replay lookup, snapshot,
  fs.capture commit, manifest upsert/delete, reference retain and
  session_snapshot row are one atomic settlement, and each settlement records a
  monotonic snapshotVersion (payload + CaptureResult).
- Consequence: a later-committing capture can never carry an older full
  snapshot. If a nohup child Close commits first, the parent run Close snapshots
  after the child settlement and its later full snapshot includes the child's
  files. If the parent closes first, the child settles later and its snapshot is
  the newest. Either way deletion reconciliation cannot erase the other lane's
  writes; the last writer is always the newest view.
- updated_seq remains the event-seq attribution and the upsert/delete guard.
  The explicit version is per Service/process, which matches the in-memory
  per-session execution space the provider owns (one owning process per
  session). A future multi-process capture surface would need a durable version.

3. capture mode override (blocked, task stays unchecked)
- No config.toml loader exists in this tree. tenants.config is written/validated
  by the admin API (internal/httpapi/admin.go:98,122) and read live only by the
  quota fragment (internal/quota/limits.go:250). cmd/keel/serve.go:422 builds the
  host tenant document from env; the stored document is not merged into the
  runtime (models have a separate override path).
- Required field: tenant document fs.capture.mode = per-tool-call|checkpoint|
  manual, plus a section in internal/host/config.go:37 TenantConfig with
  Merge/Validate.
- Required entry points: capture decision at
  internal/bashbridge/layerworkspace.go:272 (Mount.Finish at :175); an explicit
  checkpoint caller (Service.CaptureWorkspace exists at service.go:465 but only
  LayerWorkspaces.capture calls it); task completion
  (internal/tasklog/service.go:268 Close has no callback); subagent return
  (internal/agentext/subagent.go:418-455 emits a command hook only); manual mode
  needs an explicit attach/checkpoint surface that does not exist.
- Landing only the field and skipping Close capture would silently drop
  settlements until a trigger exists, so the task is kept unchecked with this
  blocker recorded.

Tests (see the .txt files in this directory)
- go test -p 1 -count=1 ./internal/fileplugin -run
  'TestCaptureReconcilesDeletions|TestCaptureDeletesWrittenThenRemoved|
  TestCaptureDeletedPayloadIsBounded|TestCaptureDeletedPayloadByteBound|
  TestCaptureSettlementsAreSerialized|TestPostgresManifestDeleteStaleGuard'
  — PASS (DSN set).
- go test -p 1 -count=1 ./internal/bashbridge -run
  'TestBashRmIsReconciledByCapture|TestParentCloseAndJobCloseOrderingKeepsJobWrites|
  TestJobSettlesBeforeParentKeepsJobWrites|TestBashWriteCapturedIntoManifestAndTree|
  TestBackgroundJobCaptureSettlesWithoutRunLease' — PASS.
- go test -p 1 -count=1 (fileplugin, bashbridge, layerfs, kernel, store,
  agentext, toolsurface, servicehost) — PASS (only KEEL_TEST_DSN exported from
  .env.local; the serve password vars would change cmd/keel's own test servers).
- go test -p 1 -count=1 ./cmd/keel/... — PASS in a clean shell.
- go test -race -p 1 -count=1 ./internal/fileplugin/... ./internal/bashbridge/...
  — PASS.
- gofmt -l internal/fileplugin internal/bashbridge internal/store — empty.
- go build / go vet (changed packages + cmd/keel) — exit 0.
- scripts/conformance.sh — first run had one flaky failure in internal/agent
  TestCheckpointCutInMovedUnreadInputFailSafe (checkpoint found=false); the test
  passed in isolation and the full rerun was 1089 pass / 0 fail / 1 skip.
  conformance-summary.txt and conformance-summary-rerun.txt are both kept.
- openspec validate keel-harness-v2 --strict — valid.

Known limits / residuals
- Ordering is per Service/process (see above); it covers the production
  single-process workspace and the parent-run/nohup-job pair that share it.
- A cross-process settlement could compute a deleted list the guarded
  DeleteStale then declines to apply; the payload is the settlement's observed
  diff, the manifest stays newer. Not reachable with today's process-local
  workspace.
- A crash between the fs.capture commit and the manifest reconciliation is not
  repaired by a replay (lookupCapture returns the replay); the next capture
  diffs the manifest against the workspace and repairs stale rows.
