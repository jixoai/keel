# Design: keel-board-v1

## 1. Context

keel 当前的协作事实分成三层，各自有明确的真值：

| 层 | 真值 | 作用域 | 进入模型上下文 |
| --- | --- | --- | --- |
| Session 账本 | `events`（`tenant_id, session_id, seq`） | 一个 Session | 是 |
| task-log | `task_cards` / `task_events` | 一个 Session（可跨会话拼树） | 否（工具与面板） |
| superagent | `collab_work_items` / `collab_assignments` / `collab_attentions` | 一个 tenant + mode | 否（工单看板） |

`agent-board` 需要第四层：**租户级、跨 Session、跨 Bot、跨模式**的任务/线程日志。它不能塞进
任何一层：

- 放进 Session 账本：任务会被某个会话的串行 run 与上下文预算绑住；跨 Bot 认领会制造
  “谁持有会话”的伪问题；保留期、配额、审计也会被模型上下文污染。
- 放进 task-log：task-log 的语义是“这次会话做了哪些卡”，不是“这个租户的队列”。
- 放进 superagent：其他模式、插件、人和外部 Issue 都进不来；且会迫使 superagent 的
  Mandate/Assignment/Attention 变成通用概念。

因此本设计新增一个**独立的 board 日志与投影**，并把它放在既有基础设施上：

- 身份：Account 是权限主体，Bot 是 Account 下的命名执行身份；Role 只表达身份/灵魂，不授予权限。
- 权限：tenant 级 `board.read/write/manage` 权限键 + 频道级 `resource_relations`
  （`resource_type=board_channel`，relation `owner/editor/viewer`）。
- 存储：PostgreSQL 单库、每表 `tenant_id`、RLS 强制；不新增数据库/队列。
- 实时：SSE + `Last-Event-ID`，游标是 board 事件序号；断线可续。
- 客户端：nsh 同一份 TSX 产出 web 与 TUI；不新增运行时。

约束沿用 `docs/ARCHITECTURE.md`：100–200 逻辑租户、1000 持续会话、2GB RSS、重启不丢不重、
不引入新语言/运行时。Board 是协作元数据，不是 token 流；它的写入量远低于聊天，允许用
更简单的强一致方案。

## 2. Goals / Non-Goals

**Goals**

- 一套可被 CLI、agent、人、插件、superagent 与外部 Issue 共用的任务/线程契约。
- 任务队列的强一致认领：并发 claim 只有一个成功，租约过期后旧 claim 不能结算。
- 任务依赖可表达、可检测环、完成时在同一写事务内 ready 依赖者。
- 所有状态变化可审计、可从日志重建；投影损坏不丢事实。
- 权限与租户边界在 API、存储、UI 三处一致；跨租户一律 404。
- nsh web/TUI 同源，队列/任务卡/线程/租约倒计时/live tail 的五态完整。
- superagent 与 GitHub 作为适配器接入，不制造第二真值。

**Non-Goals**

- 不照搬 Vercel 后端、Vercel API、OIDC、team/project/deployment、grants 引擎。
- 不把 board 事件写进 Session 账本，也不让 board 事件进入模型上下文。
- 不重写 superagent 的 Ticket/Mandate/Assignment/Attention；不迁移已归档工单。
- 不做通用 Slack/GitHub 双向同步；phase 2 只做 GitHub issue/PR 的最小入站与回复出站。
- 不做全文搜索、聚合、DM、presence、read receipts、每线程 ACL、跨租户看板。
- 不新增调度器/消息队列/语言运行时；watchers/schedules 复用既有 durable/jobqueue/triggers。

## 3. 概念映射：vercel/board → keel

| vercel/board | keel 落点 | 说明 |
| --- | --- | --- |
| tenant | `tenant_id` | 所有 board 行带 tenant；RLS 强制；跨租户 404 |
| board | tenant 的 `board_*` 表 | 一个 tenant 一个 board；不做多 board |
| channel | `board_channels.path` | 路径文法相同；`work/` 是子树查询语法，不是另一个频道 |
| thread | root message + `board_threads` | 线程只有一层；回复的 `re` 指向根 |
| message | `board_messages` + `board_events` | 文本 ≤10,000 字符；ID 是 `message.posted` 的 board seq |
| task | 同一线程根 + `board_tasks` | 任务不是新实体，是线程根上的状态 |
| claim | `board_tasks.state=claimed` + `claim_*` | claim 是租约，不是永久锁 |
| lease | `claim_until` | 默认 900s，范围 30–3600s |
| epoch | `board_tasks.epoch` | 每次成功 claim +1；写操作必须匹配 |
| token | `claim_token_hash` | 只存 SHA-256；原文只在 claim 响应出现一次 |
| needs | `board_task_needs` | 有界 DAG，最多 20 个前置 |
| labels | `board_labels` | `board.*` 保留；`board.task` 是状态标签 |
| watcher | phase 3 `board_watchers` + durable consumer | 不用 Vercel watcher 注册/唤醒 |
| schedule | phase 3 `board_schedules` + jobqueue | 不用 Vercel timer |
| profile | **Bot** | 命名执行身份；不是 Role，不授予权限 |
| principal | **Account** | 认证与授权主体 |
| actor | `account:<id>` 或 `account:<id>+bot:<id>` | 显示“Bot via Account” |
| grants / can | `board.read/write/manage` + channel ReBAC | 不实现 Vercel grants 数据模型 |
| SSE cursor | `board_events.seq` | 每 tenant 单调递增；不透明字符串视图 |
| PostgreSQL | keel 既有 PG | 单库、RLS、tenant_id |

关键区别：

- **命名陷阱**：vercel/board 的 `profile` 是 principal 下的命名执行身份，本设计映射到
  keel 的 **Bot**；keel 自己的 `Profile` 是 `Account + Role` 的角色文档，不承担 actor 身份，
  不用于 claim/by/for。文档中出现的 “profile” 除引用 Vercel 概念外，一律指 Bot。
- **Account 是唯一权限主体。** Bot 只是“这次写入以谁的名义”，不拥有独立权限键；
  这符合 `internal/access/keys.go` 的既有裁决（Role 不授予权限）。
- **`by` 与 `owner` 分离。** `owner` 是创建任务的 Account；`by` 是实际执行创建动作的
  Actor。Account 本身（不带 Bot）可以改自己或自己任意 Bot 创建的任务；某个 Bot 只能改
  它自己创建的任务。这与 Vercel 的 “principal 改所有 profile、profile 只改自己” 同构。
- **`for` 只指向 owner 的 Actor。** 可以指向 owner Account 本身或它的某个 Bot；不允许把
  任务指派给别的 Account 的 Bot（跨主体指派是未来的 offer/accept，不在本 change）。
- **认证不引入 OIDC。** 沿用 keel Account 的会话/API key；`GET /v1/board/me` 返回当前
  Account、可选 Bot、权限与频道 can 映射。

## 4. 数据模型与存储

### 4.1 Board 日志（唯一真值）

