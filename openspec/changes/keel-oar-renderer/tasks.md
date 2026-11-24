# Tasks: keel-oar-renderer

## 1. 投影读取 API（后端）

- [x] 1.1 `internal/harnessapi` 增加只读投影处理器：会话可见性校验 → `external_ref` →
      journal 路径 → `oar.ProjectionFromJournal` → `oar.ProjectionJSON`。
- [x] 1.2 typed 错误：非外部会话 404、journal 未配置 501、损坏 500；空 journal 返回空投影。
- [x] 1.3 `cmd/keel/serve.go` 装配 journal dir（`-oar-journal-dir` > `KEEL_OAR_JOURNAL_DIR`），
      注册路由，accessgate 矩阵补声明。
- [x] 1.4 测试：真 PG + 临时 journal 的契约测试（有记录/空/损坏/跨租户/非外部会话/未配置）。
      复跑须带 `KEEL_TEST_DSN=...`；用 `DSN=` 会七个测试全 SKIP 但仍打印 `ok`（假绿陷阱）。

## 2. 客户端渲染（前端）

- [x] 2.1 `web/src/data/http.ts`：`externalProjection(sessionId)`；失败类型化。
- [x] 2.2 `web/src/plugins/chunks/oar-run.tsx`：`OarRunCard`（runtime/simulated/行/usage/错误/
      关联任务），全部 kit 组件与令牌；`BUNDLED_CHUNKS` 注册。
- [x] 2.3 `keel.ui.tool-renderer` 宿主接入：外部事件（`oar.record`）按会话取投影，
      经槽位渲染；失败/未配置回退现有文本行；TUI 降级为文本。
- [x] 2.4 测试：fixture 投影渲染、simulated 徽标、行折叠、失败回退、TUI 文本降级。

## 3. 任务图不污染证明

- [x] 3.1 宿主测试：已有任务卡的会话跑 fake OAR 一轮，`tasklog` 行数/`task.created` 不变；
      投影 `taskId` 指向原卡。
- [x] 3.2 UI 断言：关联任务打开原卡，不产生第二张卡。

## 4. E2E 与证据

- [x] 4.1 fake runtime 确定性 E2E：外部会话 → 记录 → 投影 API → 外部运行卡截图。
- [x] 4.2 若本机有 codex：live 分支一条（复用 W9 证据，不重复烧 token）。
- [x] 4.3 `evidence/`：API 契约输出、投影 JSON、截图、任务图前后对比。

## 5. 收口

- [x] 5.1 `docs/OAR.md` 的渲染小节从「W12 UI owns」改为已交付；`MAINLINE.md` 更新。
- [ ] 5.2 子代理交叉复核（reviewer 逻辑 + vision 卡片视觉）；P0/P1 清零。
- [ ] 5.3 归档本 change；如产品面变化则发 v0.3.0。

## 6. 已知边界（如实登记）

- **任务卡可视落点**：会话工作区的「打开任务卡」只完成任务面板读取+选中并 toast；
  任务面板目前只挂在团队运行页，接入工作区是独立产品 change，本 change 不做。
- **`oar.record` 识别**：依据 `postTurnEvents` 的 `note === "oar.record"`（chat-surface 对
  external.event 取 payload `type`）；非 OAR 外部事件仍走原文本行。
- **TUI**：外部运行在 TUI 降级为文本行，不发投影请求。
