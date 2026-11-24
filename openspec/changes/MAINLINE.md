# MAINLINE — Keel 程序板

> Owner 2026-11-22 裁决。本文件是唯一程序板（canonical board）；任务真值仍是各 change 的
> `tasks.md` + 证据。勾选数只表示板面状态，不表示本文件重新验收过实现。
> 更新：2026-11-25（v0.3.1 Windows 启动修复）· 分支：`main` · 基线：`44e774b`

## 当前状态（板面快照）

| Change | 勾选 | 状态 | 处置 |
|---|---:|---|---|
| `keel-harness-v2` | 133/174 | **已归档 2026-11-24（v0.2.0 已发布）** | 收口门与 G11 证据在 `archive/2026-11-24-keel-harness-v2/`；不再推进 |
| `runtime-foundation` | 39/47 | 已折叠（8 残余由 W11 关闭） | 见 `FOLDED.md` 2026-11-24 收口记录；不单开线 |
| `keel-ecosystem` | 4/72 | 冻结参考/backlog | Owner 2026-11-22；解冻前先做 PI parity 审计（多数能力已由 v2 以不同形态交付） |
| `keel-agent-team-v1` | 61/62 | 已被取代 | 归档；最后 1 条设计任务不做 |

已交付/已归档的锚点（只作事实引用，不重复验收）：

- **v0.3.0 自包含发布（2026-11-25）**：tag `v0.3.0`；五平台单体二进制内嵌 PostgreSQL 18 +
  web 客户端；macOS `keel install` 生成 ad-hoc 签名 `.app`（不公证），`keel start` 零配置；
  发布资产与 SHA256SUMS 已出，下载页/首页/README 已切一键路径。v0.3.1 修复 Windows 内嵌 PG 的 `.exe` 查找与双击无参注册/启动（Windows 实机待 Owner 复验）。

- 身份/Bot/TeamBot/共享/审计：`keel-bot-identity-access` 2026-11-22 归档，A6 10/10 步、
  304/304 断言、P0/P1/P2=0；见 `.agents/docs/E2E-REPORTS.md` 2026-11-22 条目。
- `keel-harness-v2` 2026-11-24 归档：W11 收口门 E0/G0–G11 的证据、G11 活实例报告
  （10 步/10 断言/212 文件审计，复核 8.5/10）与 41 项未勾任务的逐条处置见
  `archive/2026-11-24-keel-harness-v2/DISPOSITION.md`；v0.2.0 于同日发布
  （tag `38a0713`，六资产 + SHA256SUMS 全量校验，keel.jixoai.com 已切版）。
- 任务→bash→capture→任务面板：W3/W4 主体已勾（面板已交付）。
- Web + TUI：W7/TUI 证据在；W10 触发器已勾；W12 UI 槽位已交付（`evidence/w12-ui-slots.md`）。
- 恢复：W8 子代理 resume 已交付；W11 G2 的 kill -9/容器重启端到端证据由 W2/W12 recovery
  切片的真实 SIGKILL 测试补强；G1 覆盖表 30 台机器 0 gap。

v0.2.0 发布后仍开的口子（归档板 41 项，按去向分组）：

1. **Alpha 残余（已关闭并归档 2026-11-24）**：OAR 专用渲染器 + 宿主任务图闭环由 `keel-oar-renderer` 交付
   （投影读取 API `GET /v1/sessions/{id}/external/projection`、外部运行卡经
   `keel.ui.tool-renderer`、任务图前后计数不变；reviewer 9/10）。
   R1 v2 已绿（pin e7361f8 / scenario r1-basic-v2 / 77 allow rules；152 leaf paths、
   103 allowed、0 unexpected；负控证明 allowlist 不能吞掉全部差异），仅 E0 的容器电池
   平台受限；G4 300MB 目标/组 OOM 同属平台受限（468MB ≤500MB 已记 PARTIAL）。
