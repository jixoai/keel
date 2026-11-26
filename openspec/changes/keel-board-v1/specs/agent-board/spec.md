# agent-board

## Purpose

为 keel 提供一个租户级、跨 Session、跨 Bot 的通用任务与线程标准：任务就是线程根，回复就是工作日志，认领是带租约与 epoch 围栏的 claim，依赖与标签组成可路由的队列，所有状态变化可从追加日志重建；board 线程还必须能通过 Slack、GitHub、Linear 与任意 Chat SDK adapter 双向同步，并保持去重、重试、凭据隔离与不可信输入防护。

## ADDED Requirements

### Requirement: 租户级追加日志是唯一真值

系统 MUST 为每个 tenant 维护一条 append-only 的 board 事件日志，事件 MUST 携带
`tenant_id`、`channel`、tenant 内单调递增的 `seq`、`kind`、`payload`、`actor` 与时间。
所有频道、消息、线程、任务、标签与依赖状态 MUST 是该日志的投影；投影 MUST 可以从日志
按序重放重建，重建结果 MUST 与增量 fold 逐字段一致。Board 日志 MUST NOT 写入 Session
账本、MUST NOT 进入模型上下文、MUST NOT 替代 task-log 或 superagent 的既有记录。

#### Scenario: 投影可从日志重建

- **WHEN** 运维清空 board 投影并从 `board_events` 按 `seq` 重放
- **THEN** 重建后的频道、消息、任务、标签与 needs 与重建前逐字段相等，且重复重建不产生重复行

#### Scenario: 日志与 Session 账本隔离

- **WHEN** 一个 Board 任务被认领、完成或阻塞
- **THEN** 只追加 board 事件，不写入任何 Session 的 `events` 表，也不改变模型上下文

#### Scenario: 未知事件不阻塞读

- **WHEN** 旧版本实例读到新版本写入的未知 `kind`
- **THEN** 该事件保留在日志中，fold 跳过它，既有投影仍可读

### Requirement: 频道、线程与消息契约

系统 MUST 使用路径式频道（小写字母、数字、`_`、`-`，段间 `/`，总长 ≤2048），频道路径
MUST NOT 改变。线程 MUST 只有一层：回复的 `re` MUST 指向根消息；回复 MAY 额外记录
`reply_to` 指向被直接回复的消息。消息文本 MUST 支持 markdown 风格纯文本，长度 ≤10,000
字符；消息 ID、任务 ID 与 cursor MUST 是不透明字符串，客户端 MUST NOT 解析、计数或按它
排序。频道首次发帖 MAY 自动创建频道；显式创建 MUST 幂等并返回 `created`。

#### Scenario: 首次发帖创建频道

- **WHEN** 调用者在尚不存在的 `work` 频道发帖
- **THEN** 频道被创建，消息成为线程根，后续 `GET /channels` 能看到它

#### Scenario: 回复进入同一线程

- **WHEN** 调用者对根消息 `re=812` 回复
- **THEN** 新消息的 `re` 为 `812`、`thread_id` 为 `812`，读取根线程时按序包含该回复

#### Scenario: 频道路径不可变

- **WHEN** 任何请求试图修改既有频道的路径
- **THEN** 请求被拒绝，不产生事件，原路径与消息关联保持不变

### Requirement: 任务即线程根与七态状态机

系统 MUST 把任务定义为线程根上的状态，而不是独立实体。任务状态 MUST 是
`waiting`、`ready`、`claimed`、`blocked`、`completed`、`failed`、`canceled` 之一，并
MUST 按本 spec 的合法边迁移；终态 `completed` 与 `canceled` MUST NOT 再迁出。任务根
MUST 记录 `owner`（创建任务的 Account）、`by`（执行创建的 Actor）、可选的 `for`
（owner 的某个 Actor）、`needs`、`epoch`、`claim`、`blocked`、`result` 与 `reason`。
任务状态 MUST 同时投影为根的 `board.task` 标签。

