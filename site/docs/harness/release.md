---
title: Release and upgrade
---

# Release and upgrade

A keel release is a tagged commit, five self-contained platform binaries built from it in CI, one
checksum file, and a documented handoff for the running process. Nothing is built on a laptop, so a published SHA means
something.

## 1. One version constant, two version numbers

`keel` has a product version and a protocol version, and they are not the same thing:

| Number | Where | Meaning |
|---|---|---|
| product version | `cmd/keel/main.go:23` | the release (`0.2.0`); what `keel version` prints |
| HarnessAPI version | `harnessapi.APIVersion` | the `/v1` contract; a client pins the major and asks `/v1/capabilities` |

The comment above the constant states the separation (`cmd/keel/main.go:20`), and `printVersion`
renders `keel <version>` plus build facts (`cmd/keel/main.go:128`). Bumping the product version is
a source change: it is committed, reviewed and tagged like any other.

## 2. The tag is the trigger, and the gate runs first

The release workflow fires on a `v*` tag (or a manual dispatch with an explicit tag). Before anything
is built, it compares the tag to the binary's own report:

```bash
# what the gate does, locally
go run ./cmd/keel version          # prints: keel 0.5.0
# tag must be v + that number, or the run fails before publishing
```

The gate is `.github/workflows/release.yml:43`: `go run ./cmd/keel version` is parsed, and a mismatch between the tag
and the reported version aborts the run with "bump the version constant before tagging"
(`.github/workflows/release.yml:50`). A tag that lies about the binary is worse than a red build.

## 3. What a tag publishes

| Artifact | Platform / content |
|---|---|
| `keel-<ver>-darwin-arm64.tar.gz` | macOS Apple silicon, self-contained |
| `keel-<ver>-darwin-amd64.tar.gz` | macOS Intel, self-contained |
| `keel-<ver>-linux-amd64.tar.gz` | Linux x86-64, self-contained |
| `keel-<ver>-linux-arm64.tar.gz` | Linux arm64, self-contained |
| `keel-<ver>-windows-amd64.zip` | Windows x86-64 (`keel.exe`), self-contained |
| `keel-web-<ver>.zip` | the packaged web client (optional, for `serve -web-dir`) |
| `SHA256SUMS.txt` | sorted, one line per artifact |

Each platform binary embeds a relocatable PostgreSQL 18 runtime, the packaged web client and the
README; the archive adds `README.md` (`scripts/package-self-contained.sh`). `keel start` needs no
`KEEL_DSN`, no external PostgreSQL and no Docker; macOS `keel install` generates an ad-hoc signed
`.app` from the same binary. **The downloaded binary is not code-signed**; on macOS clear the
quarantine flag once: `xattr -dr com.apple.quarantine ./keel`.

The TUI is not a packaged artifact: `nsh package` has no `tui` target, so `keel tui` runs from a
development host (`nsh run --target tui`) or an embedded `nsh-platform-tui`
(`web/README.md:167`).

## 4. The pipeline, in order

1. **Version gate** — tag ↔ binary (`go run ./cmd/keel version`).
2. **Web client** — install the pinned NSH release and package the site (`scripts/package-web.sh`).
   The NSH version is pinned to a release, not the rolling nightly, because a nightly drift already
   broke this build once.
3. **Self-contained binaries** — for each `GOOS/GOARCH`, download the matching relocatable
   PostgreSQL 18 runtime (zonky/theseus Maven artifact), extract it into `cmd/keel/assets`,
   embed the web client, and cross-compile with `CGO_ENABLED=0`, `-trimpath`, `-s -w`.
4. **Checksums** — `sha256sum` over every artifact in sorted order (`SHA256SUMS.txt`).
5. **Publish** — create the release on this repository, or upload to it on a retry.

The release is published here and nowhere else: there is no aggregate outlet, no mirror into
another repository and no cross-repository token. The GitHub Release for the tag is the single
distribution point, so there is no second page whose checksums could drift.

The web build is deliberately fail-closed: v0.2.0 shipped without its zip because the step was
`continue-on-error`, and a partial release is worse than a red run that can be fixed and re-tagged
(`.github/workflows/release.yml:85`).

Reproducibility is the intent, not a guarantee: `-trimpath` and a pinned toolchain make a rerun of
the same source produce the same bytes (`.github/workflows/release.yml:7`); a different Go patch
release or a different NSH pin is a different input.

## 5. Cutting the site over

The site is a static VitePress build; version strings live in the pages, not in a config constant.
A release cutover is therefore a version bump, the page edits, and then the gates:

```bash
# 1. the version constant (one file, one line)
sed -n '23p' cmd/keel/main.go

# 2. where the site quotes the version
rg -n 'v0\.2\.0' site/docs/index.md site/docs/downloads.md site/docs/zh/downloads.md

# 3. commit the bump, then tag and push
git tag v0.2.1 && git push origin v0.2.1

# 4. after the release is green, point the pages at the new tag and rebuild the site
cd site && pnpm build
```

