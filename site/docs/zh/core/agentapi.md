# AgentAPI 参考

`AgentAPI` 是 HTTP 协议名。按产品口径它就是 **Harness API**：由 Harness 提供服务，协议名保留原样
（见[术语表](/zh/glossary)）。本页是端点参考与请求/响应形状；下面的示例全部取自运行中的
`keel serve`（所述修订）。

## 1. 两个 HTTP 面，一个进程壳

| 面 | 包 | 用途 | 谁提供 |
|---|---|---|---|
| **Core 壳** | `internal/httpapi` | 内核直连面：租户、会话、输入、事件、SSE、run 行、文件、运行时排水 | `cmd/runtime`（运行时）与 `cmd/keel`（产品子集） |
| **Harness API** | `internal/agentapi` | 产品面：会话列表、取消、审批、插件、触发器、子代理、能力探测 | `cmd/keel serve` |

两者都挂在 `/v1` 下；破坏性形状变更需要 `/v2`。新能力只增不改，由客户端发现——见 §6。

认证按部署不同，且属于网络中间件、永不属于内核：

- **`keel serve`**：浏览器用 HMAC 签名的 HttpOnly cookie，CLI/TUI 用
  `Authorization: Bearer <passphrase>`。服务端注入租户并剥离客户端传来的 `X-Tenant-ID`
  （`docs/WEBUI.md`、`cmd/keel/auth.go`）。`--password ""` 关闭认证（仅回环开发），此时只回答回环 Host。
- **`cmd/runtime`**：每个请求用 `X-Tenant-ID` 指定租户；管理员端点额外要求
  `Authorization: Bearer <KEEL_ADMIN_KEY>`。`X-Tenant-ID` 是行 id，从不是身份凭证；未配置 admin key
  时管理员 API 整体关闭（503）。

## 2. Core 壳端点

来自 `internal/httpapi/server.go:146`：

| 方法 | 路径 | 语义 |
|---|---|---|
| `GET` | `/v1/health` | 存活；恒 200，含 goroutine/heap 计数 |
| `GET` | `/v1/ready` | PG Ping + 运行 schema 检查（表与内核迁移列）；未就绪或排水中返回 503 + `Retry-After` |
| `GET` | `/v1/metrics` · `GET` `/metrics` | JSON 快照与 Prometheus text 0.0.4；标签基数固定，禁止 tenant/session/run 标签 |
| `POST` | `/v1/tenants` | 建租户（admin key）；body `{"name":"…"}`，id 来自 `X-Tenant-ID` |
| `PUT`/`GET` | `/v1/tenants/{id}/config` | 替换/读取租户配置文档；未知字段或非法值 400，body 超限 413，租户不存在 404 |
| `GET` | `/v1/tenants/{id}/usage` | 按 session/provider/model 分组的 token 计数与总计（只有计数，无金额） |
| `POST` | `/v1/sessions` | 建会话；返回 `{"sessionId":"…"}` |
| `POST` | `/v1/sessions/{id}/input` | 提交输入；body `{"text","requestId?","delivery?:queue\|guide","selection?"}` |
| `POST` | `/v1/sessions/{id}/retry` | 重问早先的输入；账本记录 `retryOf` 与早先的 seq |
| `POST` | `/v1/sessions/{id}/run` | 不新增输入也入队一个 run（缺失/跨租户会话 404，且不为它建 runs 行） |
| `GET` | `/v1/sessions/{id}/run` | run 行：`status`/`attempts`/`cursorSeq`/`lastError`/`leaseUntil` |
| `GET` | `/v1/sessions/{id}/run/loadout` | finish 时记录的 loadout pin：`loadoutId`/`revision`/`generation`/`ownerEpoch` |
| `POST` | `/v1/sessions/{id}/events` | 追加事件——只接受 `user.message`，其他 400；跨租户/不存在 404 |
| `GET` | `/v1/sessions/{id}/events` | 已提交事件，带 `after`/`limit`（快照与续传） |
| `GET` | `/v1/sessions/{id}/events/stream` | SSE；`after`/`Last-Event-ID` 续传，`partials=1` 开启 token 帧 |
| `POST` | `/v1/files` | 上传 blob（multipart 或 raw body）；返回 `fileId` + `url` |
| `GET` | `/v1/files/{id}` · `/v1/files/{id}/meta` | 流式读字节（Range/offset 续传）/ 元数据 |
| `POST` | `/v1/mcp/servers/{name}/signin` · `/v1/mcp/callback` | MCP OAuth 登录起点与回调（缺 `KEEL_SECRET_KEY` 等配置时 fail-closed） |
| `POST` | `/v1/runtime/drain` · `GET` `/v1/runtime/status` | 显式排水（`{"mode":"handoff"\|"shutdown","deadlineMs":N}`）与运行时可观测 |