#### Scenario: 新任务初始状态

- **WHEN** 调用者在 `work` 创建没有 needs 的任务
- **THEN** 任务初始为 `ready`，根带 `board.task=ready`，可被 claim

#### Scenario: 有依赖的任务等待

- **WHEN** 创建任务时指定 `needs=[810]`，且 810 尚未 completed
- **THEN** 任务初始为 `waiting`，`needs` 读取为 `["810"]`，不可被 claim

#### Scenario: 终态不可复活

- **WHEN** 对一个 `completed` 或 `canceled` 任务调用 complete/fail/claim
- **THEN** 返回 409 `task_finished`，不产生事件，状态不变

#### Scenario: 根编辑阻塞任务

- **WHEN** 非 owner 编辑处于 waiting/ready/claimed 的任务根
- **THEN** 任务变为 `blocked`（kind `edited`），既有 claim 结束，owner 可 unblock 或 cancel

### Requirement: Claim 是租约且必须被 epoch 围栏

系统 MUST 把 claim 实现为租约：默认 900 秒，允许 30–3600 秒，服务端 MUST 给出请求值与
tenant cap 的较小者。每次成功 claim MUST 使任务的 `epoch` 加一，并 MUST 只存储 claim
token 的 SHA-256；token 原文 MUST 只在 claim 响应中出现一次，MUST NOT 出现在任何读接口、
SSE、列表、审计或日志中。所有 claim 写操作（renew/release/complete/fail/block）
MUST 同时携带匹配的 `epoch` 与 `token`；不匹配、claim 已结束或租约已过期 MUST 返回 409
`claim_lost` 且不改变状态。并发 claim 同一任务 MUST 只有一个成功，其余 MUST 返回 409
`task_claimed`。租约过期后任务 MUST 可被重新 claim，旧 claim 的写 MUST 失败。

#### Scenario: 并发认领只有一个成功

- **WHEN** 两个调用者同时 claim 同一个 `ready` 任务
- **THEN** 恰好一个收到 `{task, claim}`，另一个收到 409 `task_claimed`，任务只有一个 claim 事件

#### Scenario: 租约过期后旧 claim 失效

- **WHEN** 租约到期后旧持有者调用 complete，另一调用者已重新 claim
- **THEN** 旧持有者收到 409 `claim_lost`，任务保持新持有者的 `claimed` 状态

#### Scenario: token 不进入读视图

- **WHEN** 调用 `GET /tasks`、`GET /messages/{id}` 或 `GET /events/stream`
- **THEN** 响应中的 claim 只有 actor 与 until，没有 token 或 token hash

#### Scenario: 自带 token 可重建 claim

- **WHEN** 调用者用 32 字节 base64url token claim 任务
- **THEN** 服务端使用该 token 的 hash 落库，调用者在另一个进程用同一 token 可以续写 claim

#### Scenario: 只读调用者看到有效状态

- **WHEN** 只有频道 viewer 权限的调用者列出一个租约已过期的 claimed 任务
- **THEN** 该任务按 `ready` 返回并标注租约已过期，调用者不需要 `board.write` 即可看到正确状态

### Requirement: 依赖 needs 是有界 DAG

系统 MUST 允许任务声明最多 20 个前置任务；前置 MUST 存在、是任务、可被调用者读取、且
MUST NOT 是 `canceled`。创建或修改 needs 时 MUST 检测自环与环，环检测的遍历 MUST 有界
（最多 500 个任务），超过上限 MUST 返回 400 `invalid_needs`。完成一个任务 MUST 在同一写
事务内 ready 所有“全部前置已 completed”的直接依赖者；失败或取消的前置 MUST NOT ready
任何依赖者。修改 needs MUST 只允许 owner 或频道 owner，且任务处于 waiting/ready/blocked
且没有 live claim；live claim 时 MUST 返回 409 `task_claimed`，终态时 MUST 返回 409
`task_finished`。

