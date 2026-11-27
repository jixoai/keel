# Design: keel-board-v1

## 0. 交付路径与版本固定

**独立仓库先行，keel 导入。** 本设计的所有核心实现（协议、日志、PG 存储、投影、API、
SSE、watchers/schedules、sync 引擎与 Slack/GitHub/Linear/Chat SDK 适配）都落在
`github.com/jixoai/keel-board`；keel 不维护第二套 `internal/board`。

| 项 | 约定 |
| --- | --- |
| 独立仓库 | `github.com/jixoai/keel-board`，Apache-2.0 |
| 上游参考 pin | `vercel/board@26428ed4e0661ff362c59abdf1215997c59891b0`（Apache-2.0，仅概念/契约） |
| 独立仓库 pin | phase 0 产出不可变 commit + `v0.1.0` tag；README/NOTICE 记录 |
| keel 依赖 | `go.mod` 固定 `github.com/jixoai/keel-board@<phase-0-commit>` 的 pseudo-version；MUST NOT 浮动 branch/tag |
| 升级 | 独立 PR + keel-board conformance + keel 集成/E2E；不得静默跟随 main |
| 许可 | keel-board `LICENSE` Apache-2.0；`NOTICE` 写明独立实现与上游归属；keel `NOTICE` 记录 module+commit |

**为什么独立仓库先行**：board 核心 + sync 是一个可脱离 keel 独立运行的标准实现；先在独立
仓库用 in-memory store、测试 auth/authorizer、假平台适配器完成 conformance，能证明协议不是
被 keel 的某个内部结构“倒推”出来的。keel 只在 phase 1 提供身份/RBAC/PG/SSE/secret 适配，
不复制核心代码。这样 sync 的 provider 契约、错误语义、重试/去重可以在没有 keel 依赖时被
第三方实现和测试。

**阶段**：

| 阶段 | 内容 | 退出条件 |
| --- | --- | --- |
| Phase 0 | 独立仓库：核心协议/日志/存储/API/SSE → watchers/schedules/attachments/reactions/blocks → sync 引擎 + Slack/GitHub/Linear 原生 + Chat SDK bridge → 测试/E2E | 独立仓库全绿，产出 commit/tag |
| Phase 1 | keel 导入：身份/RBAC/ReBAC/PG/SSE/secret 适配；superagent 只读镜像→命令桥；启用 sync | keel 集成全绿 |
| Phase 2 | nsh UI：队列/任务卡/线程/租约/live/五态；sync/watcher/schedule 状态与故障 UI；删除旧 superagent 看板（破坏性变更，无兼容开关） | 全量 E2E、复核、归档 |

Sync MUST 在 phase 0 完成；phase 1 只做“接通 keel 的身份/权限/存储/凭据”，不补 sync 功能。

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

Sync 是标准的一部分，不是附加功能：board 线程从一开始就要能双向映射到 Slack、GitHub、
Linear 与任意 Chat SDK adapter 的线程；否则“通用任务/线程标准”会在第一个外部集成处失效。
独立仓库 phase 0 必须自带 sync，keel phase 1 只负责把它接到自己的身份、权限、PG 与 secret
上。

## 2. Goals / Non-Goals

**Goals**

- 一套可被 CLI、agent、人、插件、superagent 与外部平台共用的任务/线程契约。
- 任务队列的强一致认领：并发 claim 只有一个成功，租约过期后旧 claim 不能结算。
- 任务依赖可表达、可检测环、完成时在同一写事务内 ready 依赖者。
- 所有状态变化可审计、可从日志重建；投影损坏不丢事实。
- 权限与租户边界在 API、存储、UI 三处一致；跨租户一律 404。
- nsh web/TUI 同源，队列/任务卡/线程/租约倒计时/live tail 的五态完整。
- **Sync 全量双向**：Slack/GitHub/Linear 原生 + Chat SDK 通用桥；thread 镜像、去重 marker、
  出站重试、凭据隔离、不可信输入、失败恢复。
- superagent 作为适配器接入，不制造第二真值。
- **单一任务面**：board 是唯一的任务/线程看板。Phase 2 删除旧 superagent 看板的
  web/TUI 路由、页面与只读数据源；不保留双看板、兼容开关或旧数据回退路径。
  superagent 仍是 Ticket/Assignment/Attention 的真值，但人只看 board。

**Non-Goals**

- 不照搬 Vercel 后端、Vercel API、OIDC、team/project/deployment、grants 引擎。
- 不把 board 事件写进 Session 账本，也不让 board 事件进入模型上下文。
- 不重写 superagent 的 Ticket/Mandate/Assignment/Attention；不迁移已归档工单。
- 不把 sync 降级为“只做 GitHub 最小入站”；Slack/GitHub/Linear/Chat SDK 全量属于 phase 0。
- 不在 keel-board/keel 内嵌 Node/Chat SDK 运行时；Chat SDK 只通过签名 HTTP bridge 接入。
- 不做全文搜索、聚合、DM、presence、read receipts、每线程 ACL、跨租户看板。
- 不新增外部调度器/消息队列服务/语言运行时；watchers/schedules/sync 使用 PG durable 表与
  进程内 worker，keel 导入后可复用 keel jobqueue/triggers。

## 3. 概念映射：vercel/board → keel

| vercel/board | keel 落点 | 说明 |
| --- | --- | --- |
| tenant | `tenant_id` | 所有 board 行带 tenant；RLS 强制；跨租户 404 |
| board | tenant 的 `board_*` 表 | 一个 tenant 一个 board；不做多 board |
| channel | 投影 `channels[path]` | 路径文法相同；`work/` 是子树查询语法，不是另一个频道 |
| thread | root message + 投影 `threads[root]` | 线程只有一层；回复的 `re` 指向根 |
| message | 投影 `messages[id]` + `board_events` | 文本 ≤10,000 字符；ID 是 `message.posted` 的 board seq |
| task | 同一线程根 + 投影 `tasks[root]` | 任务不是新实体，是线程根上的状态 |
| claim | `tasks[root].state=claimed` + claim 字段 | claim 是租约，不是永久锁 |
| lease | `claim.until` | 默认 900s，范围 30–3600s |
| epoch | `tasks[root].epoch` | 每次成功 claim +1；写操作必须匹配 |
| token | `claim_token_hash` | 只存 SHA-256；原文只在 claim 响应出现一次 |
| needs | `board_task_needs` | 有界 DAG，最多 20 个前置 |
| labels | `board_labels` | `board.*` 保留；`board.task` 是状态标签 |
| watcher | `board_watchers` + durable consumer | phase 0 随标准交付；不用 Vercel 注册/唤醒 |
| schedule | `board_schedules` + durable worker | phase 0 随标准交付；不用 Vercel timer |
| sync entry | `board_sync_entries`（key/platform/secret ref） | 每个平台账号一个 key；凭据不落 board 日志 |
| external target | `board_sync_threads.target` | `key:channel[:thread]`；目标与 board thread 的绑定 |
| origin / copy | `messages[id].origin` + `board_sync_copies` | 消息来自哪个平台线程；出去了哪一份 |
| dedupe / delivery marker | `board_sync_inbox` / `board_sync_outbox` | 入站 occurrence ID；出站确定性 delivery ID；echo marker |
| provider | `sync.Provider` Go 接口 | 原生 Slack/GitHub/Linear；Chat SDK 走 bridge |
| webhook | `/v1/board/sync/webhooks/{key}` | adapter 验签，不走 board 会话认证 |
| source read / typing | `/v1/board/sources/{key}/*` / ephemeral | 只读平台；typing 不落日志 |
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

