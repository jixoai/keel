---
title: Files and blobs
---

# Files and blobs

Keel has two file layers, and they answer different questions:

- **Session files** are the agent's workspace: a per-session path tree whose bytes are content
  addressed. File tools write them through `fs.write`; a bash session's changes are captured at
  settlement by `fs.capture`; reads mount them through `file_manifest`.
- **Uploaded blobs** are attachments and media referenced from event payloads: user uploads,
  images sent to a model, generated audio. They live in `blobs` / `blob_chunks` / `blob_refs` and
  are fetched with `GET /v1/files/{id}`.

Both are content addressed and deduplicated. The ledger is the authority for what a session
**references**: a manifest row is a projection of committed events, a blob reference is counted by
the event that named it, and bytes no committed event references are garbage.

## 1. The two layers side by side

| | Session files | Uploaded blobs |
|---|---|---|
| Identity | `(tenant_id, session_id, path)` → content hash | `(tenant_id, blob_id)`, unique `(tenant_id, sha256)` |
| Path projection | `file_manifest` (`internal/store/schema.sql:855`) | none — payloads carry `blobId` / `fileId` |
| Snapshot | `session_snapshot` records the tree hash at a `seq` (`internal/store/schema.sql:871`) | per-blob `expires_at`, reference rows keyed by event seq |
| Bytes | layerfs CAS, two object kinds: file bytes and canonical tree listings (`internal/layerfs/doc.go:5`) | PG 64KiB chunks or local CAS large objects |
| Written by | the file plugin (`keel.file.read/write/edit/capture`) | `POST /v1/files` (`internal/httpapi/server.go:235`) |
| Settled by | `fs.write`, `fs.capture` events | event append automatically retains a referenced blob (`docs/BLOB-LIFECYCLE.md:33`) |
| Garbage collection | `Collect` sweeps unreferenced objects past the retention window; not scheduled by the current shell (`internal/layerfs/store.go:27`) | TTL policy + orphan CAS scan (`docs/BLOB-LIFECYCLE.md:69`) |

The split is deliberate: a session's workspace must survive a fork and a replay (it is a tree), while
an attachment must survive being referenced by any event in any session (it is a blob).

## 2. Session files: manifest, tree object, snapshot

`file_manifest` is the mount, not the bytes. One row per `(tenant, session, path)` carries the hash,
mode, size, the `updated_seq` that established it, and an optional `last_task_id`
(`internal/store/schema.sql:855`). The bytes live in the layerfs CAS as **file objects**; a directory
listing is a **tree object**, a canonical rendering addressed by its own hash
(`internal/layerfs/tree.go:147`).

A capture produces one tree object and records it in `session_snapshot` with the event's `seq`
(`internal/fileplugin/manifest.go:311`). That row is what makes a fork and a historical read possible:
`SnapshotAtOrBefore` returns the newest tree at or before a sequence
(`internal/fileplugin/manifest.go:343`), and the fork overlay decodes the parent's tree by hash
instead of copying rows (`internal/fileplugin/fork.go:18`).

File bytes are retained by the events that name them. `Collect` deletes objects no event references
once they are older than the retention window — 24h by default
(`internal/layerfs/store.go:27`, `internal/layerfs/store.go:502`). Unlike the blob sweeper,
`keel serve` does not schedule this sweep today, so treat reclamation as a maintenance step in the
current shell.

```
fs.write (one path)                   fs.capture (one settlement)
        │                                     │
        ▼                                     ▼
file_manifest upsert                 tree object + session_snapshot(seq, treeHash)
        │                                     │
        └──────────────► layerfs CAS ◄────────┘
                         file objects, tree objects
```

## 3. The write path: the ledger commits first

There is exactly one write path (`internal/fileplugin/service.go:353`): the content streams into
layerfs, the `fs.write` event commits, and only then does `file_manifest` change. If the process dies
between the two steps, the ledger still holds the write; what can be missing is only the derived
manifest row, and the surface reports that projection gap instead of inventing file state
(`internal/fileplugin/service.go:1011`). The ledger never becomes the derivative.

Every write needs the ambient session scope and the tool-call id; replaying the same call dedupes to
the first event. `expectedHash` is the precondition for editing an existing file and must be empty
for a create, so a concurrent change surfaces as a conflict instead of a silent overwrite.