#### Scenario: 完成前置 ready 依赖者

- **WHEN** 任务 814 的 needs 是 `[812]`，812 被 complete
- **THEN** 同一请求内 814 变为 `ready` 并带 `board.task=ready`，814 可立即被 claim

#### Scenario: 失败前置不 ready 依赖者

- **WHEN** 812 被 fail 或 cancel，814 的 needs 仍是 `[812]`
- **THEN** 814 保持 `waiting`，直到 812 被 reopen 并最终 complete，或 814 被取消/改 needs

#### Scenario: 环被拒绝

- **WHEN** 任务 A 的 needs 设为 `[B]`，同时 B 的 needs 设为 `[A]`
- **THEN** 第二次修改返回 400 `invalid_needs`，两个任务的状态与 needs 都不改变

#### Scenario: 已取消的前置不可引用

- **WHEN** 创建任务时 `needs` 指向一个 `canceled` 任务
- **THEN** 返回 400 `invalid_needs`，不创建任务

### Requirement: 标签与保留命名空间

系统 MUST 支持消息/任务根上的 key-value 标签；key MUST 匹配
`^[a-z0-9][a-z0-9_.-]*$` 且长度 1–64，value MUST 为 1–256 字符且不含 NUL、孤立代理项或
控制字符。`board.` 前缀 MUST 是 board 的保留命名空间：客户端设置或取消任何 `board.*`
标签 MUST 返回 400 `reserved_label`；`board.task` MUST 只由任务写操作维护。标签过滤
MUST 支持 `key`、`key=value`、`key=a|b`，重复过滤 MUST 为 AND，`|` MUST 为 OR；MUST
按“消息本身或其线程根”的标签匹配。

#### Scenario: 客户端写 board 标签被拒

- **WHEN** 调用者在创建任务或发帖时携带 `board.task=completed`
- **THEN** 返回 400 `reserved_label`，不创建消息/任务，不产生事件

#### Scenario: 根标签命中整个线程

- **WHEN** 根带 `step=fix`，用 `label=step=fix` 读取频道
- **THEN** 根与其回复都命中该过滤

#### Scenario: 多值过滤是 OR

- **WHEN** 使用 `label=state=open|draft`
- **THEN** 标签值为 `open` 或 `draft` 的消息都命中

### Requirement: 幂等写与游标分页

系统 MUST 接受可选的 `idempotencyKey`（1–200 字符），并在同一 tenant 内保证：相同 key
与相同请求返回首次结果，不重复产生事件；相同 key 与不同请求 MUST 返回 409
`duplicate_id`。幂等结果 MUST 与事件在同一事务提交，且 MUST NOT 包含 claim token。
列表读 MUST 使用不透明 cursor 分页：`limit` 默认 50、最大 500；`after` MUST 返回其后的
下一页；非法 cursor MUST 返回 400 `invalid_cursor`。消息 ID MUST 可作为 cursor 使用。

#### Scenario: 重试不重复发帖

- **WHEN** 同一 `idempotencyKey` 与同一正文重试发帖
- **THEN** 返回首次创建的消息 ID，日志中只有一条 `message.posted`

#### Scenario: 同 key 不同请求被拒

- **WHEN** 同一 `idempotencyKey` 发送不同正文
- **THEN** 返回 409 `duplicate_id`，不产生第二条消息

#### Scenario: 游标翻页

- **WHEN** 列表返回 `limit=50` 与 `cursor`，调用者用 `after=<cursor>` 再读
- **THEN** 返回接下来的 50 条，不重复上一页的记录

### Requirement: 权限、频道访问与租户隔离

