# internal/kernel — 契约内核

> 变更来源：`openspec/changes/archive/keel-service-graph`（W1 契约内核与持久化边界），
> 含 2026-11-07「审查后收敛（P0）」——**session 行是租约的单一线性化点**，
> 以及实现级复审后的三项加固（claim 不可复活已结算 run / 运行路径瞬时失败重试 /
> 配对查找有界化）。

契约内核是**事件账本的唯一写入口**与**运行租约的唯一签发者**。它是宪法级组件，
不是插件：服务图内的服务只能拿到下面这几个窄接口，拿不到表句柄、拿不到 store、
也不能自己分配 seq。

```
                 ┌───────────────────────────────┐
  服务（loop/tools/llm/…） ──► Committer / Reader ─┤
                 │            LeaseManager        │  kernel.Kernel
  worker / handoff ────────► │                   │
                 └───────────┬───────────────────┘
                             │ LogStore（窄持久化 seam）
                  ┌──────────┴──────────┐
              MemStore（测试）        PGStore（生产，sessions 行锁）
```

## API 面

| 类型 / 接口 | 作用 |
|---|---|
| `Scope` | tenant + session；会话身份是这一对，同 UUID 跨租户是两个互不可见的会话 |
| `EventKind` | 已注册事件种类：`user.message` / `assistant.message` / `tool.result` / `compaction` / `run.started`（保留）/ `run.failed`（保留）。保留项有词表与围栏但**无发射者**：run 的起止已由 runs 行记录，发射会改动客户端可见事件流（R1 平价门钉着），故暂不发射 |
| `CommitClass` | `ClassOwner`（owner-scoped）与 `ClassEdge`（edge-scoped），见下 |
| `OwnerToken` / `Lease` | fencing token（owner + epoch）与带过期时间的租约；`Lease.Reclaimed` 标记「接管了过期租约」（给 worker 指标用，不参与 fencing） |
| `CommitRequest` / `Receipt` | 一次提交（含幂等键）与其回执（seq、kind、是否复用已有事实） |
| `Committer` | `Commit` + `Lookup`：写入与「该 generation 是否已提交」查询 |
| `Reader` | `LoadEvents` + `UnresolvedToolCalls`：读侧投影 |
| `LeaseManager` | `Claim` / `ClaimScope` / `Renew` / `Finish` / `Release` / `LeaseState` |
| `LogStore` / `SessionTxn` | 持久化 seam（MemStore / PGStore 实现），**不交给任何插件** |
| `Limits` / `AuditFunc` / `Options.OnCommit` | 可选准入与审计/提交观察钩子（内核不内置配额与指标策略） |
| `Error` + 哨兵错误 | `errors.Is(err, kernel.ErrStaleEpoch)` 等；`Error` 携带 op/scope/kind/token 供审计 |

`kernel.New(log LogStore, Options{})` 构造 `*Kernel`，它同时实现
`Committer` / `Reader` / `LeaseManager`（`seams.go` 里的窄接口声明，运行时的
worker / runner / httpapi 只依赖这些接口，不依赖 `*Kernel` 或 store）。
`Options.OnCommit` 是提交观察钩子：生产路径的事件不再经过
`store.AppendEvent`，因此 `keel_compactions_total` 之类的指标改由此钩子喂数。

## 单一线性化点与锁序

- 租约权威在 **session 行**（`sessions.owner / owner_epoch / lease_until`）——与 seq
  分配同一把行锁；`Claim` / `Renew` / `Commit` / `Finish` / `Release` 全部以它为线性化点。
- owner-scoped `Commit` 在持锁事务内**重读 epoch**：claim 不可能插在「校验」与「追加」之间。
- 所有事务统一锁序 **sessions → runs**；`runs.owner / owner_epoch / lease_until` 只是
  给旧队列扫描（`store.ClaimRun`）与 run status 读取器的镜像，只在持 session 锁时写。
- 全局 `Claim` 用 `FOR UPDATE OF sessions SKIP LOCKED` 选候选，并发 worker 各自拿到不同
  会话而不是互相阻塞。

### claim 不可复活已结算的 run（复审 P0）

候选扫描拿到的是 `runs` 行的**语句快照**，因此「扫描时可领 → 写入前被别的写者结算」
是真实窗口。防线有三层：

1. **候选谓词双侧核对**：既看授权（`sessions.lease_until`），也看镜像
   （`runs.lease_until`），任一侧显示「仍被持有」即 fail-closed，不进入候选。
2. **claim 写入带谓词**：`UPDATE runs ... WHERE (claimablePredicate)`，0 行即
   `RowsAffected()==0` → 整事务回滚 → 全局 claim 重扫候选（有界 4 次）/
   `ClaimScope` 返回 `ErrNoRunnable`；session 行的 epoch 递增随回滚一起丢弃。
3. **ClaimScope 预检镜像**：持锁后镜像仍 live 时直接 `ErrLeaseHeld`，不写任何一行。

