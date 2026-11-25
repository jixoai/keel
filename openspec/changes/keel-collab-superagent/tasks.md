# Tasks: keel-collab-superagent

## 1. 领域与存储

- [ ] 1.1 定义 Ticket/Assignment/Subscription/Attention/Channel/Source cursor 类型与状态机；
      全部带 tenant_id。
- [ ] 1.2 新表与迁移：`collab_work_items`、`collab_assignments`、`collab_subscriptions`、
      `collab_attention`、`collab_source_cursors`（或等价）；幂等键/索引。
- [ ] 1.3 与现有 Bot/Session/decision/notify/task log 的吸附边界；不新增 BotKind。

## 2. Orchestrator 与 Worker

- [ ] 2.1 `superagent` mode 注册：roles=orchestrator/worker；工单看板/装配投影。
- [ ] 2.2 Orchestrator：Source 事件 → Trigger/Thread → 评估 accept/attach/stop/ignore；
      accept 创建 Ticket + Mandate；重复 Trigger 幂等。
- [ ] 2.3 WorkerHost：Ticket → Assignment → Worker Session；重启恢复/重派/幂等；
      Worker 可在 Ticket 内使用 subagent mode。
- [ ] 2.4 并发槽位：tenant 上限 + 人工 follow-up 额外槽位；排队/取消/接管。

## 3. GitHub Source/Channel（MVP）

- [ ] 3.1 GitHub Source：Issue/PR/comment/CI 读取、游标、幂等、权限/允许列表。
- [ ] 3.2 GitHub Channel：评论/PR 回写、消息 id 幂等、Outbox 重试、保序。
- [ ] 3.3 订阅/fan-out：Ticket ↔ Thread；人类 follow-up 归属；不重复创建 Ticket。
- [ ] 3.4 不可信输入：Source 文本不能改 Policy/授权；审计不含正文/密钥。

## 4. Attention 与 UI

- [ ] 4.1 结构化 Attention：问题/来源/选项/路由/过期；答复唤醒同一 Ticket。
- [ ] 4.2 工单看板：待人工/执行中/完成/失败；Ticket 详情、Assignment、报告。
- [ ] 4.3 Attention 收件箱与来源跳转；web/TUI/headless 五态。
- [ ] 4.4 不提供“新建 Super Agent Bot”入口；只提供 mode/角色配置。

## 5. 验收

- [ ] 5.1 真 PG 集成：事件幂等、Ticket/Assignment 恢复、并发槽位、跨租户负控。
- [ ] 5.2 GitHub 真链路 E2E：Issue → Ticket → Worker → PR/评论 → Attention → 完成；
      每步截图 + HTML 报告（公网）。
- [ ] 5.3 独立 reviewer 复核；P0/P1 清零；`openspec validate --strict`；归档。
