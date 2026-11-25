# Tasks: keel-collab-subagent

> 实现：2026-11-25，worktree `keel-collab-subagent`（基于 core `e6a363d`）。
> 状态图例：`[x]` 完成并有测试/验证；`[ ]` 未完成，行尾注明原因。

## 1. 深度与 durable 链接

- [x] 1.1 扩展 subagent 首条 durable payload：`mode=subagent`、`parentSessionId`、`depth`；
      定义 `DurableSubagentLink` 读取（parent/depth/mode），旧缺 depth fail closed。
      → `harnessext.DurableSubagentLink` / `parseDurableSubagentLink`；fork 分支不产生委派链接。
- [x] 1.2 实现 depth 语义：main=0；`maxDepth=3` 允许 0..3；`-1` 不限；`0` 禁止派生；
      超限返回 `subagent_depth_exceeded`。
      → `SubagentRunner.effectiveMaxDepth`（请求只能收窄），拒绝发生在创建子 Session 之前。
- [x] 1.3 实现 `maxChildren` 直接子节点上限与 `subagent_children_exceeded`。
      → Host 宽度检查 + `Sessions.subagent_*` 投影计数；runner 串行化创建段并用并发用例验证。

## 2. Host 与入口统一

- [x] 2.1 `SubagentRunner.Spawn` 走 core Host；tenant/`bot.invoke`/预算/深度/并发/审计统一。
      → `cmd/keel/collab_host.go` 装配 `collab.Host`，`SubagentSpawnHost` 为唯一创建门。
- [x] 2.2 `agent` 工具和 HTTP `/v1/sessions/{id}/subagents` 只构造请求；新增可选
      `maxDepth`（只能收窄）与结果 `depth` 投影。
- [x] 2.3 触发器/插件 spawn 路径接入同一 Host；负控覆盖绕过、跨租户、缺权。
      → 插件拿到的 `PluginContext.Subagents` 即已接 Host 的 runner，未导出 `spawnChild` 无法绕过；
      触发器当前不派生 sub-agent。负控：权限拒绝、租户不匹配、审计不可用、旧链接失败关闭。

## 3. 取消、预算与恢复

- [x] 3.1 父取消传播到未终态子树；不产生孤儿；审计父子 id/原因。
      → `SubagentRunner.CancelTree` 接到会话取消（`serve.go` cancelFn）与子代理取消
      （`POST /v1/subagents/{id}/cancel`）；拒绝路径不创建子 Session。
- [x] 3.2 turns/tokens/timeout 预算与 `maxChildren` 在创建前检查。
      → Host 预算收窄写入 `SubagentRecord.Budget`，两条 wait 路径使用有效预算；宽度在 Host 创建前检查。
- [x] 3.3 重启恢复：从 durable link 重建 depth/parent；父链缺失时 fail closed。
      → ledger 优先、`sessions.subagent_*` 投影兜底；独立 cold-runner 用例。
- [x] 3.4 fork 保持 eventSeq 语义，不计 depth；补 fork/depth 分离测试。

## 4. UI 与产品面

- [x] 4.1 会话页/TUI 子代理树：角色、depth、状态、tokens/turns、取消/重试。
      → web 运行图显示 `depth N[/maxDepth]`；注册表冷启动时回退 durable tree
      （`GET /v1/subagents?rootSessionId=`）。TUI 为仓库外 NSH artifact，本 worktree 未改。
- [x] 4.2 不新增 BotKind=sub；不提供“新建 Sub Agent”实体入口。
- [ ] 4.3 五态：加载/空/错误/无权限/成功；TUI 与 headless 分别验收。
      → 后端/API 五态错误已类型化；web 视觉验收需 NSH 工具链（本 worktree 无 node_modules），未跑。

## 5. 验收

- [x] 5.1 单测/集成：depth 0/3/-1、递归、扇出、重启、取消、权限、租户隔离。
      → `internal/harnessext/subagent_depth_test.go`、`internal/harnessapi/subagent_depth_http_test.go`、
      `internal/store/subagent_links_test.go`、`cmd/keel/collab_host_test.go`、`internal/toolsurface/agent_depth_test.go`。
- [ ] 5.2 `go test -race` 相关包 + web/TUI 回归；`openspec validate --strict`。
      → Go `-race` 已过（harnessext/harnessapi/toolsurface/collab）；`openspec validate --strict` 已过；
      web/TUI 回归未跑（无 NSH 构建链）。
- [ ] 5.3 独立 reviewer 复核；E2E 报告（树/取消/深度拒绝截图）；归档。
      → 待父级编排的独立复核与交付验收。

## 依赖 / 备注

- core `DepthPolicy`：本实现按变更要求把 `-1/0` 解释放在 subagent mode 请求层，未改 core 公共 API；
  待 core 显式 DepthPolicy 落地后 rebase 并改为消费该类型（当前 core 仍只有正数 `Limits.MaxDepth`）。
- 审计写入 `auth_events`（`collab.*` kind），只含 id/原因/预算数字，不含任务正文。