```sql
-- 每个 tenant 一条头记录；写事务锁这一行，保证 seq 无洞、事件全序。
CREATE TABLE board_heads (
    tenant_id  uuid PRIMARY KEY,
    next_seq   bigint NOT NULL DEFAULT 1,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- 追加日志。seq 是 tenant 内全局序号；channel 是事件发生时的频道路径。
-- payload 带 v=1，未知 kind 在 fold 时忽略但保留，便于前向兼容。
CREATE TABLE board_events (
    tenant_id  uuid NOT NULL,
    seq        bigint NOT NULL,
    channel    text NOT NULL,
    kind       text NOT NULL,
    payload    jsonb NOT NULL,
    dedupe_key text,
    actor_account_id uuid,
    actor_bot_id     uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, seq)
);

CREATE UNIQUE INDEX board_events_dedupe
    ON board_events (tenant_id, dedupe_key)
    WHERE dedupe_key IS NOT NULL;

CREATE INDEX board_events_channel_seq
    ON board_events (tenant_id, channel, seq);
```

- `seq` 不是 UUID；对客户端是不透明字符串（十进制）。它同时是：
  - message ID（`message.posted` 的 seq）；
  - SSE 事件 `id`；
  - list 的 `cursor`/`after`。
- `channel` 冗余在事件上，便于频道过滤与审计；频道路径不可变，因此不会出现历史路径漂移。
- `dedupe_key` 只用于幂等写；普通事件为空。唯一索引只约束非空值。
- 不做 per-channel seq：board 级 tail、跨频道列表和“保持你的位置”都需要一个租户内游标；
  per-channel 序号会让跨频道 SSE 需要多游标。频道过滤在查询/投影层完成。

### 4.2 投影表

投影不是第二真值：全部由 `board_events` 按序 fold 得到，可随时 truncate + replay。

```sql
CREATE TABLE board_channels (
    tenant_id   uuid NOT NULL,
    path        text NOT NULL,
    created_seq bigint NOT NULL,
    last_seq    bigint NOT NULL,
    messages    int NOT NULL DEFAULT 0,
    threads     int NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, path)
);

CREATE TABLE board_messages (
    tenant_id  uuid NOT NULL,
    id         bigint NOT NULL,              -- message.posted 的 seq
    channel    text NOT NULL,
    thread_id  bigint NOT NULL,              -- 根消息 ID；根自己的 thread_id = id
    re         bigint,                       -- 回复的根；根为 NULL
    reply_to   bigint,                       -- 被直接回复的消息；等于根时可省略
    text       text NOT NULL DEFAULT '',
    labels     jsonb NOT NULL DEFAULT '{}'::jsonb,
    deleted    boolean NOT NULL DEFAULT false,
    author_account_id uuid NOT NULL,
    author_bot_id     uuid,
    author_actor     text NOT NULL,
    version    bigint NOT NULL,
    revision   bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    edited_at  timestamptz,
    PRIMARY KEY (tenant_id, id)
);

CREATE INDEX board_messages_channel
    ON board_messages (tenant_id, channel, id DESC);
CREATE INDEX board_messages_thread
    ON board_messages (tenant_id, thread_id, id);
CREATE INDEX board_messages_live
    ON board_messages (tenant_id, channel, id)
    WHERE deleted = false;

CREATE TABLE board_threads (
    tenant_id   uuid NOT NULL,
    root_id     bigint NOT NULL,
    channel     text NOT NULL,
    reply_count int NOT NULL DEFAULT 0,
    last_id     bigint NOT NULL,
    last_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, root_id)
);

CREATE INDEX board_threads_channel_last
    ON board_threads (tenant_id, channel, last_at DESC, root_id DESC);

CREATE TABLE board_tasks (
    tenant_id        uuid NOT NULL,
    root_id          bigint NOT NULL,
    channel          text NOT NULL,
    state            text NOT NULL,           -- waiting|ready|claimed|blocked|completed|failed|canceled
    owner_account_id uuid NOT NULL,
    owner_actor      text NOT NULL,           -- by：创建任务时的 Actor
    for_actor        text,                    -- 指派给 owner 的某个 Actor；NULL 为池
    epoch            bigint NOT NULL DEFAULT 0,
    claim_actor      text,
    claim_until      timestamptz,
    claim_token_hash text,                    -- SHA-256 hex；永不返回
    blocked_kind     text,
    blocked_reason   text,
    result           text,
    reason           text,
    updated_seq      bigint NOT NULL,
    PRIMARY KEY (tenant_id, root_id)
);

CREATE INDEX board_tasks_queue
    ON board_tasks (tenant_id, state, root_id);
CREATE INDEX board_tasks_owner
    ON board_tasks (tenant_id, owner_account_id, state, root_id);
CREATE INDEX board_tasks_for
    ON board_tasks (tenant_id, for_actor, state, root_id);
CREATE INDEX board_tasks_claim_expiry
    ON board_tasks (tenant_id, claim_until)
    WHERE state = 'claimed';

CREATE TABLE board_task_needs (
    tenant_id uuid NOT NULL,
    task_id   bigint NOT NULL,
    need_id   bigint NOT NULL,
    position  int NOT NULL,
    PRIMARY KEY (tenant_id, task_id, need_id)
);

CREATE INDEX board_task_needs_reverse
    ON board_task_needs (tenant_id, need_id, task_id);

CREATE TABLE board_labels (
    tenant_id  uuid NOT NULL,
    message_id bigint NOT NULL,
    key        text NOT NULL,
    value      text NOT NULL,
    PRIMARY KEY (tenant_id, message_id, key)
);

CREATE INDEX board_labels_key_value
    ON board_labels (tenant_id, key, value, message_id);

-- 幂等写的结果缓存；result 不含 claim token。
CREATE TABLE board_requests (
    tenant_id      uuid NOT NULL,
    request_key    text NOT NULL,
    request_digest text NOT NULL,
    result         jsonb NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, request_key)
);
```

Phase 2 追加：

```sql
CREATE TABLE board_links (
    tenant_id     uuid NOT NULL,
    board_task_id bigint NOT NULL,
    source        text NOT NULL,       -- superagent | github
    source_id     text NOT NULL,       -- work_item_id / assignment_id / issue number
    kind          text NOT NULL,       -- ticket | assignment | attention | issue | pull_request
    direction     text NOT NULL,       -- mirror | inbound
    claim_epoch   bigint,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, source, source_id, kind)
);
CREATE UNIQUE INDEX board_links_task
    ON board_links (tenant_id, board_task_id, source, kind);
```

Phase 3 追加 `board_watchers`、`board_watch_deliveries`、`board_schedules`（见 §15）。

### 4.3 为什么不用 Session 账本/task-log

- **权限边界**：Session 属于一个 Bot/Account；board 频道需要租户级共享。把 board 塞进
  Session 会让“另一个 Bot 来认领”变成跨 Session 写，违反 Session 串行与所有权。
- **上下文边界**：Session 事件会被模型读取；board 的标签、认领、租约是协作元数据，
  不应消耗 token 或改变模型输入。
- **保留/配额**：Session 账本不可删；board 未来可能需要按 tenant 配置保留期。两者生命周期
  不同，混在一起会让任一侧的策略都难以执行。
- **投影一致性**：board 需要跨频道/跨任务的队列查询与依赖传播；Session 账本没有这些索引。

### 4.4 RLS 与租户隔离

所有 `board_*` 表加入 `store.rlsTenantTables`，与既有表一样
`ENABLE/FORCE ROW LEVEL SECURITY` + `tenant_isolation` policy。应用层仍显式带
`tenant_id` 谓词（双保险）；跨租户 ID 一律 404，不返回“存在但无权”。

### 4.5 投影重建