回归测试 `TestClaimDoesNotResurrectSettledRun` 用行锁编排复现该交错（去掉第 2 层即
复现「settled run resurrected: status = running, epoch+1」）；`TestClaimRefusesARunWithALiveMirror`
覆盖双侧核对与预检。

## 提交分两类

| 类别 | 种类 | 校验 |
|---|---|---|
| owner-scoped | `assistant.message`、`tool.result`、`compaction`、`run.started`、`run.failed` | tenant/session、**owner+epoch fence**、幂等键、配对 |
| edge-scoped | `user.message` | tenant/session、**白名单**（`EdgeKinds()`）、幂等键；**不校验 epoch** |

`Commit` 的校验顺序：种类注册 → scope → 幂等键形状 → fence（owner-scoped）→ 准入钩子
→ payload 形状 → session 存在 → **fence 重读** → 幂等（命中即回执）→ 配对 → 追加 → 提交。

fence 在幂等之前：**旧 writer 永远拿到明确拒绝（`ErrStaleEpoch` / `ErrLeaseExpired`），
不会收到「静默成功」**，这正是 drain 后僵尸进程该退出（而不是继续重试写入）的信号。

edge 端接入要求（本 change 显式登记的行为变更）：`POST /v1/sessions/{id}/events`
只接受 `EdgeKinds()`（即 `user.message`），其他 400；跨租户/不存在统一 404。

## 并发与瞬时失败重试（复审 P1-3）

与 `internal/store` 同款策略（同样的错误码、同样的退避与预算），覆盖内核的运行路径：

- 判定：`40P01`（死锁）/`40001`（序列化失败）/`55P03`（锁不可用）。适配器用
  `pgFail` 给这类错误打上可重试标记，内核用 `isRetryableTx` 识别，永不过度重试其他错误。
- **`Commit` 整体重放**（最多 6 次，20–145ms 递增退避）：重放安全来自请求自带的幂等键
  ——同一 requestId/generationId/toolCallId 要么只追加一次，要么直接返回已提交事实。
  事务从 `BeginSession` 重新开始，校验全部重做，暂态失败不产生任何半成品；重试期间不刷审计，
  预算耗尽时只记一条 store-failure 审计。
- **`Claim`/`ClaimScope`/`Renew`/`Finish`/`Release` 由 `PGStore.retryTx` 重放**：这些操作
  是 owner+epoch 条件更新，重放不可能双写；claim 的候选重扫与租约重放是两层独立机制。
- 证据：`TestPGRetryTxReplaysTransientFailures`（重放 3 次内成功、永久错误立即上抛、预算
  耗尽返回最后一次瞬态错误）、`TestPGFailClassification`（码表）、
  `TestKernelContract/mem/CommitRetriesTransientFailures`（提交路径 2 次瞬态 + 1 次成功、
  永久错误 1 次尝试、预算耗尽仅 1 条审计且 0 落账）、
  `TestConcurrentCommitsAndMigrations`（同会话 6×8 并发提交 + 并发迁移，0 逃逸失败、
  seq 无缺口、风暴后幂等仍成立；日志打印真实重放次数）。

## 幂等键命名空间

三类键各自命名空间化后写入 `events.dedupe_key`：

| 键 | 前缀 | 事件 |
|---|---|---|
| `requestId` | `req:` | user.message |
| `generationId` | `gen:` | assistant.message |
| `toolCallId` | `tool:` | tool.result |

`tool:` 与现有 `internal/agent` 写入器一致，旧账本无需迁移即可去重。
`Lookup(scope, KeyRef{...})` 是读侧（无锁）的同一查询，供调用方在**请求模型之前**主动
判断 generation 是否已提交，从而跳过这次模型调用。

现状说明（防重复最终回复的**实际**机制）：`internal/agent` 的重试安全不依赖 Lookup——
runner 由已提交账本推导 `generationId`（`u<promptSeq>-a<ordinal>`），重试推导出同一 id，
提交时被内核去重成一条；「上一轮已是终答」则由 transcript 短路（最后一条无工具调用的
assistant 直接返回）挡住。也就是说：机制是「提交期去重 + transcript 短路」，Lookup 是
留给调用方的主动查询接口（当前生产路径无调用点；加调用点前请先加测试）。

## 配对契约与查找代价（复审 P1-4）

- `tool.result` 必须带 `toolCallId`，且 payload 内 `toolCallId` 必须与之一致；
  找不到对应已提交调用 → `ErrOrphanToolResult`，账本不变。
- 结果报的 `toolName` 与调用不符 → `ErrToolResultMismatch`（拒绝「把结果挂到错误调用上」）。
- 同一 `toolCallId` 的结果只可能有一条（去重键），重复提交返回已有 seq。
- assistant 消息内 tool call 必须有 id、名字，且 id 在消息内唯一（`ErrDuplicateToolCall`）。
- `UnresolvedToolCalls(scope)` 给出「已提交调用但缺结果」的重试投影；结果提交后同一密钥
  幂等收敛，该调用永久消失。

