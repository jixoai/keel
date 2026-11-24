# Tasks: keel-site-architecture-refresh

> 审计已完成：逐页事实与 path:line 在 `evidence/fact-sheet.md`。执行按 lane 并行，
> 每个 lane 的文件集合互不重叠；`site/.vitepress/config.mts` 只由 C1 改。

## 1. 审计与基线

- [x] 1.1 逐页审计 → `evidence/fact-sheet.md`（页面/优先级/问题/当前事实/建议；含 lane 划分）。
- [ ] 1.2 基线：四道门与构建结果记录到 `evidence/baseline.txt`；关键页 before 截图。

## 2. Lane A · Onboarding + Core 契约

- [x] 2.1 `index.md`（EN 改密句）、`downloads.md` EN+ZH（PG18、EN 改密句）。
- [x] 2.2 `core/events.md` EN+ZH（25 kind / 4 class / run.finished key / 锚点 / capabilities）。
- [x] 2.3 `core/agentapi.md` EN+ZH（路由补齐、锚点、plugins 字段、201/200/202、capabilities）。
- [x] 2.4 `core/extend/provider-adapter.md` EN+ZH（file_id > url > base64）。
- [x] 2.5 `glossary.md` EN+ZH（kind 表、插件/创造模式/bundle 词条）。
- [x] 2.6 小项：`core/extend/index.md`、`core/extend/service-descriptor.md`、`core/index.md`、
      `harness/recovery-and-compaction.md`（8/8）、`harness/streaming-and-partial.md`（202 queued）。

## 3. Lane B · 插件标准 + Harness 产品面插件段

- [x] 3.1 `harness/plugins/{index,manifest,kinds}` EN+ZH：planned → 已实现/部分/Deferred；
      manifest 字段表补齐；sidecar 只 tool_provider；builtin 全列。
- [x] 3.2 `harness/plugins/{lifecycle,permissions,ui,testing}` EN+ZH：状态机/审批/桥/测试覆盖；
      保留 fs 未中介、sidecar 预算、SIGKILL sweep 的诚实边界。
- [x] 3.3 `harness/creation-mode.md` EN+ZH：6 动作/默认启用/模板；G8 Deferred 明示。
- [x] 3.4 `harness/{index,architecture,operations,approvals-and-questions}.md` EN+ZH：
      三档状态、builtin 全列、受管插件根/安装 API/参数表、TUI 口径、插件审批。

## 4. Lane C1 · Core 架构/存储 + 新页（含 sidebar）

- [x] 4.1 `core/architecture.md` EN+ZH：Loadout and assembly（四层/row id/config.toml）。
- [x] 4.2 `core/storage.md` EN+ZH：RLS 默认开、v2 表清单、GC/tree/file manifest/缩略图、8/8。
- [x] 4.3 新页 EN+ZH：`core/assembly`、`core/placement`、`core/tenancy`。
- [x] 4.4 `site/.vitepress/config.mts`：core/harness sidebar 增加本 change 全部新页
      （assembly/placement/tenancy/files/session-query/release/plugins/slots）。

## 5. Lane C2 · 缺失页（不含 sidebar）

- [x] 5.1 新页 EN+ZH：`core/files`、`core/session-query`、`harness/release`、`harness/plugins/slots`。

## 6. Lane D · 仓库内二次污染源

- [x] 6.1 `docs/HARNESS.md`（插件三档）、`docs/WEBUI.md`（tui target/版本）、`README.md` 与
      `docs/LOADOUT.md`（8/8）、`docs/MATURITY.md`（状态头）、`docs/FILE-PROTOCOL.md`（实现状态）、
      `web/src/screens/plugins.tsx` 注释（API 已实现、客户端卡片待接线）。

## 7. 收口与验收

- [x] 7.1 四道门全绿：`check-links`、`parity`、`check-citations`、`check-en-cjk`；
      `pnpm --dir site build` 成功。
- [ ] 7.2 部署：site.yml 成功；关键页公网 200 且为新内容；after 截图到 `evidence/after/`。
- [ ] 7.3 vision 子代理复核关键页（架构总览/harness 总览/插件标准/下载页，四档/双端）；P0/P1 清零。
- [ ] 7.4 归档本 change（docs-only；按 site 规格合入或 `--skip-specs`）。

## 8. 已知边界（本轮如实登记，不在本 change 修）

- **槽位 id 契约漂移**：`internal/pluginpkg/manifest.go:329` 的保留槽位含 `keel.tasks.widget`、
  无 `keel.ui.top-band`；web `BASE_SLOTS` 有 `top-band`、无 `tasks.widget`。两处都在线，
  文档按现状分别描述，未宣称一致；后续以 spec 决策统一。
- **search/export 不对称**：消息搜索只匹配顶层 `payload->>'text'`（`session_query_pg.go:64,88`），
  runner 形态的 assistant 消息可导出但不可被搜索；页面已写明，修复需产品决策。
- **layerfs GC 未排程**：`Store.Collect`（24h 保留）在 `cmd/keel`/`cmd/runtime` 无调用方；
  页面按「维护步骤」描述，不宣称自动回收。
- **net/secrets 权限 gate 未接线**：`NewPermissionedBroker` 仅测试调用；官网按 Partly implemented。