系统 MUST 以 Account 为唯一权限主体；Bot MUST NOT 拥有独立权限。租户级权限键 MUST 为
`board.read`（读 board 与 SSE）、`board.write`（发帖、创建任务、claim、标签与 claim 写）、
`board.manage`（创建/管理频道与运维）；`board.manage` MUST 在 board 路由上蕴含
`board.read` 与 `board.write`。频道内容访问 MUST 复用 ReBAC 关系：
`resource_type=board_channel`，`viewer` 读、`editor` 读写、`owner` 读写并可管理任意任务；
频道关系 MUST 按“精确路径优先，其次最近祖先”继承到子树。任务 owner 规则 MUST 允许
owner Account（含其全部 Bot 创建的任务）或执行创建的同一 Actor 做 owner 操作；频道
`owner` 关系 MUST 也可做 owner 操作。不可读的频道/消息/任务 MUST 返回 404；可读但缺少
写权限 MUST 返回 403；跨租户访问 MUST 返回 404 且不泄露存在性。

#### Scenario: 跨租户不可见

- **WHEN** tenant A 的调用者按 ID 读取 tenant B 的任务或消息
- **THEN** 返回 404 `not_found`，不返回内容、状态或事件

#### Scenario: viewer 只读

- **WHEN** 调用者只有频道 `viewer` 关系
- **THEN** 可以读取频道、任务与线程，但发帖/claim 返回 403 `forbidden`

#### Scenario: editor 可认领

- **WHEN** 调用者有频道 `editor` 关系并 claim 一个 `ready` 任务
- **THEN** claim 成功；即使任务 owner 是另一个 Account

#### Scenario: 非 owner 不能取消

- **WHEN** 频道 `editor`（非任务 owner、非频道 owner）调用 cancel
- **THEN** 返回 403/409 `task_not_owned`，任务状态不变

#### Scenario: 子树关系继承

- **WHEN** 调用者在 `work` 上有 `editor`，访问 `work/production`
- **THEN** 继承 editor 权限；若在 `work/secret` 上显式给 `viewer`，该子树按 viewer 收窄

#### Scenario: 创建者成为频道 owner

- **WHEN** 持有 `board.write` 的调用者创建频道或首次发帖自动建频道
- **THEN** 创建者在同一事务获得该频道的 `owner` 关系，可以继续管理该频道

### Requirement: 事件流与断线恢复

系统 MUST 提供 SSE 事件流，事件 `id` MUST 等于 board `seq`，并 MUST 支持
`Last-Event-ID` 与 `after` 从指定游标续读。事件 MUST 携带 cursor、type、createdAt、
channel、payload 以及事件后的 message/task 视图（适用时）。流 MUST 支持 channel、label、
state、owner、for 过滤；标签/状态事件 MUST 在变更前后任一状态匹配过滤时投递。对调用者
不可见的事件 MUST 只发送 `id` 空帧以推进游标，MUST NOT 发送内容。流 MUST 有周期心跳，
并在断开后允许客户端从最后 cursor 重连；投递语义 MUST 为 at-least-once，客户端 MUST 能按
cursor 去重。

#### Scenario: Last-Event-ID 续读

- **WHEN** 客户端在收到 cursor 120 后断线，重连带 `Last-Event-ID: 120`
- **THEN** 服务端只发送 seq > 120 的事件，不重复 120 及之前的事件

#### Scenario: 不可见事件只推进游标

- **WHEN** 订阅者没有某私有频道的读权限，该频道发生事件
- **THEN** SSE 只收到该事件的 `id` 空帧，收不到 channel、payload 或消息内容

#### Scenario: 事件离开视图也投递

- **WHEN** 订阅 `state=ready` 的客户端遇到任务被 claim
- **THEN** 客户端收到该 `task.claimed` 事件（变更前匹配 ready），可把任务移出 ready 列表

### Requirement: 审计、可观测与投影重建

系统 MUST 让每条任务状态变化、claim、依赖变更、标签变更和消息写入都可由 board 事件
审计，事件 MUST 携带 tenant、channel、seq、kind、actor 与时间。权限拒绝 MUST 写入既有
`auth_events`（`permission.denied`），且 MUST NOT 包含消息正文、token 或凭据。系统 MUST
提供 board 指标（事件数、append 延迟、live claim 数、租约过期数、SSE 连接与 lag、重建
耗时），标签 MUST 有界且不得包含消息 ID。投影 MUST 支持一致性校验；`board_heads.next_seq`
与最大事件 seq 不一致时 MUST 拒绝写入并告警，不得静默覆盖。

