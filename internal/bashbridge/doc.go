// Package bashbridge is keel's only gbash adapter (the "keel.shell" layer of
// docs/GBASH-CONTRACT.md). Everything upstream-specific lives here: the pinned
// github.com/ewhauser/gbash module is imported by this package and nowhere else
// in the repository, and gbash types never appear in an exported signature.
//
// The contract this package implements:
//
//   - One gbash Session per bridge Session. A tool call and a background job
//     each get their own bridge Session, because Session.Exec is serial and
//     background jobs do not outlive their Exec (GBASH-CONTRACT §5.2/§5.3).
//   - ExecutionRequest.Timeout is never set. Upstream's Timeout means
//     "cancel and kill"; keel's timeout means "return now, keep running". The
//     caller implements the latter with its own timer and does not cancel the
//     job context (GBASH-CONTRACT §6.1/§6.2).
//   - No real processes. The default host is gbash's virtual host ($$ == 1),
//     the command registry is the only command source (host binaries are
//     unreachable), and network access is off (curl/wget are not registered).
//   - The workspace is an injectable mount at a non-root path. gbash's sandbox
//     layout (263 virtual command stubs plus /bin /usr/bin /tmp) must land on
//     the in-memory base, never on the injected backend (GBASH-CONTRACT §4.2).
//   - Output is streamed to the caller's writer while only a fixed head/tail
//     window is retained here. The full stream is the caller's blob store job;
//     gbash's own capture buffer is deliberately small (64KiB) and is not the
//     output path.
package bashbridge
