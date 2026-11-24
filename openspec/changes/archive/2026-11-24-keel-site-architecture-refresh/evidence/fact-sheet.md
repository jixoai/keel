# 官网刷新事实表（审计产出，2026-11-24）

> 来源：子代理逐页审计（只读）+ 仓库代码。所有 `path:line` 是当前 main 的事实；
> 改写时引用必须仍指向这些位置（引用门只查行存在，语义由本表保证）。

## 0. 共享事实（所有 lane 以此为准）

- 产品版本 `0.2.0`（`cmd/keel/main.go:23`）；tag v0.2.0；PG **18**（`README.md:70`、
  `internal/store/schema.sql:1`、`.github/workflows/ci.yml:19`）。
- 首次引导：bootstrap 设置的密码即最终密码，`mustChangePassword:false`，web 直接进入；
  只有 CLI/管理员重置的临时密码强制改密（`cmd/keel/auth.go:560-572`、`docs/HARNESS.md:33`）。
- 事件 kind：**25 个注册**（`internal/kernel/validate.go:46-85`），提交类 **4 个**
  （`ClassOwner/ClassEdge/ClassExternal/ClassSystem`，`internal/kernel/kernel.go:285-314`）；
  `run.finished` 带 `KeyRun`（`validate.go:53`）；`run.started/run.failed` 仍 reserved。
- 输入响应：新建 **201**、重复 **200**、组满排队 **202 + queued{position,etaSeconds}**；
  429 只给租户配额/内存上限（`internal/httpapi/server.go:497-511`、`:924-961`）。
- RLS：**默认开启**，所有租户表 `ENABLE + FORCE ROW LEVEL SECURITY` + 非 owner 角色；
  `KEEL_RLS=off` 是显式逃生门（`cmd/keel/serve.go:306-308`、`internal/store/rls.go:17-45`）。
- 插件标准（全部已实现，除特别标注）：
  - `plugin.yaml` 严格包格式，字段含 `id/version/kind/layer/description/engines/provides/
    contributes/extensibility/requires/permissions/entry/source`（`internal/pluginpkg/manifest.go:142-170`、`:636`）；
  - 加载/校验 `pluginpkg.LoadDir`（`package.go:74`）、`extensibility.json`（`extensibility.go:107`）；
  - 生命周期 `Install/Enable/Disable/Uninstall/Update`（`internal/harnessext/plugin_manager_ops.go:504`、`:656`、`:778`、`:817`、`:987`）；
    状态含 `installed/enabled/disabled/failed/invalid/uninstalled`（`plugin_manager.go:29-49`）；卸载需先 disable；
  - API：`GET /v1/plugins`、`POST /v1/plugins/install`、`GET /v1/plugins/audit`、
    `POST /v1/plugins/{id}/enable|disable|uninstall|update`（`internal/harnessapi/plugins.go:56-62`）；
  - 安装/启用/升级走 durable approval：202 decision → 回答 → 带 decisionId 重试 201（`plugin_approval.go:15-17`、`:111-190`）；
  - 权限：`permissions.tools` 经 `Manager.InvokePluginTool` 强制、sidecar env 白名单强制、审计写 `plugin_audit`；
    **net/secrets 的 gate 已就绪但生产未接线（`NewPermissionedBroker` 仅测试调用）**——2026-11-24 Lane B 实证修正；
    **`permissions.fs` 仍只声明、不中介**（`internal/harnessext/plugin_permissions.go:19-56`）；
  - sidecar 当前只支持 **tool_provider** 连接；`health` 不在 tool surface；OAR（agent_backend）sidecar 仍 Deferred
    （`plugin_sidecar.go:22-53`、`plugins/keel.oar/plugin.yaml:1-10`）；
  - webview UI：`/plugins/<id>/*` 只服务 enabled 插件（`cmd/keel/plugins_ui.go:17-40`、`:54`）；
    `keel.plugin.*` 四动词 state/call/approve/subscribe + theme tokens（`internal/harnessapi/plugin_bridge.go:1-13`、`:75-81`）；
  - 槽位/贡献/extensibility：`docs/PLUGIN-EXTENSIBILITY.md:1-10`、`:24-60`；
    `internal/harnessext/plugin_extension.go:64-113`；`internal/pluginpkg/manifest.go:210-231`；
  - 创造模式：`keel.creation` 内置默认启用（`internal/pluginpkg/builtin/keel.creation.yaml:1-17`），
    6 个动作 catalog/scaffold/validate/install/list/remove（`internal/harnessext/creation.go:91-96`、`:119`），
    webview+react+solid-glass 模板存在（`plugins/templates/webview-react-solidglass/`）；
    **G8 端到端仍 Deferred**（归档 DISPOSITION:20），不得写成完全验收；
  - 覆盖矩阵/市场：`plugins/COVERAGE.md`、`plugins/keel.plugin-market/catalog.json`（机读真源，
    `docs/PLUGIN-CATALOG.md:1-30`）。
