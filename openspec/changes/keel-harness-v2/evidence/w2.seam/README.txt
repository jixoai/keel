W2 seam wiring (layerfs workspace + file-plugin + bash jobs) — evidence

Delivered
- internal/bashbridge/layerworkspace.go: the production Workspace provider.
  One gbashfs workspace per (tenant, session), seeded from the session
  file_manifest on first use, mounted at /workspace. The same object is the
  file plugin's session surface (WriteFileFor) and capture snapshot source, so
  read/write/edit and bash act on one bytes view.
- internal/bashbridge/workspace.go + bridge.go: Mount.Finish. Session.Close
  settles the run's changes (only when a command actually executed) before
  releasing mount resources.
- internal/layerfs/gbashfs/capture.go: Workspace.Capture builds the layerfs
  snapshot (file entries + canonical tree objects, default .git/objects ignore,
  symlinks skipped) with the same tree form as layerfs.Store.Capture. No host
  directory walk: bash writes enter the CAS at file close.
- internal/fileplugin: SessionSurface and SnapshotSource seams. Write-through
  and capture resolve per session; the directory Surface/CaptureRoot path stays
  for the local adapter. CaptureWorkspace still commits exactly one fs.capture
  (or one fs.capture_failed on failure).
- cmd/keel/serve.go: file store -> workspace provider -> JobManager bridge and
  file service all share the provider. bashSessionBackend stamps the run
  identity (tenant/session, task-gate-stamped taskId, tool-call id) on the
  bash tool context. newFileService/newFileStore split out of the old helper.
- P1-2 sandbox job commands (internal/bashbridge/jobcommands.go):
  `jobs` (session-scoped view), `ps` (tenant-wide view) and `nohup` (start a
  background job and return its keel job id). All read jobqueue.Manager through
  the JobBackend seam; `jobs` overrides upstream's unimplemented builtin; fg/bg
  stay unregistered. cmd/keel/serve.go adapts jobqueue.Manager and injects the
  commands through bashbridge.Config.Commands.
- Small items: deleted dead toolsurface.singleTool; corrected the stale
  "until layerfs lands" tasklog comment to "the production seam is not wired
  yet".

Semantics notes
- Capture key: <toolCallId>:0 (fileplugin contract). Each bash tool run
  settles once on Session.Close. nohup gives its child run a unique
  "<parentCallId>:nohup:<uuid>" identity so the two fs.capture events cannot
  dedupe.
- Attribution: the task gate already stamps taskId into the bash call
  arguments; serve forwards that same value into the capture event. No second
  attribution path is introduced. Capture still assigns that task id to every
  file in the batch snapshot (inherited W2.3 upsert semantics).
- Tenant/session isolation: workspace keys are (tenant, session); a second
  session cannot read the first session's files (TestSessionsGetIndependentWorkspaces).
- Sandbox layout isolation: /bin, /usr/bin, /tmp stay on gbash's memory mounts;
  the layerfs workspace capture sees only business files (tests assert count
  and tree).
- Timeout marker: jobqueue persists status/reason/live/exit code, not the
  per-call TimedOut flag, so `jobs`/`ps` render the persisted facts the
  `bash {action:"jobs"}` tool reads (plus live) and mark the whole view as
  background jobs. No host pid is ever rendered.

Gates (transcripts in this directory)
- go-build.txt: go build ./...
- go-vet.txt: go vet on internal/bashbridge, internal/layerfs/gbashfs,
  internal/fileplugin, internal/toolsurface, internal/tasklog, cmd/keel
- test-race.txt: go test -race on the changed packages
- test-seam.txt: production-wiring tests (write->bash read; bash write->
  capture->manifest/tree; manifest seeding; session isolation; capture_failed)
- test-gbashfs-capture.txt: workspace capture tree hash equals directory
  capture; empty workspace settles a valid tree
- test-job-commands.txt: jobs/ps/nohup bridge tests (no 127, keel job ids, no
  host pid, fg/bg stay 127)
- test-serve-wrapper.txt: run-identity stamping + job adapter unit mapping
- test-job-status-consistency.txt: real jobqueue.Manager vs sandbox adapter
  facts (KEEL_TEST_DSN)
- test-full-dsn.txt: KEEL_TEST_DSN go test ./... (zero FAIL)
- agent-runtime-e2e.txt + agent-runtime-e2e/: scripts/agent-runtime-e2e.sh PASS
  and raw artifacts; port-release.txt shows both ports released.
- residue.txt: no keel/e2e processes, no listeners on the e2e ports.

Rerun
  export KEEL_TEST_DSN="$(grep '^KEEL_TEST_DSN_W2_SEAM=' .env.local | cut -d= -f2-)"
  go build ./... && go vet ./internal/bashbridge/ ./internal/layerfs/gbashfs/ \
    ./internal/fileplugin/ ./internal/toolsurface/ ./internal/tasklog/ ./cmd/keel/
  go test -race ./internal/bashbridge/ ./internal/layerfs/gbashfs/ \
    ./internal/fileplugin/ ./internal/toolsurface/ ./internal/tasklog/ ./cmd/keel/
  go test ./...
  ./scripts/agent-runtime-e2e.sh --evidence <dir>

Known items (not closed by this slice)
- Capture is upsert-only (inherited): a bash delete removes the file from the
  workspace index but the manifest row is not reconciled away.
- A background job (tool timeout or nohup) that outlives the run may capture
  after the run lease is no longer live; the commit then fails loud as one
  fs.capture_failed event instead of forging an owner token.
- Workspaces live for the process lifetime (no eviction hook; session deletion
  is not observed by this provider).
- The provider applies layerfs' default ignore list (.git/objects); a
  config.toml ignore override would need a SeamOptions plumb-through.
- nohup joins argv back into one script with minimal quoting; the parent shell
  has already performed expansions/redirections.
