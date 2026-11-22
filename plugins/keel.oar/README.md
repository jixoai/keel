# OAR plugin package (target)

This directory is the **package payload** for `keel.oar`, the external-runtime
adapter that ships with Keel. It is intentionally not installable through the
current plugin manager yet:

- `plugin.yaml` describes the target sidecar package (`keel-oar sidecar`).
- The current `harnessext` sidecar loader accepts only `tool_provider`
  connections; OAR is an `agent_backend` (it owns sessions and speaks
  `session.open` / `message.send` / `event.stream`). The marketplace/install
  half is deferred to W12, together with the slot packaging.
- Until then, configure OAR through `host.ExternalConfig.Backends`
  (`oar.Config.ExternalBackendConfig()`) or embed `oar.Service`.

The contract lives in [`docs/OAR.md`](../../docs/OAR.md). The renderer seam is
`keel.oar.render-projection/1`; `fixtures/tool-renderer.fixture.json` is the
checked-in fixture the W12 `keel.ui.tool-renderer` slot can consume. The web UI
owns that slot; this package must not ship a parallel renderer.

Build the sidecar binary:

```bash
go build -o ./bin/keel-oar ./internal/oar/cmd/keel-oar
./bin/keel-oar sidecar -runtime=fake -usage
```

Configuration is documented in `extensibility.json` and
[`EXTENSIBILITY.md`](EXTENSIBILITY.md). No task-card instruction is ever
injected into an external prompt; attribution is carried out of band in the
session-open config and echoed on every record.
