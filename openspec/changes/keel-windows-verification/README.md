# keel-windows-verification

Windows 11 x64 real-machine acceptance for keel-0.6.0-windows-amd64.zip: asset checksum, no-arg startup, probes, web, persistence, non-ASCII path, system integration. v0.6.0 found two blocking defects (pg_ctl stdio hang, missing handoff schema on /v1/ready); fixes ship in v0.6.1 and this change records the re-verification. Evidence only; v0.6.0 remains immutable.