- 内置插件不止 3 个：`keel.file/model/bot/team-bot/spill/tasks/shell/approval/notify/creation/example/testkit`
  （`internal/pluginpkg/builtin/`、`cmd/keel/serve.go:1660-1725`）。
- Provider 文件：持久层存 `ImageRefContent{blobId}`/`MediaRefContent`，出站按
  **file_id > url > base64** 解析（`internal/agentcore/content.go:148-166`、`:179-228`；
  `internal/provider/media.go:52-79`；`internal/provider/imagefiles.go:7-30`；
  `openspec/specs/provider-files/spec.md:6-25`）；handle cache + TTL 已实现。
- Assembly：`internal/assembly` 四层（base → mode → preset → config.toml）+ row id overlay
  （`docs/ASSEMBLY.md:20-78`、`:201-217`；`internal/assembly/build.go:138`；`cmd/keel/main.go:167`）。
- Placement：组槽位/租户配额/queued 行/ETA（`internal/placement/types.go:1-26`、
  `openspec/specs/harness-core/spec.md:24-56`、`internal/httpapi/server.go:924-961`）。
- Session query：`GET /v1/sessions/search`、`GET /v1/sessions/{id}/export`、
  `/deliverables`、`/task-panel`、`/file-preview`（`internal/harnessapi/session_query.go:177-178`、`:549`；
  `task_panel.go:142-144`）。
- 表清单含 v2 新增：`groups/session_placement/file_manifest/session_snapshot/task_cards/task_events/plugins/plugin_audit/notifications`
  （`internal/store/schema.sql:894/935/855/871/749/773/582/603/824`）。
- Blob：TTL GC/orphan CAS 扫描/缩略图/file_manifest/tree（`docs/BLOB-LIFECYCLE.md:1-80`；
  `internal/blob/gc.go:76-142`；`internal/fileplugin/service.go:71`、`:353`）。
- TUI：`nsh package` **没有 tui target**（`web/README.md:167`）；开发用 `nsh run --target tui`，
  发布只产 `keel` + web zip（`release.yml:64-120`）；`KEEL_TUI_BIN` 指向用户自备 TUI。
- E2E core：**8 个 case**（`cmd/e2e/main.go:86-95`），不是 7/7。

## 1. 页面级问题（按 lane）

### Lane A：Onboarding + Core 契约

- `index.md`（EN only）：`:79-80` 英文仍写浏览器强制改密；ZH 已正确 → 对齐。
- `downloads.md` EN+ZH：PG **14+ → 18**；EN `:70` 首登改密句删除（ZH 已正确）。
- `core/events.md` EN+ZH：§1 8 kind → 25 kind 完整表（class/key/emitter）；
  §2 3 类 → 4 类（system lane）；§3 `run.finished` key 改为 `KeyRun`；
  `validate.go:36` 锚点 → `:46`；§9 capabilities 同步 agentapi。
- `core/agentapi.md` EN+ZH：路由参考补插件生命周期/审计/桥、`/sessions/search`、`/export`、
  `/deliverables`、`/task-panel`、`/file-preview`、`/v1/quota`、`/v1/admin/tenants`；
  `api.go:167` 锚点 → 路由注册 `:377`；`/v1/plugins` 示例补当前字段；input 响应 201/200/202；
  capabilities 示例补 `plugins.*`、`session.search/export/deliverables`、`task.panel`、
  `settings.skills`、`mcp.*`、`configFile`；英文强制改密句删除。
