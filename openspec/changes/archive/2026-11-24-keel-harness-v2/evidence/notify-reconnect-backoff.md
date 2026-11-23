# Notify listener reconnect backoff

Date: 2026-11-23

## Symptom

Two review instances were still running after their PostgreSQL test databases
were removed:

- `/tmp/keel-identity-app` (127.0.0.1:13758) — ~1h45m CPU time in ~4h, log
  `/tmp/keel-identity.log` at 8.7 GB.
- `/tmp/keel-app` (127.0.0.1:13760) — ~1h51m CPU time, log
  `/tmp/keel-serve.log` at 8.4 GB.

`sample` showed the hot path in
`httpapi.(*Hub).Run → listen → slog.Warn → logsafe.redactText → regexp` and
the logs were thousands of identical warnings per second:

```text
WARN notify listener stopped, retrying
err="failed to connect ... 127.0.0.1:13741: connect: connection refused"
```

## Root cause

`Hub.Run` reconnected immediately on every error and logged one warning per
attempt. With the database down, Acquire failed in microseconds, so the loop
became a busy connect+log loop. The comment claimed "bounded backoff", but no
backoff existed.

This is not a test-only path: any deployment whose database is temporarily
unreachable would spin a core and fill the disk.

## Fix

`internal/httpapi/hub.go`:

- exponential reconnect delay: 1s, 2s, 4s, … capped at 30s;
- one warning per attempt with `retryIn=<delay>` (the interval grows, so the
  log volume is bounded);
- a listener that stayed up for at least the cap resets to 1s, so an early
  blip does not slow every later reconnect;
- context cancellation stops the wait immediately.

Unit test `TestNextNotifyBackoffDoublesAndCaps` pins the delay ladder and cap.

## Verification and cleanup

- `go test -count=1 ./internal/httpapi` — ok.
- Both stale processes were stopped (SIGTERM) and their 17 GB of logs removed;
  disk free went from ~53 GiB to ~221 GiB.
- The review instance must be rebuilt only after its PostgreSQL database is
  running; it must not be left serving with a dead DB.