### 4.2 投影缓存（不使用 PG 投影表）

投影不是第二真值：全部由 `board_events` 按序 fold 得到，保存在进程内，可随时丢弃并
replay。Phase 0 复审后固化如下实现选择：

- **不建 `board_channels/messages/threads/tasks` 等 PG 投影表。** 单写锁已把同 tenant
  的写串行化；board 的读是队列/线程/任务视图，写入目标 <100 writes/s/tenant，进程内 fold
  足够。PG 只保存日志、幂等结果、sync/automation/audit 等有独立生命周期的表。
- **避免第二真值与同事务双写。** 事件提交后同进程立即 fold 到缓存；崩溃只会丢缓存，
  下一次读按 `board_heads.seq` 追平，`CheckProjection`/`Rebuild` 可从日志完整重建。
- **多进程一致性。** 共享 PG 的进程每个 `Freshness`（默认 1s）检查一次 head；写用
  expected-head CAS，输掉竞争的写追平后重新决策一次。读的是本进程缓存，因此同进程
  读己写；跨进程读最多落后一个 freshness 窗口。SSE 开流时强制追平。
- **投影内容（语义，不是 SQL schema）**：channels（path/created/last/messages/threads）、
  messages（id/channel/thread/re/root text/labels/deleted/author/version/revision/at）、
  threads（root/channel/replyCount/last/at）、tasks（state/owner/by/for/epoch/claim/blocked/
  result/reason/updatedSeq）、needs、labels、actors、attachments、origin 与 sync facts。
  `foldBoardEvent` 是纯函数；未知 kind 保留事件、fold 跳过，旧版本可读新日志。
- **未来若出现跨进程 SQL 报表/大规模分页需求**，另立 change 增加物化投影（同事务或
  outbox 维护），不在本 change 偷带。

投影一致性检查与重建（`POST /v1/board/admin/projection/check|rebuild`、`board rebuild`）
比较缓存 watermark、日志 head 与全量 replay 的 SHA-256 digest；不一致时报告并拒绝写入，
由重建替换缓存。事件本身永不被重建改写。

### 4.2a 幂等结果

幂等结果不属于投影，仍在 PG 与日志写同事务：

```sql
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

Phase 1（keel 导入）追加：

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

Phase 0（独立仓库，随标准交付）追加 watcher/schedule：

```sql
CREATE TABLE board_watchers (
    tenant_id   uuid NOT NULL,
    watcher_id  text NOT NULL,
    channel     text NOT NULL,          -- path 或 path/ 子树
    filter      jsonb NOT NULL DEFAULT '{}'::jsonb,
    action      jsonb NOT NULL,         -- 部署时声明的动作，不接受运行时 URL
    bot_id      uuid,
    enabled     boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, watcher_id)
);

CREATE TABLE board_watch_deliveries (
    tenant_id   uuid NOT NULL,
    watcher_id  text NOT NULL,
    event_seq   bigint NOT NULL,
    status      text NOT NULL DEFAULT 'pending',
    attempts    int NOT NULL DEFAULT 0,
    next_at     timestamptz NOT NULL DEFAULT now(),
    lease_until timestamptz,
    last_error  text NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, watcher_id, event_seq)
);

CREATE TABLE board_schedules (
    tenant_id      uuid NOT NULL,
    schedule_id    uuid NOT NULL,
    channel        text NOT NULL,
    re             bigint,
    text           text NOT NULL,
    labels         jsonb NOT NULL DEFAULT '{}'::jsonb,
    creator_actor  text NOT NULL,
    next_run_at    timestamptz NOT NULL,
    every_seconds  int,
    until_at       timestamptz,
    times          int,
    sent           int NOT NULL DEFAULT 0,
    status         text NOT NULL DEFAULT 'pending',
    last_error     text NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, schedule_id)
);
CREATE INDEX board_schedules_due
    ON board_schedules (tenant_id, next_run_at)
    WHERE status = 'pending';
```

Phase 0 追加 sync：

```sql
CREATE TABLE board_sync_entries (
    tenant_id   uuid NOT NULL,
    key         text NOT NULL,          -- 用户选的 entry key，如 slack / github / acme
    platform    text NOT NULL,          -- slack | github | linear | chatsdk
    config      jsonb NOT NULL DEFAULT '{}'::jsonb,
    secret_ref  text NOT NULL,          -- 只存引用，不存凭据
    enabled     boolean NOT NULL DEFAULT true,
    status      text NOT NULL DEFAULT 'active',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, key)
);

-- 入站先落盘再 ack。(tenant,key,dedupe) 是平台 occurrence 的唯一身份。
CREATE TABLE board_sync_inbox (
    tenant_id   uuid NOT NULL,
    key         text NOT NULL,
    dedupe      text NOT NULL,          -- ≤300；平台重投必须同值
    event       jsonb NOT NULL,         -- 归一化事件
    raw         jsonb NOT NULL DEFAULT '{}'::jsonb,
    status      text NOT NULL DEFAULT 'pending',
    attempts    int NOT NULL DEFAULT 0,
    next_at     timestamptz NOT NULL DEFAULT now(),
    lease_until timestamptz,
    last_error  text NOT NULL DEFAULT '',
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, key, dedupe)
);
CREATE INDEX board_sync_inbox_due
    ON board_sync_inbox (tenant_id, key, next_at)
    WHERE status IN ('pending', 'retry');

-- board thread ↔ external thread；target 是 key:channel[:thread]。
CREATE TABLE board_sync_threads (
    tenant_id       uuid NOT NULL,
    target          text NOT NULL,
    board_root_id   bigint NOT NULL,
    board_channel   text NOT NULL,
    kind            text NOT NULL,      -- channel | thread
    cause           text NOT NULL,      -- config | manual | import
    external_thread text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, target)
);
CREATE INDEX board_sync_threads_root
    ON board_sync_threads (tenant_id, board_root_id);

-- 每个 board message 在每个 target 上的副本与文件 receipt。
CREATE TABLE board_sync_copies (
    tenant_id        uuid NOT NULL,
    message_id       bigint NOT NULL,
    target           text NOT NULL,
    external_thread  text NOT NULL,
    external_message text NOT NULL,
    files            jsonb,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, message_id, target)
);

-- 出站 durable 队列。delivery_id 由 thread+target+message+revision 决定。
CREATE TABLE board_sync_outbox (
    tenant_id    uuid NOT NULL,
    delivery_id  text NOT NULL,
    board_root_id bigint NOT NULL,
    message_id   bigint NOT NULL,
    target       text NOT NULL,
    kind         text NOT NULL,         -- post | edit | delete | react | files
    payload      jsonb NOT NULL DEFAULT '{}'::jsonb,
    status       text NOT NULL DEFAULT 'pending',
    attempts     int NOT NULL DEFAULT 0,
    next_at      timestamptz NOT NULL DEFAULT now(),
    lease_until  timestamptz,
    last_error   text NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, delivery_id)
);
CREATE INDEX board_sync_outbox_lane
    ON board_sync_outbox (tenant_id, board_root_id, next_at)
    WHERE status IN ('pending', 'retry');

CREATE TABLE board_sync_source_cursors (
    tenant_id  uuid NOT NULL,
    key        text NOT NULL,
    stream     text NOT NULL,
    cursor     text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, key, stream)
);