实测响应形状：

```json
// POST /v1/sessions
{"sessionId":"a681bd12-b772-462f-b4ce-122b69977818"}

// POST /v1/sessions/{id}/input
{"created":true,"requestId":"9e5088a0-892c-4af3-b123-43c8a6c7fcf4","seq":1,"status":"committed","submissionId":"05c9689a-f556-45cf-8996-9b08c56144e0"}

// GET /v1/sessions/{id}/run
{"attempts":1,"cursorSeq":3,"lastError":"","leaseUntil":null,"status":"done"}
```

## 3. Harness API 端点

来自 `internal/agentapi/api.go:167`：

| 方法 | 路径 | 语义 |
|---|---|---|
| `GET` | `/v1/capabilities` | 能力文档（§6） |
| `GET` | `/v1/providers` · `GET` `/v1/models` | 已配置的 provider 与模型（产品配置） |
| `GET` | `/v1/sessions?limit=&after=` | 会话列表，keyset 分页，带 `runStatus` overlay |
| `POST` | `/v1/sessions/{id}/cancel` | 停止当前 attempt：幂等；`cancelling` 时 202，外部会话 409，未知 404，seam 未接线（跨进程）501 |
| `GET`/`POST` | `/v1/sessions/{id}/decisions` | 列出 / 打开审批 decision |
| `GET`/`POST` | `/v1/sessions/{id}/decisions/{decisionId}` | 读取 / 作答（幂等；重复相同答案返回 `answered` 且不产生新事件；冲突 409） |
| `GET` | `/v1/plugins` | 已装配插件、generation、hooks 与工具清单 |
| `GET` | `/v1/triggers` · `POST` `/v1/triggers/{id}/fire` | 触发器审计与手动触发（带幂等键） |
| `GET` | `/v1/subagents` · `GET` `/v1/subagents/{id}` | 子代理列表 / 详情 |
| `POST` | `/v1/sessions/{id}/subagents` | 派生子代理（`wait:true` 同步等待；断线不取消子代理） |

实测的列表与插件响应（有省略）：

```json
// GET /v1/sessions?limit=2
{"hasMore":false,"nextCursor":"","sessions":[
  {"sessionId":"a681bd12-…","createdAt":"2026-11-10T03:48:24Z","updatedAt":"…","lastSeq":0,"runStatus":"idle","lastMessageAt":null,"title":""},
  {"sessionId":"586897e3-…","createdAt":"…","updatedAt":"…","lastSeq":3,"runStatus":"done","lastMessageAt":"…","title":"hello keel"}]}

// GET /v1/plugins   （serve 进程默认注册一个 example 插件）
{"generation":1,
 "hooks":{"subscriptions":3,"emitted":0,"delivered":0,"dropped":0,"failed":0,"queued":0,"capacity":1024},
 "plugins":[{"id":"keel.example","version":"1.0.0","state":"active",
             "provides":["cap:example.status"],
             "tools":["example_audit_tail","example_status"],
             "hooks":["trigger.after","decision.answered","subagent.finished"], …}]}
```

## 4. 外部（AgentBackend）端点