- `foldBoardEvent(state, event)` 是纯函数：输入当前投影行（只加载事件触及的行），输出新行。
- 写事务内：追加事件后立即 fold 并更新投影；不做异步最终一致（board 写入量低，简单优先）。
- 重建命令（内部/运维）在一个事务内按 `seq` 重放，最后比对投影哈希；重建幂等。
- 随机历史属性测试：同一组事件，增量 fold 与全量 replay 的投影逐字段相等。
- 未知 `kind`：保留事件，fold 跳过；不阻塞旧版本读新日志。
- 投影损坏：读取路径发现 `board_heads.next_seq` 与最大 seq 不一致时拒绝写入并告警，
  由重建修复；不静默覆盖。

## 5. 条件追加与并发

### 5.1 写路径

所有 board 写操作走同一个 `Append` 引擎：

```text
BEGIN
  -- 1. 认证与租户已由 HTTP 层完成；Append 再次校验 tenant 归属
  -- 2. 锁租户头，得到 next_seq；同 tenant 的 board 写串行化
  SELECT next_seq FROM board_heads WHERE tenant_id = $1 FOR UPDATE
  -- 3. 加载本次要校验的投影行（task/message/thread/needs）
  -- 4. 纯校验：状态、epoch/token、owner/for、needs 环、保留标签、权限
  -- 5. 逐事件分配 seq，INSERT board_events
  -- 6. 调用 fold，UPDATE 投影（含 board_labels、board_task_needs）
  -- 7. 如带 idempotencyKey，INSERT board_requests（digest + 脱敏 result）
COMMIT
-- 8. 提交后 pg_notify('board_events', '<tenant>:<seq>')；进程内 hub 唤醒 SSE
```

- **一个 tenant 一个写锁**是本设计的核心取舍。它让“并发 claim 只有一个成功”“完成时
  ready 依赖者”“needs 环检查”都在一个事务里得到强一致，不需要分布式锁或乐观重试。
- 代价是同 tenant 的 board 写串行。Board 写入是“认领/完成/发一条日志”级别，目标
  <100 writes/s/tenant；100–200 租户仍远低于 PG 能力。若未来单租户超限，再按 channel
  分片或改为 per-task 行锁；本 change 不做。
- 替代方案（已否决）：per-task `SELECT FOR UPDATE` + advisory lock。它减少锁竞争，但
  跨任务的 needs 传播、依赖环检查、频道计数需要多行一致性，容易出现死锁与部分更新。
- 读路径不取头锁；读投影 + `seq` 水位。列表读报告 `cursor`（当前最大 seq）；按 ID 读
  等待投影追到该 ID（写事务内已同步，故通常无需等待）。

### 5.2 并发 claim

1. 两个请求同时 `claim-ready` 同一任务。
2. 第一个取得 `board_heads` 行锁，校验 `state=ready` 且 `for` 允许，写
   `task.claimed`（`epoch+1`、`until`、`token_hash`），更新投影，提交。
3. 第二个取得锁后读到 `state=claimed`，返回 409 `task_claimed`，不写事件。
4. 若第一个在租约内崩溃，任务保持 `claimed` 直到 `claim_until`；之后任何任务请求按
   §6.3 先写 `task.expired` 再 claim。

### 5.3 崩溃与重试

- 事件、投影、幂等结果在同一个事务；崩溃在 commit 前等于没发生，commit 后全部可见。
- HTTP 重试带同一 `idempotencyKey`：同 digest 返回缓存结果（不含 token）；不同 digest
  返回 409 `duplicate_id`。
- 无幂等键的写不保证重试安全；客户端应使用幂等键，尤其是 claim/complete。

### 5.4 读修复

Board 不依赖后台定时器保证正确性：

- **过期 claim**：任何读/写触达该任务时，若 `state=claimed AND claim_until <= now()`，
  先按当前 epoch 追加 `task.expired`，置 `ready`，再继续。可选 sweeper 每 15s 扫描
  `board_tasks_claim_expiry`，只是降低“没人访问时状态标签仍旧”的窗口；sweeper 与请求
  竞争时由 head 锁与 epoch 条件保证只写一次。
- **等待中的任务已满足依赖**：任何 list/claim/读任务看到 `waiting` 且所有 needs 都
  `completed` 时，追加 `task.ready`。这修复“完成事务只 ready 了一部分依赖者”的中断场景。
- **客户端显示**：`state=claimed` 且 `claim_until <= now` 时，视图按 `ready` 显示并标注
  `lease ran out`；不等待服务端事件。

读路径触发的 `task.expired` / `task.ready` 是 **board 的系统写**，actor 记为
`board:system`，不需要调用者拥有 `board.write`；它只写这两类幂等修复事件，不改变消息内容，
也不绕过任何租约围栏。这样只读 viewer 也能看到正确的 effective 状态，而不会因为“读请求
没有写权限”把过期 claim 永远显示为 claimed。

## 6. 任务模型

### 6.1 任务即线程根

- 新任务：`POST /tasks {channel,text,...}` 创建线程根 + `task.created`；根文本是任务说明。
- 既有线程：`POST /tasks {re}` 把该根变成任务；根文本与作者不变；需要该频道 editor。
- 任务 ID = 根消息 ID。回复是工作日志/讨论；回复不改变任务状态。
- 一个线程最多是一个任务；重复 `re` 返回 409 `already_a_task`。
- 根删除：若未 completed/canceled，追加 `task.canceled`（reason `deleted`）并清空标签；
  已完成任务删除根不改变终态。根删除本身在 phase 2 的消息生命周期里交付。
- 非 owner 编辑根：任务处于 waiting/ready/claimed 时追加 `task.blocked`（kind `edited`，
  reason 指向编辑），结束 claim；owner 读新文本后 unblock 或 cancel。已完成/已取消不变。
  Phase 2 交付。

### 6.2 七态状态机

| 状态 | 含义 | 可离开到 |
| --- | --- | --- |
| `waiting` | 有前置任务未 completed | `ready`（全部完成）、`blocked`、`canceled` |
| `ready` | 可被认领；或被指派给某个 Actor | `claimed`、`blocked`、`canceled` |
| `claimed` | 被一个 Actor 以租约持有 | `completed`、`failed`、`blocked`、`canceled`、`ready`（release/expire） |
| `blocked` | 等人（kind + reason） | `ready`/`waiting`（unblock）、`canceled` |
| `completed` | 完成，可带 result | 无；依赖者被 ready |
| `failed` | 失败，带 reason | `ready`（reopen）、`canceled` |
| `canceled` | 不再需要，带 reason | 无；依赖者继续等待 |

事件与守卫：

| 事件 | 从 | 到 | 守卫 |
| --- | --- | --- | --- |
| `task.created` | — | `waiting` if needs>0 else `ready` | 根存在、非任务、频道可写、needs 合法 |
| `task.assigned` | waiting/ready/claimed/blocked | 不变 | owner/manage；`for` 是 owner 的 Actor 或 null |
| `task.needs_changed` | waiting/ready/blocked | waiting/ready（blocked 保持） | owner/manage；无 live claim；未终态；needs 合法 |
| `task.ready` | waiting | ready | 所有 needs completed（服务端写） |
| `task.claimed` | ready | claimed | 频道 editor；`for` 匹配；epoch+1；token hash |
| `task.renewed` | claimed | claimed | epoch+token 匹配；租约未过期 |
| `task.released` | claimed | ready | epoch+token 匹配 |
| `task.expired` | claimed | ready | epoch 匹配（服务端/sweeper 写） |
| `task.completed` | claimed | completed | epoch+token 匹配；result ≤1000 |
| `task.failed` | claimed | failed | epoch+token 匹配；reason ≤1000 |
| `task.blocked` | waiting/ready/claimed | blocked | claimed 需 holder；未 claim 需 owner/manage；kind 非 `edited`（请求） |
| `task.unblocked` | blocked | ready if needs 全完成 else waiting | owner/manage |
| `task.canceled` | 非 completed/canceled | canceled | owner/manage；结束 claim |
| `task.reopened` | failed | ready | owner/manage |

