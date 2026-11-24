---
title: 会话查询
---

# 会话查询

五条只读路由让会话的历史、文件与任务可被读取，而不必自己重读账本：

| 路由 | 返回什么 | 边界 |
|---|---|---|
| `GET /v1/sessions/search` | 会话或消息命中的一页 | `limit` 20（最大 100），`q` ≤ 200 字符 |
| `GET /v1/sessions/{id}/export` | 会话文本记录的一页 | 200 条（最大 500），每条 32KiB，每页 512KiB |
| `GET /v1/sessions/{id}/deliverables` | 会话文件 manifest 的行 | 100（最大 500） |
| `GET /v1/sessions/{id}/task-panel` | 任务树、成员、祖先、附件、事件 | 成员 200/500，事件 500/2000 |
| `GET /v1/sessions/{id}/file-preview` | 单个会话文件的有界分页 | 200 行（最大 2000），256KiB |

路由由 `sessionQueryRoutes`（`internal/harnessapi/session_query.go:176`）与
`taskPanelRoutes`（`internal/harnessapi/task_panel.go:142`）注册。没有一条会写状态：search 读事件、
export 读事件、deliverables 与 file-preview 读 `file_manifest`、任务面板读任务投影。接入面未配置时
返回 **501** 与 `status: "not_wired"`（`internal/harnessapi/models.go:111`）；会话不存在或跨租户
返回 **404**（`internal/harnessapi/completion.go:203`）。

## 1. Search

```bash
API=http://127.0.0.1:13750
KEY=keel_bot_xxx

# 消息（默认 scope）
curl -sS -G "$API/v1/sessions/search" -H "Authorization: Bearer $KEY" \
  --data-urlencode 'q=deploy' --data 'limit=20' --data 'kind=user'

# 会话（标题或最后一条消息命中）
curl -sS -G "$API/v1/sessions/search" -H "Authorization: Bearer $KEY" \
  --data-urlencode 'q=deploy' --data 'scope=sessions'
```

| 参数 | 规则 |
|---|---|
| `q` | 必填；最多 200 字符（`internal/harnessapi/session_query.go:49`） |
| `scope` | `messages`（默认）或 `sessions` |
| `kind` | `user`、`assistant` 或空 |
| `sessionId` | 把消息搜索收窄到一个会话；必须是 UUID |
| `limit` | 1–100，默认 20（`internal/harnessapi/session_query.go:45`） |
| `after` | 上一页的 `nextCursor`；非法游标 400 |

**可见性是关系，不是租户范围。** 候选行经过与会话列表同一个
`Access.Visible(..., KeySessionRead, "session", ids)` 接缝过滤
（`internal/harnessapi/session_query.go:341`）；拒绝即 **403**，部分可见则过滤该页而游标仍有效。
没有 access 接入面的部署 fail-closed 返回 **503**，而不是返回租户全部会话
（`internal/harnessapi/session_query.go:194`）。

命中里的标题是派生的，不是存储的：会话标题取第一条 `user.message` 的文本，折叠空白并截断
（`internal/sessiontitle/sessiontitle.go:1`）。因此命中与其列表行不可能互相矛盾。

**当前边界。** 消息谓词只匹配 payload 顶层 `text`（`internal/harnessapi/session_query_pg.go:64`、
`:88`）。以运行时的真实形态 `{role, content:[{type:"text",…}]}` 落库的助手消息会被导出（见 §2），
但不会被消息搜索命中。需要时请搜 user 消息，或直接导出会话。

## 2. Export

```bash
# 类型化 JSON 页
curl -sS -G "$API/v1/sessions/<id>/export" -H "Authorization: Bearer $KEY" \
  --data 'afterSeq=0' --data 'limit=200'

# NDJSON 下载（同一页，一行一条消息）
curl -sS -G "$API/v1/sessions/<id>/export" -H "Authorization: Bearer $KEY" \
  --data 'format=ndjson' -o session.jsonl -D headers.txt
```

导出是**有界的文本记录读取**，不是会话全量转储。只导出 `user.message` 与 `assistant.message`，
没有文本的事件被丢弃；工具 payload、selection、附件以及账本其余内容都不会出现
（`internal/harnessapi/session_query_pg.go:293`、`:306`、`:310`）。

助手文本由 content 块派生：所有 `type:"text"` 块以换行连接，旧的顶层 `text` 作为兜底
（`internal/harnessapi/session_query_pg.go:293`）。只有工具调用块、没有文本的助手消息被排除。
这正是早期版本「导出只有用户消息」的修复点。