- `core/extend/provider-adapter.md` EN+ZH：删除 §6「未实现」整段，改写 file_id/url/base64 顺序。
- `glossary.md` EN+ZH：事件 kind 表更新；插件/创造模式/bundle 词条更新（bundle → assembly）。
- `core/extend/index.md` EN+ZH：扩展地图补「插件包/槽位/贡献」「assembly row 覆盖」两行。
- `core/extend/service-descriptor.md` EN+ZH：加一句「服务描述符之上还有 assembly row」。
- `core/index.md` EN+ZH：MATURITY 引用改为「按当前 change evidence」并注明旧矩阵不再唯一。
- `harness/recovery-and-compaction.md`：7/7 → 8/8。
- `harness/streaming-and-partial.md` EN+ZH：补一句组满 202 queued（非 429）。

### Lane B：Plugin 标准 + Harness 产品面插件段

- `harness/plugins/index.md`（L）：整页「planned」→ 已实现/部分/Deferred 三档。
- `harness/plugins/manifest.md`（L）：首句删除；字段表补齐；安装 API 行为；删除「What exists today instead」。
- `harness/plugins/kinds.md`（M）：三 kind 状态；sidecar 方法表改为 tool_provider 面（无 health/close request）；
  builtin 列表；TUI 用 openUrl。
- `harness/plugins/lifecycle.md`（M）：状态机补 failed/invalid/uninstalled；audit 已实现；区分后端 API 与客户端卡片滞后。
- `harness/plugins/permissions.md`（M）：已实现 + 明确边界（fs 未中介、sidecar 自身 socket/CPU 预算、SIGKILL sweep）。
- `harness/plugins/ui.md`（M）：`/plugins/<id>/` 与四动词桥已实现；theme tokens；TUI openUrl。
- `harness/plugins/testing.md`（M）：测试要求/覆盖矩阵已实现；指向 `plugins/COVERAGE.md` 与 `scripts/plugin-catalog.mjs`。
- `harness/creation-mode.md`（M）：已实现 6 动作/默认启用/模板；G8 Deferred 明示。
- `harness/index.md`（L）：§5 三档；`/plugins/<id>/` 已实现；input 201/200/202。
- `harness/architecture.md`（M）：§3 插件系统；builtin 全列；assembly/placement 指路。
- `harness/operations.md`（M）：§3 受管插件根 + 安装 API + audit；参数表补 `--config-file/--plugins-root/KEEL_PLUGINS_ROOT/KEEL_HOME`；
  TUI 段改为 `nsh run --target tui`/自备 TUI。
- `harness/approvals-and-questions.md`（S）：§6 插件审批/桥已实现。

### Lane C：Core 架构/存储 + 新页 + 导航

- `core/architecture.md` EN+ZH：§4 改「Loadout and assembly」，补四层/row id/config.toml。
- `core/storage.md` EN+ZH：RLS 段（默认开、可显式关、app.tenant_id）；表清单补 v2 分组；
  blob 段补 GC/tree/file manifest/缩略图；7/7 → 8/8。
- 新页（EN+ZH）：`core/assembly.md`、`core/placement.md`、`core/tenancy.md`、`core/files.md`、
  `core/session-query.md`、`harness/release.md`、`harness/plugins/slots.md`（内容以事实表为准）。
- 导航：`site/.vitepress/config.mts` 的 `coreSidebarEn/Zh`、`harnessSidebarEn/Zh` 增加上述页面。
- 交叉链接：architecture→assembly/placement；storage→tenancy/files；plugins/*→slots；operations→release。

### Lane D：仓库内二次污染源（不在 site/）

- `docs/HARNESS.md:98`：插件状态从「规划中」改为已实现/Deferred 三档。
- `docs/WEBUI.md:24`：`nsh package --target tui` → `nsh run --target tui`；`:47` nsh 版本 → 0.6.1。
- `README.md:36`、`docs/LOADOUT.md:168`：7/7 → 8/8。
- `docs/MATURITY.md:34-43`：加状态头（v1 矩阵，当前事实见 mainline/evidence），或按事实表更新。
- `docs/FILE-PROTOCOL.md`：加「实现状态：file_id/url/base64 已实现」头。
- `web/src/screens/plugins.tsx:1-10`：注释改为「后端安装 API 已实现；客户端安装/更新卡仍待接线」。