2. **官网 v2 刷新（已交付并归档 2026-11-24）**：`keel-site-architecture-refresh` 把 37 对中英文页面按
   harness v2 重写（插件标准 implemented/partly/deferred、事件 25 kind、API 路由补齐、
   新增 assembly/placement/tenancy/files/session-query/release/slots），四道门与部署全绿。
3. **自包含分发（P0 体验，已交付并归档 2026-11-25）**：`keel-self-contained` 把 PostgreSQL 18 +
   web 客户端内嵌进单二进制；`keel start` 零配置起服务，macOS `keel install` 生成 ad-hoc 签名
   的 `.app`（不公证）；release 矩阵按平台产出单体二进制。macOS 已实测，Linux/Windows 同机制待跑。
4. **客户端壳（P2，已交付并归档 2026-11-24）**：`keel-client-shell` 声明 `singleInstance` +
   `keel://` scheme；session/decision/settings 深链、托盘、`mod+shift+k` 唤回；桌面 host 生效，
   web/TUI no-op。macOS 包/单实例/深链分发已实测，托盘/快捷键视觉未验（截图受限）。
5. **客户端编辑器（P2，已交付并归档 2026-11-24）**：`keel-client-editor` 把 web Composer 换成
   `RichEditor`（Markdown 快捷输入、HTML 粘贴、Enter 发送/Shift+Enter 换行、5 行封顶），
   用户气泡按 Markdown 渲染、会话预览剥标记；TUI/原生保留纯文本。
6. **本机 PG 零配置（已交付并归档 2026-11-24）**：`keel-dev-pg` 先以脚本补洞，随后由
   `keel-db-cli` 收进产品 CLI：`keel db start|stop|status|dsn|env|reset`（默认
   `$KEEL_HOME/dev-pg`，仅回环、随机 0600 密码、PG18 门、reset 防护）；`keel serve`/`keel admin`
   在无 `KEEL_DSN` 时自动初始化/启动本地库（`--no-auto-db`/`KEEL_NO_AUTO_DB=1` 可关闭）；
   README/RUNBOOK/官网已改为自动初始化优先，`scripts/dev-pg.sh` 删除。
7. **客户端产品化（进行中：壳/编辑器/通知已交付，updater/perf 未开始）**：`nsh/updater`
   自更新、签名 OTA、1000 条消息长会话性能与离屏增量创建。
8. **冻结 backlog（未开始/部分）**：插件生态（checkpoint、super-agent、creation 聚合、
   attachment/commands/permission/feedback、subprocess/terminal、bundles/presets、
   telemetry OTLP、testkit、credentials/storage/identity、keel.oar、ui-* 槽位内容、
   gbash mcp CLI、fileplugin 锁回收）；多租户插件面；macOS 原生 target；Rust+gRPC layerfs。
9. **运维/测试债**：聚合仓 `FDE2E_RELEASES_TOKEN` 未配置（镜像跳过）；父仓 8 个子模块检出
   待同步（browser 有他人 WIP）；TUI 设置页 raw JSON 1 例失败；headless 模式 4 个布局差异。

**下一阶段没有活跃主线**：任何新工作先按 OpenSpec 立项（或经 Owner 解冻
`keel-ecosystem`），不得直接在归档板上继续勾选。候选排程（P1 Alpha 残余 →
P2 客户端产品化 → P3 解冻审计 → P4 运维债）在本地报告
`.agents/reports/keel/remaining-plan-2026-11-24/PLAN-AFTER-V0.2.0.md`（报告目录不入库，
上方的四组口子即入库摘要）。

## Owner 裁决（2026-11-22，记录，不再讨论）

1. `keel-harness-v2` 是唯一活跃主线；其他 change 不得并行推进。
2. `runtime-foundation` 残余折叠进 W11 G4/G10（RLS/quota/RSS/chaos/cleanup），不单开线。
3. `keel-ecosystem` 冻结为参考/backlog；只有 W11 实际需要的条目才可后续取用。
4. `keel-agent-team-v1` 已被交付的 Bot/TeamBot 模型取代；最后一条设计任务不完成。
5. Alpha 范围是垂直切片（见下）；范围外一律 defer，不扩线。
6. Skill 脚本默认允许（Owner override）；不新增 deny-by-default。

