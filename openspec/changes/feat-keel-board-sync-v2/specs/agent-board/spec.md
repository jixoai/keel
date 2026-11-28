# agent-board

## ADDED Requirements

### Requirement: Sync 与自动化健康可观测

系统 MUST 为具备 `board.manage` 的调用者提供只读的 sync/自动化健康事实，且 MUST NOT 为了
展示而伪造计数、时间戳或状态。每个 sync entry 的健康 MUST 包含：

- 该 entry 的外部 targets 列表，MUST 可按 entry key 枚举，并 MAY 继续按 board thread
  枚举；每项 MUST 给出 key 前缀化的外部 target、board root、pending/leased/dead 数、
  attempts、稳定错误码与 undelivered 数；
- 最老 outstanding（pending 或 leased）入站/出站行的时间戳与年龄；当该队列没有
  outstanding 行时 MUST 为 `null`，MUST NOT 写成 0；
- 最近 24 小时进入死信且未被后续成功清除的出站投递数。

自动化 watcher 的只读 DTO MUST 给出 dead 数、最近错误码与最近投递时间；没有死信时
`dead` MUST 为 0，`lastError` MUST 省略或为空。所有健康字段 MUST 只含外部 ID、计数、
时间与稳定错误码，MUST NOT 含消息正文、凭据、平台原始错误 body 或 connector secret。

#### Scenario: 无积压是 null 不是 0

- **WHEN** 一个 sync entry 的 inbox 与 outbox 都没有 pending/leased 行
- **THEN** 该 entry 的 `lag.inbox` 与 `lag.outbox` 均为 `null`，`ageSeconds` 不出现，
  客户端显示“无积压”而不是“0 秒”

#### Scenario: 有积压给出真实最老年龄

- **WHEN** 一个 entry 的 inbox 有一条 3 分钟前创建的 pending 行
- **THEN** `lag.inbox.oldestAt` 等于该行的创建时间，`lag.inbox.ageSeconds` 约为 180，
  `lag.inbox.pending` ≥ 1

#### Scenario: targets 按 entry 隔离

- **WHEN** tenant A 的 `slack` entry 有两个 board_sync_threads 映射，tenant B 有一个
- **THEN** `GET /sync/entries/slack/targets` 只返回 A 的两个 target，且每项的 board root、
  dead/pending 与 undelivered 来自 A 的 outbox 行

#### Scenario: target 可继续按 board thread 枚举

- **WHEN** 调用者读取 `GET /sync/threads?thread=<board thread>`
- **THEN** 返回该 board thread 的全部外部 target，语义与 `targets` 列表一致

#### Scenario: 24h 失败窗口

- **WHEN** 一个 entry 有一条 25 小时前进入死信的投递和一条 1 小时前进入死信的投递
- **THEN** `failures24h` 只计后者；重试成功并清除的旧死信不再计入

#### Scenario: watcher 死信可见

- **WHEN** 一个 watcher 有一条 dead 投递且最近一次失败错误码为 `rate_limited`
- **THEN** `GET /watchers` 的该 watcher `dead` ≥ 1、`lastError` 为 `rate_limited`、
  `lastDeliveryAt` 非空

#### Scenario: 权限与脱敏

- **WHEN** 没有 `board.manage` 的调用者读取 targets/lag/failures24h 或 watcher 统计
- **THEN** 返回 403；有权限的响应也不含消息正文、凭据或平台原始错误 body
