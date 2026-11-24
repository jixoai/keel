---
title: 放置与组
description: 会话的 run 在哪里执行——组、带 epoch 的租约、可解释的 FIFO 排队（202 + 位置 + ETA），以及队列永不越过的 429 边界。
---

# 放置与组

[装配](/zh/core/assembly)决定一个进程由什么构成，放置决定会话的 run 在哪里执行。模型冻结在 harness
规格里（`openspec/specs/harness-core/spec.md:24`），实现在 `internal/placement`，包契约写在
`internal/placement/types.go:1` 的文件头。

## 1. 组与租约

**组**既是放置单元也是协作单元：它拥有自己的并发槽位（`max_concurrent_runs`）、内存预算、插件装配集
与健康状态；本 change 内一个 worker 进程承载一个组（`internal/placement/types.go:5`、
`openspec/specs/harness-core/spec.md:26`）。组状态是 `active` / `draining` / `retired`，draining 的组
不再接受新放置（`internal/placement/types.go:64`）。

会话的归属是**租约而不是静态绑定**：`(epoch, lease_until)`。活租约不能被接管，过期租约可以——这正是
内核无状态契约得以保持的原因（`internal/placement/types.go:6`）。迁移会话＝旧组排水、新组从账本重建，
不复制任何进程内状态。

## 2. 策略

| 策略 | 含义 | 实现 |
|---|---|---|
| `pack`（默认） | 密度优先：按内存预算填充租户的 pack 组（`pack-default`） | `internal/placement/types.go:51` |
| `user` | 每用户一组——当前 user == tenant，即每租户一组（隔离 SKU） | `internal/placement/types.go:54` |
| `pin` | 调用方指定的组，或配置的 pin 目标 | `internal/placement/types.go:57` |

## 3. 计数在数据库里

两个限额都在数据库里计数，因此两个组卖不出同一个租户槽位，两个 worker 也超卖不了同一个组
（`internal/placement/types.go:10`）。`runs` 表仍是唯一的活跃 run 账目——放置只新增 `groups` 与
`session_placement`，绝不另立账本（`internal/placement/types.go:21`、`internal/store/schema.sql:894`、
`internal/store/schema.sql:935`）。

## 4. 组满就排队，不失败

当唯一触顶的是组槽位数时，run 变成持久 FIFO 条目：状态 `queued`，每会话一条，按数据库序列排序
（`internal/placement/types.go:12`）。输入本身已经落账（消息无论如何都是持久的），所以 HTTP 边缘如实
报告等待，而不是假装 run 已启动：

| 响应 | 何时 |
|---|---|
| `201` | 新提交且 run 已被准入（`internal/httpapi/server.go:497`） |
| `200` | 同一 `requestId` 已提交过（幂等重复） |
| `202` + `queued{position, active, limit, etaKnown, etaSeconds, etaSamples, etaBasis}` | run 在满组中等待（`internal/httpapi/server.go:927`） |
| `429` + `Retry-After` | 租户 run 配额耗尽，或组内存预算已满（`internal/httpapi/server.go:866`） |
| `409` + `reason` | 放置冲突：`epoch_mismatch`、`lease_held`、`group_draining`、`group_unavailable`、`pin_requires_group`、`already_placed`（`internal/placement/types.go:105`） |
| `501` | 内存受限组缺少估算——重试也不会好（`internal/placement/types.go:384`） |

队列位置从 1 开始，ETA 只由已观测的 attempt 推导（`avgDuration × ceil(position/limit)`，20 样本窗口、
每组保留 50 条）（`internal/placement/types.go:463`）。没有历史时 `etaSeconds` 是 `null`，也不发
`Retry-After`——未知的等待绝不报成 0（`internal/httpapi/server.go:952`）。

提升是唯一的受守卫状态迁移 `queued → pending`，按 FIFO 在 `ReleaseRun`、终态 attempt 结算与恢复扫描时
执行，因此崩溃后队列会收敛（`internal/placement/types.go:16`）。`queued` 行永远不会被内核 claim。

## 5. 429 的边界

组满**不再**是 429：它是可解释的排队准入，run 保留自己的位置。租户配额仍是硬拒绝
（`429` + `Retry-After`），否则队列就会变成超卖通道（`internal/placement/types.go:14`）。组的**内存**
预算同理：估算装不下的 run 在准入时就被拒绝，而不是排在一个没人能满足的数字后面；提升时会再检查一次
（`internal/placement/types.go:167`）。

## 6. 接线与开关

`keel serve` 与 `cmd/runtime` 把 gate 挂成准入闸门；gate 配置错误会让启动失败，而不是静默退回进程内
worker 信号量（`cmd/keel/serve.go:417`）。因此 run 并发由数据库持久准入，内核只保留 event-rate 维度
（`quota.Limits.EventsOnly`），一个 run 不会被计两次（`cmd/keel/serve.go:409`、
`internal/quota/limits.go:227`）。

| 环境变量 | 默认 | 含义 |
|---|---|---|
| `KEEL_PLACEMENT_STRATEGY` | `pack` | 未放置会话的默认策略（`internal/placement/wiring/fromenv.go:21`） |
| `KEEL_PLACEMENT_GROUP_ID` | — | pin 策略的目标组（`internal/placement/wiring/fromenv.go:22`） |
| `KEEL_GROUP_MAX_CONCURRENT_RUNS` | `0`（不限） | 策略首次使用时自动建组的槽位数（`internal/placement/wiring/fromenv.go:27`） |
| `KEEL_GROUP_MEMORY_LIMIT_BYTES` | `0`（无上限） | pack 策略的内存预算（`internal/placement/wiring/fromenv.go:30`） |
| `KEEL_PLACEMENT_LEASE_MS` | 包默认（30s） | 放置租约 TTL（`internal/placement/wiring/fromenv.go:33`、`internal/placement/types.go:468`） |
| `KEEL_PLACEMENT_QUEUE_INTERVAL_MS` | 包默认 | 崩溃恢复队列扫描周期（`internal/placement/wiring/fromenv.go:36`） |

## 7. 校验

```bash
go test ./internal/placement/...
```

该命令覆盖内存与 PG 两种 store、队列（FIFO 顺序、只提升一次、不超卖）与 gate；端到端放置路径由 HTTP
边缘的 `internal/httpapi/placement_e2e_test.go` 走通。