## Alpha 垂直切片（只做这些）

- 身份/Bot/TeamBot/共享/审计。
- tasks → bash → capture → task panel。
- 一个 OAR 外部工具（Codex）。
- UI slots + 一个 Native chunk 插件。
- 重启恢复 + memory gate + resource cleanup。
- Web + TUI。

## 活动切片与依赖顺序

```text
W0.5 插件全景/扩展面冻结
  -> W9 OAR（Codex；实现前先冻结 OAR 契约，W0-6）
  -> W12 slots + Native chunk
  -> W2/W12 recovery（组恢复、supervisor、配置化 resume）
  -> W11 收口门（E0/G0-G11）
```

- **W0.5**：catalog id/status/wave、layer、四层装配、seam、extensibility 格式冻结；后续
  slots 和 OAR 都依赖它。
- **W9**：OAR 契约冻结 + sidecar + record stream + 外部工具渲染器；只接 Codex 一条真链路。
- **W12 slots**：`keel.ui` 槽位机制 + 基础槽位 + 四形态 + `chunk` 验收；只要求一个 Native
  chunk 插件，不铺 42 个 W12 插件。
- **W2/W12 recovery**：组槽位/排队或 429 口径、组 OOM、supervisor/升级阶梯、resume 配置；
  复用已交付的 W8 subagent resume，不重做。
- **W11**：按 `acceptance.md` 的 E0→G0→G1→…→G11 顺序收口；前置门未绿，后续门不成立。

## 明确 Defer（Alpha 不做）

- macOS 原生打包/签名/更新器（保留 web/TUI；macOS target 产物后置）。
- layerfs Rust + gRPC（Phase A Go CAS 继续用；ADR 保留为扩展实现）。
- 多租户插件面（`plugin_state(tenant_id,...)`、`/v1/plugins` 租户过滤）。
- super-agent。
- 完整 spill / compaction / checkpoint / prompt-cache。
- bundles / presets。
- 全部 42 个 W12 插件（只做 slots + 一个 Native chunk 验收所需的条目）。

## 规则

- **只有 `keel-harness-v2` 前进**；`runtime-foundation`/`keel-ecosystem`/`keel-agent-team-v1`
  不再勾任务、不再开分支、不再单独验收。
- 任何新需求先判断是否属于 Alpha 垂直切片；不属于则登记到本文件 Defer 或 `FROZEN.md`。
- 本文件不替代 `tasks.md`；勾选/证据变更回各 change，程序级状态只改本文件与
  `.agents/docs/MAINLINE-STATUS.md`。
- 归档不改变历史：`keel-agent-team-v1` 以 `--skip-specs` 归档，不把旧 AgentTeam 规格写进
  `openspec/specs`。

## 链接

- `openspec/changes/archive/2026-11-24-keel-harness-v2/tasks.md`
- `openspec/changes/archive/2026-11-24-keel-harness-v2/DISPOSITION.md`
- `openspec/changes/archive/2026-11-24-keel-harness-v2/acceptance.md`（验收判据）
- `openspec/changes/runtime-foundation/FOLDED.md`
- `openspec/changes/keel-ecosystem/FROZEN.md`
- `openspec/changes/archive/2026-11-22-keel-agent-team-v1/SUPERSEDED.md`
- G11 报告：https://local-kzf-keel.fde2e.com/report.html
- OAR + 官网验收包：https://local-kzf-keel-oar.fde2e.com/report.html
- `openspec/changes/archive/2026-11-24-keel-oar-renderer/`（投影 API + 外部运行卡 + 任务图证明）
- `openspec/changes/archive/2026-11-24-keel-site-architecture-refresh/`（官网 v2 刷新，37 对页面）