- 终态写操作：`completed`/`canceled` 拒绝任何变更（409 `task_finished`）；`failed` 只接受
  `reopen`/`cancel`。`unblock` 非 blocked、`reopen` 非 failed 是幂等 no-op（返回当前任务，
  `outcome: unchanged`），不写事件。
- `task.blocked` 会结束 claim（清空 claim_*），因此阻塞后只有 owner/manage 能 unblock。
- 依赖者传播：`task.completed` 提交时，对每个直接依赖者检查是否所有 needs 都 completed；
  是则在同一事务追加 `task.ready`。最多处理 `needs_fanout_max`（默认 100）个；超出的由
  §5.4 读修复在后续任务请求中补齐，不丢失。
- 失败/取消的前置不会 ready 依赖者；它们继续 `waiting`，直到 owner 取消或前置被 reopen
  并最终完成。

### 6.3 视图

任务根的消息视图带 `task`：

```json
{
  "id": "812",
  "channel": "work",
  "text": "fix CVE-123 in acme/web",
  "labels": { "step": "fix", "board.task": "ready" },
  "task": {
    "state": "ready",
    "owner": "acct_...",
    "by": "acct_...+bot:...",
    "for": null,
    "needs": ["810"],
    "epoch": 0,
    "claim": null,
    "blocked": null,
    "result": null,
    "reason": null
  },
  "version": 12,
  "revision": 10,
  "createdAt": "2026-11-27T00:00:00Z"
}
```

- `claim` 只有 `{actor, until}`，**永远没有 token**。
- `board.task` 是根的标签投影，值等于 `task.state`；根被删除时标签清空。
- 回复视图不重复 `task`；标签过滤按“消息或线程根”匹配。

## 7. Claim、租约与 token

### 7.1 Token

- Claim 响应返回 `{epoch, token, until}`；`token` 是 32 字节随机数的 base64url（43 字符）。
- 服务端只存 `SHA-256(token)` 的 hex；事件 payload 与投影都只含 hash。
- 任何读接口（`/tasks`、`/messages/{id}`、`/events/stream`、`/me`）都 MUST NOT 返回 token；
  `board_requests.result` 也不含 token。
- 调用方可自带 token（必须是 32 字节 base64url）：用于“从 run ID 派生、跨进程重建”的场景；
  自带 token 时服务端仍只存它的 SHA-256，不需要额外保存原文。
- 校验用常量时间比较；token 错误、epoch 过期、claim 已结束、租约已过期统一返回
  409 `claim_lost`，不区分原因，避免 oracle。

### 7.2 租约

- `leaseSeconds` 缺省 900；请求范围 30–3600；服务端给 `min(request, tenant_cap)`，
  `tenant_cap` 默认 3600，且 MUST NOT 低于 300。
- `renew` 从 `now()` 起延长，epoch 不变；可在租约任何时刻 renew；过期后 renew 返回
  `claim_lost`（先按 §5.4 写 expired）。
- `release` 归还未完成任务：`ready`（claim 期间 needs 不可变，因此前置已满足）。
- `expired`：见 §5.4；`task.expired` 的 epoch 必须等于当前 claim epoch，否则忽略。
- 客户端倒计时用 `claim.until`；到期显示 `lease ran out` 并按 `ready` 提供 claim 动作。

### 7.3 Claim-ready 选择顺序

`POST /tasks/claim-ready {channel, label?, from?, leaseSeconds?, waitSeconds?, token?}`：

1. 频道（或 `channel/` 子树）内 `state=ready`（含租约已过期的 claimed，按 effective state）；
2. 先选 `for` 等于调用者 Actor 的任务；
3. 再选 `for IS NULL` 且 `owner_account_id = 调用者 Account` 的任务；
4. 若 `from` 列出 owner Account ID，则再选这些 owner 的 `for IS NULL` 任务；
5. 每组内按 `root_id` 升序（oldest first）。
6. 没有任务时：`waitSeconds=0` 立即返回 `{task:null}`；`waitSeconds>0` 最多等待
   `min(waitSeconds, 60)`，用 `board_events` 通知唤醒，超时返回 `{task:null}`。
7. `from` 只接受 Account ID（UUID）；handle/名称返回 400 `invalid_from`，避免同名歧义。

### 7.4 指派

- `for` 必须是 owner Account 的 Actor（Account 本身或它的 Bot）；否则 400 `invalid_assignee`。
- 被指派的任务只有该 Actor 能 claim（即使租约过期），其他 Actor 返回 409 `task_assigned`。
- `assign {for:null}` 回到池；owner/manage 可在 waiting/ready/claimed/blocked 时改；
  failed 需先 reopen，completed/canceled 不可改。
- `claim-ready` 对别的 Actor 的指派任务不选；显式 `claim` 命中时返回 `task_assigned`。

## 8. 依赖（needs）

### 8.1 校验

`needs` 是任务 ID 数组，最多 20 个；创建/改 needs 时逐条检查：

- 目标必须存在、是任务、与当前任务同 tenant；
- 目标 MUST 可被调用者读取（否则 `invalid_needs`，不泄露存在性）；
- 目标 MUST NOT 是 `canceled`（永远不能完成）；`failed` 允许（可 reopen）；
- 目标 MUST NOT 是自身；新增依赖后 MUST NOT 形成环；
- 环检测从新增的前置向下游走，最多 500 个任务；超过上限返回 `invalid_needs`
  （reason `too_deep`），不做无界遍历；
- 已 completed 的前置不展开其 needs（它完成后不再变化，且不会回到未完成）。

### 8.2 改 needs

`POST /tasks/{id}/needs {needs:[...]}`：

- 允许：owner/manage；任务处于 waiting/ready/blocked；无 live claim。
- live claim 返回 409 `task_claimed`；租约已过期先 expired 再判断（等价于无 claim）。
- 终态返回 409 `task_finished`。
- 同一集合是 no-op，不写事件。
- 写入 `task.needs_changed`；若 blocked 保持 blocked；否则 needs 为空 → `task.ready`；
  全部 completed → `task.ready`；否则 `waiting`。
- 依赖者传播与 `task.ready` 同一事务；超过 fanout 上限由读修复补齐。

### 8.3 失败/取消的前置

- `failed`/`canceled` 前置不会 ready 依赖者；依赖者保持 `waiting`，UI 显示 `needs #id (failed)`。
- `reopen` 只把 failed 任务变 `ready`；依赖者仍等待，直到它再次 completed。
- `canceled` 前置永远无法完成；依赖者的 owner 必须 cancel 或改 needs（改 needs 时取消的前置
  不能作为新 needs）。

## 9. 标签与保留命名空间

### 9.1 文法

- key：`^[a-z0-9][a-z0-9_.-]*$`，1–64 字符；不得是 `constructor`/`__proto__`/`prototype`。
- value：1–256 字符，不含 NUL/孤立代理项/控制字符；不得有换行。
- 每个 key 一个 value；`labels` 是对象。
- 过滤：`key`、`key=value`、`key=a|b`；重复 `--label` 是 AND；`|` 永远是 OR。

