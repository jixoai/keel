# Proposal: keel-collab-superagent

> 立项：2026-11-25。依赖 `keel-collab-core`；参考 `deepseek-ai/dsh-superagent-prototype`
> 的 Source → Trigger/Thread → Ticket → Assignment → Worker 模型。
> 定位：super-agent 是一种“外部事件驱动的工单/调度”协作模式，不是更大的 sub-agent。

## Why

研究原型后确认：super-agent 的核心不是一个主 Agent 派生很多子 Agent，而是：

- 持续监听外部 Source（GitHub/Feishu 等）；
- 把事件变成可授权、可追踪的 Ticket；
- 把 Ticket 派给 Worker Bot/Session；
- Worker 在自己目录/会话里实现，必要时再使用 sub-agent；
- 通过结构化 Attention 找人决策；
- 最终把结果写回原 Channel。

这与 sub-agent 的父子树正交：sub-agent 解决“任务往下拆”，super-agent 解决“外部世界不断
来活，怎么接单、派工、追踪、找人、回写”。当前 keel 已有 Bot 角色、Session、触发器、
决策、通知、任务卡，但缺少一套 mode-agnostic 的 WorkItem/Assignment/Attention/Channel
编排层。本 change 在 core 契约上实现第一个 super-agent 模式。

## What Changes

1. **superagent mode 描述**：roles = orchestrator/worker；topology = work-queue +
   assignment；workUnit = ticket；支持多 Worker 并发槽位、订阅、Attention、Channel。
2. **工单生命周期**：Source 事件进入 Trigger/Thread；orchestrator 评估并 accept/attach/
   stop/ignore；accept 创建 Ticket（授权范围/来源/强度）；Ticket 可 resume/stop/retry。
3. **Assignment**：Ticket 绑定一个 Worker Bot/Session；Assignment 独立于 Session 结束；
   支持重派、重启恢复、幂等派工；Worker 可在自己的 Ticket 目录工作并再派 sub-agent。
4. **Source/Channel（MVP：GitHub）**：读取 Issue/PR/评论/CI；结果回写评论/PR；
   Feishu 作为后续 Source，不在本 change 首版。
5. **Attention**：Worker 的结构化问题路由回原对话/来源；人答复后唤醒同一 Ticket；不靠
   自由文本摘要推断“等待人工”。
6. **策略与权限**：谁能通过哪个 Source 发起工作；`bot.invoke`/permission 边界；
   Ticket 的 Mandate 与 Policy 版本；租户隔离；审计。
7. **可靠性**：Source 游标、Trigger/Ticket 幂等、Assignment 重启恢复、Outbox/Channel
   重试、并发槽位；不丢事件、不重复终态回复。
8. **UI**：工单看板（待人工/执行中/完成/失败）、Ticket 详情、Attention 收件箱；
   orchestrator/worker 角色配置；TUI/headless 五态。

## Capabilities

### New Capabilities

- `collaboration-superagent`: 外部 Source 驱动的工单/派工/人机交接模式。

### Modified Capabilities

<!-- 首版以新 capability 落地；若复用 task-log/supervision 的既有 requirement，
     实现时再补精确 delta，不在提案阶段虚构修改。 -->

## Impact

- 新增 superagent mode/编排器/Worker 适配；复用 Bot、Session、权限、decision/notify、
  task log、trigger 基座。
- 新增 durable Ticket/Assignment/Subscription/Attention/Source cursor 存储（具体表由 design
  冻结）；所有记录带 tenant_id。
- MVP 只接 GitHub Source；Feishu、多 Source 聚合、自动 merge 不在首版。
- 不改 `keel-ecosystem` 旧板；不新增 BotKind=super。

## Out of scope

- Feishu Source、跨 Source 身份映射、自动 merge/auto-approve。
- 多租户插件市场、macOS 原生 target、Rust+gRPC。
- 将 super-agent 做成一个 Bot 类型或一个“更大的 sub-agent”工具。
