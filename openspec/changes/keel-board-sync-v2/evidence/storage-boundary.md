# Storage dialect boundary evidence

Baseline: board `v0.1.2` (`006a43b`, read-only sync/automation health, unchanged).
Delivery: board `80bff1b` tag `v0.1.3` (storage boundary; same API) and
`3d6115b` tag `v0.1.4` (assembly gate + discovered boundary proof).
Keel: `github.com/jixoai/keel-board v0.1.4`.

## Boundary proof

```sh
cd keel-board
for pkg in ./store ./syncstore ./audit ./automation ./server ./sync ./sync/... \
           ./log ./board ./protocol ./access ./blob ./metrics ./conformance; do
  go list -deps "$pkg" | grep 'jackc/pgx' && echo "PGX IN $pkg"
done
# no output
```

`go test ./pgadapter -run TestStorageBoundary -v` passes; the test discovers
every module package with `go list github.com/jixoai/keel-board/...`, skips only
`pgadapter`, `cmd` and `scripts`, runs `go list -deps` on the rest, and asserts
`pgadapter` still depends on pgx. A new subpackage cannot escape the check.

## Conformance

```sh
cd keel-board
KEEL_BOARD_TEST_DSN='postgres://kzf@127.0.0.1:5432/postgres?sslmode=disable' \
  go test -count=1 ./storage ./conformance ./pgadapter/...
```

Observed:

- `conformance`: ok, including `TestStorageMemory` (the dialect-neutral suite
  runs against the in-memory adapters).
- `pgadapter`: ok, including `TestStoragePostgres` (fresh schema, all four
  PostgreSQL adapters), the two-handle cross-process lease assertion, and
  `TestStorageBoundary`.
- `pgadapter/schema`: ok (`TestMigrationLockIDIsSchemaDerived`).

`cmd/board` runs the same gate after opening each PostgreSQL adapter:
`Capabilities.Require(Durable, CrossProcessLeases, Migrations)`. An adapter
that does not declare them fails startup with typed `storage.ErrUnsupported`.
Keel's `internal/boardintegration` does the same for the mounted board surface.

`conformance.RunStorage` covers append log, sync entry/inbox/outbox lease,
targets/lag/failures null semantics, automation watcher lease, audit, and the
capability/typed-error contract. It imports no database driver.

## Capabilities

| Capability | PostgreSQL | In-memory log | In-memory sync/audit/automation |
| --- | --- | --- | --- |
| `Durable` | true | false | false |
| `CrossProcessLeases` | true | false | false |
| `Migrations` | true | false | false |
| `ListenNotify` | false (Wait polls the head) | true (in-process wakeup) | false |

`storage.Capabilities.Require` returns a wrapped `storage.ErrUnsupported`;
the suite asserts that for the zero capability set and for the PostgreSQL
adapter's undeclared `ListenNotify`.

## Full board gates

```sh
KEEL_BOARD_TEST_DSN='postgres://kzf@127.0.0.1:5432/postgres?sslmode=disable' \
  go test -count=1 ./...
# all packages ok
KEEL_BOARD_TEST_DSN='postgres://kzf@127.0.0.1:5432/postgres?sslmode=disable' \
  go test -race -count=1 ./...
# exit 0
```
