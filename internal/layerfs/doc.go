// Package layerfs is Keel's Phase A content-addressed byte layer (ADR 0001).
//
// It stores exactly two kinds of immutable objects:
//
//   - file objects: raw file bytes, addressed by sha256(content);
//   - tree objects: a canonical directory listing, addressed by
//     sha256(canonical entries).
//
// The package deliberately knows nothing about sessions, tasks or absolute
// paths. A tree object carries the relative names of one directory listing
// (that is what a tree object is); path → hash bookkeeping is the file-plugin's
// `file_manifest`. This is the Phase A implementation: an in-process Go CAS
// over a local directory using the same two-level CAS layout as internal/blob
// (blob.CASPath). The gRPC/Rust service in ADR 0001 can replace this package
// behind the same object model without touching callers.
//
// Streaming discipline: Put, ReadRange, Materialize and Capture never hold a
// whole file in memory. The fixed chunk is 64KiB (the same chunk size the blob
// layer uses), and a large file costs one chunk per operation.
//
// Reference counting is event-derived: every ledger event that references a
// blob retains it under (session, eventSeq), and the same pair is a no-op on
// replay. The journal lives beside the objects (`refs.log`) so a process
// restart reconstructs the index. GC only deletes unreferenced objects older
// than the retention window.
package layerfs