#### Scenario: 状态变化可追溯

- **WHEN** 一个任务从 ready 到 claimed 再到 completed
- **THEN** 日志中有对应 task.claimed 与 task.completed 事件，含 actor、epoch、时间，可回答谁在何时完成

#### Scenario: 权限拒绝不含正文

- **WHEN** 未授权调用者尝试读取私有频道任务
- **THEN** `auth_events` 记录 permission.denied 与所需权限，不包含任务文本或 claim token

#### Scenario: 校验失败拒绝写入

- **WHEN** 投影水位与日志最大 seq 不一致
- **THEN** 写请求返回 503 `unavailable`，不追加事件，告警提示需要重建

### Requirement: 客户端五态与 web/TUI 同源

系统 MUST 提供 nsh 的 Board 队列、任务卡与线程界面，web 与 TUI MUST 使用同一份 TSX 与
同一客户端状态源。界面 MUST 覆盖加载、空、错误、权限不足、成功五态，并 MUST 显示任务
状态、owner/by/for、needs、阻塞 kind/reason、结果/原因、租约倒计时与 live tail 连接状态。
动作 MUST 按服务端规则只展示当前调用者允许的操作，但服务端 MUST 在每次写入时重新校验。
Claim token MUST 只保存在客户端内存；刷新或换标签页后 MUST 显示“claim 不在本标签页”的
提示，MUST NOT 从 localStorage 恢复 token。队列 MUST 默认按 oldest first 排序，并支持
状态 tabs、channel/label/owner/for 过滤与分页。

#### Scenario: 租约倒计时到点

- **WHEN** 客户端显示一个 `claimed` 任务，租约剩余 3 秒后到期
- **THEN** 倒计时归零后行内显示 `lease ran out`，状态按 `ready` 呈现并出现 claim 动作，无需刷新

#### Scenario: 刷新后不冒充持有者

- **WHEN** 用户在持有 claim 的页面刷新
- **THEN** 页面不再显示可 complete/renew 的持有态，而显示“yours, but not from this tab”或可重新 claim

#### Scenario: 五态可达

- **WHEN** 队列分别处于加载、无任务、请求失败、无权限、正常数据
- **THEN** 五种状态都有明确文案与可达的下一步动作（重试/登录/清筛选/新建）

#### Scenario: web 与 TUI 同源

- **WHEN** 同一 BoardQueue 在 nsh web 与 TUI target 渲染
- **THEN** 状态 tabs、筛选、任务行与权限动作一致，平台差异只允许图标/悬停等不可移植项

### Requirement: 双向线程同步（Slack/GitHub/Linear 与 Chat SDK 通用桥）

系统 MUST 提供 sync 引擎，把 board thread 与外部平台 thread 双向镜像。Sync MUST 在 phase 0
交付 Slack、GitHub、Linear 三个原生适配器，并通过签名 HTTP bridge 支持任意 Chat SDK
adapter；MUST NOT 把 sync 降级为“只做 GitHub 最小入站”。每个 sync entry MUST 有独立的
key、platform、路由与凭据；MUST 支持 root 起 thread、attach 已有 thread（channel target
backfill root+replies，thread target 只接收 join 之后的消息）、reply/edit/delete/reaction、
files、typing、permalink 与 source read。系统 MUST 跳过来源 target/key、平台自身 bot、
`noSync` 消息；同一 board thread 的投递 MUST 保序，不同 thread MUST 可并行。GitHub 原生
MUST 覆盖 PR/issue 生命周期、comments、review、CI/state/label/assignee；board root 默认
不创建 issue（与标准一致），已有 PR/issue 线程 MUST 双向。Linear 原生 MUST 支持 board
root 创建 issue、comments、状态/属性/标签/assignee。Chat SDK bridge MUST 以 HMAC +
timestamp + nonce 交换归一化事件与投递请求，MUST NOT 把 Node/Chat SDK 运行时嵌入
keel-board/keel 核心。