-- bridge 重放防护：nonce 在时间窗内唯一，过期可清。
CREATE TABLE board_sync_bridge_nonces (
    tenant_id  uuid NOT NULL,
    key        text NOT NULL,
    nonce      text NOT NULL,
    seen_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, key, nonce)
);
```

所有持久化的 `board_*` 表（core log、sync、watch/schedule、audit）都带 `tenant_id` 并
加入 RLS；投影是进程内缓存，不是表。`board_events` 与 `board_requests` 的 RLS 见 4.4。

### 4.3 为什么不用 Session 账本/task-log

- **权限边界**：Session 属于一个 Bot/Account；board 频道需要租户级共享。把 board 塞进
  Session 会让“另一个 Bot 来认领”变成跨 Session 写，违反 Session 串行与所有权。
- **上下文边界**：Session 事件会被模型读取；board 的标签、认领、租约是协作元数据，
  不应消耗 token 或改变模型输入。
- **保留/配额**：Session 账本不可删；board 未来可能需要按 tenant 配置保留期。两者生命周期
  不同，混在一起会让任一侧的策略都难以执行。
- **投影查询**：board 需要跨频道/跨任务的队列与依赖视图；Session 账本没有这些结构。

### 4.4 RLS 与租户隔离

所有持久化的 `board_*` 表（core log 的 `board_heads/board_events/board_requests`、sync、
automation、audit）都 `ENABLE/FORCE ROW LEVEL SECURITY` + tenant policy：policy 允许
`tenant_id = current_setting('keel.tenant_id', true)`，以及仅限 operator 读的
`keel.bypass_rls='on'`。应用层仍显式带 `tenant_id` 谓词（双保险）；未设置 tenant 的语句
匹配零行（fail closed），跨租户 ID 一律 404，不返回“存在但无权”。

keel 导入时这些表同样注册到 `store.rlsTenantTables`，由 keel 启动迁移复核
ENABLE/FORCE/policy；board 独立运行时由 `store` 自己的 0003/0006/0008/0009 迁移保证。

### 4.5 投影重建

- `foldBoardEvent(state, event)` 是纯函数：输入当前缓存状态，输出新状态；事件不依赖
  未加载的 PG 投影行。
- 写事务只追加事件与幂等结果；提交后同进程立即 fold 到缓存，随后唤醒 SSE/board feed。
  崩溃在两者之间只会丢缓存，下一次读按 head 追平；不做异步最终一致。
- 重建命令（内部/运维）按 `seq` 全量 replay 到新缓存，最后比对 watermark 与 SHA-256
  digest；校验通过才原子替换，失败保留旧缓存。重建幂等，不改写事件。
- 随机历史属性测试：同一组事件，增量 fold 与全量 replay 的投影逐字段相等。
- 未知 `kind`：保留事件，fold 跳过；不阻塞旧版本读新日志。
- 投影损坏：缓存 head 超过日志、或 `board_heads.seq` 与最大 seq 不一致时拒绝写入并告警，
  由重建修复；不静默覆盖。

## 5. 条件追加与并发

### 5.1 写路径

所有 board 写操作走同一个 `Append` 引擎：

```text
BEGIN
  -- 1. 认证与租户已由 HTTP 层完成；Append 再次校验 tenant 归属并 SET LOCAL keel.tenant_id
  -- 2. 锁租户头；同 tenant 的 board 写串行化
  SELECT seq FROM board_heads WHERE tenant_id = $1 FOR UPDATE
  -- 3. 在本进程缓存上做纯校验：状态、epoch/token、owner/for、needs 环、保留标签、权限
  -- 4. 逐事件分配 seq，INSERT board_events
  -- 5. 如带 idempotencyKey，INSERT board_requests（digest + 脱敏 result）