`site/scripts/build.mjs` is the build contract: dead links fail the build, EN/ZH parity must hold,
and every `path:line` citation must resolve (`sed -n '1,10p' site/scripts/build.mjs`). A failing step
leaves the previously deployed revision in place (`sed -n '9,10p' site/scripts/build.mjs`), so a bad
cutover is a non-deploy, not a broken page.

Rules for the edit:

- **Checksums are quoted, never guessed.** The page's SHA block and `SHA256SUMS.txt` must be the same
  strings; copy them from the release, do not retype.
- **All three locale surfaces move together**: `index.md` (hero action), `downloads.md`,
  `zh/downloads.md`. The parity gate will not catch a stale version string — grep is the check.
- **Do not rebuild a published version to make the page match.** If a SHA changed, that is a new
  patch release.

## 6. Mirrors

GitHub is the origin; the same path suffix is served by mirrors for slow networks:

```bash
VER=0.5.0
BASE=https://github.com/jixoai/keel/releases/download/v$VER
MIRROR=https://gh.fde2e.tweb.xin/jixoai/keel/releases/download/v$VER

# 1. the mirror's checksum file must be the origin's file, byte for byte
curl -fsSL "$BASE/SHA256SUMS.txt" -o sums.origin
curl -fsSL "$MIRROR/SHA256SUMS.txt" -o sums.mirror
cmp sums.origin sums.mirror

# 2. the artifact must match that checksum file
curl -fsSL "$MIRROR/keel-web-$VER.zip" -o "keel-web-$VER.zip"
shasum -a 256 -c sums.mirror --ignore-missing
```

- `https://gh.fde2e.tweb.xin/` — self-built mirror, domestic first choice; prefix-swap the GitHub
  host and keep the rest of the path.
- `https://ghfast.top/https://github.com/…` — public proxy fallback; same rule.

Both mirrors are GitHub proxies: they point at the same `jixoai/keel` release path, not at an
aggregate repository.

A user who verifies the checksum is protected even on a mirror: the file is fetched by hash, not by
trust.

## 7. Upgrading a running deployment

The upgrade primitive is a **shift change**, not a hot swap: drain the old process, then start the new
binary. New and old writers must never coexist, because the old write path does not pass through the
fencing check (`docs/HANDOFF.md:24`).

```bash
# 1. start the new runtime against the same DSN (KEEL_RUNTIME_ID required)
# 2. put the old one into handoff drain
curl -sS -X POST "$OLD/v1/runtime/drain" \
  -H "Authorization: Bearer $KEEL_ADMIN_KEY" \
  -d '{"mode":"handoff","deadlineMs":30000}'

# 3. watch it stop claiming and drain in-flight runs
curl -sS "$OLD/v1/runtime/status" -H "Authorization: Bearer $KEEL_ADMIN_KEY"
```

During handoff the old process stops claiming new runs but keeps accepting edge input; in-flight runs
drain to their commit boundary, and uncommitted partials (a half-streamed message, a running tool) are
dropped at the deadline (`docs/HANDOFF.md:78`). The new owner claims with `epoch+1` and rebuilds from
the ledger; SSE clients reconnect with `Last-Event-ID` and the first frame is `cursor+1`.

SIGTERM is **shutdown** drain, not handoff: it refuses new writes and abandons in-flight runs without
finishing or releasing them, so the new process may wait one bounded lease TTL
(`docs/HANDOFF.md:83`).

## 8. Rolling back

The rollback is the same protocol in reverse (`docs/HANDOFF.md:39`):

1. **Drain first** — handoff mode, or SIGTERM; confirm `claiming=false` and `inFlightRuns=0`, or wait
   out the deadline.
2. **Stop the new process, start the old binary** against the same DSN. The old binary tolerates the
   newer columns because migrations are additive.
3. **Never run both writers.** Rollback has the same no-coexistence rule as upgrade.
4. **Do not roll back data.** The ledger is append-only; unknown event kinds are skipped by older
   readers, and a client may need to filter them.
5. **Expect a bounded window**: an old-style `Finish` does not clear `sessions.lease_until`, so a
   session can stay unclaimable for one TTL; `KEEL_LEASE_MS` shrinks it (`docs/HANDOFF.md:51`).

For the release itself: a published tag is immutable. The workflow's upload step uses `--clobber`
(`.github/workflows/release.yml:126`), which repairs a partial or failed run; it is not a way to change bytes users
already verified. Ship the fix as a new patch version.

## 9. Verifying a download

```bash
VER=0.5.0
curl -LO "https://github.com/jixoai/keel/releases/download/v$VER/SHA256SUMS.txt"
curl -LO "https://github.com/jixoai/keel/releases/download/v$VER/keel-$VER-linux-amd64.tar.gz"
shasum -a 256 -c SHA256SUMS.txt --ignore-missing
tar -xzf "keel-$VER-linux-amd64.tar.gz" && ./keel version
```

The last line must print the same version the tag names — that is the version gate's invariant, checked
again on the user's machine.