| 边界 | 值 | 执行位置 |
|---|---|---|
| 每页消息数 | 默认 200，最大 500 | `internal/harnessapi/session_query.go:54` |
| 每条字符数 | 32,768（被截断时带 `textTruncated`） | `internal/harnessapi/session_query_pg.go:345` |
| 每页字节数 | 512KiB；页在最后一条放得下的消息处停止 | `internal/harnessapi/session_query.go:446` |

每页返回 `nextAfterSeq`、`hasMore`、`truncated`；行数上限或字节上限任一触发，`hasMore` 都为 true。
NDJSON 额外给 `X-Keel-Next-After-Seq`、`X-Keel-Has-More` 与 `Content-Disposition` 文件名，
客户端不必解析正文即可翻页。

不存在「一次导出全部」的形态：要整份记录的调用方按 `hasMore=false` 翻到末页为止。

## 3. Deliverables

`GET /v1/sessions/{id}/deliverables` 列出该会话的文件 manifest——与模型文件工具写入的是同一个投影，
不是第二份文件真值（`internal/harnessapi/session_query.go:535`）。

```bash
curl -sS -G "$API/v1/sessions/<id>/deliverables" -H "Authorization: Bearer $KEY" \
  --data 'limit=100'
```

- 按 `path` 排序；游标是最后一条 path 的 base64 不透明串，因此在文件持续新增时仍是稳定的
  keyset 分页（`internal/harnessapi/session_query.go:630`）。
- 每行带 `hash`、`size`、`mode`、`updatedSeq`、`lastTaskId`，以及指向该 `(path, hash)` 的
  `previewUrl`。
- `limit` 1–500，默认 100（`internal/harnessapi/session_query.go:63`）。

## 4. 任务面板

面板是**一次投影读取**，而不是四次往返：任务树、成员卡、会话外的父卡、活跃附件引用与任务事件流
（`internal/harnessapi/task_panel.go:3`）。

```bash
curl -sS -G "$API/v1/sessions/<id>/task-panel" -H "Authorization: Bearer $KEY" \
  --data 'memberLimit=200' --data 'eventLimit=500'
```

| 列表 | 边界 | 截断事实 |
|---|---|---|
| cards | 默认 1000，最大 5000（`internal/tasklog/panel.go:33`） | `cardsTruncated` |
| members | 默认 200，最大 500（`internal/tasklog/panel.go:35`） | `truncated`、`truncatedMemberCount` |
| events | 默认 500，最大 2000（`internal/tasklog/panel.go:37`） | `eventsTruncated` |
| attachments | 默认 1000，最大 5000（`internal/tasklog/panel.go:39`） | `attachmentsTruncated` |
| ancestors | 默认 50，最大 200（`internal/tasklog/panel.go:41`） | `ancestorsTruncated` |

顺序是确定的——cards 按创建序号再按 task id，members 按最新活动，events 按账本序号
（`internal/tasklog/panel.go:9`）——因此同一状态两次读取渲染一致。父卡在别的会话时出现在
`ancestors` 里；面板绝不复制那个会话的账本。附件只是 `(taskId, path, hash)` 引用：字节仍在文件层，
经文件路由解析。

## 5. 文件预览

```bash
curl -sS -G "$API/v1/sessions/<id>/file-preview" -H "Authorization: Bearer $KEY" \
  --data-urlencode 'path=report.md' --data 'offset=0' --data 'limit=200' \
  --data 'hash=<hash-from-deliverables>'
```

路径经会话自己的 manifest 解析，因此预览不可能点名会话并不持有的文件
（`internal/harnessapi/task_panel.go:117`）。返回携带 manifest 当前 `hash`；调用方传 `hash=` 时，
响应还会带 `referenceHash` 与 `matchesReference`——`false` 表示该文件在引用记录之后已变化，
面板必须如实说明，而不是假装读到了旧字节（`internal/harnessapi/task_panel.go:139`）。

分页与模型 `read` 工具拿到的是同一条有界行读取：默认 200 行 / 256KiB，最多 2000 行
（`internal/fileplugin/read.go:13`、`:14`）。路径不存在 404；参数非法 400。

## 6. 这些路由刻意不做的事

- **不做全量转储。** 导出是分页的，没有无上界形态。
- **没有写路径。** 任务面板是对 `task_cards`/`task_events` 的只读折叠；deliverables 与 preview 读
  `file_manifest`；search 与 export 读 `events`。
- **没有跨租户读取。** 每条查询都带 `tenant_id`；别的租户的会话 id 是 404，而不是空页。
- **不发明标题。** 标题来自 `internal/sessiontitle`，没有任何重命名存储。

deliverables 与 preview 背后的 manifest/tree/blob 模型见[文件与 Blob](/zh/core/files)；
`/v1` 其余接口见 [Harness API 参考](/zh/core/agentapi)。