#### Scenario: Slack 双向镜像

- **WHEN** Slack 频道里出现一条新消息，board 对应 thread 随后出现一条回复
- **THEN** 新消息成为 board 线程根/回复，board 回复出现在同一 Slack thread；edit/delete/reaction
  也同步到对应副本

#### Scenario: GitHub PR 生命周期与评论

- **WHEN** GitHub PR 打开、收到 review/comment、合并
- **THEN** board 对应线程出现根、回复与 `state/review/head` 等标签；board 回复写回该 PR 的评论，
  merge 后平台状态反映在 board 标签上

#### Scenario: Linear issue 与 board root 互建

- **WHEN** Linear issue 被创建/评论，或 board root 在 mapped team channel 发布
- **THEN** 对应 board 线程/Linear issue 被创建，评论与回复双向；状态/属性/标签保持同步

#### Scenario: attach 与 thread target 的差异

- **WHEN** 调用者把一个已有 board thread attach 到 Slack channel target，或 attach 到已存在的
  Slack thread target
- **THEN** channel target 收到 root + 历史回复的 backfill；thread target 只收到 join 之后的新消息

#### Scenario: Chat SDK 通用桥

- **WHEN** 部署方用任意 Chat SDK adapter 的 sidecar 推送一条归一化事件并请求一条出站投递
- **THEN** board 按 dedupe 只导入一次，并通过 HMAC 校验的 `/deliver` 调用 sidecar；核心进程不加载
  Node/Chat SDK

#### Scenario: 文件与 typing

- **WHEN** Slack 消息带文件，或 board 对一个已同步线程调用 typing
- **THEN** 文件经 blob lifecycle 落到 board（超限降级为附件说明行），typing 只在支持平台尽力显示
  且不写入日志

### Requirement: Sync 去重、重试与失败恢复

系统 MUST 为每个入站 occurrence 使用稳定 `dedupe`（≤300 字符），并以
`(tenant,key,dedupe)` 唯一约束保证重投只导入一次；同一 action 的后续 occurrence（如
reaction add/remove/add）MUST 有不同的 dedupe。导入消息 MUST 使用确定性 client ID；出站
MUST 使用确定性 `delivery_id`；receipt/copy MUST 唯一。出站失败 MUST 按有界指数退避重试
（默认 1s 起至 30s、5 次），之后写 `message.sync_failed`，MUST 继续后续消息；每 target 的
失败 MUST 可见，并由后续成功清除。入站 MUST 先持久化再 ack；存储失败 MUST 返回 5xx 让平台
重试；处理失败 MUST 重试并在超过上限后进入 dead letter，且 MUST 只阻塞该 external thread
lane。重启 MUST 从 durable inbox/outbox/source cursor 恢复。系统 MUST 通过平台 bot 身份、
来源 target/key、`noSync` 与隐藏 echo marker 抑制回声；平台既无可靠 bot 身份又无 marker 时
MUST fail closed（不发送）。

#### Scenario: 重复 webhook 只导入一次

- **WHEN** 平台因超时重投同一个 message webhook
- **THEN** board 只产生一条消息，第二次返回已有结果

#### Scenario: reaction 的三次 occurrence

- **WHEN** 同一人连续 add、remove、add 同一 reaction
- **THEN** 系统视为三个不同 occurrence，最终 board 状态为 added，不合并中间事件

#### Scenario: 出站失败后继续

- **WHEN** 一条消息投递到 Slack 连续失败 5 次
- **THEN** 记录 `message.sync_failed` 与失败原因，同 thread 的下一条消息继续投递；后续成功清除该
  target 的失败标记

#### Scenario: 重启恢复