## 4. Capture: one event per settlement

Run settlement snapshots the execution space and commits exactly one `fs.capture` event — or one
`fs.capture_failed` on the failure path (`internal/fileplugin/service.go:574`). The payload carries
`treeHash`, `count`, `newObjects`, `reusedObjects`, `bytes`, `deleted`, `deletedCount` and
`snapshotVersion` (`internal/fileplugin/service.go:728`).

- **Idempotency.** The settlement key is `callID:baseSeq`; a harness-owned background job adds a
  `job:` namespace so the owner lane and the system lane never dedupe into each other.
- **Deletion reconciliation.** A manifest path the workspace no longer has is recorded as deleted in
  the same event. The event document is bounded to 256 paths / 32KiB
  (`internal/fileplugin/service.go:625`); `deletedCount` always reports the full number.
- **Ordering.** Explicit writes and captures of one session are serialized, so a later-committing
  capture can never carry an older snapshot. A crash before the commit leaves zero `fs.capture`
  events; the restarted run re-captures.
- **Retention.** The capture retains every file and tree object it names, under
  `(session, eventSeq)` (`internal/fileplugin/service.go:794`), which is also the replay identity.

## 5. Reads are bounded pages

A file read is never "the whole file". The default page is 200 lines / 256KiB, and the caller may ask
for up to 2000 lines; a single line is capped at 1MiB (`internal/fileplugin/read.go:13`,
`internal/fileplugin/read.go:16`). The page reports `totalLines`, `returnedLines`, `nextOffset` and
`truncated`, so a client resumes by offset rather than by re-reading.

The panel preview uses the same bounded read: the path must exist in the session's own manifest, and
the hash in the answer is the manifest's current hash, not the caller's remembered one
(`internal/harnessapi/task_panel.go:117`). See [Session query](/core/session-query).

## 6. Blobs: references, TTL, orphan sweep

A blob is complete only after its bytes are stored; a reference is added in the same transaction as
the event that names it (`docs/BLOB-LIFECYCLE.md:33`). Replay of the same `(blob, session, eventSeq)`
is a no-op, so a retried commit never inflates the count.

TTL reaping only touches the rows a policy marked: `ref_count = 0`, `complete`, `expires_at` in the
past, and not pointed at by another row's `thumb_blob_id` (`internal/store/blob_lifecycle.go:93`).
**An ordinary upload carries no expiry** — `SetBlobExpiry` with a `nil` mark means "never expires"
(`internal/store/blob_lifecycle.go:235`), so nothing is reaped by TTL until a retention policy opts
in.

Deletion order is strict (`docs/BLOB-LIFECYCLE.md:92`):

1. one transaction: `blob_refs` → `blobs` metadata → `blob_chunks`;
2. commit;
3. outside the transaction: re-check `CASBlobExists(sha256)`, then unlink the CAS file (and prune
   empty directories).

The re-check has no tenant predicate on purpose: the CAS file is shared across tenants by content
(`internal/store/blob_lifecycle.go:248`). A failed or crashed unlink is retried by the orphan scan,
which only visits `<2-hex>/<2-hex>/<64-hex>` files older than the grace window and never enters the
`layerfs/` subtree (`internal/blob/gc.go:142`).

| Loop | Default | Why |
|---|---|---|
| TTL sweep | `KEEL_BLOB_GC_INTERVAL=1m` | claim expired, unreferenced, complete rows (`internal/blob/gc.go:86`) |
| Orphan CAS scan | `KEEL_BLOB_ORPHAN_INTERVAL=30m`, grace 1h | retry unlinks and crash leftovers (`docs/BLOB-LIFECYCLE.md:109`) |
| Thumbnail scan | `KEEL_BLOB_THUMB_INTERVAL=30s` | claim `pending` thumbnails (`cmd/keel/serve.go:984`) |

## 7. Thumbnails

Image uploads (`jpeg`/`png`/`gif`) move through `pending → ready | failed`; `failed` is terminal, so
a broken image is never retried in a storm (`docs/BLOB-LIFECYCLE.md:130`). The thumbnail is an
ordinary blob row (`image/png`), so it uses the same storage and tenant path as any other blob.

