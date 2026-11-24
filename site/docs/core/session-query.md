---
title: Session query
---

# Session query

Five read routes make a session's history, files and tasks legible without re-reading the ledger:

| Route | What it returns | Bound |
|---|---|---|
| `GET /v1/sessions/search` | a page of session or message hits | `limit` 20 (max 100), `q` ≤ 200 runes |
| `GET /v1/sessions/{id}/export` | a page of transcript text | 200 messages (max 500), 32KiB/message, 512KiB/page |
| `GET /v1/sessions/{id}/deliverables` | the session's file manifest rows | 100 (max 500) |
| `GET /v1/sessions/{id}/task-panel` | task tree, members, ancestors, attachments, events | 200/500 members, 500/2000 events |
| `GET /v1/sessions/{id}/file-preview` | a bounded page of one session file | 200 lines (max 2000), 256KiB |

The routes are registered by `sessionQueryRoutes` (`internal/harnessapi/session_query.go:176`) and
`taskPanelRoutes` (`internal/harnessapi/task_panel.go:142`). None of them writes state: search reads
events, export reads events, deliverables and file-preview read `file_manifest`, and the task panel
reads the task projection. A route whose seam is not wired answers **501** with `status: "not_wired"`
(`internal/harnessapi/models.go:111`); a missing or cross-tenant session answers **404**
(`internal/harnessapi/completion.go:203`).

## 1. Search

```bash
API=http://127.0.0.1:13750
KEY=keel_bot_xxx

# messages (the default scope)
curl -sS -G "$API/v1/sessions/search" -H "Authorization: Bearer $KEY" \
  --data-urlencode 'q=deploy' --data 'limit=20' --data 'kind=user'

# sessions (title or last message matched)
curl -sS -G "$API/v1/sessions/search" -H "Authorization: Bearer $KEY" \
  --data-urlencode 'q=deploy' --data 'scope=sessions'
```

| Parameter | Rule |
|---|---|
| `q` | required; at most 200 runes (`internal/harnessapi/session_query.go:49`) |
| `scope` | `messages` (default) or `sessions` |
| `kind` | `user`, `assistant`, or empty |
| `sessionId` | narrows a message search to one session; must be a UUID |
| `limit` | 1–100, default 20 (`internal/harnessapi/session_query.go:45`) |
| `after` | the previous page's `nextCursor`; malformed cursors are 400 |

**Visibility is a relation, not a tenant scope.** Candidates are filtered through the same
`Access.Visible(..., KeySessionRead, "session", ids)` seam the session list uses
(`internal/harnessapi/session_query.go:341`); a denied decision is **403** and a partially visible page
is filtered while its cursor stays valid. A deployment without the access surface fails closed with
**503** instead of returning every session in the tenant
(`internal/harnessapi/session_query.go:194`).

Hit titles are derived, never stored: the session's title is the first `user.message` text, collapsed
and truncated (`internal/sessiontitle/sessiontitle.go:1`). A hit and its list row therefore cannot
disagree.

**Current boundary.** The message predicate matches the payload's top-level `text` only
(`internal/harnessapi/session_query_pg.go:64`, `:88`). An assistant message persisted as
`{role, content:[{type:"text",…}]}` — the runner's real shape — is exported (§2) but is not matched by
message search. Search for a user message or export the session when that matters.

## 2. Export

```bash
# typed JSON page
curl -sS -G "$API/v1/sessions/<id>/export" -H "Authorization: Bearer $KEY" \
  --data 'afterSeq=0' --data 'limit=200'

# NDJSON download (same page, line-per-message)
curl -sS -G "$API/v1/sessions/<id>/export" -H "Authorization: Bearer $KEY" \
  --data 'format=ndjson' -o session.jsonl -D headers.txt
```

Export is a **bounded read of transcript text**, not a dump of the session. Only `user.message` and
`assistant.message` are exported, and an event with no text is dropped; tool payloads, selections,
attachments and the rest of the ledger never appear
(`internal/harnessapi/session_query_pg.go:293`, `:306`, `:310`).

Assistant text is derived from content blocks: every `type:"text"` block is joined with newlines, and
the legacy top-level `text` is the fallback (`internal/harnessapi/session_query_pg.go:293`). A
tool-only assistant message has no text and is excluded. This is the fix behind the export returning
only user messages in earlier builds.

