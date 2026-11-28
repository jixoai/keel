# Storage dialect boundary evidence

Baseline: board `v0.1.2` (`006a43b`, read-only sync/automation health, unchanged).
Delivery: board commit `80bff1b`, annotated tag `v0.1.3` (storage boundary; same API).
Keel: `github.com/jixoai/keel-board v0.1.3`.

## Boundary proof

```sh
cd keel-board
for pkg in ./store ./syncstore ./audit ./automation ./server ./sync ./sync/... \
           ./log ./board ./protocol ./access ./blob ./metrics ./conformance; do
  go list -deps "$pkg" | grep 'jackc/pgx' && echo "PGX IN $pkg"
done
# no output
```

`go test ./pgadapter -run TestStorageBoundary -v` passes; the test runs the same
`go list -deps` check for every domain/server/sync/log package and asserts
`pgadapter` still depends on pgx, so the boundary cannot drift silently.

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
  PostgreSQL adapters) and `TestStorageBoundary`.
- `pgadapter/schema`: ok (`TestMigrationLockIDIsSchemaDerived`).

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
# ok: access audit automation blob board cmd/board conformance log metrics
#     pgadapter pgadapter/schema protocol server store sync sync/chatsdk
#     sync/chatsdk/sidecartest sync/github sync/linear sync/slack syncstore

KEEL_BOARD_TEST_DSN='postgres://kzf@127.0.0.1:5432/postgres?sslmode=disable' \
  go test -race -count=1 ./...
# exit 0
```
