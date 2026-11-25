# Design: keel-collab-superagent

## 1. 拓扑

```text
Source (GitHub)
  → Trigger / Thread
  → Orchestrator Bot（评估、接单、路由、回写）
  → Ticket（可持久工单：Mandate + 状态 + Source 引用）
  → Assignment（Ticket → Worker Bot/Session）
  → Worker Session（自己的目录/循环，可再使用 subagent mode）
  → Report / Attention
  → Channel（GitHub 评论 / PR）
```

orchestrator 与 worker 都是 **Bot 角色**，不是新的 BotKind；同一个 Bot 可以在不同
WorkItem 中扮演不同角色。Ticket/Assignment 是协作层对象，不是 Session 的属性。

## 2. 原型概念到 keel 的映射

| 原型 | keel 首版落点 |
|---|---|
| Source | `collab.Source` 适配器；MVP GitHub Issue/PR/comment/CI |
| Trigger | 复用/扩展现有 trigger/事件输入；保留来源元数据与幂等键 |
| Thread | Source 内的对话/评论线程；用于订阅与上下文 |
| Ticket | 新 `collab_work_items`（ticket 类型）；Mandate/来源/状态 |
| Assignment | 新 `collab_assignments`；绑定 Ticket → Worker Bot/Session |
| Subscription | 新 `collab_subscriptions`；Ticket ↔ Thread；fan-out 输入 |
| Report | Assignment 的结构化报告（progress/waiting/ready/blocked/failed） |
| Attention | 复用 decision 语义；新增 ticket 关联与来源路由 |
| Channel | `collab.Channel`；MVP GitHub 评论/PR；Outbox 重试 |
| Policy/Mandate | 复用权限 + 新 Ticket mandate（来源/强度/范围/Policy 版本） |
| Executor | `collab.WorkerHost`：创建/唤醒 Worker Session，管理槽位 |

## 3. 状态机

```text
Ticket: open → ended
          ↑        │ human follow-up / retry
          └────────┘
Assignment: pending → running → ended
Worker Report: progress | waiting | ready | blocked | failed | dropped
Attention: open → answered | cancelled | expired
```

- Ticket 可跨多个 Assignment；Assignment 结束不等于 Ticket 结束。
- 只有人的 stop 会撤销 Mandate；Policy 变更不取消在途 Ticket，只记录并投递。
- Worker 的 `ready/blocked/failed` 不直接等于 Ticket 终态；由 Ticket 规则和结果决定。

## 4. 并发与恢复

- Worker 槽位：全局 tenant 配额 + mode 上限；人工 follow-up 可用额外槽位，避免被新工单饿死。
- Assignment 启动/输入投递/报告写 Outbox/effect，先持久化后执行。
- 重启：running Assignment 标记中断并以 recovery note 重启；未读输入重新投递；Source
  游标不倒退、不重复消费。
- 幂等：Trigger id、Ticket accept、Assignment requestId、Channel message id 均幂等。

## 5. 权限与安全

- 所有 Ticket/Assignment/Subscription/Attention 带 tenant_id。
- Orchestrator/Worker 都以自己的 Bot 权限执行；跨 Bot 调用仍看 `bot.invoke`。
- Source 输入是**不可信文本**：不能修改 Policy、不能授权新工作；只有允许的人/规则可产生
  Mandate。
- Channel 写入不能泄露其他租户/私有 Source 内容；审计不含正文/密钥。

## 6. MVP 边界

- 只做 GitHub Source；Feishu 后续。
- 只做 ticket/assignment/attention/channel 主链路；不做自动 merge、市场、跨 Source 身份映射。
- UI 先做工单看板 + Ticket 详情 + Attention 收件箱；不做完整项目管理。
- 复用现有 Bot/Session/decision/notify/task log；若字段不够，在本 change 增表，不修改
  `keel-ecosystem` 旧板。