| Bound | Value | Enforced at |
|---|---|---|
| Messages per page | 200 default, 500 max | `internal/harnessapi/session_query.go:54` |
| Characters per message | 32,768 (`textTruncated` when cut) | `internal/harnessapi/session_query_pg.go:345` |
| Bytes per page | 512KiB; the page stops at the last message that fits | `internal/harnessapi/session_query.go:446` |

Each page reports `nextAfterSeq`, `hasMore` and `truncated`; `hasMore` is true when either the row
bound or the byte bound stopped the page. NDJSON adds `X-Keel-Next-After-Seq` and `X-Keel-Has-More`
headers plus a `Content-Disposition` filename, so a client can page without parsing the body.

There is no "export everything" form: a caller that wants the whole transcript pages until
`hasMore=false`.

## 3. Deliverables

`GET /v1/sessions/{id}/deliverables` lists the session's file manifest — the same projection the
model's file tools write, never a second file truth (`internal/harnessapi/session_query.go:535`).

```bash
curl -sS -G "$API/v1/sessions/<id>/deliverables" -H "Authorization: Bearer $KEY" \
  --data 'limit=100'
```

- Rows are sorted by `path`; the cursor is an opaque base64 of the last path, so the page is
  keyset-stable while files are added (`internal/harnessapi/session_query.go:630`).
- Each row carries `hash`, `size`, `mode`, `updatedSeq`, `lastTaskId` and a `previewUrl` that points at
  the `file-preview` route for that exact `(path, hash)`.
- `limit` is 1–500, default 100 (`internal/harnessapi/session_query.go:63`).

## 4. Task panel

The panel is **one projection read** instead of four round trips: the tree, its member cards, the
parents outside the session, the live attachment references, and the task event stream
(`internal/harnessapi/task_panel.go:3`).

```bash
curl -sS -G "$API/v1/sessions/<id>/task-panel" -H "Authorization: Bearer $KEY" \
  --data 'memberLimit=200' --data 'eventLimit=500'
```

| List | Bound | Truncation fact |
|---|---|---|
| cards | 1000 default, 5000 max (`internal/tasklog/panel.go:33`) | `cardsTruncated` |
| members | 200 default, 500 max (`internal/tasklog/panel.go:35`) | `truncated`, `truncatedMemberCount` |
| events | 500 default, 2000 max (`internal/tasklog/panel.go:37`) | `eventsTruncated` |
| attachments | 1000 default, 5000 max (`internal/tasklog/panel.go:39`) | `attachmentsTruncated` |
| ancestors | 50 default, 200 max (`internal/tasklog/panel.go:41`) | `ancestorsTruncated` |

Ordering is deterministic — cards by creation sequence then task id, members by newest activity, events
by ledger sequence (`internal/tasklog/panel.go:9`) — so two reads of the same state render identically.
A card whose parent lives in another session appears in `ancestors`; the panel never copies that
session's ledger. Attachments are `(taskId, path, hash)` references only: the bytes stay in the file
layer and are resolved through the file routes.

## 5. File preview

```bash
curl -sS -G "$API/v1/sessions/<id>/file-preview" -H "Authorization: Bearer $KEY" \
  --data-urlencode 'path=report.md' --data 'offset=0' --data 'limit=200' \
  --data 'hash=<hash-from-deliverables>'
```

The path is resolved through the session's own manifest, so a preview cannot name a file the session
does not hold (`internal/harnessapi/task_panel.go:117`). The answer carries the manifest's current
`hash`; when the caller passes `hash=`, the response also carries `referenceHash` and
`matchesReference` — `false` means the file changed since the reference was recorded, and the panel
must say so rather than pretend the old bytes were read (`internal/harnessapi/task_panel.go:139`).

The page is the same bounded line read the model's `read` tool gets: 200 lines / 256KiB by default,
2000 lines max (`internal/fileplugin/read.go:13`, `:14`). A missing path is 404; a bad parameter
is 400.

## 6. What these routes deliberately do not do

- **No whole-session dump.** Export pages; there is no size-unbounded form.
- **No write path.** The task panel is a read-only fold over `task_cards`/`task_events`; deliverables
  and preview read `file_manifest`; search and export read `events`.
- **No cross-tenant reads.** Every query carries `tenant_id`; a session id from another tenant is a
  404, not an empty page.
- **No invented titles.** Titles come from `internal/sessiontitle`; nothing stores a rename.

See [Files and blobs](/core/files) for the manifest/tree/blob model behind deliverables and preview,
and [Harness API reference](/core/agentapi) for the rest of the `/v1` surface.
