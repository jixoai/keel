W3 capture policy override — evidence (2026-11-23)
=================================================

Scope
-----
The missing W3 item from openspec/changes/keel-harness-v2/tasks.md:

  capture 策略覆写：tenant document
  filesystem.capture.mode = per-tool-call | checkpoint | manual

The repository has no config.toml loader. The policy is therefore carried by
the single tenant-configuration vocabulary (host.TenantConfig), which the admin
API already stores and validates. No second configuration document exists.

What landed
-----------
1. Vocabulary and strict validation
   - internal/layerfs/capturemode.go: CaptureMode, the three values,
     DefaultCaptureMode = per-tool-call, ParseCaptureMode (closed set),
     CaptureModeError (invalid_capture_mode) and CaptureTriggerError
     (capture_trigger_missing), plus the per-attempt context pin helpers.
   - internal/host/config.go: TenantConfig.Filesystem.Capture.Mode; Merge
     overlays the tenant document on defaults; Validate rejects unknown and
     whitespace-padded values; canonical JSON round-trips (marshal -> parse).
   - Omitted mode = per-tool-call. A stored document mode overrides the boot
     document mode.

2. Capture decision
   - internal/bashbridge/layerworkspace.go: SessionIdentity gains CaptureMode.
     Mount resolves the mode once from the identity (falling back to the
     runner-pinned context) and never re-reads configuration at close time.
     - per-tool-call: Mount.Finish settles one fs.capture per bash execution
       (unchanged behavior).
     - checkpoint/manual: Mount.Finish records a pending marker instead of
       settling.
     - LayerWorkspaces.Checkpoint consumes the pending marker and settles
       exactly one fs.capture; a second call with nothing pending is a no-op.
       A stable call id is the idempotency key: a replay returns the prior
       settlement without appending another event. A failed or replayed
       settlement restores the marker so a later fresh checkpoint can capture.
     - Background jobs always settle at their own close (system lane, jobId
       attribution), independent of the run's mode: a job may outlive the run,
       so its Close is the only trigger.
   - internal/agent/worker.go: the run-completion trigger. The worker resolves
     the attempt's policy once, at attempt start, before any tool executes;
     pins the mode on the attempt's tool context; and after RunAttempt returns
     (before run.finished and before the lease settle) invokes Checkpoint for
     checkpoint mode. A canceled attempt still settles its writes (the hook
     gets a non-canceled context); a process shutdown abandons without adding
     work.
   - internal/agent/runner.go: RunCapture + CapturePolicy seam and
     ResolveCapturePolicy; checkpoint/manual without a non-nil Checkpoint hook
     fails with *layerfs.CaptureTriggerError.

3. Explicit manual surface
   The smallest model-visible surface inside the closed tool-action contract:
   bash {action:"checkpoint"}.
   - internal/toolsurface: ActionCheckpoint added to the bash closed action
     set (run, jobs, checkpoint); permission key file.write (it persists the
     workspace, it is not another command execution).
   - internal/harnessext/shell.go: internal keel.shell.checkpoint tool behind
     the action. The action needs no job manager; it reads the ambient session
     identity stamped by the bash backend wrapper. An unwired surface answers a
     typed backend_error (reason=checkpoint_unavailable), never a silent
     success.
   - cmd/keel/capture_policy.go: production resolver. It reads the tenant row
     per attempt (stored document over boot document), so an admin PUT affects
     the next run; it adapts LayerWorkspaces.Checkpoint to both the
     run-completion hook and the bash action.
   - bashSessionBackend copies the pinned mode into SessionIdentity.

4. Pinning across rotation
   The mode is resolved once per attempt and attached to that attempt's tool
   context; the workspace provider resolves it at mount time. A run cannot
   switch capture policy mid-flight. Test
   TestCheckpointPolicyPinsAndFiresOnce rotates the source after the first
   attempt pins and asserts the in-flight tool still sees checkpoint while the
   next attempt sees manual.

Tests
-----
No test requires Postgres; the new suites use the in-memory kernel/manifest
seams. KEEL_TEST_DSN is not set in this environment, so the existing PG-gated
suites were not run (their skip behavior is unchanged).

  go test -count=1 ./internal/host ./internal/bashbridge ./internal/toolsurface \
      ./internal/harnessext ./internal/agent ./internal/servicehost \
      ./internal/fileplugin ./cmd/keel

Result: see test-targeted.txt.

Focused suites captured separately:
  test-host.txt              capture config parse/merge/validate/round-trip
  test-bashbridge.txt        per-tool-call, checkpoint, manual, unknown mode,
                             job exception
  test-agent.txt             policy pin, run-close hook, manual no-hook,
                             typed fail-closed
  test-toolsurface.txt       checkpoint in the closed action set + delegation
  test-harnessext.txt        checkpoint internal tool + unwired typed error

Limitations
-----------
- No config.toml loader exists, and none was invented. A deployment that later
  adds one must map it onto filesystem.capture.mode.
- The checkpoint hook settles on the owner lane and therefore needs a live run
  lease. The run-completion trigger and the bash action both run inside a run;
  a checkpoint outside a run fails with the service's typed lease error.
- The explicit surface is model-visible (bash action), not an HTTP/UI route.
  It was chosen as the smallest surface consistent with the closed action
  contract; the action set, permission map and tool-surface spec were updated
  together.
- fileplugin's full suite is slow (~140s) but passed; the new code does not
  change fileplugin.
