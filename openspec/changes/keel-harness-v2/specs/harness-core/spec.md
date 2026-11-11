# Harness Core（内核之外的系统工程层）

本 spec 定义 Harness 与内核的边界、放置模型（Group / lease / 配额）、责任分层原则，
以及通知与审批的持久事实语义。内核契约见 `agent-loop-contract`。

## ADDED Requirements

### Requirement: 内核与 Harness 的边界

内核 MUST 只做「把一个会话的一次 run 跑成可重放账本」；内核 MUST NOT 感知用户、租户、
Group、插件、文件系统。Harness MUST 通过注入（工具清单、system sections、message-parts、
invoke 回调）向内核提供一切能力。内核包 MUST NOT import 任何产品层包，该约束 MUST 由
架构测试钉住（沿用 `internal/architecture` 的 `go list -deps` 断言模式）。

#### Scenario: 内核包 import 产品层即失败
- **WHEN** `internal/loop` 或 `internal/kernel` 直接 import placement/task/fs 包
- **THEN** `go test ./internal/architecture/...` 失败并指出违规 import

#### Scenario: 内核不知道用户
- **WHEN** Harness 为两个不同用户的会话各起一个 run
- **THEN** 内核侧看到的只有会话 id 与注入物，任何事件、日志、span 标签中都不出现用户或租户标识

### Requirement: Group 是放置与协作单元

Group MUST 同时是**项目组**与**资源/放置容器**：每个 Group MUST 由一个 Harness worker
进程承载（本 change 内一组一 worker；一组多 worker 留到后续 change）。一个 Group MAY
承载多个用户的会话；同一个用户的不同会话 MAY 加入不同的 Group。Group MUST 拥有独立的
并发槽位（`max_concurrent_runs`）、内存上限、插件装配集与健康状态。

#### Scenario: 一个用户跨组工作
- **GIVEN** 用户 A 的会话 S1 加入项目组 P，会话 S2 留在组 Q
- **WHEN** S1 在 P 中消耗并发槽位执行任务
- **THEN** S2 在 Q 中的并发额度不受影响；A 的用户级配额在 DB 侧跨组聚合

#### Scenario: 组是进程边界
- **WHEN** 承载组 G 的 worker 进程因内存超限被杀
- **THEN** 组 G 内的会话被标记为可接管，其它组的进程与连接不受影响

### Requirement: 会话归属是 lease，不是静态绑定

`session → group` MUST 记录为带 `epoch` 与 `lease_until` 的 placement 事实。任何 Harness
进程 MUST 能接管任何会话（内核的无状态契约不变）。迁移 MUST 走「旧组排水 → 新组从账本
重建」，不得复制进程内状态。

#### Scenario: 排水迁移后 seq 连续
- **WHEN** 组 G1 排水，会话被 G2 接管
- **THEN** 客户端按 `Last-Event-ID` 续传，事件 seq 连续，不丢事件、不重复最终回答

#### Scenario: 双组争抢同一会话
- **WHEN** G1 与 G2 同时尝试持有同一会话的 lease
- **THEN** 只有 epoch 更高者写入成功，落败方按 409 放弃，不产生第二次并发 run

### Requirement: 组并发槽位与用户配额

每个 Group MUST 用 `max_concurrent_runs` 限制同时在跑的 run。槽位满时 MUST 明确排队或
拒绝（429 + `retry_after`），不得静默丢弃。用户配额（并发 run、tokens、存储字节）MUST 在
DB 侧聚合，跨组一致；分组不得成为超卖配额的通道。

#### Scenario: 组满时的可解释失败
- **WHEN** 组内已有 `max_concurrent_runs` 个 run 在跑，第 N+1 个请求到达
- **THEN** 请求返回 429 并带 `retry_after`，或进入队列并返回排位；客户端 UI 可展示原因

#### Scenario: 跨组配额不超卖
- **GIVEN** 用户 A 的并发配额为 15，A 的会话分布在组 P 与 Q
- **WHEN** A 在两个组中合计已启动 15 个 run
- **THEN** 第 16 个 run 被拒绝或排队，不因分组而越过配额

### Requirement: 责任分层

插件 MUST 只向上声明「我做什么」（工具、hook、触发器、能力），MUST NOT 依赖或查询同层
插件的运行时保证（例如 skills 的 patch 不得询问文件系统是否有写保护，bash 不得询问
layerfs 是否强一致）。需要组合语义时 MUST 由 Harness 在装配期决定并注入。

#### Scenario: 插件不检查同层
- **WHEN** 装配 skills 与 file 两个插件
- **THEN** skills 只调用 file-plugin 的公开工具契约，不读取 file-plugin 的内部状态或保证声明

#### Scenario: 装配期决定组合
- **WHEN** Harness 需要「写入前校验任务归属」
- **THEN** 该校验由 Harness 的装配层完成，而不是让 file-plugin 去认识 task-plugin

### Requirement: 通知与审批是持久事实

通知的「已消费」MUST 是持久事件（ack）；审批的 open/answer/timeout MUST 是 durable 状态，
不得用进程内定时器推断。系统级原生推送 MUST 只在「有 ack 能力且在线」的消费者未在窗口内
ack 时触发，不得以「没人点已读」作为唯一判据。URL 形态的消费 MUST 单次有效且不可猜。

#### Scenario: 在线客户端即时消费
- **GIVEN** 一个持有 live 连接的客户端已收到通知
- **WHEN** 客户端在窗口内 ack
- **THEN** 不触发系统原生推送；ack 落为持久事件

#### Scenario: 离线时推送一次
- **GIVEN** 没有任何在线消费者
- **WHEN** 通知超过窗口未被 ack
- **THEN** 触发一次原生推送；进程重启后不重复推送

#### Scenario: 审批超时可回放
- **WHEN** 审批在超时后被重新读取
- **THEN** 返回持久化的超时状态（而非重新计时），答案与超时顺序可从账本复现

### Requirement: 单进程多租户模式保留

`pack` 策略下单进程承载多租户 MUST 继续成立；进程切分 MUST 是 Harness 的放置策略，不是内核
契约，也不是密度目标的必要条件。

#### Scenario: 单进程密度不回归
- **WHEN** 以 `pack` 策略在单进程内跑 1000 个会话
- **THEN** 行为与 v1 一致，且 RSS 满足下一条「内存预算」的要求

### Requirement: 内存预算

后端整体（内核 + Harness + layerfs）MUST 满足：**目标 ≤300MB、硬上限 ≤500MB**（以 RSS 计）。
前端（NSH 客户端）MUST 可分离部署（另一台机器 / 手机浏览器），其 surface VM MUST 有配额
（默认单 surface 32–64MB）且关闭即卸。各组件 MUST 遵守自己的缓冲、输出与并发上限；MUST NOT
以"整体上限宽松"为由取消单项有界约束。参考：v1 实测 1000 会话峰值 523.8–639.1MB，**本预算
低于既有基线，是待达标项**。

#### Scenario: 时间线不回归
- **WHEN** 以 `pack` 跑 1000 会话 + M 并发 run，并打开与关闭重型插件面板
- **THEN** 后端 RSS 峰值 ≤500MB、稳态落在 ≤300MB 目标区间且可解释；面板关闭后内存回落可测

#### Scenario: 单组越界不扩散
- **WHEN** 一个 Group 的资源触顶
- **THEN** 只有该组的会话受影响，其它组的 RSS 与连接不受影响
