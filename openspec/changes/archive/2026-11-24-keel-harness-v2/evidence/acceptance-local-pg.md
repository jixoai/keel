# Acceptance battery without Docker

Date: 2026-11-23

## Change

`scripts/acceptance-all.sh` previously reset its dedicated databases through
`docker exec keel-pg psql -U postgres` and built every DSN from a hard-coded
`postgres://postgres:runtime@127.0.0.1:13741/<db>`.

The battery now uses the local PostgreSQL client and derives every dedicated
database from `KEEL_TEST_DSN`:

- `KEEL_TEST_DSN` supplies host, port, user, password and query parameters;
- `dsn <db>` replaces only the database path;
- `db_reset <db>` runs `DROP DATABASE IF EXISTS ... WITH (FORCE)` and
  `CREATE DATABASE` through `psql` (default `$KEEL_PSQL`, overridable);
- `KEEL_ACCEPTANCE_MAINTENANCE_DSN` can name a different maintenance
  connection; it defaults to `KEEL_TEST_DSN`.

No container, image or Docker socket is used.

## Verification

With the local Homebrew PostgreSQL 18 on 5432:

```bash
KEEL_TEST_DSN='postgres://kzf@127.0.0.1:5432/postgres?sslmode=disable'
# dsn keel_smoke -> postgres://kzf@127.0.0.1:5432/keel_smoke?sslmode=disable
psql "$KEEL_TEST_DSN" -v ON_ERROR_STOP=1 -q \
  -c 'DROP DATABASE IF EXISTS keel_smoke WITH (FORCE)' \
  -c 'CREATE DATABASE keel_smoke'
psql 'postgres://kzf@127.0.0.1:5432/keel_smoke?sslmode=disable' -Atc 'select current_database()'
# keel_smoke
psql "$KEEL_TEST_DSN" -q -c 'DROP DATABASE keel_smoke WITH (FORCE)'
```

`bash -n scripts/acceptance-all.sh` passes. The Linux-only cgroup/2GB perf
cell remains a platform limitation, not a Docker one; the rest of the battery
can now run on this machine.

Owner constraint: no Docker for tests or review environments.