| Bound | Value | Anchor |
|---|---|---|
| Long edge | 320px, never upscaled | `internal/blob/thumb.go:51` |
| Decoded pixels | 25 MP | `internal/blob/thumb.go:54` |
| Encoded source | 64MiB | `internal/blob/thumb.go:59` |
| Orphan safety net | `expires_at = now + 24h` | `internal/blob/thumb.go:61` |

`GET /v1/files/{id}/thumbnail` answers `200` (ready, PNG), `202` (pending), `404` (no thumbnail
for this blob) or `409` (failed, or ready without an object) (`internal/httpapi/server.go:1299`).

## 8. Quota

Storage quota is per tenant and measured, not cached: `tenants.quota_storage_bytes` defaults to `0`
(= unlimited) (`internal/store/schema.sql:703`), and `AdmitStorage` sums the tenant's stored bytes
before admitting a write (`internal/quota/edge.go:295`). The upload path is wrapped by the edge quota
middleware, which charges the declared size of `POST /v1/files`
(`cmd/keel/edge_quota.go:58`); an over-quota upload answers **413**, while stream/concurrency quotas
answer **429** (`internal/quota/edge.go:334`).

The file plugin enforces the same idea one level down: layerfs caps stored bytes at the commit point
of `Put`, so an over-quota write leaves no visible object (`internal/layerfs/store.go:234`), and the
tool surface reports `quota_exceeded` (`internal/fileplugin/types.go:34`).

## 9. Operating it

Upload, inspect, preview:

```bash
API=http://127.0.0.1:13750
KEY=keel_bot_xxx                        # a Bot-bound key; see the API reference

# upload (multipart)
curl -sS -X POST "$API/v1/files" -H "Authorization: Bearer $KEY" \
  -F "file=@./diagram.png;type=image/png"

# metadata + thumbnail status
curl -sS "$API/v1/files/<fileId>/meta" -H "Authorization: Bearer $KEY"
curl -sS -o thumb.png -w '%{http_code}\n' \
  "$API/v1/files/<fileId>/thumbnail" -H "Authorization: Bearer $KEY"

# download with range resume
curl -sS -H "Authorization: Bearer $KEY" -H 'Range: bytes=0-65535' \
  "$API/v1/files/<fileId>" -o part.bin
```

Inspect the session file view in the same PostgreSQL database:

```sql
-- the mount: what paths the session has, and at which seq
SELECT path, hash, size, updated_seq, last_task_id
  FROM file_manifest WHERE tenant_id = $1 AND session_id = $2
 ORDER BY path LIMIT 20;

-- the snapshots: which tree the session looked like at a seq
SELECT seq, tree_hash FROM session_snapshot
 WHERE tenant_id = $1 AND session_id = $2 ORDER BY seq DESC LIMIT 5;
```

Configuration that matters:

| Setting | Effect | Anchor |
|---|---|---|
| `KEEL_CAS_DIR` | CAS root for large blobs and layerfs; unset = ephemeral temp root with a warning | `cmd/keel/serve.go:1931` |
| `KEEL_BLOB_GC_INTERVAL` / `KEEL_BLOB_ORPHAN_INTERVAL` | TTL sweep and orphan scan cadence | `cmd/keel/serve.go:988` |
| `KEEL_BLOB_THUMB_INTERVAL` | thumbnail scan cadence | `cmd/keel/serve.go:984` |
| `tenants.quota_storage_bytes` | per-tenant storage ceiling, `0` = unlimited | `internal/store/schema.sql:703` |

## 10. Where the details live

- [Storage and runtime](/core/storage) — the blob tables, chunking decision and backpressure.
- [Session query](/core/session-query) — `deliverables`, `file-preview` and their bounds.
- [Provider adapter](/core/extend/provider-adapter) — how `file_id > url > base64` is resolved outbound.
- `docs/BLOB-LIFECYCLE.md` — the full lifecycle contract, crash-point table and test evidence.
- `docs/FILE-PROTOCOL.md` — the path/hash protocol and tool-facing file actions.
- `internal/pluginpkg/builtin/keel.file.yaml:1` — the built-in plugin that supplies the backend for
  `keel.file.read/write/edit/capture`.
