W2.3 layerfs (Go CAS Phase A) + file-plugin — evidence

Delivered
- internal/layerfs: content-addressed file objects and canonical tree objects
  over the blob CAS layout (blob.CASPath), streaming Put/ReadRange/Materialize,
  event-derived refcounts with an append-only journal, GC, capture with stat
  cache and the default .git/objects ignore.
- internal/layerfs/gbashfs: gbash FileSystem adapter over layerfs. It refuses
  the root and the sandbox layout paths, mounts at /workspace by default, and
  isolates /bin, /usr/bin, /tmp to gbash's memory FS.
- internal/fileplugin: session-scoped file_manifest (memory and PostgreSQL),
  read paging, write/edit with content-hash preconditions and unique-match edit,
  one fs.write per explicit write, one fs.capture per batch, fs.capture_failed on
  failure, materialize from manifest/snapshot, DirSurface write-through.
- internal/toolsurface: real read/write/edit; Backends.Files; typed
  conflict/quota/io/unavailable/plugin_disabled errors.
- keel.file: internal plugin backend (keel.file.read/write/edit/capture) and
  builtin manifest. Disabling it keeps the eleven-tool surface and returns
  plugin_disabled.
- cmd/keel/serve.go: file service construction and backend/factory registration.

Gates (transcripts in this directory)
- go-build.txt: go build ./...
- go-vet.txt: go vet on the changed packages
- test-race.txt: go test -race on layerfs, gbashfs, fileplugin, toolsurface, agentext
- test-scale.txt: 100,000-file capture -> exactly one fs.capture event
- test-gbash.txt: gbash writes/reads through the non-root layerfs mount
- test-write-through.txt: paging, write-through, edit uniqueness, materialize
- test-fs-write-order.txt: tool.result then fs.write key separation, capture batch/failure
- test-full-dsn.txt: go test ./... with KEEL_TEST_DSN (zero FAIL)
- agent-runtime-e2e.txt: scripts/agent-runtime-e2e.sh PASS
- agent-runtime-e2e/: the script's raw artifacts

Rerun
  scripts/layerfs-evidence.sh
  export KEEL_TEST_DSN="$(grep '^KEEL_TEST_DSN_W2_LAYERFS=' .env.local | cut -d= -f2-)"
  go test ./...
  ./scripts/agent-runtime-e2e.sh

Known integration items (not in this slice)
- file_manifest and session_snapshot DDL is not in internal/store/schema.sql
  (owned by another lane). fileplugin.SchemaDDL carries the exact DDL; add both
  tables to the RLS table list in internal/store/rls.go.
- W2.2 must pass the session's gbash workspace as fileplugin.Surface and its
  directory as CaptureRoot; this slice tests that seam but does not edit W2.2.
- Fence precision: the file plugin commits fs.write with the session's current
  live lease token (kernel.LeaseState). In the normal in-process run this is the
  run's own token. To close the stale-attempt window completely, thread the
  claimed OwnerToken into the tool context (add Token to agentext.RunScope,
  pass lease.Token through RunControl.BeginRun/the worker, and prefer it over
  LeaseState in fileplugin.liveToken). Those files belong to the runtime lane.
- Capture is upsert-only (the spec's wording); manifest rows for files deleted
  from the workspace are not removed yet. A reconcile pass should delete paths
  absent from the snapshot.
- Quota is a store-wide byte cap in Phase A; per-tenant enforcement belongs to
  the layerfs service (ADR 0001 / W1).
- last_task_id stays empty until task-log supplies the current task id to the
  file plugin.
- Absolute skill paths are not materialized into the session manifest yet.
- Phase A CAS is local disk; ADR 0001's Rust/gRPC service can replace it behind
  the same object model.