COMMIT
-- 6. 同进程把新事件 fold 进缓存；唤醒 board feed/SSE
-- 7. 其他进程在 freshness 窗口内按 head 追平
```

- **一个 tenant 一个写锁**是本设计的核心取舍。它让“并发 claim 只有一个成功”“完成时
  ready 依赖者”“needs 环检查”都在一个事务里得到强一致，不需要分布式锁或乐观重试。
- 代价是同 tenant 的 board 写串行。Board 写入是“认领/完成/发一条日志”级别，目标
  <100 writes/s/tenant；100–200 租户仍远低于 PG 能力。若未来单租户超限，再按 channel
  分片或改为 per-task 行锁；本 change 不做。
- 替代方案（已否决）：per-task `SELECT FOR UPDATE` + advisory lock。它减少锁竞争，但
  跨任务的 needs 传播、依赖环检查、频道计数需要多行一致性，容易出现死锁与部分更新。
- 读路径不取头锁；读本进程缓存 + watermark。列表读报告 `cursor`（当前最大 seq）；按 ID
  读在前面的事务提交后已 fold，若缓存落后则先追平再读。

### 5.2 并发 claim

1. 两个请求同时 `claim-ready` 同一任务。
2. 第一个取得 `board_heads` 行锁，校验 `state=ready` 且 `for` 允许，写
   `task.claimed`（`epoch+1`、`until`、`token_hash`），提交后 fold 到缓存。
3. 第二个取得锁后读到 `state=claimed`，返回 409 `task_claimed`，不写事件。
4. 若第一个在租约内崩溃，任务保持 `claimed` 直到 `claim_until`；之后任何任务请求按
   §5.4 先写 `task.expired` 再 claim。

### 5.3 崩溃与重试

- 事件与幂等结果在同一个事务；投影是提交后的进程内 fold。崩溃在 commit 前等于没发生，
  commit 后事件全部可见；缓存缺失由下一次读/重启 replay 追平。
- HTTP 重试带同一 `idempotencyKey`：同 digest 返回缓存结果（不含 token）；不同 digest
  返回 409 `duplicate_id`。
- 无幂等键的写不保证重试安全；客户端应使用幂等键，尤其是 claim/complete。

### 5.4 读修复

Board 不依赖后台定时器保证正确性：

- **过期 claim**：任何读/写触达该任务时，若 `state=claimed AND claim_until <= now()`，
  先按当前 epoch 追加 `task.expired`，置 `ready`，再继续。可选 sweeper 扫描缓存中的
  claim 到期集合，只是降低“没人访问时状态标签仍旧”的窗口；sweeper 与请求竞争时由 head
  锁与 epoch 条件保证只写一次。Phase 0 不启用 sweeper（读修复已覆盖正确性）。
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
  已完成任务删除根不改变终态。根删除在 phase 0 的消息生命周期中交付。
- 非 owner 编辑根：任务处于 waiting/ready/claimed 时追加 `task.blocked`（kind `edited`，
  reason 指向编辑），结束 claim；owner 读新文本后 unblock 或 cancel。已完成/已取消不变。
  Phase 0 交付。

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
- phase 0 只开放任务创建时的标签与 `board.task` 过滤；独立的 `POST /messages/{id}/labels`
  在 phase 0 与消息编辑一起交付。

## 10. API（phase 0 最小闭集）

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

Phase 0 追加（独立仓库）：`POST /tasks/batch`、`GET /tasks/batch/{id}`、`GET /threads`、
`POST /messages/{id}/edit`、`POST /messages/{id}/delete`、`POST /messages/{id}/labels`、
`GET /actors`、`/watchers*`、`/schedules*`、`/attachments*`、`/reactions*`、`/sync/*`、
`/sources/*`（见 §14.10）。Phase 2 只加 UI，不改这些路由。

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
| 429 | `rate_limited` | 超过写速率（可选，phase 0 打点不强制） |
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
| `board.manage` | 创建/管理频道访问、sync entries/凭据/收件箱/源读取、列出所有频道元数据、运维重建 | 在 board 路由上蕴含 board.read 与 board.write；不绕过频道内容读权限 |

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
- **Sync 例外**：平台 webhook 不走 board 会话认证，由 adapter 验签；bridge 走 HMAC。Sync 导入
  的作者是平台 actor（`<key>:user:<id>`），写入沿用该 actor 的 board 权限边界，但 sync 作为
  宿主内部能力可以写 `origin/from` 等客户端不能伪造的字段；普通调用者不能调用 sync 导入。
- Sync 管理、原始收件箱、source read 需要 `board.manage`；普通成员只看自己可读频道的
  copy/状态。凭据轮换还要求既有 `secret.write` 权限。

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
- **at-least-once**：断线重连从最后 cursor 之后重放；客户端按 cursor 去重。phase 0 的 SSE
  不要求 ack；watcher 才引入 delivery lease/ack。

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
6. **BoardSyncStatus**（phase 2）：entries/platform/状态、每个 target 的 last delivery、inbox/
   outbox lag、失败与 dead letter、凭据状态、重试按钮；只对 `board.manage` 显示敏感字段。
7. **BoardAutomation**（phase 2）：watchers/schedules 列表、启用/停用、最近运行/失败原因；
   不在 UI 编辑任意运行时 URL。

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

## 14. Sync（全量，phase 0）

Sync 不是附加功能，而是 board 标准的一半：一个 board thread 从一开始就要能双向映射到
Slack thread、GitHub PR/issue、Linear issue，或任意 Chat SDK adapter 的线程。keel-board 在
phase 0 自带完整 sync 引擎与三个原生适配器，keel phase 1 只把它接到自己的身份、权限、
PG 与 secret 上。

### 14.1 层级与真值

```text
platform webhook ──▶ provider.receive（验签/归一化/dedupe）
                   ──▶ board_sync_inbox（先落盘，再 200/202）
                   ──▶ sync function（import / 默认导入）
                   ──▶ board_events（message.posted / edited / deleted / reaction / labels）

board_events ──▶ sync engine（thread.synced / message.* / reaction / labels）
             ──▶ board_sync_outbox（每 target 一个 delivery_id）
             ──▶ sync function（deliver / start） ──▶ provider.post
             ──▶ board_sync_copies + message.synced receipt；失败 → message.sync_failed
```

| 层 | 职责 | 持久化 |
| --- | --- | --- |
| provider | 平台验签、事件归一化、发消息、文件/typing/permalink/source read | SDK 缓存可有可无；凭据不落日志 |
| sync engine | inbox/outbox、排序、去重、重试、receipt、copy、thread 绑定、echo 抑制 | `board_sync_*` + board 事件 |
| sync function | 决定 import/deliver/start；默认 helper `mirror/into/render` | 纯代码/配置 |
| board | 事件日志与投影 | `board_events`（唯一真值） |

Sync 的持久状态全部在 board PG；provider/ adapter 重启后从 inbox/outbox 与 source cursor
恢复，不依赖进程内队列。Sync 写入不是 Session 账本，不消耗模型上下文。

### 14.2 Provider 契约（Go）

`keel-board/sync.Provider`：

| 方法 | 必需 | 作用 |
| --- | --- | --- |
| `Receive(key, request) ([]InboundEvent, error)` | 是 | 验签、限长、解析平台 webhook，返回归一化事件与稳定 dedupe；不通过返回 4xx/5xx |
| `ResolveChannel(key, name) (id, error)` | 是 | 名称 → 平台 channel ID；未知 → `UnreadableError('channel_not_found')` |
| `ResolveTarget(target) (target, channel, kind, error)` | 是 | thread 或 channel 目标；`kind ∈ {channel,thread}` |
| `Post(target, kind, body) (Receipt, error)` | 是 | 发 root/reply；返回 `{thread, message, files?}` |
| `Event(context) (Event, error)` | 是 | 构建 sync function 看到的 typed envelope |
| `Edit/Delete/React/UploadFiles/FetchFile/StartTyping/Permalink/Locate/URL/Read/Join/Mention/FetchRoot` | 否 | 平台能力；缺失时按“不支持”跳过或降级，不得伪造成功 |

- `InboundEvent` 统一形状：`kind`（message/edited/deleted/reaction/lifecycle）、`channel`、
  `thread`、`subthread`、`message`、`text`、`author`、`isRoot`、`context`、`files`、`links`、
  `mentioned`、`lifecycle{type,title,url,body,labels,unset,owns,reply,createRoot,retitled}`。
- `OutboundBody`：`markdown`、`files`、`as`（验证作者）、`deployments`（可选）。
- `Receipt`：`thread`、`message`、`files`（平台 ID）。
- `canStartThreads`、`uploadsFiles`、`singleReaction` 是 provider 能力声明；channel 目标在
  `canStartThreads=false` 时拒绝，已有 thread 目标仍可写。
- provider MUST 在 `Receive` 里完成平台认证后才返回事件；board 不承担平台签名。
- provider MUST 为同一 occurrence 返回同一个 `dedupe`，为不同 occurrence（如 reaction
  add/remove/add）返回不同值；不得用当前时间兜底。
- 原生 provider 用 Go 实现；不引入 Node/运行时。Chat SDK 走 §14.4 的外部 bridge。

### 14.3 原生适配器

| 能力 | Slack | GitHub | Linear |
| --- | --- | --- | --- |
| 入站 | `message.channels/groups`、edit/delete、reaction add/remove、files、link unfurl | PR/issue opened/closed/reopened、comments、review submitted/dismissed、workflow run→CI labels、label/assignee | issue created/updated、comments、properties/labels/assignee |
| 出站 | root 起 thread、reply、edit/delete、reaction、files、typing | 已有 PR/issue thread 的 comment/edit/delete/reaction；状态标签重渲染 | board root 创建 issue、comment/edit/delete、状态/属性、reaction（尽力） |
| 线程 ID | `slack:C123[:ts]` | `github:owner/repo:123`（PR）、`github:owner/repo:issue:45`、`…:123:rc:<comment>`（归组） | `linear:ENG:<issueUUID>`、`…:c:<comment>`（归组） |
| 状态标签 | board 不自动写平台状态 | `state/review/head/tag.*/assignee.*`，按 GitHub 事件 reconcile | `state=<workflow type>`、`priority/project/tag.*/assignee.*` |
| 源读取 | channel/thread/permalink，join 可选 | PR/issue/comment 读 | team/project/issue 读 |
| 已知限制 | `chat.postMessage` 无 idempotency key：崩溃在“平台已接受、receipt 未写”之间可能重复一次 | 默认不因 board root 创建 issue（与 vercel/board 标准一致）；已有 thread 双向 | 平台 reaction webhook 不带 issue，入站 reaction 不同步；出站尽力 |

- Slack 频道键支持名称（`#prs`）与 ID（`C0123`）；名称通过缓存 listing 解析，未知/私有
  频道按 ID 映射；bot 只收到被邀请的频道；`chat:write.customize` 决定能否按作者名发；
  webhook 需在 3 秒内回答，所以先落 inbox 再 ack。
- GitHub 用 App 安装令牌；`botUserId` 用于识别自身评论，缺失时从 `viewer` 查一次；
  review thread 归组到 PR；PR/issue 的 state/label/assignee 在每个事件 reconcile；
  board labels 不反向写 GitHub（与标准一致），但 `state` 等由 GitHub 事件维护的标签可
  驱动 reread/render。
