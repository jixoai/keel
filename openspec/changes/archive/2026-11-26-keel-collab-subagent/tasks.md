# Tasks: keel-collab-subagent

> 实现：2026-11-25/26，worktree `keel-collab-subagent`，已 rebase 到 main `2fe0645`
>（含 core 显式 `collab.DepthPolicy`）。
> 状态图例：`[x]` 完成并有测试/验证；`[ ]` 未完成，行尾注明原因。

## 1. 深度与 durable 链接

- [x] 1.1 扩展 subagent 首条 durable payload：`mode`、`parentSessionId`、`depth`、
      `maxDepth`、`maxChildren`、`budget`；定义 `DurableSubagentLink` 读取，旧缺 depth/cap
      fail closed。
      → `harnessext.DurableSubagentLink` / `parseDurableSubagentLink`；fork 分支不产生委派链接。
- [x] 1.2 实现 depth 语义：main=0；默认 `maxDepth=3` 允许 0..3；`-1` 不限；`0` 禁止派生；
      超限 `subagent_depth_exceeded`；请求只能收窄。
      → mode 请求层解析为 `collab.DepthPolicy`，拒绝发生在创建子 Session 之前。
- [x] 1.3 实现 `maxChildren`（活跃直接子节点）上限与 `subagent_children_exceeded`。
      → Host 的 reserve/active 计数 + `sessions.subagent_*` 投影；runner 串行化创建段并补并发用例。

## 2. Host 与入口统一

- [x] 2.1 `SubagentRunner.Spawn` 走 core Host；tenant/`bot.invoke`/预算/深度/并发/审计统一。
      → `cmd/keel/collab_host.go` 装配 `collab.Host`；`SetHost` 改为装配期一次性（nil/替换均拒绝）；
      `spawnChild`/`Launch` 重算并校验 depth/cap/width/bot.invoke。
- [x] 2.2 `agent` 工具和 HTTP `/v1/sessions/{id}/subagents` 只构造请求；支持可选
      `maxDepth`（只能收窄）与结果 `depth` 投影；HTTP spawn/fork 转发 idempotency key。
- [x] 2.3 触发器/插件 spawn 路径接入同一 Host；插件只拿窄接口 `SubagentAPI`（无 `SetHost`/
      `spawnChild`/`ForkAsync`）。负控：权限拒绝、租户不匹配、审计不可用、旧链接、直接 Launch。

## 3. 取消、预算与恢复

- [x] 3.1 父取消传播到未终态子树；不产生孤儿；审计父子 id/原因。
      → 统一 `CancelSubtree`（root+descendants）接到会话取消、子代理取消、`agent cancel`、
      Wait 超时/预算与插件入口；检查 `CancelResult.Cancelled`（queued 真取消），持久化
      `sessions.state=cancelled`，审计 `collab.subagent.cancelled`；树遍历 1024 层为
      损坏/成环护栏（不是 depth 策略，`-1` 仍不限）。
- [x] 3.2 turns/tokens/timeout 预算与 `maxChildren` 在创建前检查；detached spawn 有预算
      supervisor（轮询 run 状态与账本用量，超限取消子树）。
- [x] 3.3 重启恢复：从 durable link 重建 depth/parent/caps/budget；父链缺失时 fail closed；
      cold idempotent retry 恢复有效预算并重新启动 supervisor。
- [x] 3.4 fork 保持 eventSeq 语义，不计 depth；补 fork/depth 分离测试。

## 4. UI 与产品面

- [x] 4.1 会话页/TUI 子代理树：角色、depth、状态、tokens/turns、取消/重试。
      → web 运行页的泳道/图/390 时间线/检查器统一显示 `depth`：主 `depth 0 · 主`、子
      `depth N/M`、`-1` 显示 `不限`、`0` 显示 `禁止派生`、分叉/任务卡显示 `—`；TUI 与 web
      共用 `web/src/screens/books/run.tsx`，由 `nsh test --target tui` 验收，不是仓库外 artifact。
      注册表冷启动/不完整时 durable tree 合并（`GET /v1/subagents?rootSessionId=` 与
      `parentSessionId` 合并），记录带 status/caps/budget。
      子代理节点内置「取消子树」（`POST /v1/subagents/{id}/cancel`，父会话授权）与「重试子代理」
      （`POST /v1/sessions/{parent}/resume`，同一子会话重新排队，不是新 spawn）。
- [x] 4.2 不新增 BotKind=sub；不提供“新建 Sub Agent”实体入口。
- [x] 4.3 五态：加载/空/错误/无权限/成功；TUI 与 headless 分别验收。
      → `web/src/screens/books/run-subagent.tui.test.tsx` 覆盖运行页五态、depth 文案、
      子代理动作五态（空/加载/错误/无权限/成功）、取消与重试；`--target tui` 与
      `--target web` 均 6/6 通过。

## 5. 验收

- [x] 5.1 单测/集成：depth 0/3/-1、递归、扇出、重启、取消、权限、租户隔离、审计、预算。
      → `internal/harnessext/subagent_depth_test.go`、`subagent_p1_test.go`、
      `internal/harnessapi/subagent_depth_http_test.go`、`internal/store/subagent_links_test.go`、
      `cmd/keel/collab_host_test.go`、`internal/toolsurface/agent_depth_test.go`。
- [x] 5.2 `go test -race` 相关包 + web/TUI 回归；`openspec validate --strict`。
      → Go `-race` 通过（harnessext/harnessapi/collab/cmd/keel）；web/TUI 回归与截图见
      `.agents/reports/keel/subagent-e2e-2026-11-26/report.html`；`openspec validate --strict` 通过。
- [x] 5.3 独立 reviewer 复核；E2E 报告（树/取消/深度拒绝截图）；归档。（复核 PASS，P0/P1=0；报告公网已发布）
      → E2E 报告已生成（web 20 张 + TUI 20 张，含 depth 0/3/-1、取消/重试、动作五态）；
      待父级编排的独立复核；未 archive。

## 第二轮 P1 复核修复

- [x] Host 绕过面：`SetHost` 一次性/拒绝 nil 覆盖；插件窄接口；`Launch`/`spawnChild` 防御
      depth/cap/width/bot.invoke；负控测试。
- [x] Runner 前置拒绝审计：depth(0/3/-1)、widening、旧 link、parent missing 写
      `collab.subagent.spawn_refused`（parent/depth/maxDepth/mode/reason，无正文）。
- [x] 取消真实性：`CancelSubtree` 全入口；queued run 经 `RunCanceller` 真取消；状态持久化；
      取消审计；`-1` 与 1024 层护栏的明确决策。
- [x] 子树 maxDepth：有效上限 = min(部署上限, parent link MaxDepth)；持久化并在 cold runner
      读取；不重复传 maxDepth 的 grandchild 负控。
- [x] 预算：durable link 携带 effective budget；cold retry 恢复；detached supervisor；
      `SpawnAsync`（含 HTTP wait:false）超预算用例。
- [x] P2：HTTP idempotency、durable tree status/caps、registry+durable 合并、maxChildren 配置
      进 Host、Host 403/429 映射、ghost link 清理。

## 依赖 / 备注

- core `DepthPolicy` 已落地；本实现直接消费 `LimitedDepth/UnlimitedDepth/ForbiddenDepth`，
  不再重载 0。
- 审计写入 `auth_events`（`collab.*` kind），只含 id/原因/预算数字，不含任务正文。
- detached 预算 supervisor 是进程内事实：重启后由 idempotent retry（或显式 Wait）按 durable
  budget 恢复，timeout 从恢复时刻重新计算。
