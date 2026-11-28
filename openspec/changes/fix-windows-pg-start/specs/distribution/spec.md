# distribution

## ADDED Requirements

### Requirement: Local PostgreSQL start does not wait on daemon stdio

`keel start` and `keel db start` MUST return once PostgreSQL reports ready, even while
the postmaster keeps running. The start path MUST NOT wait for EOF on a stdout/stderr
pipe inherited by the daemonized postmaster. On failure, the error MUST name the
pg_ctl/PG log paths.

#### Scenario: first Windows start
- **WHEN** `keel db start` initializes a fresh local cluster on Windows
- **THEN** it returns after PostgreSQL reports ready and the command exits while the
  postmaster continues listening on 127.0.0.1

#### Scenario: no-argument product start
- **WHEN** a downloaded Windows `keel.exe` is launched with no arguments and no
  `KEEL_DSN`
- **THEN** `keel start` reaches `/v1/ready` 200 instead of hanging after the cluster
  becomes ready

#### Scenario: start failure is diagnosable
- **WHEN** `pg_ctl start` fails
- **THEN** the error names the pg_ctl log and the PostgreSQL log, not only the exit
  status