- Linear 用 app actor；team key 路由；issue 状态映射到 `triage/backlog/unstarted/started/
  completed/canceled`；board root 的 first line 作为 title、全文作为 description；
  comment thread 归组到 issue。
- 三个原生 provider 都不使用 Vercel Connect/OIDC；凭据由 §14.8 的 `SecretResolver` 提供。

### 14.4 Chat SDK 通用 bridge（不嵌入 Node）

keel-board/keel 核心是 Go，不能嵌入 Chat SDK 的 Node 运行时；但“任意 Chat SDK adapter”
仍必须支持。方案是**签名 HTTP bridge**：

```text
Chat SDK adapter sidecar（部署方自己的 Node 进程，可选）
  ├─ 接收平台 webhook，构造 Chat SDK adapter
  ├─ 调 adapter 解析/发送
  └─ 与 keel-board 交换归一化事件/投递请求

keel-board ── POST {bridge}/deliver ──▶ sidecar ──▶ 平台
keel-board ── POST {bridge}/resolve ──▶ sidecar（channel/target 解析）
keel-board ◀─ POST /v1/board/sync/bridge/{key}/events ── sidecar（平台 webhook 转归一化）
```

- **Inbound**：sidecar 验平台签名后，`POST /v1/board/sync/bridge/{key}/events`，body 是
  归一化 `InboundEvent[]` + `dedupe`；header `X-Keel-Board-Timestamp`、`X-Keel-Board-Nonce`、
  `X-Keel-Board-Signature = HMAC-SHA256(secret, timestamp + nonce + body)`。board 先查
  nonce 重放窗口（5 分钟）再落 inbox；同 dedupe 幂等。
- **Outbound**：board `POST {bridgeURL}/deliver`，HMAC 同上，body
  `{deliveryId,target,kind,body}`；sidecar 用 Chat SDK adapter 发，返回
  `{thread,message,files?}`。失败返回 5xx，board 按 §14.7 重试。
- **Resolve**：`POST {bridgeURL}/resolve`，body `{name?|target?}`，返回 channel/target 元数据；
  sidecar 可用 adapter 的 `channelIdFromThreadId`、`toChatId/fromChatId` 等。
- **契约**：字段、大小上限、错误码（`unsupported`/`auth`/`rate_limited`/`transient`/
  `permanent`）、HMAC 版本、nonce 窗口固定；bridge 不接收任意 URL，只接受配置里的 baseURL。
- **为什么不是直接依赖 `chat`/`@chat-adapter/*`**：那会把 Node 运行时变成 keel 的部署依赖，
  违反“不引入新语言/运行时”。bridge 让 adapter 留在部署方进程，board 只依赖一个签名 HTTP
  契约；原生 Slack/GitHub/Linear 不经过 bridge。
- **安全**：bridge secret 每 entry 独立；sidecar 不接触 board 的数据库/其他 tenant；board
  不把平台 token 交给 bridge；bridge 不向 board 返回凭据。

### 14.5 Thread 镜像生命周期

- **绑定**：`sync <board-id> <target>` 或 root 在 mapped channel 内由 `start` 建立；
  `board_sync_threads` 记录 `target → board_root/channel/kind/cause/external_thread`。
- **root 起 thread**：board root 在 mapped channel 投递时，sync function 对每个
  `canStartThreads` 平台收到一次 `target=null`；`start(channel)` 写 `thread.synced`
  （cause `config`），随后 backfill root + replies；root 来自该平台 channel 时不回投。
- **attach 已有 thread**：channel target 先 post root，再按序 backfill 全部回复；thread target
  只接收 join 之后的消息（不 backfill 历史）。
- **入站**：平台事件按 `thread`/`subthread` 找到 board thread；没有绑定则按 helper 路由创建
  root；`import` 写 `message.posted/edited/deleted/reaction/labels`，带 `origin` 与 `from`。
- **出站**：board thread 的每个 message/edit/delete/reaction/labels 事件，对每个 target
  生成一条 outbox；跳过来源 target、来源 key、`noSync` 消息；默认 renderer 不读标签，
  自定义 renderer 可读标签并触发 copy 更新。
- **回复目标**：若 `replyTo` 来自外部 subthread（GitHub review thread、Linear comment），
  回复回到该 subthread；否则回主 thread。board 不自动引用被回复消息，作者自己写 quote。
- **labels**：board labels 默认不写平台；平台状态用 `state`/`review`/`tag.*`/`assignee.*`
  等标签入站 reconcile；由平台拥有的 key 在下次平台事件时覆盖，board 侧手改会被纠正。
- **files/links/mentions/typing**：见 §14.12。

### 14.6 去重 marker 与幂等

| 对象 | marker | 唯一约束/行为 |
| --- | --- | --- |
| 入站事件 | `dedupe` = 平台 occurrence ID（≤300） | `(tenant,key,dedupe)` 唯一；重投返回已处理结果，不二次导入 |
| 导入消息 | `client_id = "sync:<key>:<dedupe>:<op>"` | board 写入幂等；同 ID 不同内容 409 `duplicate_id` |
| 出站投递 | `delivery_id = "delivery:<thread>:<target>:<message>:<revision>"` | `board_sync_outbox` PK；重试复用同一 ID |
| copy/receipt | `(message,target)` 唯一 | `message.synced` upsert；later success 清除 `undelivered` |
| reaction occurrence | provider 的 reaction ID；无则 webhook ID+线程+消息+emoji+人 | add/remove/add 是三次不同 occurrence，不得合并 |
| bridge | HMAC + timestamp + nonce | `board_sync_bridge_nonces` 5 分钟窗口内唯一 |

**Echo 抑制**：

1. 跳过平台自己的 bot 消息（Slack bot user、GitHub `botUserId`、Linear app actor）。
2. 跳过来源 target/key：一条从 Slack A 进来的消息不回投 Slack A。
3. 平台保留 markdown/metadata 时，出站 body 末尾带隐藏 marker：
   `<!-- keel-board:<tenant>:<message>:<targetHash> -->`（GitHub/Linear/兼容平台）；
   Slack 用 `metadata.event_type=keel_board`（`chat.postMessage` 支持时）。入站看到 marker
   即视为 echo 丢弃，marker 在入库前剥离，永不显示。
4. 平台既无 bot 身份又无隐藏 marker 时，provider MUST 配置 `botUserId`/等价身份，否则
   outbound fail closed（不发送），避免自激循环。
5. 两个 sync entry 同时映射同一平台频道是配置错误；启动时检测并告警，避免双导入/双投递。

### 14.7 出站重试与失败恢复

- **排序**：每个 board thread 一条 lane（`board_root_id`），同一 thread 的投递按事件顺序；
  不同 thread 并行；一个 thread 的慢平台不阻塞其他 thread。
- **重试**：attempt 1 失败后 1s、2s、4s、8s、16s（上限 30s），默认 5 次；429 遵守
  `Retry-After`，5xx/网络错误重试，4xx（除 408/409/425/429）视为 permanent。
- **终态**：5 次后写 `message.sync_failed`（含 target/error/attempts），继续后续事件；
  该 target 的失败留在 `undelivered`，后续成功清除。失败可见、可重试（sync 管理 API）。
- **编辑/删除/反应**：best-effort，失败跳过并记录；不阻塞消息投递。
- **文件**：先 post 文本并记 receipt，再上传文件；文件失败按 target/文件重试，成功记
  `message.files_synced`；平台不支持上传时在文本加附件说明行。