### 9.2 保留

- 前缀 `board.` 是 board 自己的命名空间；客户端设置/取消任何 `board.*` 返回
  400 `reserved_label`，错误带 hint（“complete it with the claim”等）。
- `board.task` 由任务写操作维护，值 = 七态之一；它是根标签，因此按标签过滤线程时
  回复也命中。
- 任务创建时允许携带普通标签；保留前缀即使出现在 `labels` 里也拒绝整个写，不静默丢弃。
- phase 1 只开放任务创建时的标签与 `board.task` 过滤；独立的 `POST /messages/{id}/labels`
  在 phase 2 与消息编辑一起交付。

## 10. API（phase 1 最小闭集）

Base：`/v1/board`。认证沿用 keel；tenant 来自凭据，不出现在路径。所有响应 JSON；
错误 `{ "error": "...", "code": "...", "hint"?: "..." }`。

### 10.1 路由

| Method | Path | Body / Query | 返回 | 权限 |
| --- | --- | --- | --- | --- |
| GET | `/me` | — | `{account, bot?, actor, can:{board:{read,write,manage}, channels:[...]}}` | board.read |
| GET | `/channels` | `prefix?`, `after?`, `limit?` | `{channels:[{path,messages,threads,can}], cursor}` | board.read |
| POST | `/channels` | `{path}` | 201 `{channel, created}` | board.write 或 board.manage |
| GET | `/messages` | `channel`, `after?`, `limit?` | `{messages:[...], cursor}` | board.read + viewer |
| GET | `/messages/{id}` | `after?`, `limit?` | `{message, replies, cursor}` | board.read + viewer |
| POST | `/messages` | `{channel?, re?, text, labels?, idempotencyKey?}` | 201 `{message}` | board.write + editor |
| POST | `/tasks` | `{channel, text, labels?, needs?, for?, claim?, leaseSeconds?, idempotencyKey?}` 或 `{re, needs?, for?, claim?, leaseSeconds?, idempotencyKey?}` | 201 `{task, claim?}` | board.write + editor |
| GET | `/tasks` | `channel?`, `state?`, `label?`, `by?`, `owner?`, `for?`, `needs?`, `after?`, `limit?` | `{tasks:[...], cursor}` | board.read + viewer |
| POST | `/tasks/claim-ready` | `{channel, label?, from?, leaseSeconds?, waitSeconds?, token?}` | `{task, claim}` 或 `{task:null}` | board.write + editor |
| POST | `/tasks/{id}/claim` | `{leaseSeconds?, token?}` | `{task, claim:{epoch,token,until}}` | board.write + editor |
| POST | `/tasks/{id}/renew` | `{epoch, token, leaseSeconds?}` | `{task, until}` | board.write + editor |
| POST | `/tasks/{id}/release` | `{epoch, token}` | `{task}` | board.write + editor |
| POST | `/tasks/{id}/complete` | `{epoch, token, result?}` | `{task}` | board.write + editor |
| POST | `/tasks/{id}/fail` | `{epoch, token, reason}` | `{task}` | board.write + editor |
| POST | `/tasks/{id}/block` | `{reason, kind?, epoch?, token?}` | `{task}` | holder 或 owner/manage |
| POST | `/tasks/{id}/unblock` | `{}` | `{task}` | owner/manage |
| POST | `/tasks/{id}/reopen` | `{}` | `{task}` | owner/manage |
| POST | `/tasks/{id}/assign` | `{for: actor|null}` | `{task}` | owner/manage |
| POST | `/tasks/{id}/needs` | `{needs:[id...]}` | `{task}` | owner/manage |
| POST | `/tasks/{id}/cancel` | `{reason}` | `{task}` | owner/manage |
| GET | `/events/stream` | `channel?`, `label?`, `state?`, `owner?`, `for?`, `after?`, `Last-Event-ID` | SSE | board.read + viewer |

Phase 2 追加：`POST /tasks/batch`、`GET /tasks/batch/{id}`、`GET /threads`、
`POST /messages/{id}/edit`、`POST /messages/{id}/delete`、`POST /messages/{id}/labels`、
`GET /actors`。Phase 3 追加：`/watchers*`、`/schedules*`、`/attachments*`。

### 10.2 游标与分页

- message/task ID、cursor 都是不透明字符串（内部 board seq 的十进制）。
- `GET /messages?channel=...` 无 `after`：返回该频道**最新** `limit` 条，按升序排列；
  有 `after`：返回 `after` 之后最旧的一页（前向读）。`limit` 默认 50，最大 500。
- `GET /tasks` 按 `root_id` 升序（oldest first）；`after` 是上一页最后一个 root_id；
  `limit` 默认 50，最大 500。`state=open`（默认）含 waiting/ready/claimed/blocked；
  `state=all` 含终态；`state=<one>` 精确过滤。租约过期的 claimed 按 effective `ready` 过滤。
- `GET /messages/{id}` 的 `after` 是回复游标；无 `after` 返回根 + 最新 `limit` 条回复。
- 非法 cursor → 400 `invalid_cursor`。cursor 不携带授权；越权频道仍 404/过滤。

### 10.3 错误码

| HTTP | code | 场景 |
| --- | --- | --- |
| 400 | `invalid_request` | 字段缺失/文法错误/矛盾参数 |
| 400 | `invalid_cursor` | cursor 无法解析 |
| 400 | `invalid_needs` | 不存在/不可读/已取消/成环/超深/超过 20 |
| 400 | `invalid_from` | `from` 不是 Account ID |
| 400 | `invalid_assignee` | `for` 不是 owner 的 Actor |
| 400 | `reserved_label` | 设置/取消 `board.*` |
| 400 | `too_large` | 文本/标签/结果超限 |
| 401 | `unauthenticated` | 无凭据/凭据失效 |
| 403 | `forbidden` | 有读权限但缺少写/管理权限；带 `missing` |
| 404 | `not_found` | 频道/消息/任务不存在或调用者不可读 |
| 404 | `board_disabled` | tenant feature flag 关闭 |
| 409 | `task_claimed` | claim 活着的任务 / 对已 claim 的任务做 owner 操作 |
| 409 | `claim_lost` | epoch/token 不匹配、claim 已结束、租约过期 |
| 409 | `task_assigned` | `for` 指向其他 Actor |
| 409 | `task_not_owned` | owner-only 操作由非 owner 发起 |
| 409 | `task_finished` | 对终态任务做不允许的变更 |
| 409 | `already_a_task` | `re` 已是任务 |
| 409 | `duplicate_id` | 同一幂等键不同请求 |
| 429 | `rate_limited` | 超过写速率（可选，phase 1 打点不强制） |
| 503 | `unavailable` | DB/依赖不可用；不产生部分写入 |

### 10.4 幂等

- `idempotencyKey`（或任务/消息创建时的 `id`）1–200 字符；同一 tenant 全局唯一。
- 服务端对规范化后的请求做 digest；同 key 同 digest 返回首次结果（token 脱敏）；
  不同 digest → 409 `duplicate_id`。
- claim 的首次响应含 token；重试响应含 `{epoch, until}` 但不含 token；调用方要么重新 claim，
  要么使用自带 token。
- 幂等结果与事件在同一事务写入；不会出现“事件成功但结果没记”或反之。

## 11. 权限与租户边界

### 11.1 权限键

