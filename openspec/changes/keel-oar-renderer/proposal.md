# Proposal: keel-oar-renderer

> 立项：2026-11-24（v0.2.0 发布后）。上游：`keel-harness-v2` W9（OAR 记录层已交付）
> 与 W12（`keel.ui` 槽位已交付）。本 change 只补 Alpha 垂直切片最后一项：
> **OAR 外部工具在客户端可见**，以及「外部工具不进任务图」的宿主证明。

## Why

W9 把 OAR（外部 Agent 运行时）的记录层做到了可用：真实 `codex exec --json` 已跑通
（`OAR-OK`、usage 17538）、journal 幂等、typed 错误、进程组回收、`oar.Projection` 折叠
（`internal/oar/projection.go`，schema `keel.oar.render-projection/1`）。W12 交付了
`keel.ui` 槽位机制（含 `keel.ui.tool-renderer` 声明、chunk 装载、目标/能力校验、失败隔离）。
但两者还没接上：

- 客户端没有消费 `external.event` 里的 OAR 记录——对话里只有一行「外部事件 · …」
  （`web/src/screens/messages.tsx` 的 postTurnEvents 分支）；
- `oar.Projection` 只存在于 Go 侧（CLI/journal），没有产品 API 把它交给 UI；
- 「OAR 不创建任务卡」只有代码注释（`projection.go` 的 TaskID 注释），没有宿主级断言。

Alpha 切片要求「一个 OAR 外部工具（Codex）」与「UI slots + 一个 Native chunk」；
本 change 把最后一段补成可见、可验的产品行为。

## What Changes

1. **投影读取 API**：`GET /v1/sessions/{id}/external/projection`
   - 只读；租户/会话可见性走现有 seam；
   - 数据源＝该会话 external backend 的 OAR journal（`oar.ProjectionFromJournal`）；
   - 返回 `keel.oar.render-projection/1`；无记录返回同 schema 空投影；
   - 非外部会话 → 404（typed）；部署未配置 OAR journal → 501（typed）；
   - 不新增写路径、不改变事件账本。
2. **客户端渲染**：
   - 对话流对 `external.event`（`type=="oar.record"`）按会话聚合成一张
     **外部运行卡**：runtime/simulated 徽标、agentPath、call/result/status 行、
     usage、错误、归属 taskId（只引用已有任务卡，不新建卡）；
   - 渲染器以 **内置 chunk 贡献** 注册进 `keel.ui.tool-renderer`（`toolName:"oar"`），
     经现有 `SlotRegistry`/`sync`/失败隔离路径；装载失败回退到现有的一行文本；
   - 全部使用 kit 官方组件与主题令牌，不手写第三方样式。
3. **任务图证明**：宿主测试断言 OAR 记录（带 taskId）投影后 **不产生** 新任务卡，
   `task_panel` 行数不变，归属只体现在已有卡的资源/事件上。
4. **证据**：fixture 驱动的确定性 E2E（fake runtime）+ 一张外部运行卡截图 +
   投影 API 的契约测试；若本机有 codex，再跑一条 live 分支（record 层已有 W9 证据）。

## Impact

- 后端：`internal/harnessapi`（新只读路由 + journal 读取 seam）、`cmd/keel/serve.go`
  （装配 journal dir；仅在配置了 OAR 时启用）、`internal/oar`（仅导出既有 projection）。
- 前端：`web/src/screens/messages.tsx`、`web/src/plugins/{base,bundled,slots}.tsx`、
  新增 `OarRunCard` chunk；不新增 transport。
- 规格：`agent-execution`（投影读取）、`plugin-ui-extension`（外部记录的 tool-renderer 消费）。
- 不改变：内核事件、任务图写入、OAR 记录 schema、现有工具卡/面板。