- **入站**：webhook 先落 `board_sync_inbox` 再 ack；存储失败 500 让平台重试；事件语法
  永久无效时 200 + `sync.rejected` 审计（避免平台无限重投）；处理失败按 1s 起退避，默认
  5 次（function 10 次可配），之后 dead letter，只阻塞该 external thread lane。
- **重启**：inbox/outbox 的 lease 到期后可被任意实例接管；source cursor 持久化；
  重复事件由 dedupe 吸收；不依赖进程内状态。
- **平台已知缺口**：Slack 无 idempotency key，崩溃窗口可能重复一次；文档与 UI 显示
  `duplicate risk`，不假装 exactly-once。

### 14.8 凭据隔离

- `SecretResolver(tenant, key) (Credential, error)` 每 entry 取一次；原生 provider 只拿到
  自己 entry 的凭据，不能读取其他 tenant/key 的 secret。
- 凭据 MUST NOT 写入 board 事件、`board_sync_*` 行、指标、日志、错误体；DB 只存
  `secret_ref`。若宿主选择加密存储，使用 tenant 级密钥，解密只在调用点。
- webhook signing secret 与 API token 分开；bridge HMAC secret 独立于平台 token。
- 最小 scope：Slack 只要事件/写/文件/typing 所需；GitHub App 只要 Issues/PR/Actions；
  Linear 只要 read/write/comments/issues；source read/join 单独 opt-in。
- 轮换：旧凭据可验证窗口内双活 → 新凭据生效 → 旧凭据停用；轮换失败 fail closed（不发送），
  状态 `credential_invalid`，不静默降级到无认证。
- sync 配置/凭据/收件箱/源读取需要 `board.manage` + 既有 secret 权限；普通频道成员只看到
  自己可读的 copy/状态。
- Webhook 路由不走 board 会话认证：平台请求由 provider 验签；未知 key/错误签名/超时 body
  一律拒绝。board 不信任请求里的 tenant/key，tenant 由 entry 配置决定。

### 14.9 不可信输入

平台 payload 一律不可信；sync 是外部输入进入 board 的主要入口之一：

- **大小**：webhook body ≤1 MB（可配）；单事件 raw ≤256 KB；文本 ≤10,000 字符（超出按码点
  截断并标记 `truncated`，不切开代理项）；文件 ≤25 MB、每条消息 ≤10 个；links ≤10。
- **字符**：NUL/孤立代理项/控制字符清洗；标签 key/value 按 board 文法；作者名/频道名
  限长；mention 只解析已存在的 actor，未知 handle 保持文本。
- **Markdown/HTML**：平台 HTML 先转纯文本/markdown；渲染时用现有 sanitizer，禁止
  `<script>`/`<iframe>`/事件属性/`javascript:`；外链展示只允许 http/https，抓取只允许 https。
- **URL/SSRF**：附件/链接抓取只允许 https，解析后拒绝 loopback/私网/link-local/云 metadata
  地址；不跟随重定向到被拒地址；连接 5s、总 30s、最大 25 MB；DNS 重绑定在每次连接前复查。
- **文件**：按 magic bytes 判类型；可执行/压缩炸弹/超大文件拒绝或降级为链接行；文件名
  去路径/控制字符；上传走 blob lifecycle，不整文件进内存。
- **同步函数**：只运行部署时声明的代码/插件动作，MUST NOT 在运行时接受 URL 或任意脚本；
  bridge 只调用配置里的 baseURL。
- **重放**：webhook timestamp ±5 分钟；bridge nonce 唯一；重复 dedupe 幂等。

### 14.10 Sync 管理与源读取 API

| Method | Path | 权限 | 作用 |
| --- | --- | --- | --- |
| POST | `/v1/board/sync/webhooks/{key}` | provider 验签（无 board token） | 平台 webhook 入口；先落 inbox 再 ack |
| POST | `/v1/board/sync/bridge/{key}/events` | bridge HMAC | Chat SDK sidecar 推送归一化事件 |
| GET | `/v1/board/sync/entries` | board.manage | 列出 entries/状态/平台 |
| GET | `/v1/board/sync/entries/{key}/status` | board.manage | 单 entry 的 inbox/outbox lag、失败、dead letter |
| POST | `/v1/board/sync/entries` | board.manage + secret.write | Phase 1（接 tenant secret store 后）：新建 entry（只传 secret_ref） |
| PATCH | `/v1/board/sync/entries/{key}` | board.manage + secret.write | Phase 1：启停/轮换/路由 |
| GET | `/v1/board/sync/inbox?key=&state=dead` | board.manage | 死信调试（不含 raw body/正文） |
| POST | `/v1/board/sync/inbox/{id}/retry?key=` | board.manage | 重试 dead letter |
| GET | `/v1/board/sync/outbox/failures?key=` | board.manage | 出站失败/dead letter 列表 |
| POST | `/v1/board/sync/outbox/{id}/retry` | board.manage | 重试出站死信 |
| GET/POST | `/v1/board/sync/threads` | board.manage | 列出/attach 外部 thread 映射 |
| GET | `/v1/board/sources/{key}/read?target=&limit=&after=` | board.manage | 按需读平台 channel/thread/issue |
| POST | `/v1/board/sources/{key}/join` | board.manage | Slack 公共频道 join（opt-in） |

- `GET /v1/board/sync/status` 可给 UI 汇总（entries、targets、失败数、last event）。
- Board → sidecar 的两个调用 `POST {bridgeURL}/resolve` 与 `POST {bridgeURL}/deliver` 不属于
  board 路由，由 board 用同一 HMAC 契约主动调用；sidecar 不暴露其他 board 管理能力。
- 所有 sync 管理响应不含 token/secret/raw 敏感 body；raw inbox 只对 `board.manage` 开放且
  做字段脱敏。
- Phase 0 独立仓库用配置文件/环境变量构造 registry（`cmd/board`），因此 entry 的创建/轮换
  没有安全的 secret 落点：只提供只读 entries/status 与 source read/join。Phase 1 接 tenant
  secret store 后再提供 POST/PATCH entries（10.6），避免 Phase 0 在 board 日志或 flag 里
  存明文 secret。

### 14.11 适配器配置与路由

```yaml
sync:
  slack:
    platform: slack
    secret_ref: tenant/slack/prod
    routes:
      "#errors-www": errors-www
      "#prs": prs
  github:
    platform: github
    secret_ref: tenant/github/app
    routes:
      "acme/web": { pulls: prs, issues: issues }
  linear:
    platform: linear
    secret_ref: tenant/linear/app
    routes:
      ENG: dev
  telegram:
    platform: chatsdk
    bridge_url: https://bridge.internal/telegram
    secret_ref: tenant/bridge/telegram
    routes:
      "-1001234567890": support
```

- 每个 entry 一个 key；key 出现在 external ID、target、webhook 路径与 `--source`。
- `mirror(channel)` = import + deliver + start；`into(channel)` = 只 import；`render(fn)` 覆盖
  出站渲染；自定义函数可以只用 `import`/`deliver`/`start`，不调用就什么都不做。
- 频道名解析失败：未知频道记录并跳过，最多每分钟重试；无法确认的 name key 期间对可能路由
  的事件保持/重试，不落到 fallback；new root 只在已解析频道 start。
- 一个 platform channel/repo 在一个 entry 内只能映射一次；跨 entry 重复由配置审计告警。