- **WHEN** 进程在 inbox/outbox 处理中崩溃并重启
- **THEN** 未 ack 的事件/投递被重新领取，按 dedupe/delivery_id 幂等完成，不重复产生消息

#### Scenario: dead letter 隔离

- **WHEN** 一个 external thread 的事件持续失败并进入 dead letter
- **THEN** 其他 external thread 与 board 主路径继续工作，dead letter 可查询与重试

#### Scenario: echo 抑制

- **WHEN** board 投递的消息被平台作为入站事件回传，或消息带隐藏 marker
- **THEN** sync 丢弃该事件，不产生第二条 board 消息或循环

### Requirement: Sync 凭据隔离与不可信输入

系统 MUST 通过 per-tenant/per-entry `SecretResolver` 获取平台凭据，MUST NOT 把 token、
signing secret 或 bridge secret 写入 board 事件、`board_sync_*` 行、指标、日志或错误体。
Provider MUST 在接收事件前验证平台签名；Chat SDK bridge MUST 验证 HMAC、timestamp 与
nonce，并有重放窗口。凭据 MUST 最小 scope、可轮换；轮换失败 MUST fail closed，MUST NOT
静默降级为无认证。平台 payload MUST 视为不可信：MUST 限长、schema 校验、清洗 NUL/孤立
代理项/控制字符、消毒 markdown/HTML、限制 link/file 大小与数量；抓取 URL MUST 只允许
https 公网地址，MUST 拒绝 loopback/私网/link-local/云 metadata 与重定向到这些地址，MUST
有连接/总超时。系统 MUST NOT 允许运行时数据注入回调 URL、脚本或任意 bridge baseURL。

#### Scenario: 跨租户凭据隔离

- **WHEN** tenant A 的 Slack entry 投递消息
- **THEN** 只使用 tenant A 的 secret；tenant B 的 webhook/secret 不可见，跨租户 key 返回 404

#### Scenario: secret 不泄露

- **WHEN** 读取 board 事件、sync 状态、inbox 或日志
- **THEN** 响应/日志中没有 token、signing secret、bridge secret 或原始凭据

#### Scenario: sync 管理需要 board.manage

- **WHEN** 没有 `board.manage` 的调用者读取 sync entries/inbox 或调用 source read
- **THEN** 返回 403，不返回平台凭据、原始事件或平台内容

#### Scenario: 伪造 webhook

- **WHEN** 请求缺少/伪造平台签名或 timestamp 超出窗口
- **THEN** 返回 401/403，不落 inbox，不产生事件

#### Scenario: SSRF 尝试

- **WHEN** 平台消息里的附件/链接指向 `http://127.0.0.1`、私网或云 metadata
- **THEN** 抓取被拒绝，消息以附件说明行降级落地，不发起内网请求

#### Scenario: 超长/恶意文本

- **WHEN** 平台发送超过 10,000 字符或含脚本/控制字符的文本
- **THEN** 文本按码点截断并清洗，HTML 被消毒，消息仍落地且不执行脚本

#### Scenario: bridge 重放

- **WHEN** 同一 bridge nonce 在窗口内重复提交
- **THEN** 返回 401/409，不二次导入

### Requirement: superagent 适配

系统 MUST 通过适配器把 superagent 的 Ticket、Assignment、Attention 与 worker report
映射为 board 任务、claim、blocked 与线程回复；适配 MUST 先单向镜像，再提供命令桥，且
MUST NOT 制造第二真值：superagent 仍是 Ticket/Assignment 的真值，board 只在 supervisor
成功后更新投影。映射 MUST 幂等：Ticket/Assignment/Attention/report 重复同步 MUST NOT
产生重复任务、claim 或回复。已归档工单 MUST 保持只读，MUST NOT 被改写。GitHub 的
PR/issue/评论 MUST 由统一 sync 处理；superagent 的 GitHub Source MUST NOT 另起第二套
评论同步，两者 MUST 用映射行与外部 event ID 去重。