**查找代价与取舍**（`SessionTxn.FindToolCall`，在 session 写锁内执行）：

- 实现：一条 kind 过滤的倒序查询
  （`kind='assistant.message' AND payload @? '$.content[*] ? (@.type=="toolCall" && @.id==...)'
  ORDER BY seq DESC LIMIT 1`），配 `internal/kernel/schema/0002` 的两个偏索引：
  `events_assistant_scan (tenant_id, session_id, seq DESC) WHERE kind='assistant.message'`
  与 `events_assistant_tool_calls`（GIN jsonb_path_ops，同谓词）。计划器在会话维度查询上
  选前者（EXPLAIN 实测走 `events_assistant_scan`），因此**回扫只在 assistant 消息之间步进**，
  命中即停；大表上 GIN 提供按 id 的 O(log n) 备选路径。
- 为什么不做「硬窗口」：结果必须**永不丢**——把窗口外合法的 `tool.result` 判成孤儿会让
  那一轮上下文永久缺结果。窗口只能用「步进单位」有界（assistant 消息），不能用「找不到就拒绝」
  有界。
- 残留最坏情况：孤儿结果（错误路径）或极长会话里往前找很久才会走遍该会话的 assistant
  消息（每行一次堆取回）；正常路径 ≤1–2 行。这在写锁内是可接受量级，但被记录为下一处优化点。
- 后续可选方案（记录不改）：`tool_calls` 投影表（(tenant, session, tool_call_id) 主键 +
  name/seq/result_seq），与 assistant 提交同事务写入，可把查找降为 O(1)；代价是第二条派生
  写路径与旧账本回填问题，收益在当前负载下不足以支付，故暂缓。

## lease / fencing 语义

- `Claim` 返回 `owner` + `epoch`，每次领取 `epoch+1`；`attempts` 沿用既有记账语义。
- `Renew` 只对「owner+epoch 匹配且未过期」的租约做条件更新；过期即 `ErrLeaseExpired`，
  owner/epoch 不匹配即 `ErrStaleEpoch`（不允许僵尸复活自己的 claim）。
- 过期租约可被新 owner 领取（claimable 条件：pending，或 running 且双侧视角都不认为被持有），
  领取后旧 token 一律拒绝。
- `Finish` 校验 token 后再落结果（done / 重新排队 / failed，与 `store.FinishRun` 同策略），
  旧 owner 的 Finish 被拒——它不能清掉新 owner 的租约或改写其 run 状态。
- `Release` 是「起跑前交还」（drain、准入拒绝）：run 回到 pending，任何 owner 可再领。

## 租户边界

所有读写都带 `(tenant, session)` 复合键；「别人的会话」与「不存在的会话」返回**同一个**
`ErrNotFound`，不泄露存在性（读取路径在空页时才做一次存在性探测，热路径只多一次查询）。
edge 层把 `ErrNotFound` 映射成 404。

## 故意不在内核

- **配额策略**：`Limits` 只是准入钩子（claim/commit 路径各一个回调），内核不存价格/额度；
  per-tenant 并发、流、带宽、存储的策略由默认 loadout / 边缘提供。
- **agent loop 语义**：消息如何生成、压缩策略、prompt 组装都在服务侧；内核只认事实。
- **SSE / blob / HTTP**：传输与流式读取不在内核。
- **投影与 checkpoint**：checkpoint 只是投影，内核不读它做真值。
- **多节点共识**：单主 PG + 租约（Non-Goal）。
- **外部 backend 会话**（`state_owner=external` 等）由 external-gateway 工作流负责登记。

## schema

`internal/kernel/schema/` 是本工作流的新增迁移，全部为增量、由 `kernel.NewPG(ctx, pool)`
在启动时按文件名字典序幂等应用（与 `store.Migrate` 共用 advisory lock）：

- `0001-kernel-fencing.sql`：`sessions` 三列（租约权威）+ `runs` 两列镜像 + 1 个索引。
- `0002-tool-call-index.sql`：配对查找的两个偏索引（assistant 扫描 btree + 工具调用 GIN）。

## 测试

`go test ./internal/kernel`：内存实现全量跑；设置 `KEEL_TEST_DSN` 时同一套用例也跑
PostgreSQL 实现（沿用仓库 gated integration 模式，无 DSN 自动 skip）。

覆盖：重复 requestId / generationId / toolCallId、孤儿结果、重复结果、错误挂载、
缺结果重试与幂等收敛、双 owner claim、旧 epoch 晚提交（含审计）、过期回收、
跨租户读写 404 语义、drain 期间客户端输入、claim 与 stale commit 交错（0 落账、
最终回复 = 1）、**claim 不复活已结算 run（含预修复复现验证）**、**活镜像 run 拒绝领取**、
**瞬时失败重放与预算耗尽**、**同会话并发提交 × 并发迁移风暴**、追加失败回滚无 seq 空洞、
准入钩子拒绝可回退、kind/键形状校验。