### 14.12 文件、链接、mentions、typing、源读取

- **文件**：入站按 provider 的 `FetchFile` 拉取，走 blob lifecycle 存储；失败 3 次后在文本
  加一行 `📎 name (size) didn't come over: reason`，消息仍落地；超过 25 MB/10 个/空文件
  按降级行处理。出站：平台支持上传则上传并记 `message.files_synced`；GitHub 无上传 API，
  用“在 board: #id”行；Slack 文件上传失败时把行加进文本并保留后续编辑。
- **链接**：Slack permalink 在入库时解析为 `links`，能对应到 board message 的用 board 快照，
  否则用平台 unfurl；私网/私有频道不抓取；快照只读、不复制文件。
- **mentions**：平台 mention 用 `@[actor-id]` 存储；出站按目标平台 native mention 解析；
  未知 actor 保持文本；导入的 platform user 以 `key:user:<id>` 命名，不映射到其他平台身份。
- **typing**：`POST /messages/{id}/typing` 只在已同步 target 上尽力显示，不写日志，不重放；
  Slack 用 assistant status，GitHub/不支持平台 no-op。
- **源读取**：`sources/{key}/read` 读平台 channel/thread/issue/PR，返回归一化只读结果，
  不写入 board；需要 `board.manage`，因为平台 token 可能读到比调用者更多内容。Slack
  private/DM/Slack Connect 按平台限制拒绝；GitHub/Linear 按 app 权限。

## 15. superagent 适配（keel phase 1）

### 15.1 分阶段，不制造第二真值

| 阶段 | 方向 | 行为 |
| --- | --- | --- |
| 1a 镜像 | superagent → board（单向） | Ticket/Assignment/Attention/report 投影为 board 任务/claim/blocked/reply；board 对镜像任务只读 |
| 1b 命令桥 | board 动作 → supervisor API | board 上的 claim/complete/fail/block/unblock/assign/cancel 调用 supervisor 既有 API；supervisor 成功后 board 才更新投影 |
| 1c 切换（不在本 change） | board 成为真值 | 另立 change，给 `collaboration-superagent` 写 MODIFIED delta |

1a/1b 都不双写：superagent 的 `collab_*` 仍是 Ticket/Assignment 的真值；board 是统一队列的
只读/命令视图。若 bridge 关闭，board 对镜像任务返回 409 `mirror_read_only`。

### 15.2 映射

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
- **与 GitHub sync 的关系**：GitHub 的 PR/issue/评论由 `keel-board` 的 GitHub sync 统一处理；
  superagent 的 GitHub `Source` 仍可为 Ticket 提供事件，但不得再起第二套 GitHub 评论同步。
  两者用 `board_links` 与外部 event ID 去重；默认只启用一个 GitHub 入口。

## 16. Watchers 与 schedules（phase 0）

- **Watcher**：`board_watchers`（tenant、watch id、channel/filter、action、bot、enabled）；
  durable consumer 按 `board_events` 顺序匹配，写入 `board_watch_deliveries`
  （at-least-once，lease + ack + 退避 + 死信）。动作以 Bot 身份执行 board 写或调用已审批
  插件工具；**不在运行时接受 URL**，只有部署时声明的代码/插件动作。动态唤醒仍可用标签
  （如 `wake=<session>`），watcher 读到标签后唤醒对应会话，不执行任意回调。
- **Schedule**：`board_schedules`（tenant、channel/re、text、labels、creator actor、
  next_run_at、every/until/times）；durable worker 领取到期行，发送前重新校验创建者
  当前仍有 editor 权限；权限被回收则取消并记录原因。消息幂等键
  `schedule:<id>:<n>`，重试不重复发。
- 两者都自带 PG durable worker（独立仓库可单独运行）；keel 导入后可复用 keel jobqueue/
  triggers，但不改变语义。不新增外部调度器；失败不阻塞 board 主路径；每个 watcher/
  schedule 独立失败与重试。

## 17. 失败与安全矩阵

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
| Webhook 签名错误/replay | 401/409，不落 inbox，不产生事件 | provider 测试 |
| 重复 webhook/dedupe | 200，返回已处理结果，只导入一次 | inbox 幂等测试 |
| 出站平台失败 | 5 次退避后 `message.sync_failed`，后续消息继续 | 故障注入 |
| 平台 429 | 遵守 Retry-After，退避，不丢弃 | 限流测试 |
| 凭据缺失/失效 | fail closed，不发送，状态 `credential_invalid` | 凭据测试 |
| 跨租户 sync key | 404，不读取/不投递 | A/B tenant 测试 |
| 不可信 payload/超长/坏 URL | 400/413 或清洗/截断；不抓私网、不执行脚本 | 红队/安全测试 |
| bridge HMAC/nonce 失败 | 401，不落 inbox | bridge 契约测试 |
| inbox dead letter | 只阻塞该 external thread lane，其他线程继续 | 队列测试 |
| echo marker/bot 自身 | 丢弃，不产生循环 | 回声测试 |
| source read 无 manage | 403，不读取平台 | 权限测试 |

安全要点：

- Token hash 用 SHA-256；比较用 `subtle.ConstantTimeCompare` 或等价实现。
- `board_events.payload` 含消息正文，但只在有频道读权限的 API/SSE 中返回；审计事件
  （`auth_events`）不写正文。
- 日志/指标不记录 token、完整消息正文、用户凭据。
- 所有写接口校验 CSRF/租户上下文（沿用 keel 既有中间件）；board 不新增认证方式。

## 18. 审计、观测与保留

### 18.1 审计

- `board_events` 本身是领域审计：每条状态变化都有 tenant、channel、seq、kind、actor、
  payload、时间。任务 claim/renew/complete 等可据此回答“谁在何时做了什么”。
- Sync 审计：webhook 接收/拒绝、inbox 处理/死信、outbound 成功/失败、凭据轮换、source
  read/join 都写 `board_audit`（tenant_id、at、kind、key、platform、target、delivery_id、
  status、attempts、bounded detail），不含 raw body、消息正文、token 或 secret。审计写失败
  只记错误日志，不改变请求的原有结果（安全拒绝本身除外）。
- `board_audit` 与其它 `board_*` 持久表一样 FORCE RLS；`GET /v1/board/admin/audit?limit=`
  （board.manage）只读当前 tenant 的最近记录。
- 权限拒绝、跨租户尝试写既有 `auth_events`（`permission.denied`），detail 只含
  action/channel/所需权限，不含正文与 token。
- 适配器写 `board_links` 与事件；superagent 原审计不变。

### 18.2 指标

- `board_events_total{kind}`、`board_append_duration_ms`、`board_tasks_live_claims`、
  `board_lease_expired_total`、`board_sse_connections`、`board_sse_lag_seq`、
  `board_projection_rebuild_duration_ms`。
- Sync：`board_sync_inbox_depth{key,status}`、`board_sync_outbox_depth{key,status}`、
  `board_sync_delivery_failures_total{key,platform}`、`board_sync_dead_letters_total{key}`、
  `board_sync_webhook_duration_ms{key,platform}`、`board_sync_dedupe_hits_total{key}`、
  `board_sync_bridge_errors_total{key}`。
- `GET /metrics` 以 Prometheus text 暴露上述指标（根路径，运维面；不含 tenant/正文）。
- 指标标签有界：kind/key/platform/status 不使用消息 ID、target 全量或任意用户文本；不在
  高基数标签放消息 ID、external message ID 或 secret。

### 18.3 保留与配额

