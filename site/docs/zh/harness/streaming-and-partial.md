# 流式与 partial

Harness 会话以流的形式被消费。本页描述客户端可以观察的两个通道——已提交事件与 token 级
partial 旁路——以及围绕它们的交付保证：续传游标、唤醒、心跳、准入与 drain 帧。

## 1. 两个通道，一个权威

| 通道 | 端点 | 承载 | 游标影响 | 权威性 |
| --- | --- | --- | --- | --- |
| 已提交事件 | `GET /v1/sessions/{id}/events/stream` | durable 账本事件 | 每帧 `id: <seq>` | **唯一真值** |
| partial 增量 | 同一端点 + `?partials=1` | 在途 token 文本/thinking | 无——partial 帧不带 `id:` | 咨询性、可丢弃 |

这条不变量写在传输层本身：partial 帧不是事件——永不入库、永不重放、永不携带 SSE `id`，
因此无法移动客户端的 `Last-Event-ID` 游标；从游标续传的客户端绝不会看到同一条 partial 两次
（`internal/httpapi/partials.go:5`、`internal/httpapi/server.go:690`）。

## 2. 已提交流

`streamEvents`（`internal/httpapi/server.go:628`）按顺序做这些事：

1. **先划租户边界**——跨租户或不存在的会话在写任何 SSE 头*之前*返回 404；一条会以 200 空
   体永不完结的流被拒绝（`internal/httpapi/server.go:648`）。
2. **准入**——token bucket 准入器为新流设界（429 + `Retry-After`），控制车道领取调度槽位；
   耗尽同样如此应答（`internal/httpapi/server.go:653`、`:662`）。
3. **响应头**——`text/event-stream`、`no-store`、`X-Accel-Buffering: no`，随后立刻 flush，
   让客户端看到流确实活着。
4. **唤醒**——handler 订阅进程内 hub，由提交时的 PostgreSQL `LISTEN/NOTIFY` 唤醒；5 秒兜底
   定时器覆盖通知丢失，15 秒心跳防止中间层回收空闲流（`internal/httpapi/hub.go:86`、`internal/httpapi/server.go:715`）。
5. **帧**——已提交事件写为 `id: <seq>\nevent: <kind>\ndata: <payload>\n\n`；`seq` 即续传
   游标（`internal/httpapi/server.go:628`）。

客户端用 `Last-Event-ID`（请求头）或 `?after=`（查询参数）续传；两者汇入同一解析器
（`internal/httpapi/server.go:639`）。非流式快照是
`GET /v1/sessions/{id}/events?after=&limit=`。

### 遇到缺口时客户端必须做什么

投影按契约保持防御姿态：`seq > lastSeq + 1` 的事件进入缓冲（容量 4096）并置
`needsResync()`；客户端随后重新拉取缺失区间（`list(after=lastSeq)` 分页 → 应用 → 再信任
流），而不是用实时帧修补视图。完整规则见 `packages/chat-surface/src/engine.ts` 与
`docs/CHAT-SURFACE.md` §6。

## 3. 反压

流路径端到端有界，因为产品的目标是在 2GB 内支撑一千个并发会话
（`docs/ARCHITECTURE.md` §1）：

- 阻塞式 `Write` 本身就是反压——慢客户端让生产者停下来，而不是把整段历史缓存在内存里；
- 调度器把类别隔开（控制/SSE 小帧 vs 图片/大对象流），1MiB 图片不能排在审批帧前面
  （`internal/stream/budget.go:153`）；
- 每写截止与进度截止杀掉停滞客户端；已发送 offset 被记录，重连客户端因此是续传而非重来
  （`internal/stream/guard.go:21`）；
- blob（图片、文件）从存储流式输出——分块读 → `base64` → socket——绝不整对象进内存
  （`README.md`、`docs/ARCHITECTURE.md` §5）。

过载永远以诚实的 429/503 + `Retry-After` 呈现，绝不静默丢弃已提交事实。

## 4. Partials：token 级旁路

这条旁路的存在，是为了让 UI 在文本生成过程中即可显示，同时不削弱日志。它刻意很窄：

```
provider 流（模型的 SSE 帧）
   │
   ▼
agentext.StreamTee            包裹 provider StreamFn；提取 text/thinking 增量
   │                          internal/agentext/streamtee.go:24
   ▼
agentext.PartialHub           进程本地，按 (tenant, session)；有界缓冲（16）
   │  Publish / Subscribe     internal/agentext/turns.go:360、:386、:407
   ▼
agentapi.PartialSource        把 hub 适配为 HTTP 帧类型
   │                          internal/agentapi/partials.go:16
   ▼
SSE ?partials=1               event: partial, data: {...}，无 id 行
                              internal/httpapi/partials.go:16
```

性质，逐条照设计原文：

- **非 durable**——partials 永不进入事件日志，也不出现在 `GET /events`；重连客户端无法重放
  它们（`internal/agentext/doc.go`）。
- **可丢弃**——hub 是有界环形缓冲；慢消费者丢帧而不是阻塞生产者
  （`internal/agentext/turns.go:373`、`internal/httpapi/partials.go:28`）。
- **无游标**——帧没有 `id:`，`Last-Event-ID` 不受影响；随后到达的已提交
  `assistant.message` 是权威副本，并清除客户端上的 partial
  （`packages/chat-surface/src/engine.ts`）。
- **显式选择**——只有请求带 `?partials=1` 时才咨询生产者；`GET /v1/capabilities` 报告该部署
  是否有 partial 源（`internal/agentapi/api.go:262`）。

进程若需要知道 partial 是否在流动，hub 通过其 `Stats` 暴露计数器（published、dropped、
subscribers）（`internal/agentext/turns.go:430`）。

## 5. 流的排水

正在排水的进程不会静默切断流。每条打开的 SSE 流都会收到最后一帧告知：

```
event: drain
data: {"retry_after_ms": 3000}
```

随后连接按限速节奏关闭，避免整批客户端同时重连造成惊群
（`internal/httpapi/drain.go:129`、`:159`）。排水期间新 API 请求得到 503，而探针、metrics 与
SSE flush 路径保持可达（`internal/httpapi/drain.go:105`）。客户端重连到新 runtime 并从游标
续传：重连后的第一帧是 `cursor + 1`——跨 handoff 的已提交事件不丢不重
（`docs/HANDOFF.md`「怎么用」）。

## 6. 验收证据

流式行为由测试钉住，而不是靠文字承诺：

- `internal/httpapi/ops_test.go`——SSE 帧格式、心跳与游标行为；
- `internal/httpapi/server_test.go`——写头前的租户边界、用 `after`/`Last-Event-ID` 续传；
- `internal/agentapi/partials_sse_test.go`——partial 帧不带 `id`、不移动游标；
- `cmd/e2e`——跨进程无状态场景（blob 续传、2000 事件序列无缺口），摘要见 `README.md`；
- `packages/chat-surface/test`——10k 事件投影上界与重连/重新同步状态机
  （`docs/CHAT-SURFACE.md` §8）。
