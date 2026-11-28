# v0.6.0 release closure — facts

> Recorded 2026-11-29 from the GitHub tag/Release and workflow runs. Bytes are
> immutable after publication: no asset is rebuilt for this note.

## Tag and commit

| item | value |
|---|---|
| tag | `v0.6.0` (annotated) |
| tag object | `d1353c71d51966baea255c25285c85b7c3bc0126` |
| commit | `01862dab39337fde91fcdd591074a97e4218a717` |
| commit title | `test(keel): final gates pass — 540 TUI, 56 UI shots, UNCOVERED=0` |
| remote tag | `refs/tags/v0.6.0` → annotated object → `01862dab` |

## Release

| item | value |
|---|---|
| Release page | https://github.com/jixoai/keel/releases/tag/v0.6.0 |
| title | `keel 0.6.0` |
| state | published, not draft/prerelease, 2026-11-28T16:25:20Z |
| workflow | https://github.com/jixoai/keel/actions/runs/46164106543 (success, 10m38s) |
| outlet | this repository only; no aggregate mirror and no `FDE2E_RELEASES_TOKEN` |

## Assets and SHA-256

`SHA256SUMS.txt` asset digest: `sha256:c40a3d8ddbe60171ecf04156ba4330273dda819f01d77996f9dee69458dc990b`

| asset | bytes | SHA-256 |
|---|---:|---|
| `keel-0.6.0-darwin-amd64.tar.gz` | 105007091 | `416c06bc579e986819f88fb4965617aa663003d29889d7488d6625321eb867ef` |
| `keel-0.6.0-darwin-arm64.tar.gz` | 104555573 | `01c47d6d9dd810f46b5e1f521713712f7da83214ed69891c8369bb6f4b76a598` |
| `keel-0.6.0-linux-amd64.tar.gz` | 48579825 | `c54c274a1803cf6c40182adbf8a282be267b57764d3067f9f04bf347b3d8fa37` |
| `keel-0.6.0-linux-arm64.tar.gz` | 46114199 | `6250a972d0873551e6cd712f8a01439e384e5ed278f5d0bf485e80af46492571` |
| `keel-0.6.0-windows-amd64.zip` | 59275550 | `44d3777847491459f18af30c219951ba4f0af1dde6ced8f96ba5ac43e5576274` |
| `keel-web-0.6.0.zip` | 18638664 | `f432828b7770d5efae043deded763ec2cd5a86716567326a258b7bdd24c45d8f` |

Downloaded `SHA256SUMS.txt` matches the GitHub asset digests above.

## Verification

- Local final gates: 540/540 TUI, 56 screenshots, 26/26 matrix, file coverage
  588/588 (UNCOVERED=0), `go test -p 1 ./...` with real PostgreSQL PASS.
- Remote CI: https://github.com/jixoai/keel/actions/runs/46161849322
  (`test`, `chaos-nightly` 600s SIGKILL, `image` all success).
- Conformance: https://github.com/jixoai/keel/actions/runs/46161858886 (success).
- Release workflow: https://github.com/jixoai/keel/actions/runs/46164106543 (success).

## Download page

After the site deployment from the v0.6.0 docs commit:

- https://keel.jixoai.com/downloads
- https://keel.jixoai.com/zh/downloads
- Public UI report: https://keel.jixoai.com/reports/keel-v0.6.0/

## Known external items

1. **Windows is built, not runtime-verified.** `keel-0.6.0-windows-amd64.zip` is
   part of the release, but this version has no Windows-machine launch/install
   evidence; do not claim Windows verification.
2. **No code signing.** macOS downloads may be quarantined by Gatekeeper; the
   installed `.app` is ad-hoc signed locally, not notarized.
3. The release is published by this repository only. Historical `keel-v*`
   releases in `jixoai/fde2e-releases` are not maintained or deleted here.
