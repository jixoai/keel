# release-distribution Specification

## Purpose

Keep Keel releases independent: the repository's own GitHub Release is the only distribution outlet, with no aggregate mirror, cross-repository token, or second release page to drift.

## Requirements

### Requirement: Keel releases are independent

Keel MUST publish only to `github.com/jixoai/keel` Releases. The release workflow MUST NOT require or read `FDE2E_RELEASES_TOKEN`, and MUST NOT mirror to `jixoai/fde2e-releases`.

#### Scenario: release without fde2e secrets
- **WHEN** a `v*` tag is pushed and no fde2e secrets exist
- **THEN** the release workflow builds the five binaries and web zip, publishes `SHA256SUMS.txt`, and succeeds without a mirror step

### Requirement: Release acceptance is recorded

A release is not complete until its assets, checksums, website download page and binary version are verified. A published SHA MUST NOT be rebuilt to make acceptance pass.

#### Scenario: v0.5.0 acceptance
- **WHEN** v0.5.0 is checked
- **THEN** the release assets, SHA256SUMS, download page and `keel version` output agree
