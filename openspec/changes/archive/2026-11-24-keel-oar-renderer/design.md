# Design: keel-oar-renderer

## 1. 投影读取（后端）

- **路由**：`GET /v1/sessions/{id}/external/projection`（只读，无 request body）。
- **可见性**：复用会话读取的租户/归属校验；跨租户/缺失会话 404。
- **数据源**：
  - `sessions.external_ref`（已有列，见 `internal/store/schema.sql`）是该会话的
    backend 会话句柄；
  - OAR journal 文件命名 `session-<ref>.ndjson`（`internal/oar/journal.go`）；
  - 目录解析顺序：`-oar-journal-dir` 标志 > `KEEL_OAR_JOURNAL_DIR` 环境变量；
    空 → 501 `oar_not_wired`（typed，不返回空投影冒充可用）。
- **响应**：`oar.ProjectionJSON` 原样（schema `keel.oar.render-projection/1`）。
  - journal 存在但为空 → 空 `items`、`agents` 的同 schema 文档；
  - journal 损坏 → 500 `projection_corrupt`（不静默吞）；
  - 非外部会话（`external_ref` 为空）→ 404 `not_external_session`。
- **装配**：`cmd/keel/serve.go` 仅当 journal dir 非空时注册该路由；否则路由不存在
  （404）或在已注册时返回 501——二者择一并写测试。选择「总是注册、未配置返回 501」，
  这样客户端能区分「没有这个能力」与「没有这个会话」。

## 2. 客户端渲染（前端）

- **数据获取**：`web/src/data/http.ts` 增 `externalProjection(sessionId)`；仅在
  turn 的 `postTurnEvents` 出现 `external.event` 且 `type == "oar.record"` 时拉一次，
  按 `sessionId` 缓存，失败静默回退（不阻塞对话），错误只写在卡内。
- **渲染**：`OarRunCard`（内置 chunk，`web/src/plugins/chunks/oar-run.tsx`）：
  - 头部：`OAR` + runtime（codex/fake）+ `simulated` 徽标（fake 必须显示，不得伪装 live）；
  - 主体：按 `Projection.items` 行渲染（agentPath、kind/state、name、message/error、
    usage），长文本按行截断、可展开；
  - 归属：`taskId` 只作「关联任务」链接（打开已有卡），不渲染成新卡；
  - 全部使用 kit 组件与令牌；无新增颜色/字体。
- **槽位**：`keel.ui.tool-renderer` 的宿主在外部事件分支解析贡献（`toolName:"oar"`），
  贡献以内置 chunk 注册（`BUNDLED_CHUNKS["keel.oar:tool-renderer"]`），经
  `SlotRegistry` 的装载/失败隔离；装载失败或无贡献 → 现有「外部事件 · …」一行。
  `targets` 覆盖 web/tui；TUI 目标用文本行降级（不渲染卡片）。
- **为什么不用 client-side fold**：`oar.Projection` 是唯一记录模型（docs/OAR.md §6）；
  客户端再 fold 一遍会分叉语义，故走后端投影文档。

## 3. 任务图不污染

- 宿主层测试：在一个已有任务卡的会话上，用 fake OAR sidecar 跑一轮，断言
  `tasklog` 行数不变、无新 `task.created` 事件；投影的 `taskId` 指向原卡。
- UI 断言：外部运行卡的「关联任务」打开的是原卡（不产生第二张卡）。

## 4. 兼容与回退

- 未配置 OAR 的部署：路由 501、客户端不显示卡片（只有既有文本行）——零行为变化。
- OAR 记录 schema 不变；投影 schema 不变；不新增事件 kind。
