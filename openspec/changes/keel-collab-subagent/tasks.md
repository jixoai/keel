# Tasks: keel-collab-subagent

## 1. 深度与 durable 链接

- [ ] 1.1 扩展 subagent 首条 durable payload：`mode=subagent`、`parentSessionId`、`depth`；
      定义 `DurableSubagentLink` 读取（parent/depth/mode），旧缺 depth fail closed。
- [ ] 1.2 实现 depth 语义：main=0；`maxDepth=3` 允许 0..3；`-1` 不限；`0` 禁止派生；
      超限返回 `subagent_depth_exceeded`。
- [ ] 1.3 实现 `maxChildren` 直接子节点上限与 `subagent_children_exceeded`。

## 2. Host 与入口统一

- [ ] 2.1 `SubagentRunner.Spawn` 走 core Host；tenant/`bot.invoke`/预算/深度/并发/审计统一。
- [ ] 2.2 `agent` 工具和 HTTP `/v1/sessions/{id}/subagents` 只构造请求；新增可选
      `maxDepth`（只能收窄）与结果 `depth` 投影。
- [ ] 2.3 触发器/插件 spawn 路径接入同一 Host；负控覆盖绕过、跨租户、缺权。

## 3. 取消、预算与恢复

- [ ] 3.1 父取消传播到未终态子树；不产生孤儿；审计父子 id/原因。
- [ ] 3.2 turns/tokens/timeout 预算与 `maxChildren` 在创建前检查。
- [ ] 3.3 重启恢复：从 durable link 重建 depth/parent；父链缺失时 fail closed。
- [ ] 3.4 fork 保持 eventSeq 语义，不计 depth；补 fork/depth 分离测试。

## 4. UI 与产品面

- [ ] 4.1 会话页/TUI 子代理树：角色、depth、状态、tokens/turns、取消/重试。
- [ ] 4.2 不新增 BotKind=sub；不提供“新建 Sub Agent”实体入口。
- [ ] 4.3 五态：加载/空/错误/无权限/成功；TUI 与 headless 分别验收。

## 5. 验收

- [ ] 5.1 单测/集成：depth 0/3/-1、递归、扇出、重启、取消、权限、租户隔离。
- [ ] 5.2 `go test -race` 相关包 + web/TUI 回归；`openspec validate --strict`。
- [ ] 5.3 独立 reviewer 复核；E2E 报告（树/取消/深度拒绝截图）；归档。