| 键 | 允许 | 备注 |
| --- | --- | --- |
| `board.read` | 访问 board 读路由、列表、SSE | 仍需频道 viewer 关系才能看内容 |
| `board.write` | 创建频道、发消息、创建任务、claim 与 claim 写操作、标签 | 仍需频道 editor/owner 关系 |
| `board.manage` | 创建/管理频道访问、列出所有频道元数据、运维重建 | 在 board 路由上蕴含 board.read 与 board.write；不绕过频道内容读权限 |

### 11.2 频道 ReBAC

复用 `resource_relations`：

- `resource_type = 'board_channel'`
- `resource_id = <channel path>`（精确路径，不含尾 `/`）
- relation：
  - `viewer` → read
  - `editor` → read + write（发帖/回复/创建任务/claim/标签/claim 结算）
  - `owner` → editor + assign/cancel/unblock/reopen/needs on any task + 管理频道访问
- subject：`account` 或 `group`；不使用 `team_bot`（授权主体永远是 Account）。
- **创建频道即 owner**：`POST /channels` 或首帖自动建频道时，创建者 MUST 在同一事务获得该
  频道的 `owner` 关系；否则创建者会立刻失去对自己频道的管理能力。其他人只能由 owner 或
  `board.manage` 授予关系（phase 2 的频道访问管理 UI）。
- **子树继承**：解析 `work/production` 时，先查精确路径；没有再逐级向上查 `work`；
  最近祖先的关系生效（most specific wins）。因此 `work` 上的 editor 覆盖 `work/production`；
  在 `work/secret` 上显式给 viewer 可以收窄该子树。
- 不引入 `work/*` 模式元组；`*` 只体现在 tenant 级 `board.*` 键，不体现在内容访问。

### 11.3 任务 owner 规则

- `owner_account_id` 是创建任务的 Account；`owner_actor` 是 `by`。
- 以 Account 身份（无 Bot）调用：可以对其 owner 为自己的所有任务做 owner 操作
  （无论 `by` 是 Account 还是它的任意 Bot）。
- 以 Bot 身份调用：只能对 `owner_actor` 恰为该 Actor 的任务做 owner 操作。
- 频道 `owner` 关系可以对频道内任意任务做 owner 操作。
- owner 规则仍要求调用者当前拥有该频道 `editor` 关系（或频道 `owner`）；权限被收回后，
  只剩频道 owner 能操作。
- 非 owner 在可读频道执行 owner 操作 → 403/409 `task_not_owned`；不可读 → 404。

### 11.4 404 vs 403

- 无频道读关系：频道、消息、任务、事件一律 404（不确认存在）。
- 有读无写：403 `forbidden`，`missing` 指出 `board.write` 或 `channel:editor`。
- 可读频道内不存在的 ID：404 `not_found`。
- 跨租户：404 `not_found`，不泄露租户。
- `board_disabled`：404，避免枚举。
- 越权写不产生事件；权限拒绝写 `auth_events`（kind `permission.denied`），detail 不含正文/token。

## 12. SSE 与读取

### 12.1 流

`GET /v1/board/events/stream`：

- `Accept: text/event-stream`；`Last-Event-ID` 优先于 `after`。
- 事件帧：

```text
id: 123
event: board
data: {"cursor":"123","type":"task.claimed","createdAt":"...","channel":"work","payload":{...},"message":{...},"task":{...}}

```

- 对调用者不可见的事件：只发 `id: 123` 空帧（无 `data`），让客户端游标前进但不泄露内容。
  这保留了“游标可能跳过你看不到的事件”的已知信息量，与 Vercel 的取舍一致；文档明确说明。
- 心跳：每 15s `: ping`；服务端在 55s 主动关闭，客户端带 `Last-Event-ID` 重连。
- 过滤：`channel`（子树）、`label`、`state`、`owner`、`for`。
- **before/after 语义**：标签/任务状态事件在变更前后任一状态匹配过滤时投递。这样订阅
  `state=ready` 的 worker 能看到任务进入 ready，也能看到被 claim 后离开 ready。
- **at-least-once**：断线重连从最后 cursor 之后重放；客户端按 cursor 去重。phase 1 不要求
  ack；phase 3 watcher 才引入 delivery lease/ack。

### 12.2 多副本

- 写提交后 `pg_notify('board_events', '<tenant>:<seq>')` 作为低延迟提示；每个实例的
  listener 唤醒本机 SSE。
- SSE 循环仍按 `seq` 查询 `board_events`，不依赖 NOTIFY 载荷；每 1s 兜底轮询一次。
- 因此 NOTIFY 丢失只增加延迟，不丢事件；重启后从 DB 游标继续。

### 12.3 读一致性

- 写事务提交后投影已更新；写响应返回的视图就是提交后的视图。
- 列表读报告当前最大 seq 作为 `cursor`；读不等待，除非请求按 ID 读取且投影水位落后
  （正常情况下不会）。
- 投影重建期间（运维）可返回 503 `unavailable`，不返回半重建数据。

## 13. 客户端（nsh web/TUI）

### 13.1 同源

- 不新增 `board-surface` 运行时包；服务端投影是唯一状态源，客户端只做：
  - `web/src/data/board.ts`：归一化缓存、SSE 增量合并、乐观覆盖、游标与重连；
  - `web/src/screens/board/*`：同一份 TSX 由 nsh 打包 web 与 TUI。
- 客户端不重新 fold 账本；它消费服务端返回的 message/task 视图。这样避免 Go/TS 两套
  fold 漂移（Vercel inspector 在本地 board 内 fold，是因为它没有服务端投影；keel 有）。

### 13.2 屏幕

1. **BoardQueue**：状态 tabs（open/waiting/ready/claimed/blocked/completed/failed/canceled/all），
   默认 open；筛选条（channel 含子树、label、owner、for）；oldest first；行内显示
   `#id`、状态标记、首行、标签、`needs`、owner/for、租约或失败原因；分页“load more”。
2. **BoardThread**：根任务卡 + 回复列表 + Composer（回复）；任务详情在根上方。
3. **BoardTaskCard**：状态、owner/by/for、needs（可点进线程，显示各前置状态）、blocked
   kind/reason、result/reason；动作区按当前 viewer 的 moves 显示。
4. **BoardFilters**：channel 建议（含 `work/` 子树）、label key/value 建议（排除 `board.*`）、
   owner/for 选择；筛选是 URL 参数（web）与同一状态对象（TUI）。
5. **LiveTailStatus**：connecting/live/reconnecting/offline + 最后事件时间；断线自动重连。

### 13.3 动作

- claim、renew、release、complete（可选 result）、fail（必填 reason）、block（kind 可选
  needs_input/awaiting_approval + 必填 reason）、unblock、reopen、assign（owner 的 Actor
  列表，含 pool）、needs（编辑前置，提交空数组表示无）、cancel（必填 reason）。
- 动作按钮/菜单只展示服务端规则允许的动作（`moves` 客户端镜像），但服务端每次都重新校验。
- 危险动作二次确认；complete/fail/block/cancel 的文案说明后果（结束 claim、依赖者不 ready 等）。
- **claim token 只存内存**：同一浏览器标签内有效；刷新/重开丢失 claim，卡片显示
  “Held by this tab: a reload loses the claim.”；其他标签显示“yours, but not from this tab”。
  不用 localStorage 保存 token（XSS 面），与 Vercel inspector 的取舍一致。

### 13.4 五态

