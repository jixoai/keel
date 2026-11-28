# distribution

## ADDED Requirements

### Requirement: Windows release runtime verification

A published Windows amd64 release SHALL NOT be described as Windows-verified until the
published asset has been launched on a real Windows machine. CI build success alone MUST
NOT count as runtime verification. A release whose first start leaves `/v1/ready` non-200
MUST be reported as not verified, even if PostgreSQL becomes ready. A published asset MUST
NOT be rebuilt or replaced to make acceptance pass.

#### Scenario: clean Windows launch
- **WHEN** the published Windows zip is extracted on a Windows machine with no Keel data
- **AND** its SHA-256 matches the release `SHA256SUMS.txt`
- **AND** `keel version` reports the release version
- **THEN** launching `keel.exe` with no arguments, without `KEEL_DSN`, Docker, or a manual
  `initdb`, extracts the embedded runtime, initializes the local database, starts the web
  client, and answers `/v1/ready` with 200

#### Scenario: first start never reaches ready
- **WHEN** no-argument start extracts the runtime and PostgreSQL becomes ready, but
  `/v1/ready` never returns 200
- **THEN** the release is reported as not Windows-verified and the failure is recorded

#### Scenario: restart persistence
- **WHEN** a session is created, the process is stopped and started again with no
  external database or environment variable
- **THEN** the session is still readable and remains in the same `KEEL_HOME`

#### Scenario: non-ASCII path
- **WHEN** the zip is extracted under a directory containing spaces and Chinese characters
- **THEN** runtime extraction, data directory resolution, and startup succeed

### Requirement: Windows acceptance evidence is recorded

Windows acceptance MUST be recorded as an OpenSpec change with reproducible commands and
captured output, not only as a narrative claim. A defect found during acceptance MUST be
recorded with the Windows version, path, exit code, and logs.

#### Scenario: evidence review
- **WHEN** a reviewer opens the Windows acceptance change
- **THEN** the recorded commands and outputs reproduce the PASS/FAIL table without
  access to the accepting machine

#### Scenario: defect found
- **WHEN** acceptance finds a real Windows defect
- **THEN** the defect is recorded without rewriting the released asset, and a separate
  fix change owns the code change
