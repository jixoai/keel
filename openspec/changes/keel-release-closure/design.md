# Design: keel-release-closure

## Release boundary

- Source of truth: `github.com/jixoai/keel` Releases.
- No `FDE2E_RELEASES_TOKEN`, no `jixoai/fde2e-releases`, no aggregate mirror.
- Keep the existing tag `v*` workflow, web zip, five binaries, `SHA256SUMS.txt`.
- The optional mirror step is removed rather than left as a dormant fde2e coupling.

## Acceptance

- `git grep FDE2E_RELEASES_TOKEN` returns nothing in keel.
- Release workflow still passes version gate and publishes the same artifact set.
- v0.5.0 release page and `keel.jixoai.com/downloads` remain valid; no rebuild.
- `openspec validate --specs --strict` failures caused by keel placeholder Purpose are fixed; unrelated historical placeholders are not touched.
