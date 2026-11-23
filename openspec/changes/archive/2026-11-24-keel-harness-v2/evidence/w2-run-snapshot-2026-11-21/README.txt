W2 run snapshot + external-kernel capability matrix — evidence (2026-11-21)

Worktree : /Users/kzf/Development/GitHub/jixoai/fde2e/.worktrees/keel-w2-snapshot
Base     : 889c01b (task/keel-harness-v2 lineage; not pushed)
Commits  : 8557c9f run snapshot implementation
           421b5dd gateway capability matrix
           8941c76 tool-manifest hash moved to agenttool (architecture boundary fix)

Artifacts
  01-gofmt.txt              gofmt -l over every changed file (empty = formatted)
  02-build.txt              go build of changed packages
  03-vet.txt                go vet of changed packages
  04-test-full-dsn.txt      go test -p 1 -count=1 ./... with KEEL_TEST_DSN
  05-conformance.txt        scripts/conformance.sh (PI conformance + runtime suites)
  conformance.json          machine-readable conformance summary
  06-openspec-strict.txt    openspec validate keel-harness-v2 --strict
  07-targeted-tests.txt     verbose run of the new run-snapshot / gateway / kernel / servicehost tests
  SUMMARY.txt               numeric roll-up

Reproduce (KEEL_TEST_DSN is read from .env.local and never printed):
  export KEEL_TEST_DSN="$(grep '^KEEL_TEST_DSN=' .env.local | cut -d= -f2-)"
  gofmt -l internal/agent/runner.go internal/agent/snapshot.go internal/agent/worker.go \
    internal/agent/run_snapshot_test.go internal/agenttool/manifest.go internal/toolsurface/hash.go \
    internal/kernel/validate.go internal/kernel/run_finished_test.go internal/servicehost/host.go \
    internal/servicehost/host_test.go internal/gateway/initialize.go internal/gateway/conn.go \
    internal/gateway/errors.go internal/gateway/capabilities_test.go
  go build ./internal/agent ./internal/agenttool ./internal/toolsurface ./internal/kernel ./internal/gateway ./internal/servicehost
  go vet   ./internal/agent ./internal/agenttool ./internal/toolsurface ./internal/kernel ./internal/gateway ./internal/servicehost
  go test -p 1 -count=1 ./...
  scripts/conformance.sh /tmp/conformance.json
  openspec validate keel-harness-v2 --strict --no-interactive

Scope notes
  - configHash is the pinned loadout identity (id + revision); the worker writes it into
    run.finished after the attempt, from the generation the attempt pinned. Generation is
    excluded because every reassembly increments it even when the configuration did not change.
  - anchorEventSeq is the newest committed config.rotated seq when one exists; otherwise the
    run-start read-through (the newest committed sequence the transcript was built from).
  - toolManifestHash is hashed from the exact []AgentTool slice injected into the loop, not
    from the host registry at settle time; a mid-run MCP/plugin refresh only affects the next run.
  - Gateway capability requirements are opt-in: an empty RequiredCapabilities keeps legacy
    activation; required unknown names are invalid_capability, required-but-undeclared bits are
    unsupported_capability.