- Phase 0/1：append-only，不自动删除；tenant 删除由运维 purge 级联。
- Phase 2 收口后：按 tenant 配置保留期（默认建议 365 天）与事件配额；保留策略只影响
  `board_events`/投影/sync inbox-outbox 的归档/删除，不影响 superagent 记录。
- 这是 Owner 可推翻的默认值（见 §22），不阻塞 phase 0/1。

## 19. 测试与验收

1. **单元**：状态机全边表、token 生成/hash/常量时间、needs 环/深度/取消前置、标签文法/
   保留前缀、cursor 编解码、fold 纯函数；sync 的 dedupe/delivery ID/echo marker/URL 守卫。
2. **PG 集成**：RLS A/B 租户、并发 claim、租约过期、幂等重试、needs 完成传播、
   投影重建哈希、head 锁串行；sync inbox/outbox lease、dead letter、source cursor。
3. **HTTP**：路由权限矩阵、404/403、错误码、SSE Last-Event-ID/隐藏事件/心跳；sync
   webhook/bridge 签名、重放窗口、管理 API 权限。
4. **Provider 契约**：native 三适配器 + Chat SDK bridge 共用 conformance（验签、重复投递、
   存储失败重试、add/remove/add occurrence、inbound/outbound 重试、稳定 event ID、并发上下文、
   不可解析 target、canStartThreads、thread attach、文件降级）。
5. **E2E（独立仓库）**：fake Slack/GitHub/Linear 服务器 + fake Chat SDK bridge；双向镜像、
   edit/delete/reaction、文件、typing、source read、出站失败/重试/dead letter、重启恢复。
6. **E2E（keel + nsh）**：队列五态、筛选、全部任务动作、租约倒计时、live tail 断线重连、
   sync 状态/失败/重试、web/TUI 同源；每步截图。
7. **证据**：脚本生成 HTML 图文报告；每个改动屏幕/状态的 before/after 截图；
   `UNCOVERED=0`；独立子代理复核（禁 Codex），P0/P1 清零。
8. **性能**：单租户 100 writes/s append 延迟、1000 SSE 连接内存、100–200 租户 RLS 查询、
   sync inbox/outbox 吞吐与 5 次重试预算；不突破 2GB RSS 目标。

## 20. 迁移、导入与回滚

- **独立仓库先行**：phase 0 在 `github.com/jixoai/keel-board` 完成 core + sync + watch；
  产出 `v0.1.0` tag 与不可变 commit。keel 在此之前不写平行 core。
- **导入**：keel `go.mod` 固定 `github.com/jixoai/keel-board@<phase-0-commit>` 的
  pseudo-version；`internal/boardintegration` 只做身份/权限/PG/SSE/secret/telemetry 适配，
  不复制领域逻辑。升级走独立 PR + conformance + keel 集成测试。
- **Schema**：keel-board 自带新增表/索引/RLS 条目；keel 在启动时幂等迁移同一 PG，
  不改既有表。Alpha 零兼容：没有旧字段/旧路径垫片。
- **Feature flag**：tenant config `board.enabled=false` 默认；关闭时 `/v1/board/*` 返回
  404 `board_disabled`。开启走 tenant config 管理（`board.manage`/运维）。
- **Sync 启用**：`board.sync.enabled=false` 默认；entries/凭据逐 tenant 配置；未配置时
  不注册 webhook、不拉 inbox、不投递，状态 `sync_disabled`。
- **Phase 1 适配**：`board.superagent.mirror=false`、`board.superagent.bridge=false` 默认；
  开启只读镜像不改变 superagent；开启 bridge 才接受 board 动作。
- **回滚**：关闭 flag 即停止读写/sync；表保留（append-only，不删数据）；不影响 superagent/
  Session。若 schema 需回退，drop 新增表即可（alpha 无生产数据承诺）。
- **不做数据迁移**：不导入 Vercel 数据、不迁移已归档工单、不双写旧看板；sync 只从启用的
  entry 的 source cursor/当前时间开始，不回灌历史平台消息（可选 backfill 另立）。

## 21. Risks / Trade-offs

- **[单租户写串行化限制吞吐]** → Board 写入量低；先测量再分片。若单租户 >100 writes/s，
  后续按 channel 分片或改 per-task 行锁；本 change 不提前优化。
- **[SSE 跨副本延迟]** → NOTIFY 只是提示，DB tail 是正确性；1s 兜底轮询；断线从
  Last-Event-ID 恢复。
- **[不可见事件推进游标泄露“有活动”]** → 与 Vercel 已知取舍一致；只泄露量，不泄露内容/频道；
  文档明确；需要更强隐藏时未来加 per-channel cursor（非本 change）。
- **[Phase 1 双真值风险]** → 明确 1a 只读、1b 命令桥；superagent 成功后才更新 board；
  不对 `collab_*` 做双写；1c 另立 change。
- **[Sync 平台 API 变化/限流]** → provider 隔离、版本化契约、429/Retry-After、每 key
  退避与熔断；适配器升级走独立 PR 与 conformance。
- **[Echo/双投递]** → bot 身份 + origin target + 隐藏 marker；缺失身份 fail closed；
  两个 entry 映射同一频道启动告警。
- **[Slack 无 idempotency key]** → 崩溃窗口可能重复一次；文档/UI 明示，不声称 exactly-once；
  可用隐藏 marker 做尽力去重。
- **[Sync 凭据泄露]** → SecretResolver、只存 ref、日志/事件/指标脱敏、每 tenant/key 隔离、
  轮换 fail closed。
- **[不可信输入/SSRF]** → 限长/schema/字符清洗、URL 私网拒绝、无重定向、文件 magic/size、
  超时；红队测试。
- **[Chat SDK bridge 成为隐藏运行时]** → bridge 是可选外部进程，不是 keel 依赖；契约固定、
  HMAC、nonce；原生 Slack/GitHub/Linear 不经过 bridge。
- **[Claim token 泄露]** → 只存 hash、响应一次、读接口/SSE/审计/日志全脱敏；红队测试。
- **[标签作为状态导致索引/漂移]** → `board.task` 只由任务写维护；投影由事件确定性 fold
  并可重建；事件是唯一真值。
- **[租约僵尸]** → lazy expiry + sweeper + 客户端倒计时；owner 可 cancel；可选未来
  “unassigned claim 自动回到池”策略（Owner 决策）。
- **[UI 复杂度]** → 服务端 `moves` 规则与客户端镜像共用同一份表驱动；只展示允许动作；
  五态与 TUI 同源测试。
- **[保留期/配额未定]** → phase 0/1 不删除、不计量；phase 2 后由 Owner 定；不阻塞核心。
- **[Apache-2.0 归属]** → 只转译概念；若未来复制代码，按 §proposal 的 NOTICE/许可流程执行。

## 22. Open Questions（Owner 可推翻的默认值，不阻塞 phase 0/1）

1. **保留期与配额**：phase 0/1 默认 append-only 不删除、不计量；phase 2 后建议 365 天 +
   `board_events` 配额。Owner 可改为更短/更长或从 phase 1 开始计量。
2. **租约上限**：默认 900s、范围 30–3600s、tenant cap 3600s。Owner 可把 cap 降到 900s
   或提高到其它值；不影响协议。
3. **superagent 切换**：本 change 只做 1a 镜像 + 1b 命令桥，不做 1c 切换。Owner 若希望
   board 成为唯一真值，需要另立 change 并给 `collaboration-superagent` 写 MODIFIED delta。