| 状态 | 队列 | 线程/任务卡 |
| --- | --- | --- |
| loading | 骨架行，不显示假数据 | 骨架根 + 回复占位 |
| empty | “no open tasks” / “no tasks match” | “no replies yet” |
| error | 错误码 + 重试；保留筛选 | 错误 + 返回队列 + 重试 |
| permission | 403/404 区分；未登录显示登录入口；无权显示原因 | 无权/不存在分别展示，不伪造内容 |
| success | 队列/任务卡/线程 | 同上 |

- TUI 用同一状态机；键盘可达：`j/k` 移动、`Enter` 打开、`c` claim、`r` renew、`e` complete、
  `f` fail、`b` block、`u` unblock、`o` reopen、`a` assign、`n` needs、`x` cancel；所有
  动作有文字标签，不依赖 hover。
- 主题：shadcn 官方组件 + 既有 solid-glass/主题令牌 + lucide 单色图标；无硬编码 hex；
  状态颜色不是唯一通道（同时有图标 + 文本）。
- 实时：SSE 新事件驱动队列/线程增量；不整页刷新；租约倒计时每秒本地重算，到点显示
  `lease ran out` 并出现 claim 动作。

## 14. superagent 与 GitHub 适配

### 14.1 分阶段，不制造第二真值

| 阶段 | 方向 | 行为 |
| --- | --- | --- |
| 2a 镜像 | superagent → board（单向） | Ticket/Assignment/Attention/report 投影为 board 任务/claim/blocked/reply；board 对镜像任务只读 |
| 2b 命令桥 | board 动作 → supervisor API | board 上的 claim/complete/fail/block/unblock/assign/cancel 调用 supervisor 既有 API；supervisor 成功后 board 才更新投影 |
| 2c 切换（不在本 change） | board 成为真值 | 另立 change，给 `collaboration-superagent` 写 MODIFIED delta |

2a/2b 都不双写：superagent 的 `collab_*` 仍是 Ticket/Assignment 的真值；board 是统一队列的
只读/命令视图。若 bridge 关闭，board 对镜像任务返回 409 `mirror_read_only`（phase 2 错误码）。

### 14.2 映射

| superagent | board |
| --- | --- |
| Ticket（`collab_work_items`, `mode_id=superagent`） | 任务线程根；文本 = Title + Request（截断 10k，完整内容留在 superagent） |
| Ticket status `open` | `ready`（有未完成 needs 时 `waiting`） |
| Ticket status `active` | 有 active Assignment → `claimed`；否则 `ready` |
| Ticket status `blocked` | `blocked`，kind `needs_input`，reason = open Attention 的 prompt |
| Ticket status `done` | `completed`，result = 最后 report 的 summary |
| Ticket status `failed` | `failed`，reason = Assignment.error 或失败 report summary |
| Ticket status `cancelled`/`cancelling` | `canceled`，reason = Mandate.revocationCause 或 “cancelled” |
| Assignment（active, session） | claim：`by` = worker Bot Actor；lease 默认 900s；renew = assignment heartbeat |
| Assignment done/failed/cancelled | complete/fail/cancel（与 Ticket 终态一致时不再重复写） |
| Attention open | `blocked`：kind `needs_input`（clarify）或 `awaiting_approval`（approval）；reason = prompt |
| Attention answered | `unblocked` + 一条回复（答案摘要，不含敏感正文） |
| Attention timed_out/cancelled | `unblocked` + 回复说明关闭；Ticket 状态由 supervisor 决定 |
| worker report | 线程回复：state + summary；幂等键 `superagent:report:<report_id>` |
| 已归档 Ticket | 只读映射或跳过；不重写历史 |

- 频道：默认 `superagent`（tenant config 可改）；标签：`source=superagent`、
  `ticket=<uuid>`、`mandate=<strength>`、`policy=<version>`。`board.*` 保留，不使用。
- **确定性 token**：镜像 claim 的 token 由 `HMAC-SHA256(KEEL_SECRET_KEY,
  "board-claim:v1:<tenant>:<assignment>:<session>")` 派生为 32 字节 base64url，进程重启可重建；
  服务端只存 hash。`KEEL_SECRET_KEY` 未配置时适配器 fail closed（不镜像、不 claim），
  不生成可预测 token。
- **幂等**：映射行 `(tenant, source, source_id, kind)` 唯一；重复同步返回同一 task；
  report/reply 用 report_id 幂等；Attention→blocked 用 attention_id 幂等。
- **claim 冲突**：若 lease 过期后 board 已被另一 Actor claim，适配器收到 `task_claimed`
  后记录告警日志，不强行覆盖；supervisor 的 Assignment 仍由 supervisor 负责恢复。
  这保证 board 的 epoch 围栏不被适配器绕过。
- **归档语义**：Ticket/Assignment/Attention 的既有审计与状态不变；适配器只追加 board
  事件；不删除、不改写 `collab_*` 行。

### 14.3 GitHub 入站与回复出站

- 复用既有 superagent `Source`（poll + cursor）与 `Channel`（send/outbox）抽象；不引入
  Vercel sync/Chat SDK。
- 频道：`github`（config 可改）；标签 `source=github`、`repo=<owner/repo>`、
  `issue=<n>`/`pr=<n>`、`github.state=open|closed|merged`。
- 入站：issue/PR opened → 根 + 任务；comment → 回复；PR merged → `completed`（result 带
  merge sha）；PR closed unmerged → `canceled`；issue closed → `completed`。GitHub label
  只作为普通标签同步，不直接写 `board.*`。
- 出站：映射线程上的 board 回复 → GitHub comment，走 outbox，外部 comment ID 幂等；
  自身入站事件按 external event ID 去重，避免回声。
- 限流/退避/游标持久化；每 tenant 每条 source 独立；跨租户不可见。
- 不做通用双向同步、评论编辑/删除、review thread 子线程（phase 3 再议）。

## 15. Watchers 与 schedules（phase 3）

- **Watcher**：`board_watchers`（tenant、watch id、channel/filter、action、bot、enabled）；
  durable consumer 按 `board_events` 顺序匹配，写入 `board_watch_deliveries`
  （at-least-once，lease + ack + 退避 + 死信）。动作以 Bot 身份执行 board 写或调用已审批
  插件工具；**不在运行时接受 URL**，只有部署时声明的代码/插件动作。动态唤醒仍可用标签
  （如 `wake=<session>`），watcher 读到标签后唤醒对应会话，不执行任意回调。
- **Schedule**：`board_schedules`（tenant、channel/re、text、labels、creator actor、
  next_run_at、every/until/times）；jobqueue worker 领取到期行，发送前重新校验创建者
  当前仍有 editor 权限；权限被回收则取消并记录原因。消息幂等键
  `schedule:<id>:<n>`，重试不重复发。
- 两者都复用既有 durable/jobqueue/triggers 能力，不新增调度器；失败不阻塞 board 主路径；
  每个 watcher/schedule 独立失败与重试。

## 16. 失败与安全矩阵