#### Scenario: Ticket 投影为任务

- **WHEN** 一个 superagent Ticket 被创建或首次被适配器看到
- **THEN** board 出现一个任务线程，标签含 `source=superagent` 与 `ticket=<id>`，重复同步不新增第二个

#### Scenario: Attention 投影为 blocked

- **WHEN** Ticket 上出现一个 open Attention
- **THEN** 对应任务变为 `blocked`，kind 为 `needs_input` 或 `awaiting_approval`，reason 为问题文本

#### Scenario: report 成为回复

- **WHEN** worker 提交一份 report
- **THEN** 任务线程出现一条对应回复，重复提交同一 report 不产生第二条

#### Scenario: 命令桥不双写

- **WHEN** bridge 开启，调用者在 board 上 complete 一个镜像任务
- **THEN** 先调用 supervisor 的完成 API，成功后才追加 board 事件；supervisor 拒绝时不产生 board 状态变化

#### Scenario: GitHub 不双同步

- **WHEN** 同一 GitHub PR 同时被统一 sync 和 superagent Source 看到
- **THEN** 只产生一条 board 线程/回复；映射行与 external event ID 去重，不起第二套评论同步

### Requirement: Watcher 与 schedule 扩展

系统 MUST 在 phase 0 提供 durable watcher 与 schedule，且两者 MUST 复用 board 事件与
durable worker（独立仓库自带 PG worker；keel 导入后可复用 keel jobqueue/triggers），MUST NOT
新增外部调度器或运行时。Watcher MUST 以部署时声明的代码/插件动作运行，MUST NOT 在运行时
接受任意 URL；动作 MUST 以 Bot 身份执行并受 `board.write` 与频道关系约束。Schedule MUST
持久化发送计划，触发时 MUST 重新校验创建者的频道权限；权限被回收 MUST 取消并记录原因。
投递 MUST 为 at-least-once，重试 MUST 有退避与死信，MUST NOT 因一个 watcher/schedule 失败
阻塞 board 主路径。

#### Scenario: watcher 失败不阻塞 board

- **WHEN** 一个 watcher 动作连续抛错并进入死信
- **THEN** 其他 watcher、任务与消息写入继续工作，board 事件不丢失

#### Scenario: schedule 触发前权限被回收

- **WHEN** schedule 创建者失去频道 editor 权限后到达触发时间
- **THEN** schedule 被取消并记录 `revoked`，不发送消息

#### Scenario: watcher 不在运行时接 URL

- **WHEN** 动态数据里出现一个回调 URL
- **THEN** watcher 不访问该 URL，只执行已部署的动作或唤醒既有会话

### Requirement: 失败与降级语义

系统 MUST 在任何失败下保持“要么全部提交、要么全部不提交”：事件、投影与幂等结果 MUST
在同一事务。数据库不可用 MUST 返回 503 `unavailable`，MUST NOT 返回部分结果或部分写入。
SSE 断线 MUST 允许从最后 cursor 重连，MUST NOT 丢失可见事件（允许重复）。租约过期 MUST
按 effective `ready` 对待，MUST NOT 让旧 claim 继续写。非法 needs、保留标签、错误 token、
终态写、跨租户与越权 MUST 使用本 spec 定义的类型化错误，且 MUST NOT 产生副作用。

#### Scenario: 数据库故障无部分写

- **WHEN** 事务提交前数据库连接失败
- **THEN** 客户端收到 503 `unavailable`，日志、投影、幂等结果都没有新增

#### Scenario: 断线重连不丢可见事件

- **WHEN** SSE 连接中断 30 秒后客户端带最后 cursor 重连
- **THEN** 期间所有对调用者可见的事件都被补发，客户端按 cursor 去重

#### Scenario: 租约过期不阻塞新认领

- **WHEN** 一个 claim 的租约已过期且没有旧持有者写入
- **THEN** 另一个调用者可以立即 claim，任务从 effective ready 进入新的 claimed
