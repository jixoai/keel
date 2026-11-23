# Session Query（历史检索与有界导出）

本 spec 定义 `keel.session-query` 的读模型：`GET /v1/sessions/search` 的有界检索与
`GET /v1/sessions/{id}/export` 的有界导出。两者只读事件账本，MUST 带租户/会话作用域与分页，
MUST NOT 提供整会话 dump。实现真值：`internal/harnessapi/session_query.go`（路由与边界）、
`internal/harnessapi/session_query_pg.go`（PostgreSQL 读）、`web/src/data/session-query.ts`
（客户端）。

## ADDED Requirements

### Requirement: 有界检索与会话/消息两种作用域

`GET /v1/sessions/search?q=&scope=&limit=&after=` MUST 支持
`scope=messages`（默认，检索 user/assistant 消息正文）与 `scope=sessions`（检索派生标题与
最后一条消息）。`q` MUST 非空且不超过 200 个码点；`limit` MUST 默认 20、最大 100；分页 MUST
是 keyset（消息按 `(created_at, session_id, seq)`，会话按 `(updated_at, session_id)`），
MUST NOT 使用 OFFSET。消息作用域 MAY 另带 `sessionId` 与 `kind=user|assistant` 收窄。
归档会话 MUST 不出现在任何结果里。

#### Scenario: 消息命中
- **WHEN** 调用 `GET /v1/sessions/search?q=退款&scope=messages&limit=20`
- **THEN** 返回至多 20 条 `{sessionId, sessionTitle, seq, kind, role, at, snippet}`；`nextCursor`
  只在还有下一页时出现，同一组结果翻页不重复、不遗漏

#### Scenario: 会话命中
- **WHEN** 调用 `GET /v1/sessions/search?q=发布&scope=sessions`
- **THEN** 返回匹配派生标题或最后消息的会话 `{sessionId, title, createdAt, updatedAt,
  lastMessageAt, snippet}`；归档会话不出现

#### Scenario: 参数拒绝
- **WHEN** `q` 为空、超过 200 码点，或 `limit` 为 0/101，或 `kind=system`
- **THEN** 回答 400；MUST NOT 用默认值静默继续

### Requirement: 检索不越过租户与会话可见性

每一页候选 MUST 先按 `tenant_id` 读取，再经与 `GET /v1/sessions` 相同的
`access.Visible(KeySessionRead, "session", ids)` 过滤：不可见的会话 MUST NOT 出现在结果里；
键级拒绝 MUST 回答 403，MUST NOT 用空列表冒充「没有结果」。部署未接可见性 seam 时 MUST
fail closed（503），MUST NOT 返回租户全部会话。

#### Scenario: 共享可见性
- **WHEN** 调用者只能读租户内的一个会话，而租户里另一个会话也有匹配消息
- **THEN** 结果只含可读会话；不可读会话既不出现也不泄露标题/片段

#### Scenario: 无可见性 seam
- **WHEN** 部署未接线 `access.Visible`
- **THEN** 检索回答 503，不返回任何行

### Requirement: 导出有界且只导出转写文本

`GET /v1/sessions/{id}/export?afterSeq=&limit=&format=json|ndjson` MUST 只导出该会话的
`user.message` / `assistant.message` 文本；MUST NOT 导出 tool 参数/结果、selection、attachments
或账本里的其它事件。`limit` MUST 默认 200、最大 500；单条消息文本 MUST 截到 32 KiB 并标
`textTruncated`；单页文本总量 MUST 不超过 512 KiB。`format=json` MUST 返回 typed page
`{sessionId, title, messages, nextAfterSeq, hasMore, truncated, limits}`；
`format=ndjson` MUST 返回同一页的逐行 JSON，并带 `X-Keel-Next-After-Seq` / `X-Keel-Has-More`。
会话 MUST 先按租户校验存在性：缺失或跨租户 MUST 404。

#### Scenario: 第一页
- **WHEN** 一个会话有 250 条文本消息，调用 `export?limit=200`
- **THEN** 返回 200 条、`nextAfterSeq` 为第 200 条 seq、`hasMore=true`；MUST NOT 一次返回 250 条

#### Scenario: 跨租户
- **WHEN** 调用者用另一个租户的会话 id 导出
- **THEN** 回答 404，响应体不含该会话的消息或标题

#### Scenario: 工具输出不导出
- **WHEN** 会话里有 `tool.result` 的文本
- **THEN** 导出页不包含该条；只有 user/assistant 消息出现

#### Scenario: 助手回复按 content 块导出
- **WHEN** 会话里有一条 `assistant.message`，payload 为运行时的真实形态
  `{role, content:[{type:"text", text:"答复"}], inputSeqs, stopReason}`（没有顶层 `text` 字段）
- **THEN** 导出页包含该条，`text` = 各 `type:"text"` 块的文本（多块以换行连接）；
  只有 `toolCall` 块、没有文本块的助手消息 MUST NOT 出现；旧形态（顶层 `text`）仍 MUST 导出


### Requirement: 客户端检索与导出入口

客户端 MUST 在会话列表的搜索框提供服务器检索入口：输入不少于 2 个字符后 MUST 读取一页
消息命中并列出 `sessionTitle + snippet`，点击 MUST 打开对应会话；还有下一页时 MUST 提供
「继续搜索」。部署没有该能力时入口 MUST 不出现，本地列表过滤仍可用。客户端 MUST 提供
「导出会话」动作：调用一次有界导出并把返回的 JSONL 交给剪贴板；MUST NOT 在客户端拼出
服务端没有返回的消息。

#### Scenario: 搜索并打开
- **WHEN** 在搜索框输入「发布准备」
- **THEN** 出现「消息结果」与命中行；点击命中打开该会话

#### Scenario: 导出
- **WHEN** 在一个有消息的会话上执行「导出会话」
- **THEN** 剪贴板得到该页 JSONL；提示写明复制了多少条、是否还有更多