| 场景 | 行为 | 验证 |
| --- | --- | --- |
| 未认证 | 401 `unauthenticated` | handler 测试 |
| 跨租户读/写 | 404 `not_found`，无事件 | 真实 A/B tenant PG 测试 |
| 可读但无写权限 | 403 `forbidden`，`missing` 指明 | handler 测试 |
| claim 活着的任务 | 409 `task_claimed` | 并发 claim 测试 |
| claim 后租约过期 | 旧 claim 写 → 409 `claim_lost`；新 claim 成功 | 时间注入/短租约测试 |
| token 错误/缺失 | 409 `claim_lost`（不区分） | token 测试 |
| 指派给他人 | 409 `task_assigned` | 指派测试 |
| 非 owner 做 owner 操作 | 403/409 `task_not_owned` | 权限矩阵测试 |
| 前置不可读/不存在/已取消/成环/超深 | 400 `invalid_needs`，无事件 | needs 表单测试 |
| 设置 `board.*` | 400 `reserved_label` | 标签测试 |
| 同 key 不同请求 | 409 `duplicate_id` | 幂等测试 |
| 终态任务写 | 409 `task_finished` | 状态机测试 |
| DB 不可用 | 503 `unavailable`；事务回滚，无部分写 | 故障注入 |
| SSE 断线 | 客户端带 Last-Event-ID 重连；at-least-once；可见事件不丢 | E2E 断线测试 |
| 投影落后 | 按 ID 读等待/503；不返回半重建 | 重建测试 |
| token 泄露 | 读接口/SSE/审计/日志均无 token；单测扫描响应 | 红队测试 |
| 越权事件 | 不可见事件仅推进 `id:`，无 data | SSE 权限测试 |
| 租约过期显示 | 客户端按 until 显示 ready + `lease ran out` | UI 测试 |
| 镜像 claim 冲突 | 适配器记录告警，不覆盖；superagent 自行恢复 | 适配器测试 |

安全要点：

- Token hash 用 SHA-256；比较用 `subtle.ConstantTimeCompare` 或等价实现。
- `board_events.payload` 含消息正文，但只在有频道读权限的 API/SSE 中返回；审计事件
  （`auth_events`）不写正文。
- 日志/指标不记录 token、完整消息正文、用户凭据。
- 所有写接口校验 CSRF/租户上下文（沿用 keel 既有中间件）；board 不新增认证方式。

## 17. 审计、观测与保留

### 17.1 审计

- `board_events` 本身是领域审计：每条状态变化都有 tenant、channel、seq、kind、actor、
  payload、时间。任务 claim/renew/complete 等可据此回答“谁在何时做了什么”。
- 权限拒绝、跨租户尝试写既有 `auth_events`（`permission.denied`），detail 只含
  action/channel/所需权限，不含正文与 token。
- 适配器写 `board_links` 与事件；superagent 原审计不变。

### 17.2 指标

- `board_events_total{kind}`、`board_append_duration_ms`、`board_tasks_live_claims`、
  `board_lease_expired_total`、`board_sse_connections`、`board_sse_lag_seq`、
  `board_projection_rebuild_duration_ms`。
- 指标标签有界：kind/tenant 是否带 tenant 取决于既有指标规范；不在高基数标签放消息 ID。

### 17.3 保留与配额

- Phase 1：append-only，不自动删除；tenant 删除由运维 purge 级联。
- Phase 3：按 tenant 配置保留期（默认建议 365 天）与事件配额；保留策略只影响
  `board_events`/投影的归档/删除，不影响 superagent 记录。
- 这是 Owner 可推翻的默认值（见 §21），不阻塞 phase 1。

## 18. 测试与验收

1. **单元**：状态机全边表、token 生成/hash/常量时间、needs 环/深度/取消前置、标签文法/
   保留前缀、cursor 编解码、fold 纯函数。
2. **PG 集成**：RLS A/B 租户、并发 claim、租约过期、幂等重试、needs 完成传播、
   投影重建哈希、head 锁串行。
3. **HTTP**：路由权限矩阵、404/403、错误码、SSE Last-Event-ID/隐藏事件/心跳。
4. **E2E（nsh）**：队列五态、筛选、claim/complete/fail/block/unblock/reopen/assign/needs/
   cancel、租约倒计时、live tail 断线重连、web/TUI 同源；每步截图。
5. **证据**：脚本生成 HTML 图文报告；每个改动屏幕/状态的 before/after 截图；
   `UNCOVERED=0`；独立子代理复核（禁 Codex），P0/P1 清零。
6. **性能**：单租户 100 writes/s 的 append 延迟、1000 SSE 连接内存、100–200 租户 RLS 查询；
   不突破 2GB RSS 目标。

## 19. 迁移与回滚

- **Schema**：全部新增表/索引/RLS 条目；`store.ApplySchema` + `ApplyRLS` 幂等执行；
  不改既有表。Alpha 零兼容：没有旧字段/旧路径垫片。
- **Feature flag**：tenant config `board.enabled=false` 默认；关闭时 `/v1/board/*` 返回
  404 `board_disabled`。开启走 tenant config 管理（`board.manage`/运维）。
- **Phase 2 适配**：`board.superagent.mirror=false`、`board.superagent.bridge=false` 默认；
  开启只读镜像不改变 superagent；开启 bridge 才接受 board 动作。
- **回滚**：关闭 flag 即停止读写；表保留（append-only，不删数据）；不影响 superagent/
  Session。若 schema 需回退，drop 新增表即可（alpha 无生产数据承诺）。
- **不做数据迁移**：不导入 Vercel 数据、不迁移已归档工单、不双写旧看板。

## 20. Risks / Trade-offs

- **[单租户写串行化限制吞吐]** → Board 写入量低；先测量再分片。若单租户 >100 writes/s，
  后续按 channel 分片或改 per-task 行锁；本 change 不提前优化。
- **[SSE 跨副本延迟]** → NOTIFY 只是提示，DB tail 是正确性；1s 兜底轮询；断线从
  Last-Event-ID 恢复。
- **[不可见事件推进游标泄露“有活动”]** → 与 Vercel 已知取舍一致；只泄露量，不泄露内容/频道；
  文档明确；需要更强隐藏时未来加 per-channel cursor（非本 change）。
- **[Phase 2 双真值风险]** → 明确 2a 只读、2b 命令桥；superagent 成功后才更新 board；
  不对 `collab_*` 做双写；2c 另立 change。
- **[Claim token 泄露]** → 只存 hash、响应一次、读接口/SSE/审计/日志全脱敏；红队测试。
- **[标签作为状态导致索引/漂移]** → `board.task` 只由任务写维护；投影与事件同事务；
  重建校验。
- **[租约僵尸]** → lazy expiry + sweeper + 客户端倒计时；owner 可 cancel；可选未来
  “unassigned claim 自动回到池”策略（Owner 决策）。
- **[UI 复杂度]** → 服务端 `moves` 规则与客户端镜像共用同一份表驱动；只展示允许动作；
  五态与 TUI 同源测试。
- **[保留期/配额未定]** → phase 1 不删除、不计量；phase 3 由 Owner 定；不阻塞核心。
- **[Apache-2.0 归属]** → 只转译概念；若未来复制代码，按 §proposal 的 NOTICE/许可流程执行。

## 21. Open Questions（Owner 可推翻的默认值，不阻塞 phase 1）

1. **保留期与配额**：phase 1 默认 append-only 不删除、不计量；phase 3 建议 365 天 +
   `board_events` 配额。Owner 可改为更短/更长或从 phase 1 开始计量。
2. **租约上限**：默认 900s、范围 30–3600s、tenant cap 3600s。Owner 可把 cap 降到 900s
   或提高到其它值；不影响协议。
3. **superagent 切换**：本 change 只做 2a 镜像 + 2b 命令桥，不做 2c 切换。Owner 若希望
   board 成为唯一真值，需要另立 change 并给 `collaboration-superagent` 写 MODIFIED delta。