| 方法 | 路径 | 语义 |
|---|---|---|
| `POST` | `/v1/external/sessions` | 打开外部会话；body `{"backend":"dsh","reference":"manual-1"}` |
| `GET` | `/v1/external/sessions` · `/{id}` | 列表 / 查询；查询会给出兼容性标记（`stateOwner`、`externalStatus`、`metering`、`nativeRecovery:false`） |
| `POST` | `/v1/external/sessions/{id}/messages` | 向 backend 发消息；返回 `{"turnId":"…","recorded":true}` |
| `POST` | `/v1/external/sessions/{id}/cancel` · `/{id}/close` | 取消一个 turn / 关闭会话 |
| `GET` | `/v1/external/backends` | backend 清单与健康状态 |

外部会话永不进入原生 run/lease 流；它们的事件是 Keel 的兼容日志（`external.event`）。完整协议与其
诚实登记的缺口见[接外部 backend](/zh/core/extend/external-backend)。

## 5. 错误与诚实拒绝

| 状态码 | 本 API 上的含义 |
|---|---|
| 400 | 请求形状错误，或内核会拒绝的载荷 |
| 401 | 凭据缺失/无效，或 `X-Tenant-ID` 缺失/非 UUID |
| 404 | 本租户视角下不存在的会话/租户（别家租户的资源给出完全相同的回答） |
| 409 | 状态冲突：原生端点遇到外部会话、decision 已作答、向外部会话写原生 edge |
| 413 | 请求体超过配置上限 |
| 429 | 准入反压：队列满，按 `Retry-After` 重试 |
| 501 | 本进程未接线该操作（例如跨进程 cancel）——API 拒绝，而不是假装成功 |
| 503 | 未就绪、排水中，或某产品 seam 未接线（"decisions are not wired"、"triggers are not wired"） |
| 502 | 子代理或 backend 失败 |

两个拒绝是刻意设计而非缺陷：**跨进程 cancel 返回 501**（取消注册在正在执行 attempt 的进程里），
**未接线 seam 返回 503**（同时能力探测里对应 flag 为 `false`，客户端据此隐藏控件）。

## 6. 能力探测与降级

能力文档是客户端获知「本部署提供什么」的唯一正确途径（`internal/agentapi/api.go:249`）。实测响应：

```json
{"apiVersion":"v1","serviceVersion":"keel-agent/0.1.0",
 "features":{"decisions":true,"hooks":true,"partials":true,"plugins":true,
             "sessions.cancel":true,"sessions.list":true,"settings.models":true,
             "settings.secrets":false,"subagents":true,"triggers":true},
 "limits":{"sessionsPageDefault":50,"sessionsPageMax":200,"subagentsPageMax":200},
 "hooks":["subagent.started","subagent.finished","trigger.before","trigger.after",
          "turn.started","turn.finished","decision.opened","decision.answered"]}
```

客户端应遵守：

- 把 `features.*` 当开关：为 `false` 即隐藏控件，而不是「试一下看看」。
- 把 `hooks` 当闭集。hook 词汇是刻意闭合的；订阅列表之外的名称是配置错误，不是扩展点。
- 不要从 `serviceVersion` 推断特性；二进制版本与 `/v1` 独立演进。

## 7. SSE 契约（摘要）

完整语义在[事件与协议](/zh/core/events)；运维要点：

| 属性 | 保证 |
|---|---|
| 帧上的 `id:` | 已提交事件的 `seq`——可用 `Last-Event-ID`/`after` 续传 |
| token partial 帧（`partials=1`） | `event: partial`，**不带** `id:`，不落库、不重放 |
| drain | 先补发未读事件，再发 `event: drain` 带 `{"retry_after_ms":N}`；用 `Last-Event-ID` 重连 |
| 顺序 | `seq` 严格递增，无缺口、不回退 |
| 唤醒 | `LISTEN/NOTIFY` + 心跳；丢一次通知只损失一个心跳周期，永不丢数据 |
